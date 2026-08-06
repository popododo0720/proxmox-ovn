# PVN database backup and restore

PVN has three independent OVSDB databases: `PVN_Control`,
`OVN_Northbound`, and `OVN_Southbound`. A backup set contains one
standalone-format snapshot per selected database. It is not an atomic
point-in-time snapshot across databases.

## Backup modes

`pvn-db-backup create` has two deliberately different modes:

| Mode | Purpose | Where it may run | Restore gate |
| --- | --- | --- | --- |
| default | routine, independently consistent backup | any healthy central voter; the three database leaders may be on different nodes | rejected by `pre-restore` |
| `--recovery-window` | fresh evidence for one manual restore | the selected database's current local leader, with writers frozen | accepted by `pre-restore` |

Both modes record the source server ID, Raft address, role, term, leader,
membership, capture interval, schema identity, size, and SHA-256 for every
snapshot. A default backup checks stable cluster identity and membership but
does not require the local node to lead every selected database. A recovery
backup requires one explicit database and unchanged local leadership for its
entire capture. Both modes currently require live clustered Raft databases;
standalone-format one-node database services are not supported.

## Routine backup and offline verification

Run as root on a healthy central voter:

```sh
pvn-db-backup create
pvn-db-backup create --database pvn-control
pvn-db-backup create --database ovn-nb --database ovn-sb
pvn-db-backup create --output /srv/pvn-backups
```

The default root is `/var/backups/pvn`. The root and each published backup set
are root-only mode `0700`; manifests and snapshots are mode `0600`. Creation
requires `pvn-central.target` active, an odd voter count, full connectivity,
stable cluster IDs and membership, and no membership operation. Snapshots are
read through each live Unix socket with `ovsdb-client backup`; clustered files
are never copied directly.

Copy the whole published directory to durable off-host storage and verify that
copy offline:

```sh
pvn-db-backup verify \
  /srv/pvn-backups/pvn-db-backup-YYYYMMDDTHHMMSSZ-NODE-RANDOM8
```

Verification checks the manifest, secure paths and modes, size and SHA-256,
database/schema identity, standalone service model, full log readability, and
a compacted temporary copy. Current verification also reads legacy v1 sets,
but only a current v2 `recovery-window` set can pass `pre-restore`. Never edit
`manifest.json`, rename one snapshot, or separate files in a set.

## Restore boundary and release gate

There is intentionally no automatic `restore` subcommand. A local program
cannot prove that writers are stopped on every PVE node, and no transaction
can restore all three databases atomically. `ovsdb-client restore` can assign
new row UUIDs and does not recreate ephemeral state. Treat a restore as
control-plane reconstruction.

Before scheduling a window, record the installed package and binary versions
on every node:

```sh
dpkg-query -W -f='${Version}\n' pvn-node
pvnctl version
```

All nodes must run one approved release that contains
`pvnctl recovery reconcile-ovn`. Before an `OVN_Northbound` restore, that
release must also contain restored-row UUID adoption and restored-UUID
reference support in the OVN renderer. Do not infer that capability merely
from the presence of this document; update the whole cluster first if the
installed release predates it.

If a database service cannot accept connections or has lost quorum, stop. Do
not replace its clustered file with a standalone snapshot. Use the upstream
OVSDB full-cluster recovery process, which creates new cluster and server
identities.

## One-database recovery window

Use a separate maintenance window and a freshly captured set for each
database. Never loop over the table or reuse a set from an earlier window.

| Database | Backup key | Snapshot | Live Unix socket | Raft port |
| --- | --- | --- | --- | --- |
| `PVN_Control` | `pvn-control` | `pvn-control.ovsdb` | `unix:/run/pvn-control/pvn-control-db.sock` | 6646 |
| `OVN_Northbound` | `ovn-nb` | `ovn-northbound.ovsdb` | `unix:/run/ovn/ovnnb_db.sock` | 6643 |
| `OVN_Southbound` | `ovn-sb` | `ovn-southbound.ovsdb` | `unix:/run/ovn/ovnsb_db.sock` | 6644 |

The runbook applies only to clustered Raft deployments and the examples use
three voters. For another supported odd voter count of at least three, provide
exactly one fresh report per reported member. Set these values before the
window:

