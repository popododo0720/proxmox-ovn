# PVN database backup and restore

PVN has three independent OVSDB databases: `PVN_Control`, `OVN_Northbound`,
and `OVN_Southbound`. A backup set contains one standalone-format snapshot per
selected database. It is not, and must never be described as, one atomic
point-in-time snapshot across those databases.

## Create and verify

Run the packaged command as root on a healthy central voter:

```sh
pvn-db-backup create
```

The default root is `/var/backups/pvn`. Both that directory and each published
backup set are root-only mode `0700`; the manifest and snapshots are mode
`0600`. To select databases or another secure absolute destination:

```sh
pvn-db-backup create --output /srv/pvn-backups
pvn-db-backup create --database pvn-control
pvn-db-backup create --database ovn-nb --database ovn-sb
```

Creation fails unless `pvn-central.target` is active and all three local Raft
databases have an odd voter count, full connectivity, a stable cluster ID, and
no membership operation in progress. The same status is checked after capture;
a selected database whose cluster ID or membership changed leaves no partial
backup set. Every snapshot is produced through the live Unix socket with
`ovsdb-client backup`, not by copying a clustered database file.

The command verifies each snapshot before atomically publishing the backup-set
directory. Verify it again after copying it to durable or off-host storage:

```sh
pvn-db-backup verify \
  /srv/pvn-backups/pvn-db-backup-YYYYMMDDTHHMMSSZ-NODE-RANDOM8
```

Verification is offline. It checks strict manifest identities and paths, file
ownership/modes/link counts, size, SHA-256, standalone service model, database
name, schema metadata, full log readability, and a compacted temporary copy.
Keep the whole directory together. Do not edit `manifest.json` or rename an
individual snapshot.

## Why restore is manual

`pvn-db-backup` intentionally has no `restore` subcommand. A local command
cannot prove that writers on every PVE node are frozen, and OVSDB cannot make a
restore atomic across the three databases. A restore also assigns new row
UUIDs and does not recreate ephemeral columns. Treat it as a control-plane
reconstruction, not an ordinary rollback.

If a clustered database service cannot accept connections or has lost quorum,
stop here. Do not replace its clustered database file with the standalone
snapshot. Follow the upstream OVSDB full-cluster recovery procedure, which
builds a new cluster with new cluster/server identities.

## One-database restore runbook

Use one maintenance window and one freshly selected backup set per database.
Never loop over all three restore commands or reuse a set from another window.
The clustered example below creates the set on one healthy voter and copies the
whole set to a different voter that is the selected database's current leader.

1. Record `pvnctl central status` on every voter. Require all three databases
   healthy, every configured voter connected, no membership change, and the
   expected cluster IDs. For the selected database, require exactly one report
   with `role` equal to `leader`; record that node and the common Raft term from
   all reports. Stop if a value differs, the terms differ, or there is not
   exactly one leader.

2. Choose exactly one database and its fixed backup key, snapshot, and local
   Unix socket:

   | Window | Database | Backup key | Snapshot | Live Unix socket |
   | --- | --- | --- | --- | --- |
   | 1 | `PVN_Control` | `pvn-control` | `pvn-control.ovsdb` | `unix:/run/pvn-control/pvn-control-db.sock` |
   | 2 | `OVN_Northbound` | `ovn-nb` | `ovn-northbound.ovsdb` | `unix:/run/ovn/ovnnb_db.sock` |
   | 3 | `OVN_Southbound` | `ovn-sb` | `ovn-southbound.ovsdb` | `unix:/run/ovn/ovnsb_db.sock` |

