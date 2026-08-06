#!/usr/bin/python3
"""Isolated fail-closed tests for pvn-db-backup."""

from __future__ import annotations

import copy
import hashlib
import json
import os
import pathlib
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


temporary = pathlib.Path(tempfile.mkdtemp(prefix=".pvn-db-backup-test-", dir=REPO))
os.chmod(temporary, 0o700)
listeners: list[socket.socket] = []
try:
    fake_bin = temporary / "bin"
    state = temporary / "state"
    socket_root = temporary / "sockets"
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
        "[ \"$1 $2 $3\" = 'is-active --quiet pvn-central.target' ] || exit 2\n",
        encoding="ascii",
    )
    (fake_bin / "pvnctl").write_text(
        f"""#!/usr/bin/python3
import json
import pathlib
import sys

status = pathlib.Path({str(status_path)!r})
sequence = pathlib.Path({str(sequence_path)!r})
counter = pathlib.Path({str(counter_path)!r})
if sys.argv[1:] != ["central", "status"]:
    raise SystemExit(2)
if sequence.exists():
    reports = json.loads(sequence.read_text())
    index = int(counter.read_text()) if counter.exists() else 0
    report = reports[min(index, len(reports) - 1)]
    counter.write_text(str(index + 1))
else:
    report = json.loads(status.read_text())
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
    replacements = {
        "/usr/bin/systemctl": str(fake_bin / "systemctl"),
        "/usr/sbin/pvnctl": str(fake_bin / "pvnctl"),
        "/usr/bin/ovsdb-client": str(fake_bin / "ovsdb-client"),
        "/usr/bin/ovsdb-tool": str(fake_bin / "ovsdb-tool"),
        "/etc/pvn/central/enabled": str(marker),
        "/run/lock/pvn-db-backup.lock": str(temporary / "backup.lock"),
        "/run/pvn-control/pvn-control-db.sock": str(sockets["PVN_Control"]),
        "/run/ovn/ovnnb_db.sock": str(sockets["OVN_Northbound"]),
        "/run/ovn/ovnsb_db.sock": str(sockets["OVN_Southbound"]),
    }
    for old, new in replacements.items():
        source = source.replace(old, new)
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
    check(len(manifest["databases"]) == 3, "all create did not capture three databases")
    for entry in manifest["databases"]:
        check(
            stat.S_IMODE((backup_set / entry["file"]).stat().st_mode) == 0o600,
            f"{entry['file']} mode is not 0600",
        )
    verified = require_success(invoke("verify", str(backup_set)), "offline verify")
    check(verified["verified"] is True, "verify did not report success")
    check("tool compact" in calls.read_text(), "verify did not compact an offline copy")

    reset_status()
    selected = require_success(
        invoke(
            "create",
            "--output",
            str(output_root),
            "--database",
            "ovn-nb",
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

    result = invoke("restore", str(backup_set))
    check(result.returncode == 2 and "invalid choice" in result.stderr, "an undocumented restore command exists")
finally:
    for listener in listeners:
        listener.close()
    shutil.rmtree(temporary)
