#!/usr/bin/python3
"""Isolated fail-closed tests for pvn-db-backup."""

from __future__ import annotations

import copy
import datetime
import hashlib
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
    for entry in manifest["databases"]:
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

    result = invoke("restore", str(backup_set))
    check(result.returncode == 2 and "invalid choice" in result.stderr, "an undocumented restore command exists")
finally:
    for listener in listeners:
        listener.close()
    shutil.rmtree(temporary)