3. Freeze control-plane writers cluster-wide. On every PVE node, stop the node
   writers/controllers; on every central voter, also stop northd. Keep all
   three OVSDB server services running so the chosen restore transaction can
   replicate through its healthy Raft cluster.

   ```sh
   systemctl stop pvn-manager.service pvn-agent.service ovn-controller.service
   # Run on every central voter too:
   systemctl stop ovn-northd.service
   ```

   Confirm those units are inactive on every applicable node. Block UI/API
   changes and manual OVN writes for the maintenance window. If cluster-wide
   quiescence cannot be proven, do not restore.

   While writers remain frozen, confirm all three database services remain
   active on every voter. On a healthy voter other than the recorded leader,
   create a set containing only this window's database and verify it at the
   source. Copy the whole directory to a new root-only path on the leader and
   verify the destination copy. Use Proxmox's native cluster identity and the
   destination node's pinned host-key file; do not accept a new host key.

   The following clustered example is run on the source voter. Replace the
   three uppercase values and set `database` to exactly one table row above.

   ```bash
   set -euo pipefail
   database=EXACT_DATABASE_NAME
   copy_node=EXACT_LEADER_PVE_NODE_NAME
   copy_ip=EXACT_LEADER_MANAGEMENT_IP

   case "$database" in
     PVN_Control) backup_key=pvn-control ;;
     OVN_Northbound) backup_key=ovn-nb ;;
     OVN_Southbound) backup_key=ovn-sb ;;
     *) exit 1 ;;
   esac

   source_root=/var/backups/pvn
   create_report=$(pvn-db-backup create --database "$backup_key")
   backup_set=$(printf '%s\n' "$create_report" | python3 -c \
     'import json, sys; print(json.load(sys.stdin)["backup_set"])')
   backup_name=${backup_set##*/}
   [ "$backup_set" = "$source_root/$backup_name" ] || exit 1
   case "$backup_name" in
     pvn-db-backup-*) ;;
     *) exit 1 ;;
   esac
   case "$backup_name" in
     *[!A-Za-z0-9._-]*) exit 1 ;;
   esac
   pvn-db-backup verify "$backup_set"

   known_hosts=/etc/pve/nodes/$copy_node/ssh_known_hosts
   copy_root=/var/backups/pvn-independent-$backup_name
   pve_ssh() {
     /usr/bin/ssh -F /dev/null -e none -i /root/.ssh/id_rsa \
       -o BatchMode=yes \
       -o PasswordAuthentication=no \
       -o KbdInteractiveAuthentication=no \
       -o PubkeyAuthentication=yes \
       -o PreferredAuthentications=publickey \
       -o IdentitiesOnly=yes \
       -o NumberOfPasswordPrompts=0 \
       -o StrictHostKeyChecking=yes \
       -o CheckHostIP=no \
       -o VerifyHostKeyDNS=no \
       -o UpdateHostKeys=no \
       -o GlobalKnownHostsFile=/dev/null \
       -o "UserKnownHostsFile=$known_hosts" \
       -o "HostKeyAlias=$copy_node" \
       -o ConnectTimeout=10 \
       -- "root@$copy_ip" "$@"
   }

   pve_ssh "set -eu; umask 077; test ! -e '$copy_root'; \
     install -d -o root -g root -m 0700 '$copy_root'"
   /usr/bin/tar -C "$source_root" -cf - -- "$backup_name" | \
     pve_ssh "set -eu; /usr/bin/tar -C '$copy_root' \
       --keep-old-files --no-overwrite-dir -xpf -"
   pve_ssh "test \"\$(stat -Lc '%U:%G:%a' '$copy_root')\" = root:root:700; \
     /usr/sbin/pvn-db-backup verify '$copy_root/$backup_name'"
   printf 'Verified leader copy: %s:%s/%s\n' \
     "$copy_node" "$copy_root" "$backup_name"
   ```

   `copy_root` must not already exist. Never overwrite, merge into, or resume a
   partially copied destination; after an operator audits any failed transfer,
   use another new path. Keep the verified source set as the independent copy.
   For a one-voter deployment, use a new root-only path on off-host durable
   storage instead of pretending the local filesystem is independent. An older
   set captured before writer quiescence, or a set that exists on only one
   voter, is not sufficient recovery evidence.