```bash
set -euo pipefail
umask 077

database=EXACT_DATABASE_NAME
voters=(
  'pve-a=192.0.2.10'
  'pve-b=192.0.2.11'
  'pve-c=192.0.2.12'
)

case "$database" in
  PVN_Control)
    backup_key=pvn-control
    snapshot=pvn-control.ovsdb
    remote=unix:/run/pvn-control/pvn-control-db.sock
    ;;
  OVN_Northbound)
    backup_key=ovn-nb
    snapshot=ovn-northbound.ovsdb
    remote=unix:/run/ovn/ovnnb_db.sock
    ;;
  OVN_Southbound)
    backup_key=ovn-sb
    snapshot=ovn-southbound.ovsdb
    remote=unix:/run/ovn/ovnsb_db.sock
    ;;
  *) exit 1 ;;
esac

pvn_cluster_id=$(python3 -c \
  'import json; print(json.load(open("/etc/pve/pvn/config.json"))["cluster"]["id"])')
[ -n "$pvn_cluster_id" ] || exit 1
```

`pvn_cluster_id` is the installation ID from `cluster.id`. It is not any of
the three OVSDB Raft cluster UUIDs.

### 1. Freeze every writer

Block PVN UI/API mutations, PVE HA and migration actions, guest lifecycle
changes that touch NICs, control-plane apply/update jobs, and manual OVN
writes. On every PVE node, stop the complete node target. Its `PartOf=`
relationships stop the manager, host configuration service, controller,
agent, and readiness gate together:

```sh
systemctl stop pvn-node.target
```

On every central voter, stop the complete central target. Its `PartOf=`
relationships cleanly stop the three database services, listeners, and
northd. Then start only the database services:

```sh
systemctl stop pvn-central.target
systemctl start pvn-control-db.service \
  ovn-ovsdb-server-nb.service ovn-ovsdb-server-sb.service
```

Do not start `ovn-northd`, do not start `pvn-central.target`, and never use
`--ignore-dependencies`. On every central voter, prove the exact service
topology:

```bash
for unit in pvn-control-db.service \
  ovn-ovsdb-server-nb.service ovn-ovsdb-server-sb.service; do
  [ "$(systemctl show --property=ActiveState --value "$unit")" = active ] || exit 1
done
for unit in pvn-node.target pvn-node-ready.service \
  pvn-central.target ovn-northd.service pvn-manager.service \
  pvn-agent.service ovn-controller.service; do
  [ "$(systemctl show --property=ActiveState --value "$unit")" = inactive ] || exit 1
done
```

The recovery commands repeat the local check, but the operator remains
responsible for proving it on every remote node.

### 2. Wait for a new Raft baseline and select the leader

Wait until `pvnctl central status` succeeds on every voter after the DB-only
restart. Discard every leader and term recorded before the freeze. Each report
must show all three databases healthy, every member connected, and no
membership transition.

The following helper collects each report through Proxmox's pinned node host
key. Status labels must equal the node's actual `hostname` value because the
final gate matches the selected leader to the backup source.

```bash
pve_ssh_node() {
  local node=$1
  local ip=$2
  local known_hosts
  shift 2
  case "$node" in ''|*[!A-Za-z0-9._-]*) return 1 ;; esac
  python3 - "$ip" <<'PY'
import ipaddress, sys
ipaddress.IPv4Address(sys.argv[1])
PY
  known_hosts=/etc/pve/nodes/$node/ssh_known_hosts
  [ -f "$known_hosts" ] || return 1
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
    -o "HostKeyAlias=$node" \
    -o ConnectTimeout=10 \
    -- "root@$ip" "$@"
}

collect_statuses() {
  local destination=$1
  local specification node ip temporary final
  status_args=()
  for specification in "${voters[@]}"; do
    node=${specification%%=*}
    ip=${specification#*=}
    [ "$node" != "$specification" ] || return 1
    temporary=$destination/.$node.tmp
    final=$destination/$node.json
    pve_ssh_node "$node" "$ip" /usr/sbin/pvnctl central status > "$temporary"
    chmod 0600 "$temporary"
    mv "$temporary" "$final"
    status_args+=("$node=$final")
  done
}

baseline_dir=$(mktemp -d /root/pvn-recovery-baseline.XXXXXX)
chmod 0700 "$baseline_dir"
collect_statuses "$baseline_dir"
leader=$(python3 - "$database" "${status_args[@]}" <<'PY'
import json, pathlib, sys

database = sys.argv[1]
reports = {}
for specification in sys.argv[2:]:
    node, separator, path = specification.partition("=")
    if not separator or node in reports:
        raise SystemExit("invalid or duplicate voter specification")
    report = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
    if report.get("healthy") is not True:
        raise SystemExit(f"{node}: central status is unhealthy")
    rows = [row for row in report.get("databases", [])
            if row.get("database") == database]
    if len(rows) != 1:
        raise SystemExit(f"{node}: selected database is missing or duplicated")
    row = rows[0]
    if (row.get("available") is not True or row.get("healthy") is not True
            or row.get("membership_change") is not False):
        raise SystemExit(f"{node}: selected database is not stable")
    reports[node] = row

count = len(reports)
for node, row in reports.items():
    if row.get("member_count") != count or row.get("connected_members") != count:
        raise SystemExit(f"{node}: selected database is not {count}/{count}")
for field in ("cluster_id", "term", "member_count"):
    if len({row.get(field) for row in reports.values()}) != 1:
        raise SystemExit(f"selected database {field} differs across voters")
if len({row.get("server_id") for row in reports.values()}) != count:
    raise SystemExit("selected database server IDs are not distinct")
if len({row.get("address") for row in reports.values()}) != count:
    raise SystemExit("selected database addresses are not distinct")
leaders = [(node, row) for node, row in reports.items()
           if row.get("role") == "leader" and row.get("leader") == "self"]
if len(leaders) != 1:
    raise SystemExit("selected database does not have exactly one leader")
leader_node, leader_row = leaders[0]
nickname = str(leader_row.get("server_id", ""))[:4].lower()
for node, row in reports.items():
    if node == leader_node:
        continue
    if row.get("role") != "follower" or str(row.get("leader", "")).lower() != nickname:
        raise SystemExit(f"{node}: follower reports another leader")
print(leader_node)
PY
)
printf 'Selected %s leader after DB-only restart: %s\n' "$database" "$leader"
```

