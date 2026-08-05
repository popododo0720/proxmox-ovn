# Raft membership operations

PVN has three independent OVSDB Raft clusters: `PVN_Control`,
`OVN_Northbound`, and `OVN_Southbound`. A PVE node can be a voter in all
three, but changing one membership does not change the other two. There is no
atomic operation across them.

## Read-only status

`pvnctl central status` is specifically a Raft-voter check. It is expected to
exit nonzero for the supported one-node standalone layout because standalone
databases do not implement the `cluster/status` command. Use `pvnctl doctor`
and systemd service status for that layout.

Run this locally on a selected central voter:

```sh
pvnctl central status
```

The defaults are the exact sockets installed by PVN and OVN 25.03:

- `/run/pvn-control/pvn-control-db.ctl` for `PVN_Control`;
- `/run/ovn/ovnnb_db.ctl` for `OVN_Northbound`; and
- `/run/ovn/ovnsb_db.ctl` for `OVN_Southbound`.

Override a path only when inspecting a deliberately nonstandard process:

```sh
pvnctl central status \
  --pvn-control-ctl /other/control.ctl \
  --ovn-nb-ctl /other/ovnnb_db.ctl \
  --ovn-sb-ctl /other/ovnsb_db.ctl
```

The command always emits one JSON result for each database. It exits zero only
when every unixctl request succeeds and every local database reports all of
these directly observable conditions:

- local status is `cluster member`;
- role is `leader` or `follower`, with a known leader;
- cluster ID, server ID, term, address, and member roster are present; and
- the local server plus connected members meet the roster's majority; and
- no add/remove membership transition is visible.

This is a conservative local readiness check, not a synthetic write test. A
membership transition or election can produce a temporary nonzero result.
Run it on more than one voter before maintenance.

## Why leave and kick are not pvnctl subcommands

`cluster/leave` and `cluster/kick` are irreversible membership mutations. A
server that leaves cannot reuse that database file to rejoin; it must be
created as a new server. More importantly, PVN cannot make the three database
changes atomic. A convenience command that continued after one partial
failure could silently leave the control plane with different voter sets.

For that reason `pvnctl` intentionally provides status only. Use the guarded,
one-database-at-a-time procedures below during a maintenance window.
The UUID in `/etc/pve/pvn/config.json` (`cluster.id`) identifies the PVN
installation and is not any OVSDB Raft cluster ID. Each database has its own
Raft cluster UUID, returned by `cluster/cid` and by `pvnctl central status`.

## Planned removal: leave on the departing voter

First drain PVN ports and gateway work from the node, take independent backups
of all three databases, and verify all three clusters healthy from the
departing node and at least one survivor. If possible, join a replacement
voter before removal. A stable three-voter cluster reduced directly to two
voters tolerates no further failure.

Record the full `cluster_id` value for each database from the JSON report.
Then, on the departing voter, perform and verify one database at a time. Replace
each placeholder with the exact recorded UUID:

```sh
expected_pvn_cid=PVN_CONTROL_RAFT_CLUSTER_UUID
actual_pvn_cid=$(/usr/bin/ovs-appctl --timeout=10 \
  --target=/run/pvn-control/pvn-control-db.ctl cluster/cid PVN_Control)
[ "$actual_pvn_cid" = "$expected_pvn_cid" ] || exit 1
/usr/bin/ovs-appctl --timeout=10 \
  --target=/run/pvn-control/pvn-control-db.ctl cluster/leave PVN_Control
/usr/bin/ovsdb-client --timeout=60 wait \
  unix:/run/pvn-control/pvn-control-db.sock PVN_Control removed
```

Check `pvnctl central status` on a surviving voter and confirm only
`PVN_Control` has the intended smaller roster. Stop if it does not. Then repeat
the same guarded operation for Northbound:

```sh
expected_nb_cid=OVN_NORTHBOUND_RAFT_CLUSTER_UUID
actual_nb_cid=$(/usr/bin/ovs-appctl --timeout=10 \
  --target=/run/ovn/ovnnb_db.ctl cluster/cid OVN_Northbound)
[ "$actual_nb_cid" = "$expected_nb_cid" ] || exit 1
/usr/bin/ovs-appctl --timeout=10 \
  --target=/run/ovn/ovnnb_db.ctl cluster/leave OVN_Northbound
/usr/bin/ovsdb-client --timeout=60 wait \
  unix:/run/ovn/ovnnb_db.sock OVN_Northbound removed
```

Verify Northbound from a survivor, then repeat for Southbound:

```sh
expected_sb_cid=OVN_SOUTHBOUND_RAFT_CLUSTER_UUID
actual_sb_cid=$(/usr/bin/ovs-appctl --timeout=10 \
  --target=/run/ovn/ovnsb_db.ctl cluster/cid OVN_Southbound)
[ "$actual_sb_cid" = "$expected_sb_cid" ] || exit 1
/usr/bin/ovs-appctl --timeout=10 \
  --target=/run/ovn/ovnsb_db.ctl cluster/leave OVN_Southbound
/usr/bin/ovsdb-client --timeout=60 wait \
  unix:/run/ovn/ovnsb_db.sock OVN_Southbound removed
```

After all three survivor checks pass, disable the departing node's central
role. Preserve its database files until backup and replacement membership are
verified, but never start those departed files again as cluster members.

## Failed voter: kick from a healthy survivor

Use `cluster/kick` only when the voter cannot run `cluster/leave`. The remaining
cluster must still be healthy; for a three-voter cluster this means two voters
are up. If quorum is already lost, do not use these commands—follow the OVSDB
manual cluster-recovery procedure instead.

On a healthy survivor, record the failed member's exact Raft address from the
`Servers` roster. Prefer that full address over a short server-ID prefix. Guard
every mutation with that database's full Raft cluster UUID. For example:

```sh
expected_pvn_cid=PVN_CONTROL_RAFT_CLUSTER_UUID
failed_pvn_address=ssl:FAILED_NODE_IP:6646
actual_pvn_cid=$(/usr/bin/ovs-appctl --timeout=10 \
  --target=/run/pvn-control/pvn-control-db.ctl cluster/cid PVN_Control)
[ "$actual_pvn_cid" = "$expected_pvn_cid" ] || exit 1
/usr/bin/ovs-appctl --timeout=10 \
  --target=/run/pvn-control/pvn-control-db.ctl \
  cluster/kick PVN_Control "$failed_pvn_address"
```

Wait for the address to disappear from the surviving `PVN_Control` roster and
re-run `pvnctl central status`. Only then repeat for
`OVN_Northbound` using its recorded cluster UUID, exact failed-node address
`ssl:FAILED_NODE_IP:6643`, and `/run/ovn/ovnnb_db.ctl`. Verify again, then do
`OVN_Southbound` with its own UUID, `ssl:FAILED_NODE_IP:6644`, and
`/run/ovn/ovnsb_db.ctl`. Stop at the first error or unexpected roster.

Do not boot the failed node with its old database files. Rebuild each database
as a new Raft server and join it using the normal central-node bootstrap flow.

## Upstream references

- [OVSDB clustered service model and maintenance](https://www.openvswitch.org/support/dist-docs/ovsdb.7.html)
- [`ovsdb-server` cluster commands](https://www.openvswitch.org/support/dist-docs/ovsdb-server.1.html)
- [`ovsdb-client wait` states](https://www.openvswitch.org/support/dist-docs/ovsdb-client.1.html)
- [OVN 25.03 central control-socket definitions](https://github.com/ovn-org/ovn/blob/branch-25.03/utilities/ovn-ctl)