4. On every voter, repeat `pvnctl central status` immediately before the
   restore. Require the same unique leader and term recorded in step 1, 3/3
   connected voters, the expected cluster ID, and no membership change. The
   verified set must be present on that leader. If leadership or term changed,
   a status call fails, or any value differs, do not issue the restore and do
   not retry another node automatically. Restart and validate the frozen
   services as in step 6, then schedule a new window and fresh backup set.

   On that leader, verify the selected set again, obtain the expected digest
   from its manifest, and require an exact typed confirmation. Replace all
   uppercase placeholders; do not use `--force` or `--no-leader-only`.

   ```sh
   set -eu
   backup_set=/srv/pvn-backups/EXACT_BACKUP_SET
   database=EXACT_DATABASE_NAME
   case "$database" in
     PVN_Control)
       snapshot=pvn-control.ovsdb
       remote=unix:/run/pvn-control/pvn-control-db.sock
       db_unit=pvn-control-db.service
       ;;
     OVN_Northbound)
       snapshot=ovn-northbound.ovsdb
       remote=unix:/run/ovn/ovnnb_db.sock
       db_unit=ovn-ovsdb-server-nb.service
       ;;
     OVN_Southbound)
       snapshot=ovn-southbound.ovsdb
       remote=unix:/run/ovn/ovnsb_db.sock
       db_unit=ovn-ovsdb-server-sb.service
       ;;
     *) exit 1 ;;
   esac

   pvn-db-backup verify "$backup_set"
   pvnctl central status
   expected_sha=$(python3 - "$backup_set/manifest.json" "$database" <<'PY'
   import json, pathlib, sys
   manifest = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
   matches = [row for row in manifest["databases"] if row["database"] == sys.argv[2]]
   if len(matches) != 1:
       raise SystemExit("selected database is not present exactly once")
   print(matches[0]["sha256"])
   PY
   )
   [ "$(sha256sum "$backup_set/$snapshot" | awk '{print $1}')" = "$expected_sha" ] || exit 1
   printf 'Type RESTORE %s %s: ' "$database" "$expected_sha"
   IFS= read -r confirmation
   [ "$confirmation" = "RESTORE $database $expected_sha" ] || exit 1
   systemctl is-active --quiet "$db_unit" || exit 1
   ```

5. Confirm the chosen database server is still active and the selected
   database still reports this voter as the unique leader at the recorded
   term. Then, on that leader only, submit exactly one atomic database restore
   transaction through its local Unix socket:

   ```sh
   /usr/bin/ovsdb-client --timeout=120 restore \
     "$remote" "$database" < "$backup_set/$snapshot"
   ```

   Stop immediately on any error. Do not attempt another database, retry a
   different voter, or switch to a comma-separated SSL remote. A lost response
   can be ambiguous even though the restore transaction itself is atomic;
   preserve the set and operator log, then recover services and inspect state
   before deciding whether a later window is needed.

6. Restart northd on every central voter, then the controller and PVN services
   on every transport node:

   ```sh
   systemctl start ovn-northd.service
   systemctl start ovn-controller.service pvn-manager.service pvn-agent.service
   ```

   Re-run `pvnctl central status` on multiple voters. On every node, require
   `pvnctl doctor`, `systemctl restart pvn-node-ready.service`, the agent
   `/healthz` check, and a deliberate test-port attach/detach plus security
   policy/dataplane test to pass before reopening API/UI changes. Preserve the
   fresh backup and complete this gate before scheduling a separate maintenance
   window, with another fresh frozen backup, for another database.

## Database-specific recovery intent

- Restore `PVN_Control` first when desired state itself is damaged. Prefer to
  leave both OVN databases in place and let normal reconciliation converge them
  to the restored desired state.
- Restore `OVN_Northbound` only for confirmed Northbound corruption. Treat
  `PVN_Control` as authoritative and verify reconciliation before considering
  any Southbound action.
- Restore `OVN_Southbound` only for confirmed Southbound corruption. Northd and
  controllers normally repopulate operational state after writers restart.

Even if three snapshots share one backup-set directory, restoring one does not
make either of the other databases part of the same transaction. Preserve the
pre-restore backup and operator logs until reconciliation and dataplane tests
are complete.
