#!/usr/bin/python3
"""Isolated fail-closed tests for pvn-db-backup."""

from __future__ import annotations

import copy
import concurrent.futures
import datetime
import hashlib
import importlib.machinery
import importlib.util
import json
import os
import pathlib
import re
import shutil
import socket
import stat
import subprocess
import sys
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
SOURCE = REPO / "deploy/scripts/pvn-db-backup"


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


# The production command deliberately rejects any path component not owned by
# root.  GitHub Actions checks out under /__w, whose outer directory is owned
# by the runner account even inside the root container, so keep the isolated
# fixture directly under the root-owned filesystem instead of the checkout.
temporary = pathlib.Path(tempfile.mkdtemp(prefix=".pvn-db-backup-test-", dir="/"))
os.chmod(temporary, 0o700)
listeners: list[socket.socket] = []
try:
    fake_bin = temporary / "bin"
    state = temporary / "state"
    socket_root = temporary / "sockets"
    receipt_root = temporary / "restore-receipts"
    fake_bin.mkdir(mode=0o700)
    state.mkdir(mode=0o700)
    socket_root.mkdir(mode=0o700)
    marker = temporary / "central-enabled"
    marker.write_text("", encoding="ascii")
    marker.chmod(0o644)
    calls = state / "calls"
    status_path = state / "status.json"
    sequence_path = state / "status-sequence.json"
    counter_path = state / "status-counter"
    fail_database = state / "fail-database"
    unit_override = state / "unit-override"
    age_status_marker = state / "age-status-on-central-read.json"

    sockets = {
        "PVN_Control": socket_root / "pvn-control.sock",
        "OVN_Northbound": socket_root / "ovn-nb.sock",
        "OVN_Southbound": socket_root / "ovn-sb.sock",
    }
    for path in sockets.values():
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(str(path))
        listeners.append(listener)

    cluster_ids = {
        "PVN_Control": "11111111-1111-4111-8111-111111111111",
        "OVN_Northbound": "22222222-2222-4222-8222-222222222222",
        "OVN_Southbound": "33333333-3333-4333-8333-333333333333",
    }
    server_ids = {
        "PVN_Control": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1",
        "OVN_Northbound": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2",
        "OVN_Southbound": "cccccccc-cccc-4ccc-8ccc-ccccccccccc3",
    }
    terms = {
        "PVN_Control": 11,
        "OVN_Northbound": 8,
        "OVN_Southbound": 7,
    }
    roles = {
        "PVN_Control": ("follower", "f055"),
        "OVN_Northbound": ("leader", "self"),
        "OVN_Southbound": ("leader", "self"),
    }
    addresses = {
        "PVN_Control": "ssl:192.0.2.10:6646",
        "OVN_Northbound": "ssl:192.0.2.10:6643",
        "OVN_Southbound": "ssl:192.0.2.10:6644",
    }
    versions = {
        "PVN_Control": "1.0.0",
        "OVN_Northbound": "7.11.0",
        "OVN_Southbound": "20.41.0",
    }
    checksums = {
        "PVN_Control": "",
        "OVN_Northbound": "2520708261 39034",
        "OVN_Southbound": "2343742948 34719",
    }
    base_status = {
        "healthy": True,
        "databases": [
            {
                "database": name,
                "available": True,
                "healthy": True,
                "cluster_id": cluster_ids[name],
                "server_id": server_ids[name],
                "address": addresses[name],
                "role": roles[name][0],
                "term": terms[name],
                "leader": roles[name][1],
                "member_count": 3,
                "connected_members": 3,
                "quorum_size": 2,
                "membership_change": False,
            }
            for name in cluster_ids
        ],
    }

    (fake_bin / "systemctl").write_text(
        "#!/bin/sh\n"
        "if [ \"$1 $2 $3\" = 'is-active --quiet pvn-central.target' ]; then exit 0; fi\n"
        "[ \"$1 $2 $3\" = 'show --property=ActiveState --value' ] || exit 2\n"
        f"override={str(unit_override)!r}\n"
        "if [ -f \"$override\" ]; then\n"
        "  read -r override_unit override_state < \"$override\"\n"
        "  if [ \"$4\" = \"$override_unit\" ]; then echo \"$override_state\"; exit 0; fi\n"
        "fi\n"
        "case \"$4\" in\n"
        "  pvn-control-db.service|ovn-ovsdb-server-nb.service|ovn-ovsdb-server-sb.service) echo active ;;\n"
        "  pvn-node.target|pvn-node-ready.service|pvn-central.target|ovn-northd.service|pvn-manager.service|pvn-agent.service|ovn-controller.service) echo inactive ;;\n"
        "  *) exit 2 ;;\n"
        "esac\n",
        encoding="ascii",
    )
    (fake_bin / "pvnctl").write_text(
        f"""#!/usr/bin/python3
import json
import os
import pathlib
import sys

status = pathlib.Path({str(status_path)!r})
sequence = pathlib.Path({str(sequence_path)!r})
counter = pathlib.Path({str(counter_path)!r})
age_status_marker = pathlib.Path({str(age_status_marker)!r})
if sys.argv[1:] != ["central", "status"]:
    raise SystemExit(2)
if sequence.exists():
    reports = json.loads(sequence.read_text())
    index = int(counter.read_text()) if counter.exists() else 0
    report = reports[min(index, len(reports) - 1)]
    counter.write_text(str(index + 1))
else:
    report = json.loads(status.read_text())
if age_status_marker.exists():
    for raw_path in json.loads(age_status_marker.read_text()):
        os.utime(raw_path, (1, 1))
    age_status_marker.unlink()
print(json.dumps(report))
""",
        encoding="ascii",
    )
    socket_map = {f"unix:{path}": name for name, path in sockets.items()}
    (fake_bin / "ovsdb-client").write_text(
        f"""#!/usr/bin/python3
import json
import pathlib
import sys

calls = pathlib.Path({str(calls)!r})
failure = pathlib.Path({str(fail_database)!r})
socket_map = {socket_map!r}
versions = {versions!r}
checksums = {checksums!r}
with calls.open("a", encoding="ascii") as log:
    log.write("client " + " ".join(sys.argv[1:]) + "\\n")
if len(sys.argv) < 4 or not sys.argv[1].startswith("--timeout="):
    raise SystemExit(2)
operation = sys.argv[2]
remote = sys.argv[3]
database = socket_map.get(remote)
if database is None:
    raise SystemExit(2)
if operation == "list-dbs" and len(sys.argv) == 4:
    print(database)
elif operation == "get-schema-version" and sys.argv[4:] == [database]:
    print(versions[database])
elif operation == "get-schema-cksum" and sys.argv[4:] == [database]:
    print(checksums[database])
elif operation == "backup" and sys.argv[4:] == [database]:
    if failure.exists() and failure.read_text().strip() == database:
        print("injected backup failure", file=sys.stderr)
        raise SystemExit(9)
    payload = {{
        "database": database,
        "version": versions[database],
        "checksum": checksums[database],
        "mode": "standalone",
        "records": [{{"id": database + "-record"}}],
    }}
    sys.stdout.write(json.dumps(payload, sort_keys=True) + "\\n")
else:
    raise SystemExit(2)
""",
        encoding="ascii",
    )
    (fake_bin / "ovsdb-tool").write_text(
        f"""#!/usr/bin/python3
import json
import pathlib
import shutil
import sys

calls = pathlib.Path({str(calls)!r})
with calls.open("a", encoding="ascii") as log:
    log.write("tool " + " ".join(sys.argv[1:]) + "\\n")
if len(sys.argv) < 3:
    raise SystemExit(2)
operation = sys.argv[1]
source = pathlib.Path(sys.argv[2])
try:
    payload = json.loads(source.read_text())
except Exception:
    raise SystemExit(3)
if operation == "compact" and len(sys.argv) == 4:
    destination = pathlib.Path(sys.argv[3])
    if destination.exists():
        raise SystemExit(4)
    shutil.copyfile(source, destination)
    destination.chmod(0o600)
elif operation == "db-is-standalone" and len(sys.argv) == 3:
    if payload.get("mode") != "standalone":
        raise SystemExit(5)
elif operation == "db-name" and len(sys.argv) == 3:
    print(payload.get("database", ""))
elif operation == "db-version" and len(sys.argv) == 3:
    print(payload.get("version", ""))
elif operation == "db-cksum" and len(sys.argv) == 3:
    print(payload.get("checksum", ""))
elif operation == "show-log" and len(sys.argv) == 3:
    if not isinstance(payload.get("records"), list):
        raise SystemExit(6)
else:
    raise SystemExit(2)
""",
        encoding="ascii",
    )
    for executable in fake_bin.iterdir():
        executable.chmod(0o755)

    tested_script = temporary / "pvn-db-backup"
    source = SOURCE.read_text(encoding="utf-8")
    receipt_root_literal = "/var/lib/pvn-restore-receipts"
    check(
        source.count(receipt_root_literal) == 1,
        "production restore receipt root literal drifted",
    )
    replacements = {
        "/usr/bin/systemctl": str(fake_bin / "systemctl"),
        "/usr/sbin/pvnctl": str(fake_bin / "pvnctl"),
        "/usr/bin/ovsdb-client": str(fake_bin / "ovsdb-client"),
        "/usr/bin/ovsdb-tool": str(fake_bin / "ovsdb-tool"),
        "/etc/pvn/central/enabled": str(marker),
        "/run/lock/pvn-db-backup.lock": str(temporary / "backup.lock"),
        "/var/lib/pvn-restore-receipts": str(receipt_root),
        "/run/pvn-control/pvn-control-db.sock": str(sockets["PVN_Control"]),
        "/run/ovn/ovnnb_db.sock": str(sockets["OVN_Northbound"]),
        "/run/ovn/ovnsb_db.sock": str(sockets["OVN_Southbound"]),
    }
    for old, new in replacements.items():
        source = source.replace(old, new)
    check(
        receipt_root_literal not in source,
        "isolated test did not replace the production restore receipt root",
    )
    tested_script.write_text(source, encoding="utf-8")
    tested_script.chmod(0o755)

    def reset_status(status: dict | None = None, sequence: list[dict] | None = None) -> None:
        status_path.write_text(json.dumps(status or base_status), encoding="utf-8")
        for path in (sequence_path, counter_path):
            if path.exists():
                path.unlink()
        if sequence is not None:
            sequence_path.write_text(json.dumps(sequence), encoding="utf-8")

    def invoke(*arguments: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(tested_script), *arguments],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def require_success(result: subprocess.CompletedProcess[str], label: str) -> dict:
        check(result.returncode == 0, f"{label} failed: {result.stderr}")
        try:
            return json.loads(result.stdout)
        except json.JSONDecodeError as error:
            raise AssertionError(f"{label} returned invalid JSON: {error}") from error

    def clone_backup(source_path: pathlib.Path, name: str) -> pathlib.Path:
        destination = source_path.parent / name
        shutil.copytree(source_path, destination)
        return destination

    def write_status_report(name: str, report: dict) -> pathlib.Path:
        path = state / name
        path.write_text(json.dumps(report), encoding="utf-8")
        path.chmod(0o600)
        return path

    output_root = temporary / "backups"
    reset_status()
    created = require_success(
        invoke("create", "--output", str(output_root)), "all-database create"
    )
    backup_set = pathlib.Path(created["backup_set"])
    check(backup_set.parent == output_root and backup_set.is_dir(), "backup set was not published")
    check(stat.S_IMODE(output_root.stat().st_mode) == 0o700, "output root mode is not 0700")
    check(stat.S_IMODE(backup_set.stat().st_mode) == 0o700, "backup set mode is not 0700")
    manifest = json.loads((backup_set / "manifest.json").read_text())
    check(manifest["consistency"] == "independent-per-database", "consistency claim drifted")
    check(manifest["format"] == "pvn-db-backup/v2", "manifest format was not upgraded")
    check(manifest["purpose"] == "general-backup", "general backup purpose was omitted")
    check(len(manifest["databases"]) == 3, "all create did not capture three databases")
    expected_database_mapping = {
        "pvn-control": (
            "PVN_Control",
            "pvn-control.ovsdb",
            f"unix:{sockets['PVN_Control']}",
        ),
        "ovn-nb": (
            "OVN_Northbound",
            "ovn-northbound.ovsdb",
            f"unix:{sockets['OVN_Northbound']}",
        ),
        "ovn-sb": (
            "OVN_Southbound",
            "ovn-southbound.ovsdb",
            f"unix:{sockets['OVN_Southbound']}",
        ),
    }
    for entry in manifest["databases"]:
        expected_name, expected_file, expected_remote = expected_database_mapping[
            entry["key"]
        ]
        check(
            (entry["database"], entry["file"])
            == (expected_name, expected_file),
            f"{entry['key']} database/snapshot mapping drifted",
        )
        check(
            f"client --timeout=120 backup {expected_remote} {expected_name}"
            in calls.read_text(encoding="ascii"),
            f"{entry['key']} fixed local Unix restore mapping drifted",
        )
        check(
            (entry["role"], entry["leader"]) == roles[entry["database"]],
            "source role and leader identity were not recorded",
        )
        check(entry["term"] == terms[entry["database"]], "Raft term was not recorded")
        check(
            entry["server_id"] == server_ids[entry["database"]],
            "Raft server ID was not recorded",
        )
        check(
            stat.S_IMODE((backup_set / entry["file"]).stat().st_mode) == 0o600,
            f"{entry['file']} mode is not 0600",
        )
    verified = require_success(invoke("verify", str(backup_set)), "offline verify")
    check(verified["verified"] is True, "verify did not report success")
    check("tool compact" in calls.read_text(), "verify did not compact an offline copy")

    legacy = clone_backup(backup_set, "legacy-v1")
    legacy_manifest_path = legacy / "manifest.json"
    legacy_manifest = json.loads(legacy_manifest_path.read_text())
    legacy_manifest["format"] = "pvn-db-backup/v1"
    legacy_manifest.pop("purpose")
    for entry in legacy_manifest["databases"]:
        for field in ("server_id", "address", "role", "term", "leader"):
            entry.pop(field)
    legacy_manifest_path.write_text(
        json.dumps(legacy_manifest, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    legacy_verified = require_success(
        invoke("verify", str(legacy)), "legacy offline verify"
    )
    check(
        legacy_verified["format"] == "pvn-db-backup/v1",
        "legacy manifest format was not preserved",
    )

    reset_status()
    selected = require_success(
        invoke(
            "create",
            "--output",
            str(output_root),
            "--database",
            "ovn-nb",
            "--recovery-window",
        ),
        "selected-database create",
    )
    selected_manifest = json.loads(
        (pathlib.Path(selected["backup_set"]) / "manifest.json").read_text()
    )
    check(
        [entry["key"] for entry in selected_manifest["databases"]] == ["ovn-nb"],
        "selected create captured the wrong database set",
    )
    check(
        selected_manifest["purpose"] == "recovery-window",
        "selected recovery backup was not marked for recovery",
    )
    check(
        selected["database_identities"] == [
            {
                key: selected_manifest["databases"][0][key]
                for key in (
                    "key",
                    "database",
                    "cluster_id",
                    "server_id",
                    "address",
                    "role",
                    "term",
                    "leader",
                    "member_count",
                    "capture_started_at",
                    "capture_completed_at",
                    "sha256",
                )
            }
        ],
        "create report omitted the selected database identity",
    )

    source_hostname = re.sub(
        r"[^A-Za-z0-9._-]", "-", socket.gethostname()
    ).strip("._-")[:63]
    source_report = copy.deepcopy(base_status)
    follower_b_report = copy.deepcopy(base_status)
    follower_b_report["databases"][1].update(
        server_id="dddddddd-dddd-4ddd-8ddd-dddddddddddd",
        address="ssl:192.0.2.11:6643",
        role="follower",
        leader="bbbb",
    )
    follower_c_report = copy.deepcopy(base_status)
    follower_c_report["databases"][1].update(
        server_id="eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
        address="ssl:192.0.2.12:6643",
        role="follower",
        leader="bbbb",
    )
    status_paths = {
        source_hostname: write_status_report("source-status.json", source_report),
        "voter-b": write_status_report("voter-b-status.json", follower_b_report),
        "voter-c": write_status_report("voter-c-status.json", follower_c_report),
    }
    selected_entry = selected_manifest["databases"][0]

    def pre_restore_arguments(
        *,
        expected_sha256: str | None = None,
        captured_after: str | None = None,
        specifications: list[str] | None = None,
    ) -> list[str]:
        values = [
            "pre-restore",
            selected["backup_set"],
            "--database",
            "ovn-nb",
            "--captured-after",
            captured_after or selected_entry["capture_started_at"],
            "--expected-sha256",
            expected_sha256 or selected_entry["sha256"],
        ]
        for specification in specifications or [
            f"{node}={path}" for node, path in status_paths.items()
        ]:
            values.extend(("--voter-status", specification))
        return values

    def reserve_restore_arguments(
        *,
        backup_set: str | None = None,
        expected_sha256: str | None = None,
        captured_after: str | None = None,
        specifications: list[str] | None = None,
        confirmation: str | None = None,
    ) -> list[str]:
        digest = expected_sha256 or selected_entry["sha256"]
        values = [
            "reserve-restore",
            backup_set or selected["backup_set"],
            "--database",
            "ovn-nb",
            "--captured-after",
            captured_after or selected_entry["capture_started_at"],
            "--expected-sha256",
            digest,
        ]
        for specification in specifications or [
            f"{node}={path}" for node, path in status_paths.items()
        ]:
            values.extend(("--voter-status", specification))
        values.extend(
            (
                "--confirm",
                confirmation or f"RESTORE OVN_Northbound {digest}",
            )
        )
        return values

    reset_status()
    pre_restore = require_success(
        invoke(*pre_restore_arguments()), "pre-restore identity gate"
    )
    check(
        pre_restore["restore_ready"] is True
        and pre_restore["live_identity_verified"] is True,
        "pre-restore did not report a verified live identity",
    )
    check(
        pre_restore["voters"] == sorted(status_paths),
        "pre-restore did not retain exact voter labels",
    )

    reset_status()
    result = invoke(
        *reserve_restore_arguments(confirmation="RESTORE OVN_Northbound wrong")
    )
    check(
        result.returncode != 0 and "--confirm must exactly match" in result.stderr,
        "restore reservation accepted a mistyped confirmation",
    )
    check(not receipt_root.exists(), "bad confirmation created the receipt ledger")

    reset_status()
    result = invoke(
        *reserve_restore_arguments(expected_sha256="A" * 64)
    )
    check(
        result.returncode != 0
        and "lowercase SHA-256 digest" in result.stderr
        and "--confirm must" not in result.stderr,
        "restore reservation built confirmation text before validating the digest",
    )
    check(not receipt_root.exists(), "invalid digest created the receipt ledger")

    reset_status()
    result = invoke(
        *reserve_restore_arguments(),
        "--remote",
        "unix:/tmp/operator-selected.sock",
    )
    check(
        result.returncode == 2 and "unrecognized arguments" in result.stderr,
        "restore reservation accepted an operator-selected remote",
    )
    check(not receipt_root.exists(), "bad remote created the receipt ledger")

    duplicate_status = state / "duplicate-json-status.json"
    encoded_status = json.dumps(source_report, separators=(",", ":"))
    duplicate_status.write_text(
        '{"healthy":true,"healthy":true,' + encoded_status[1:],
        encoding="utf-8",
    )
    duplicate_status.chmod(0o600)
    duplicate_specs = [
        f"{source_hostname}={duplicate_status}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-c={status_paths['voter-c']}",
    ]
    reset_status()
    result = invoke(
        *reserve_restore_arguments(specifications=duplicate_specs)
    )
    check(
        result.returncode != 0 and "duplicate JSON key" in result.stderr,
        "restore reservation accepted duplicate JSON voter evidence",
    )
    check(not receipt_root.exists(), "duplicate JSON created the receipt ledger")

    hardlink_status = state / "hardlink-status.json"
    os.link(status_paths[source_hostname], hardlink_status)
    reset_status()
    result = invoke(*reserve_restore_arguments())
    check(
        result.returncode != 0 and "exactly one hard link" in result.stderr,
        "restore reservation accepted hard-linked voter evidence",
    )
    hardlink_status.unlink()
    check(not receipt_root.exists(), "hard-linked evidence created the receipt ledger")

    age_status_marker.write_text(
        json.dumps([str(path) for path in status_paths.values()]),
        encoding="utf-8",
    )
    reset_status()
    result = invoke(*reserve_restore_arguments())
    check(
        result.returncode != 0
        and "voter status" in result.stderr
        and "older than" in result.stderr,
        "restore reservation did not recheck voter freshness immediately before receipt creation",
    )
    check(not receipt_root.exists(), "aged final voter evidence created a receipt")
    now = datetime.datetime.now(datetime.timezone.utc).timestamp()
    for path in status_paths.values():
        os.utime(path, (now, now))

    unsafe_receipt_target = temporary / "unsafe-receipt-target"
    unsafe_receipt_target.mkdir(mode=0o700)
    receipt_root.symlink_to(unsafe_receipt_target, target_is_directory=True)
    reset_status()
    result = invoke(*reserve_restore_arguments())
    check(
        result.returncode != 0 and "real directory" in result.stderr,
        "restore reservation followed a receipt-root symlink",
    )
    check(not list(unsafe_receipt_target.iterdir()), "receipt escaped through symlink")
    receipt_root.unlink()
    unsafe_receipt_target.rmdir()

    receipt_root.mkdir(mode=0o700)
    receipt_root.chmod(0o777)
    reset_status()
    result = invoke(*reserve_restore_arguments())
    check(
        result.returncode != 0 and "group/world writable" in result.stderr,
        "restore reservation accepted a writable receipt ledger",
    )
    check(not list(receipt_root.iterdir()), "unsafe receipt ledger received a file")
    receipt_root.rmdir()

    reset_status()
    calls_before_reservation = calls.read_text(encoding="ascii")
    reserved = require_success(
        invoke(*reserve_restore_arguments()), "restore submission reservation"
    )
    receipt_path = pathlib.Path(reserved["receipt_path"])
    check(
        reserved["restore_reserved"] is True
        and reserved["format"] == "pvn-restore-reservation/v1"
        and reserved["state"] == "reserved-before-single-restore",
        "restore reservation returned the wrong state",
    )
    check(
        reserved["database"] == "OVN_Northbound"
        and reserved["database_key"] == "ovn-nb"
        and reserved["snapshot"] == "ovn-northbound.ovsdb"
        and reserved["restore_remote"] == f"unix:{sockets['OVN_Northbound']}"
        and reserved["snapshot_sha256"] == selected_entry["sha256"]
        and reserved["source"] == source_hostname,
        "restore reservation did not bind the fixed database identity",
    )
    expected_reservation_id = hashlib.sha256(
        json.dumps(
            {
                "database_key": "ovn-nb",
                "cluster_id": selected_entry["cluster_id"],
                "term": selected_entry["term"],
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    ).hexdigest()
    check(
        reserved["receipt_id"] == expected_reservation_id,
        "restore receipt ID is not the canonical database leader epoch",
    )
    check(
        receipt_path.parent == receipt_root
        and stat.S_IMODE(receipt_path.stat().st_mode) == 0o600
        and receipt_path.stat().st_nlink == 1,
        "restore receipt is not a single mode-0600 file in the fixed ledger",
    )
    receipt_payload = json.loads(receipt_path.read_text(encoding="utf-8"))
    check(receipt_payload == reserved, "persisted receipt differs from command output")
    reservation_calls = calls.read_text(encoding="ascii")[len(calls_before_reservation) :]
    check(" restore " not in reservation_calls, "reservation invoked ovsdb-client restore")

    receipt_before = receipt_path.read_bytes()
    receipt_inode = receipt_path.stat().st_ino
    reset_status()
    result = invoke(*reserve_restore_arguments())
    check(
        result.returncode != 0
        and "already exists" in result.stderr
        and "retry is forbidden" in result.stderr,
        "restore reservation replay did not fail closed",
    )
    check(
        receipt_path.read_bytes() == receipt_before
        and receipt_path.stat().st_ino == receipt_inode,
        "restore reservation replay changed the existing receipt",
    )

    copied_recovery_set = clone_backup(
        pathlib.Path(selected["backup_set"]), "renamed-recovery-window"
    )
    reset_status()
    result = invoke(
        *reserve_restore_arguments(backup_set=str(copied_recovery_set))
    )
    check(
        result.returncode != 0 and "already exists" in result.stderr,
        "copying or renaming a set created another same-term reservation",
    )
    check(
        receipt_path.read_bytes() == receipt_before,
        "same-term copied set changed the existing receipt",
    )

    distinct_snapshot_set = clone_backup(
        pathlib.Path(selected["backup_set"]), "same-term-distinct-snapshot"
    )
    distinct_snapshot_path = distinct_snapshot_set / "ovn-northbound.ovsdb"
    distinct_snapshot_payload = json.loads(
        distinct_snapshot_path.read_text(encoding="utf-8")
    )
    distinct_snapshot_payload["records"].append({"id": "different-record"})
    distinct_snapshot_path.write_text(
        json.dumps(distinct_snapshot_payload, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    distinct_digest = hashlib.sha256(distinct_snapshot_path.read_bytes()).hexdigest()
    distinct_manifest_path = distinct_snapshot_set / "manifest.json"
    distinct_manifest = json.loads(distinct_manifest_path.read_text(encoding="utf-8"))
    distinct_manifest["databases"][0].update(
        bytes=distinct_snapshot_path.stat().st_size,
        sha256=distinct_digest,
    )
    distinct_manifest_path.write_text(
        json.dumps(distinct_manifest, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    reset_status()
    result = invoke(
        *reserve_restore_arguments(
            backup_set=str(distinct_snapshot_set),
            expected_sha256=distinct_digest,
        )
    )
    check(
        result.returncode != 0 and "already exists" in result.stderr,
        "a different snapshot in the same database leader epoch bypassed the receipt",
    )
    check(
        receipt_path.read_bytes() == receipt_before,
        "same-term distinct snapshot changed the existing receipt",
    )

    receipt_path.unlink()
    identity_drifted_status = copy.deepcopy(base_status)
    identity_drifted_status["databases"][1]["term"] += 1
    reset_status(identity_drifted_status)
    result = invoke(*reserve_restore_arguments())
    check(
        result.returncode != 0
        and "local live Raft identity differs" in result.stderr,
        "restore reservation accepted live Raft identity drift",
    )
    check(
        not list(receipt_root.iterdir()),
        "live Raft identity drift created a restore receipt",
    )

    for active_node_unit in ("pvn-node.target", "pvn-node-ready.service"):
        reset_status()
        unit_override.write_text(f"{active_node_unit} active\n", encoding="ascii")
        result = invoke(*pre_restore_arguments())
        check(
            result.returncode != 0
            and f"{active_node_unit} inactive" in result.stderr,
            f"pre-restore accepted active {active_node_unit}",
        )
        unit_override.unlink()

    reset_status()
    result = invoke(*pre_restore_arguments(expected_sha256="0" * 64))
    check(
        result.returncode != 0 and "expected SHA-256" in result.stderr,
        "pre-restore accepted the wrong recorded digest",
    )

    reset_status()
    duplicate_path_specs = [
        f"{source_hostname}={status_paths[source_hostname]}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-c={status_paths['voter-b']}",
    ]
    result = invoke(
        *pre_restore_arguments(specifications=duplicate_path_specs)
    )
    check(
        result.returncode != 0 and "path cannot be reused" in result.stderr,
        "pre-restore accepted one voter status path twice",
    )

    duplicate_label_specs = [
        f"{source_hostname}={status_paths[source_hostname]}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-b={status_paths['voter-c']}",
    ]
    reset_status()
    result = invoke(
        *pre_restore_arguments(specifications=duplicate_label_specs)
    )
    check(
        result.returncode != 0 and "unique NODE=" in result.stderr,
        "pre-restore accepted a duplicate voter label",
    )

    duplicate_server_report = copy.deepcopy(follower_c_report)
    duplicate_server_report["databases"][1]["server_id"] = (
        follower_b_report["databases"][1]["server_id"]
    )
    duplicate_server_path = write_status_report(
        "duplicate-server-status.json", duplicate_server_report
    )
    duplicate_server_specs = [
        f"{source_hostname}={status_paths[source_hostname]}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-c={duplicate_server_path}",
    ]
    reset_status()
    result = invoke(
        *pre_restore_arguments(specifications=duplicate_server_specs)
    )
    check(
        result.returncode != 0 and "reuse a Raft server ID" in result.stderr,
        "pre-restore accepted duplicate selected-database server IDs",
    )

    duplicate_address_report = copy.deepcopy(follower_c_report)
    duplicate_address_report["databases"][1]["address"] = (
        follower_b_report["databases"][1]["address"]
    )
    duplicate_address_path = write_status_report(
        "duplicate-address-status.json", duplicate_address_report
    )
    duplicate_address_specs = [
        f"{source_hostname}={status_paths[source_hostname]}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-c={duplicate_address_path}",
    ]
    reset_status()
    result = invoke(
        *pre_restore_arguments(specifications=duplicate_address_specs)
    )
    check(
        result.returncode != 0 and "reuse a Raft address" in result.stderr,
        "pre-restore accepted duplicate selected-database addresses",
    )

    wrong_term_report = copy.deepcopy(follower_c_report)
    wrong_term_report["databases"][1]["term"] += 1
    wrong_term_path = write_status_report("wrong-term-status.json", wrong_term_report)
    wrong_term_specs = [
        f"{source_hostname}={status_paths[source_hostname]}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-c={wrong_term_path}",
    ]
    reset_status()
    result = invoke(*pre_restore_arguments(specifications=wrong_term_specs))
    check(
        result.returncode != 0 and "term differs" in result.stderr,
        "pre-restore accepted a mismatched selected-database term",
    )

    wrong_leader_report = copy.deepcopy(follower_c_report)
    wrong_leader_report["databases"][1]["leader"] = "dead"
    wrong_leader_path = write_status_report(
        "wrong-leader-status.json", wrong_leader_report
    )
    wrong_leader_specs = [
        f"{source_hostname}={status_paths[source_hostname]}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-c={wrong_leader_path}",
    ]
    reset_status()
    result = invoke(*pre_restore_arguments(specifications=wrong_leader_specs))
    check(
        result.returncode != 0 and "reports a different leader" in result.stderr,
        "pre-restore accepted a follower that reported another leader",
    )

    wrong_port_report = copy.deepcopy(follower_c_report)
    wrong_port_report["databases"][1]["address"] = "ssl:192.0.2.12:9999"
    wrong_port_path = write_status_report("wrong-port-status.json", wrong_port_report)
    wrong_port_specs = [
        f"{source_hostname}={status_paths[source_hostname]}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-c={wrong_port_path}",
    ]
    reset_status()
    result = invoke(*pre_restore_arguments(specifications=wrong_port_specs))
    check(
        result.returncode != 0 and "Raft port 6643" in result.stderr,
        "pre-restore accepted a selected-database address on the wrong port",
    )

    duplicate_leader_report = copy.deepcopy(follower_c_report)
    duplicate_leader_report["databases"][1].update(role="leader", leader="self")
    duplicate_leader_path = write_status_report(
        "duplicate-leader-status.json", duplicate_leader_report
    )
    duplicate_leader_specs = [
        f"{source_hostname}={status_paths[source_hostname]}",
        f"voter-b={status_paths['voter-b']}",
        f"voter-c={duplicate_leader_path}",
    ]
    reset_status()
    result = invoke(
        *pre_restore_arguments(specifications=duplicate_leader_specs)
    )
    check(
        result.returncode != 0 and "exactly one leader" in result.stderr,
        "pre-restore accepted multiple selected-database leaders",
    )

    reset_status()
    old_boundary = "2000-01-01T00:00:00Z"
    result = invoke(*pre_restore_arguments(captured_after=old_boundary))
    check(
        result.returncode != 0 and "older than" in result.stderr,
        "pre-restore accepted an obsolete maintenance-window boundary",
    )

    capture_completed_epoch = datetime.datetime.strptime(
        selected_entry["capture_completed_at"], "%Y-%m-%dT%H:%M:%SZ"
    ).replace(tzinfo=datetime.timezone.utc).timestamp()
    source_status_path = status_paths[source_hostname]
    os.utime(
        source_status_path,
        (capture_completed_epoch - 1, capture_completed_epoch - 1),
    )
    reset_status()
    result = invoke(*pre_restore_arguments())
    check(
        result.returncode != 0 and "predates the completed" in result.stderr,
        "pre-restore accepted a voter report captured before the backup",
    )
    os.utime(source_status_path, None)

    reset_status()
    result = invoke(
        "pre-restore",
        str(backup_set),
        "--database",
        "pvn-control",
        "--captured-after",
        selected_entry["capture_started_at"],
        "--expected-sha256",
        manifest["databases"][0]["sha256"],
        "--voter-status",
        f"{source_hostname}={status_paths[source_hostname]}",
    )
    check(
        result.returncode != 0 and "recovery-window" in result.stderr,
        "pre-restore accepted a general backup set",
    )

    reset_status()
    general_follower_output = temporary / "general-follower-backups"
    general_follower = require_success(
        invoke(
            "create",
            "--output",
            str(general_follower_output),
            "--database",
            "pvn-control",
        ),
        "general follower create",
    )
    general_follower_manifest = json.loads(
        (pathlib.Path(general_follower["backup_set"]) / "manifest.json").read_text()
    )
    check(
        general_follower_manifest["databases"][0]["role"] == "follower",
        "general follower identity was not preserved",
    )

    reset_status()
    follower_output = temporary / "follower-backups"
    result = invoke(
        "create",
        "--output",
        str(follower_output),
        "--database",
        "pvn-control",
        "--recovery-window",
    )
    check(
        result.returncode != 0 and "current local Raft leader" in result.stderr,
        "selected follower created a backup",
    )
    check(not follower_output.exists(), "follower create changed the output filesystem")

    reset_status()
    missing_selection_output = temporary / "missing-selection-backups"
    result = invoke(
        "create",
        "--output",
        str(missing_selection_output),
        "--recovery-window",
    )
    check(
        result.returncode != 0 and "exactly one explicit" in result.stderr,
        "recovery window accepted an implicit all-database selection",
    )
    check(
        not missing_selection_output.exists(),
        "invalid recovery selection changed the output filesystem",
    )

    reset_status()
    unit_override.write_text("ovn-northd.service active\n", encoding="ascii")
    active_writer_output = temporary / "active-writer-backups"
    result = invoke(
        "create",
        "--output",
        str(active_writer_output),
        "--database",
        "ovn-nb",
        "--recovery-window",
    )
    check(
        result.returncode != 0
        and "ovn-northd.service inactive" in result.stderr,
        "recovery window accepted an active northd writer",
    )
    check(
        not active_writer_output.exists(),
        "active northd rejection changed the output filesystem",
    )
    unit_override.unlink()

    reset_status()
    unit_override.write_text("ovn-ovsdb-server-sb.service inactive\n", encoding="ascii")
    inactive_database_output = temporary / "inactive-database-backups"
    result = invoke(
        "create",
        "--output",
        str(inactive_database_output),
        "--database",
        "ovn-nb",
        "--recovery-window",
    )
    check(
        result.returncode != 0
        and "ovn-ovsdb-server-sb.service active" in result.stderr,
        "recovery window accepted an inactive database service",
    )
    check(
        not inactive_database_output.exists(),
        "inactive database rejection changed the output filesystem",
    )
    unit_override.unlink()

    tampered = clone_backup(backup_set, "tampered")
    with (tampered / "pvn-control.ovsdb").open("ab") as output:
        output.write(b"tamper")
    result = invoke("verify", str(tampered))
    check(result.returncode != 0 and "byte count mismatch" in result.stderr, "tamper was accepted")

    wrong_identity = clone_backup(backup_set, "wrong-identity")
    wrong_snapshot = wrong_identity / "pvn-control.ovsdb"
    payload = json.loads(wrong_snapshot.read_text())
    payload["database"] = "Wrong_Control"
    encoded = (json.dumps(payload, sort_keys=True) + "\n").encode()
    wrong_snapshot.write_bytes(encoded)
    wrong_manifest_path = wrong_identity / "manifest.json"
    wrong_manifest = json.loads(wrong_manifest_path.read_text())
    wrong_manifest["databases"][0]["bytes"] = len(encoded)
    wrong_manifest["databases"][0]["sha256"] = hashlib.sha256(encoded).hexdigest()
    wrong_manifest_path.write_text(
        json.dumps(wrong_manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8"
    )
    result = invoke("verify", str(wrong_identity))
    check(result.returncode != 0 and "expected 'PVN_Control'" in result.stderr, "wrong DB was accepted")

    traversal = clone_backup(backup_set, "traversal")
    traversal_manifest_path = traversal / "manifest.json"
    traversal_manifest = json.loads(traversal_manifest_path.read_text())
    traversal_manifest["databases"][0]["file"] = "../pvn-control.ovsdb"
    traversal_manifest_path.write_text(
        json.dumps(traversal_manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8"
    )
    result = invoke("verify", str(traversal))
    check(result.returncode != 0 and "identity mismatch" in result.stderr, "path traversal was accepted")

    reset_status()
    unsafe_target = temporary / "unsafe-target"
    unsafe_target.mkdir(mode=0o700)
    unsafe_link = temporary / "unsafe-link"
    unsafe_link.symlink_to(unsafe_target, target_is_directory=True)
    result = invoke("create", "--output", str(unsafe_link))
    check(result.returncode != 0 and "real directory" in result.stderr, "symlink output was accepted")
    writable = temporary / "writable-output"
    writable.mkdir(mode=0o777)
    writable.chmod(0o777)
    result = invoke("create", "--output", str(writable))
    check(result.returncode != 0 and "group/world writable" in result.stderr, "writable output was accepted")

    unhealthy = copy.deepcopy(base_status)
    unhealthy["databases"][1]["membership_change"] = True
    reset_status(unhealthy)
    unhealthy_output = temporary / "unhealthy-backups"
    result = invoke("create", "--output", str(unhealthy_output))
    check(result.returncode != 0 and "membership change" in result.stderr, "membership change was accepted")
    check(not unhealthy_output.exists(), "unhealthy create changed the output filesystem")

    reset_status()
    fail_database.write_text("OVN_Northbound\n", encoding="ascii")
    partial_output = temporary / "partial-backups"
    result = invoke("create", "--output", str(partial_output))
    check(result.returncode != 0 and "injected backup failure" in result.stderr, "backup failure was hidden")
    check(partial_output.is_dir(), "partial failure did not retain the secure output root")
    check(not list(partial_output.iterdir()), "partial failure left a backup set behind")
    fail_database.unlink()

    drifted = copy.deepcopy(base_status)
    drifted["databases"][2].update(
        member_count=5, connected_members=5, quorum_size=3
    )
    reset_status(sequence=[base_status, drifted])
    drift_output = temporary / "drift-backups"
    result = invoke("create", "--output", str(drift_output))
    check(result.returncode != 0 and "changed during backup" in result.stderr, "membership drift was accepted")
    check(not list(drift_output.iterdir()), "membership drift left a backup set behind")

    cluster_drifted = copy.deepcopy(base_status)
    cluster_drifted["databases"][1]["cluster_id"] = (
        "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
    )
    reset_status(sequence=[base_status, cluster_drifted])
    cluster_drift_output = temporary / "cluster-drift-backups"
    result = invoke(
        "create",
        "--output",
        str(cluster_drift_output),
        "--database",
        "ovn-nb",
        "--recovery-window",
    )
    check(
        result.returncode != 0 and "changed during backup" in result.stderr,
        "Raft cluster identity drift was accepted",
    )
    check(
        not list(cluster_drift_output.iterdir()),
        "cluster identity drift left a backup set behind",
    )

    term_drifted = copy.deepcopy(base_status)
    term_drifted["databases"][1]["term"] += 1
    reset_status(sequence=[base_status, term_drifted])
    term_drift_output = temporary / "term-drift-backups"
    result = invoke(
        "create",
        "--output",
        str(term_drift_output),
        "--database",
        "ovn-nb",
        "--recovery-window",
    )
    check(
        result.returncode != 0 and "changed during backup" in result.stderr,
        "Raft term drift was accepted",
    )
    check(not list(term_drift_output.iterdir()), "term drift left a backup set behind")

    leadership_drifted = copy.deepcopy(base_status)
    leadership_drifted["databases"][1].update(role="follower", leader="abcd")
    reset_status(sequence=[base_status, leadership_drifted])
    leadership_drift_output = temporary / "leadership-drift-backups"
    result = invoke(
        "create",
        "--output",
        str(leadership_drift_output),
        "--database",
        "ovn-nb",
        "--recovery-window",
    )
    check(
        result.returncode != 0 and "current local Raft leader" in result.stderr,
        "Raft leadership drift was accepted",
    )
    check(
        not list(leadership_drift_output.iterdir()),
        "leadership drift left a backup set behind",
    )

    server_drifted = copy.deepcopy(base_status)
    server_drifted["databases"][1]["server_id"] = (
        "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
    )
    reset_status(sequence=[base_status, server_drifted])
    server_drift_output = temporary / "server-drift-backups"
    result = invoke(
        "create",
        "--output",
        str(server_drift_output),
        "--database",
        "ovn-nb",
        "--recovery-window",
    )
    check(
        result.returncode != 0 and "changed during backup" in result.stderr,
        "Raft server identity drift was accepted",
    )
    check(
        not list(server_drift_output.iterdir()),
        "server identity drift left a backup set behind",
    )

    module_name = "pvn_db_backup_atomic_test"
    loader = importlib.machinery.SourceFileLoader(module_name, str(SOURCE))
    specification = importlib.util.spec_from_loader(module_name, loader)
    check(specification is not None, "cannot load pvn-db-backup for atomic tests")
    backup_module = importlib.util.module_from_spec(specification)
    sys.modules[module_name] = backup_module
    previous_dont_write_bytecode = sys.dont_write_bytecode
    sys.dont_write_bytecode = True
    try:
        loader.exec_module(backup_module)
    finally:
        sys.dont_write_bytecode = previous_dont_write_bytecode
    atomic_root = temporary / "atomic-receipts"
    atomic_root.mkdir(mode=0o700)
    backup_module.RESTORE_RECEIPT_ROOT = atomic_root
    atomic_payload = {
        "format": "pvn-restore-reservation/v1",
        "restore_reserved": True,
        "database_key": "ovn-nb",
    }

    def atomic_path(digit: str) -> pathlib.Path:
        return atomic_root / f"pvn-restore-ovn-nb-{digit * 64}.json"

    def expect_receipt_failure(
        path: pathlib.Path, expected: str, label: str
    ) -> str:
        try:
            backup_module.write_restore_receipt(path, atomic_payload)
        except backup_module.BackupError as error:
            message = str(error)
            check(expected in message, f"{label} returned the wrong error: {message}")
            return message
        raise AssertionError(f"{label} unexpectedly succeeded")

    regular_receipt = atomic_path("a")
    regular_receipt.write_bytes(b"partial receipt")
    regular_receipt.chmod(0o600)
    regular_before = (regular_receipt.stat().st_ino, regular_receipt.read_bytes())
    expect_receipt_failure(regular_receipt, "already exists", "regular receipt replay")
    check(
        (regular_receipt.stat().st_ino, regular_receipt.read_bytes()) == regular_before,
        "preexisting regular receipt was changed",
    )

    symlink_target = temporary / "receipt-symlink-target"
    symlink_target.write_bytes(b"outside")
    symlink_receipt = atomic_path("b")
    symlink_receipt.symlink_to(symlink_target)
    symlink_inode = symlink_receipt.lstat().st_ino
    expect_receipt_failure(symlink_receipt, "already exists", "symlink receipt replay")
    check(
        symlink_receipt.is_symlink()
        and symlink_receipt.lstat().st_ino == symlink_inode
        and symlink_target.read_bytes() == b"outside",
        "preexisting receipt symlink or its target was changed",
    )

    fifo_receipt = atomic_path("c")
    os.mkfifo(fifo_receipt, 0o600)
    fifo_inode = fifo_receipt.lstat().st_ino
    expect_receipt_failure(fifo_receipt, "already exists", "FIFO receipt replay")
    check(
        stat.S_ISFIFO(fifo_receipt.lstat().st_mode)
        and fifo_receipt.lstat().st_ino == fifo_inode,
        "preexisting receipt FIFO was changed",
    )

    directory_receipt = atomic_path("d")
    directory_receipt.mkdir(mode=0o700)
    directory_inode = directory_receipt.stat().st_ino
    expect_receipt_failure(directory_receipt, "already exists", "directory receipt replay")
    check(
        directory_receipt.is_dir()
        and directory_receipt.stat().st_ino == directory_inode
        and not list(directory_receipt.iterdir()),
        "preexisting receipt directory was changed",
    )

    concurrent_receipt = atomic_path("e")

    def concurrent_reservation() -> str:
        try:
            backup_module.write_restore_receipt(concurrent_receipt, atomic_payload)
            return "success"
        except backup_module.BackupError as error:
            return str(error)

    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        outcomes = list(executor.map(lambda _: concurrent_reservation(), range(2)))
    check(
        outcomes.count("success") == 1
        and sum("already exists" in outcome for outcome in outcomes) == 1,
        f"concurrent O_EXCL reservation was not one-success: {outcomes}",
    )
    check(
        stat.S_IMODE(concurrent_receipt.stat().st_mode) == 0o600
        and json.loads(concurrent_receipt.read_text(encoding="utf-8"))
        == atomic_payload,
        "concurrent reservation did not persist one valid mode-0600 receipt",
    )

    partial_receipt = atomic_path("f")
    original_write = backup_module.os.write
    write_count = [0]

    def partial_then_zero(descriptor: int, payload: bytes) -> int:
        write_count[0] += 1
        if write_count[0] == 1:
            return original_write(descriptor, payload[:7])
        return 0

    backup_module.os.write = partial_then_zero
    try:
        expect_receipt_failure(
            partial_receipt, "do not retry", "short receipt write"
        )
    finally:
        backup_module.os.write = original_write
    check(
        partial_receipt.exists() and partial_receipt.stat().st_size == 7,
        "short-write receipt was deleted or overwritten",
    )
    expect_receipt_failure(
        partial_receipt, "already exists", "short-write receipt retry"
    )

    file_fsync_receipt = atomic_path("1")
    original_fsync = backup_module.os.fsync

    def fail_file_fsync(descriptor: int) -> None:
        if stat.S_ISREG(backup_module.os.fstat(descriptor).st_mode):
            raise OSError("injected receipt file fsync failure")
        original_fsync(descriptor)

    backup_module.os.fsync = fail_file_fsync
    try:
        expect_receipt_failure(
            file_fsync_receipt, "do not retry", "receipt file fsync failure"
        )
    finally:
        backup_module.os.fsync = original_fsync
    check(file_fsync_receipt.exists(), "file-fsync failure deleted the receipt")
    expect_receipt_failure(
        file_fsync_receipt, "already exists", "file-fsync receipt retry"
    )

    directory_fsync_receipt = atomic_path("2")

    def fail_directory_fsync(descriptor: int) -> None:
        if stat.S_ISDIR(backup_module.os.fstat(descriptor).st_mode):
            raise OSError("injected receipt directory fsync failure")
        original_fsync(descriptor)

    backup_module.os.fsync = fail_directory_fsync
    try:
        expect_receipt_failure(
            directory_fsync_receipt,
            "do not retry",
            "receipt directory fsync failure",
        )
    finally:
        backup_module.os.fsync = original_fsync
    check(
        directory_fsync_receipt.exists(),
        "directory-fsync failure deleted the receipt",
    )
    expect_receipt_failure(
        directory_fsync_receipt,
        "already exists",
        "directory-fsync receipt retry",
    )

    hardlink_race_receipt = atomic_path("3")
    hardlink_race_alias = atomic_root / "hardlink-race-alias"

    def add_hardlink_after_file_fsync(descriptor: int) -> None:
        original_fsync(descriptor)
        if (
            stat.S_ISREG(backup_module.os.fstat(descriptor).st_mode)
            and not hardlink_race_alias.exists()
        ):
            os.link(hardlink_race_receipt, hardlink_race_alias)

    backup_module.os.fsync = add_hardlink_after_file_fsync
    try:
        expect_receipt_failure(
            hardlink_race_receipt,
            "do not retry",
            "receipt hardlink race",
        )
    finally:
        backup_module.os.fsync = original_fsync
    check(
        hardlink_race_receipt.exists()
        and hardlink_race_alias.exists()
        and hardlink_race_receipt.stat().st_ino == hardlink_race_alias.stat().st_ino
        and hardlink_race_receipt.stat().st_nlink == 2,
        "hardlink-race receipt was deleted or replaced",
    )
    expect_receipt_failure(
        hardlink_race_receipt,
        "already exists",
        "hardlink-race receipt retry",
    )

    close_failure_receipt = atomic_path("4")
    original_close = backup_module.os.close
    unclosed_descriptor: list[int] = []

    def fail_receipt_close(descriptor: int) -> None:
        try:
            is_regular = stat.S_ISREG(backup_module.os.fstat(descriptor).st_mode)
        except OSError:
            is_regular = False
        if is_regular and not unclosed_descriptor:
            unclosed_descriptor.append(descriptor)
            raise OSError("injected receipt close failure")
        original_close(descriptor)

    backup_module.os.close = fail_receipt_close
    try:
        expect_receipt_failure(
            close_failure_receipt,
            "do not retry",
            "receipt close failure",
        )
    finally:
        backup_module.os.close = original_close
        for descriptor in unclosed_descriptor:
            original_close(descriptor)
    check(close_failure_receipt.exists(), "close failure deleted the receipt")
    expect_receipt_failure(
        close_failure_receipt,
        "already exists",
        "close-failure receipt retry",
    )

    result = invoke("restore", str(backup_set))
    check(result.returncode == 2 and "invalid choice" in result.stderr, "an undocumented restore command exists")
finally:
    for listener in listeners:
        listener.close()
    shutil.rmtree(temporary)