Continue in a root shell on that exact leader, retaining or re-entering the
variables and helper above. Recollect the baseline there if the shell move took
time. From this point through restore, run backup, `pre-restore`, restore, and
forced reconciliation there only. Do not probe nodes by trying a write on each
one.

### 3. Capture fresh recovery evidence

Set the maintenance boundary only after all writers are frozen, the exact
service topology is proven, and the new Raft baseline is stable. The final
gate rejects a boundary older than one hour.

```bash
window_started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
create_report=$(pvn-db-backup create \
  --recovery-window --database "$backup_key")
backup_set=$(printf '%s\n' "$create_report" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["backup_set"])')
source_node=$(printf '%s\n' "$create_report" | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["source"]["hostname"])')
expected_sha=$(printf '%s\n' "$create_report" | python3 -c \
  'import json,sys; rows=json.load(sys.stdin)["database_identities"]; assert len(rows)==1; print(rows[0]["sha256"])')

[ "$source_node" = "$leader" ] || exit 1
source_root=/var/backups/pvn
backup_name=${backup_set##*/}
[ "$backup_set" = "$source_root/$backup_name" ] || exit 1
case "$backup_name" in pvn-db-backup-*) ;; *) exit 1 ;; esac
case "$backup_name" in *[!A-Za-z0-9._-]*) exit 1 ;; esac
pvn-db-backup verify "$backup_set"
[ "$(sha256sum "$backup_set/$snapshot" | awk '{print $1}')" = "$expected_sha" ] || exit 1
```

Copy the whole set to a different voter or durable off-host storage before
restore. For a second voter selected in `copy_node`/`copy_ip`, this refuses an
existing destination and verifies the copied set there:

```bash
copy_node=EXACT_DIFFERENT_PVE_NODE
copy_ip=EXACT_DIFFERENT_MANAGEMENT_IP
[ "$copy_node" != "$source_node" ] || exit 1
copy_root=/var/backups/pvn-independent-$backup_name
pve_ssh_node "$copy_node" "$copy_ip" \
  "set -eu; umask 077; test ! -e '$copy_root'; install -d -o root -g root -m 0700 '$copy_root'"
/usr/bin/tar -C "$source_root" -cf - -- "$backup_name" | \
  pve_ssh_node "$copy_node" "$copy_ip" \
    "set -eu; /usr/bin/tar -C '$copy_root' --keep-old-files --no-overwrite-dir -xpf -"
pve_ssh_node "$copy_node" "$copy_ip" \
  "/usr/sbin/pvn-db-backup verify '$copy_root/$backup_name'"
```

Never merge or resume a partial destination. After auditing a failed copy, use
a new empty path on another voter or genuinely off-host durable storage.

### 4. Run the mandatory fresh pre-restore gate

Collect new reports after the completed snapshot. Redirection below gives each
local file a new local modification time; do not copy reports while preserving
an older timestamp.

