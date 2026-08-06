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

Use one maintenance window per database. Never loop over all three commands.
The examples below assume the backup set has already been copied to the voter
that will receive the restore request.

1. Record `pvnctl central status` from at least two voters. Require all three
   databases healthy, every configured voter connected, no membership change,
   and the expected cluster IDs. Stop if any value differs. Take and verify a
   fresh pre-restore backup.

2. Choose exactly one database and its fixed snapshot/socket pair:

   | Database | Snapshot | Live Unix socket |
   | --- | --- | --- |
   | `PVN_Control` | `pvn-control.ovsdb` | `unix:/run/pvn-control/pvn-control-db.sock` |
   | `OVN_Northbound` | `ovn-northbound.ovsdb` | `unix:/run/ovn/ovnnb_db.sock` |
   | `OVN_Southbound` | `ovn-southbound.ovsdb` | `unix:/run/ovn/ovnsb_db.sock` |

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

4. Verify the selected set again, obtain the expected digest from its manifest,
   and require an exact typed confirmation. Replace all uppercase placeholders;
   do not use `--force`.

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

5. Confirm the chosen database server is still active on the receiving voter,
   then submit exactly one atomic database restore transaction:

   ```sh
   /usr/bin/ovsdb-client --timeout=120 restore \
     "$remote" "$database" < "$backup_set/$snapshot"
   ```

   Stop immediately on any error; do not attempt another database.

6. Restart northd on every central voter, then the controller and PVN services
   on every transport node:

   ```sh
   systemctl start ovn-northd.service
   systemctl start ovn-controller.service pvn-manager.service pvn-agent.service
   ```

   Re-run `pvnctl central status` on multiple voters. On every node, require
   `pvnctl doctor`, `systemctl restart pvn-node-ready.service`, the agent
   `/healthz` check, and a deliberate test-port attach/detach to pass before
   reopening API/UI changes.

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