```bash
status_dir=$(mktemp -d /root/pvn-pre-restore-status.XXXXXX)
chmod 0700 "$status_dir"
collect_statuses "$status_dir"
voter_options=()
for specification in "${status_args[@]}"; do
  voter_options+=(--voter-status "$specification")
done

gate_log=/root/pvn-pre-restore-$backup_name.json
pvn-db-backup pre-restore "$backup_set" \
  --database "$backup_key" \
  --captured-after "$window_started_at" \
  --expected-sha256 "$expected_sha" \
  "${voter_options[@]}" > "$gate_log"
chmod 0600 "$gate_log"
```

The command requires one distinct, root-owned mode-`0600` file per voter in a
secure absolute directory. By default each report must be at most 120 seconds
old and newer than the completed snapshot. It rejects reused paths, inodes,
labels, server IDs, or addresses; wrong `ssl:` IP endpoints or Raft ports;
different cluster IDs, terms, membership counts, or follower leader views;
anything other than one leader; a source-host mismatch; schema/digest drift;
or the wrong local service topology. `--status-max-age` can be raised only up
to 600 seconds, but recollecting is safer.

Any leadership or term change after capture invalidates this window. Do not
automatically retry on another node. Unfreeze services as described in step 7,
then begin a new window with another fresh backup.

### 5. Submit exactly one restore transaction

Immediately after a successful gate, require an exact typed confirmation and
submit one restore through the selected leader's local Unix socket. If the
operator pauses or any observable state changes after the gate, recollect the
reports and run `pre-restore` again before proceeding:

```bash
printf 'Type RESTORE %s %s: ' "$database" "$expected_sha"
IFS= read -r confirmation
[ "$confirmation" = "RESTORE $database $expected_sha" ] || exit 1

/usr/bin/ovsdb-client --timeout=120 restore \
  "$remote" "$database" < "$backup_set/$snapshot"
```

Run that command exactly once. Do not use a comma-separated SSL remote, switch
voters, add a force/no-leader option, or restore another database. A timeout or
lost response is ambiguous even if the transaction committed. Preserve all
files and logs, keep writers frozen, inspect the database, and do not blindly
retry.

### 6. Force desired-state reconciliation while writers remain frozen

Keep all database services active and keep northd, every manager, agent, and
controller inactive. From the same leader, run the dedicated recovery writer
once:

```sh
pvnctl recovery reconcile-ovn --apply --confirm "$pvn_cluster_id"
```

This performs one forced `PVN_Control` to `OVN_Northbound` reconciliation and
records the resulting operation state in `PVN_Control`. It returns
`"status":"succeeded"` only after every current desired revision is ready,
applied, and has exactly one successful reconcile operation completed by this
pass. It is required after `PVN_Control` or Northbound reconstruction; running
it for a Southbound-only reconstruction also verifies PVN-owned Northbound
state before northd repopulates Southbound. It deliberately writes both PVN
Control and Northbound, which is why normal managers must remain frozen. On
failure, keep the freeze and investigate instead of starting a second restore.

### 7. Restart through the targets and validate

After the restore and forced reconciliation both succeed, run this on every
central voter:

```sh
systemctl start pvn-central.target
```

Do not start northd with dependency overrides. Wait until
`pvnctl central status` succeeds on every voter. Then start the complete node
target on every PVE node so its readiness gate and all required components
return as one lifecycle:

```sh
systemctl start pvn-node.target
```

Before reopening UI/API and guest operations, require all of the following:

- `pvnctl central status` succeeds on every central voter;
- `pvnctl doctor` succeeds on every node;
- `pvn-node.target` and `pvn-node-ready.service` are active on every node;
- the agent `/healthz` check and every controller connection are healthy;
- logical inventory matches stable PVN IDs and names (raw OVSDB UUIDs may have
  changed); and
- a deliberate test-port attach/detach, security-policy test, and dataplane
  test pass.

Preserve the pre-restore set, independent copy, status files, gate output, and
operator log until validation is complete. Another database requires another
freeze, a new Raft baseline, a new boundary, and a new recovery-window set.

If the window is aborted before the restore, start `pvn-central.target` on all
central voters, start `pvn-node.target` on every node, and complete the same
health validation before returning service.

## Database-specific intent

- Restore `PVN_Control` when desired state itself is damaged. Prefer leaving
  both OVN databases in place, then force reconciliation from restored desired
  state.
- Restore `OVN_Northbound` only for confirmed Northbound corruption and only
  with restored-UUID adoption/reference support installed cluster-wide. Treat
  `PVN_Control` as authoritative.
- Restore `OVN_Southbound` only for confirmed Southbound corruption. Northd
  and controllers normally repopulate operational state after the target and
  node components restart.

Even when three snapshots share a routine backup directory, restoring one
does not make either of the others part of the same transaction.
