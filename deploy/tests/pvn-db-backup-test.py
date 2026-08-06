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


def encode_ovsdb_record(document: object) -> bytes:
    body = (
        json.dumps(document, sort_keys=True, separators=(",", ":")) + "\n"
    ).encode("utf-8")
    digest = hashlib.sha1(body, usedforsecurity=False).hexdigest()
    return f"OVSDB JSON {len(body)} {digest}\n".encode("ascii") + body


def decode_ovsdb_records(payload: bytes) -> list[dict]:
    records: list[dict] = []
    offset = 0
    while offset < len(payload):
        header_end = payload.find(b"\n", offset)
        check(header_end >= 0, "fixture OVSDB record has no header LF")
        header = payload[offset : header_end + 1]
        match = re.fullmatch(rb"OVSDB JSON ([1-9][0-9]*) ([0-9a-f]{40})\n", header)
        check(match is not None, "fixture OVSDB record header is invalid")
        length = int(match.group(1))
        start = header_end + 1
        body = payload[start : start + length]
        check(len(body) == length, "fixture OVSDB record is truncated")
        check(
            hashlib.sha1(body, usedforsecurity=False).hexdigest().encode("ascii")
            == match.group(2),
            "fixture OVSDB record hash is invalid",
        )
        document = json.loads(body)
        check(isinstance(document, dict), "fixture OVSDB record is not an object")
        records.append(document)
        offset = start + length
    return records


def rewrite_ovsdb_records(path: pathlib.Path, records: list[dict]) -> bytes:
    payload = b"".join(encode_ovsdb_record(record) for record in records)
    path.write_bytes(payload)
    path.chmod(0o600)
    return payload


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
    conversion_override = state / "needs-conversion.json"
    mutate_on_show_log = state / "mutate-on-show-log"
    replace_on_show_log = state / "replace-on-show-log"
    mutate_manifest_on_show_log = state / "mutate-manifest-on-show-log"
    replace_manifest_on_show_log = state / "replace-manifest-on-show-log"
    mutate_earlier_snapshot_on_show_log = state / "mutate-earlier-snapshot-on-show-log.json"
    final_anchor_race = state / "final-anchor-race.json"
    replace_schema_on_conversion = state / "replace-schema-on-conversion"
    replace_schema_on_create = state / "replace-schema-on-create"
    unit_override = state / "unit-override"
    age_status_marker = state / "age-status-on-central-read.json"
    replace_on_central_status = state / "replace-on-central-status.json"

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
    schema_root = temporary / "schemas"
    schema_root.mkdir(mode=0o700)
    schema_paths = {
        "PVN_Control": schema_root / "PVN_Control.ovsschema",
        "OVN_Northbound": schema_root / "ovn-nb.ovsschema",
        "OVN_Southbound": schema_root / "ovn-sb.ovsschema",
    }
    installed_schemas = {
        name: {
            "name": name,
            "version": versions[name],
            "cksum": checksums[name],
            "tables": {
                "Fixture": {
                    "columns": {
                        "value": {"type": {"key": "string"}},
                    },
                    "indexes": [["value"]],
                }
            },
        }
        for name in versions
    }
    normalized_schemas = copy.deepcopy(installed_schemas)
    for document in normalized_schemas.values():
        document["tables"]["Fixture"]["columns"]["value"]["type"] = "string"
    for name, path in schema_paths.items():
        path.write_text(
            json.dumps(installed_schemas[name], sort_keys=True) + "\n",
            encoding="utf-8",
        )
        path.chmod(0o644)
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
        "  pvn-node.target|pvn-node-ready.service|pvn-central.target|ovn-northd.service|pvn-ovn-northd-ready.service|pvn-ovn-db-listeners.service|pvn-ovn-host-config.service|pvn-manager.service|pvn-agent.service|ovn-controller.service) echo inactive ;;\n"
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
replace_on_central_status = pathlib.Path({str(replace_on_central_status)!r})
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
if replace_on_central_status.exists():
    replacement = json.loads(replace_on_central_status.read_text())
    replacement["remaining"] -= 1
    if replacement["remaining"] <= 0:
        target = pathlib.Path(replacement["path"])
        temporary = target.parent / ("." + target.name + ".replacement-race")
        if "payload" in replacement:
            temporary.write_text(replacement["payload"], encoding="utf-8")
        else:
            temporary.write_bytes(target.read_bytes())
        temporary.chmod(target.stat().st_mode & 0o777)
        os.replace(temporary, target)
        replace_on_central_status.unlink()
    else:
        replace_on_central_status.write_text(json.dumps(replacement))
print(json.dumps(report))
""",
        encoding="ascii",
    )
    socket_map = {f"unix:{path}": name for name, path in sockets.items()}
    (fake_bin / "ovsdb-client").write_text(
        f"""#!/usr/bin/python3
import hashlib
import json
import pathlib
import sys

calls = pathlib.Path({str(calls)!r})
failure = pathlib.Path({str(fail_database)!r})
conversion_override = pathlib.Path({str(conversion_override)!r})
replace_schema_on_conversion = pathlib.Path({str(replace_schema_on_conversion)!r})
socket_map = {socket_map!r}
schema_map = {dict((str(path), name) for name, path in schema_paths.items())!r}
schemas = {normalized_schemas!r}
versions = {versions!r}
checksums = {checksums!r}

def record(document):
    body = (json.dumps(document, sort_keys=True, separators=(",", ":")) + "\\n").encode()
    digest = hashlib.sha1(body, usedforsecurity=False).hexdigest()
    return f"OVSDB JSON {{len(body)}} {{digest}}\\n".encode() + body

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
elif operation == "needs-conversion" and len(sys.argv) == 5:
    if schema_map.get(sys.argv[4]) != database:
        raise SystemExit(2)
    if replace_schema_on_conversion.exists() and replace_schema_on_conversion.read_text().strip() == sys.argv[4]:
        schema = pathlib.Path(sys.argv[4])
        replacement = schema.parent / ("." + schema.name + ".conversion-replacement")
        replacement.write_bytes(schema.read_bytes())
        replacement.chmod(schema.stat().st_mode & 0o777)
        replacement.replace(schema)
        replace_schema_on_conversion.unlink()
    overrides = json.loads(conversion_override.read_text()) if conversion_override.exists() else {{}}
    if database != "PVN_Control":
        print("fixture clustered schema warning", file=sys.stderr)
    output = overrides.get(database, "no\\n")
    if isinstance(output, list):
        if not output:
            raise SystemExit(2)
        selected = output.pop(0)
        overrides[database] = output
        conversion_override.write_text(json.dumps(overrides))
        output = selected
    sys.stdout.write(output)
elif operation == "get-schema-version" and sys.argv[4:] == [database]:
    print(versions[database])
elif operation == "get-schema-cksum" and sys.argv[4:] == [database]:
    print(checksums[database])
elif operation == "backup" and sys.argv[4:] == [database]:
    if failure.exists() and failure.read_text().strip() == database:
        print("injected backup failure", file=sys.stderr)
        raise SystemExit(9)
    sys.stdout.buffer.write(
        record(schemas[database])
        + record({{"_date": 1, "Fixture": {{database + "-record": {{"value": database}}}}}})
    )
else:
    raise SystemExit(2)
""",
        encoding="ascii",
    )
    (fake_bin / "ovsdb-tool").write_text(
        f"""#!/usr/bin/python3
import hashlib
import json
import pathlib
import sys

calls = pathlib.Path({str(calls)!r})
mutate_on_show_log = pathlib.Path({str(mutate_on_show_log)!r})
replace_on_show_log = pathlib.Path({str(replace_on_show_log)!r})
mutate_manifest_on_show_log = pathlib.Path({str(mutate_manifest_on_show_log)!r})
replace_manifest_on_show_log = pathlib.Path({str(replace_manifest_on_show_log)!r})
mutate_earlier_snapshot_on_show_log = pathlib.Path({str(mutate_earlier_snapshot_on_show_log)!r})
final_anchor_race = pathlib.Path({str(final_anchor_race)!r})
replace_schema_on_create = pathlib.Path({str(replace_schema_on_create)!r})

def record(document):
    body = (json.dumps(document, sort_keys=True, separators=(",", ":")) + "\\n").encode()
    digest = hashlib.sha1(body, usedforsecurity=False).hexdigest()
    return f"OVSDB JSON {{len(body)}} {{digest}}\\n".encode() + body

def records(path):
    payload = path.read_bytes()
    result = []
    offset = 0
    while offset < len(payload):
        end = payload.find(b"\\n", offset)
        if end < 0:
            raise SystemExit(3)
        header = payload[offset:end + 1].decode("ascii").split(" ")
        if len(header) != 4 or header[:2] != ["OVSDB", "JSON"]:
            raise SystemExit(3)
        length = int(header[2])
        start = end + 1
        body = payload[start:start + length]
        if len(body) != length or not body.endswith(b"\\n"):
            raise SystemExit(3)
        if hashlib.sha1(body, usedforsecurity=False).hexdigest() + "\\n" != header[3]:
            raise SystemExit(3)
        result.append(json.loads(body))
        offset = start + length
    if not result:
        raise SystemExit(3)
    return result

with calls.open("a", encoding="ascii") as log:
    log.write("tool " + " ".join(sys.argv[1:]) + "\\n")
if len(sys.argv) < 3:
    raise SystemExit(2)
operation = sys.argv[1]
source = pathlib.Path(sys.argv[2])
if operation == "create" and len(sys.argv) == 4:
    if source.exists():
        raise SystemExit(4)
    schema = json.loads(pathlib.Path(sys.argv[3]).read_text())
    schema["tables"]["Fixture"]["columns"]["value"]["type"] = "string"
    if schema.get("cksum") == "":
        schema.pop("cksum")
    source.write_bytes(record(schema))
    source.chmod(0o600)
    installed = pathlib.Path(sys.argv[3])
    if replace_schema_on_create.exists() and replace_schema_on_create.read_text().strip() == str(installed):
        replacement = installed.parent / ("." + installed.name + ".create-replacement")
        replacement.write_bytes(installed.read_bytes())
        replacement.chmod(installed.stat().st_mode & 0o777)
        replacement.replace(installed)
        replace_schema_on_create.unlink()
elif operation == "compact" and len(sys.argv) == 4:
    payload = records(source)
    destination = pathlib.Path(sys.argv[3])
    if destination.exists():
        raise SystemExit(4)
    column = payload[0].get("tables", {{}}).get("Fixture", {{}}).get("columns", {{}}).get("value", {{}})
    if column.get("type") == {{"key": "string"}}:
        column["type"] = "string"
    if payload[0].get("cksum") == "":
        payload[0].pop("cksum")
    destination.write_bytes(b"".join(record(document) for document in payload))
    destination.chmod(0o600)
elif operation == "db-is-standalone" and len(sys.argv) == 3:
    payload = records(source)
elif operation == "db-name" and len(sys.argv) == 3:
    print(records(source)[0].get("name", ""))
elif operation == "db-version" and len(sys.argv) == 3:
    print(records(source)[0].get("version", ""))
elif operation == "db-cksum" and len(sys.argv) == 3:
    print(records(source)[0].get("cksum", ""))
elif operation == "show-log" and len(sys.argv) == 3:
    payload = records(source)
    if mutate_on_show_log.exists() and source.name == mutate_on_show_log.read_text().strip():
        with source.open("ab") as output:
            output.write(record({{"_date": 9, "Fixture": {{"raced": {{"value": "raced"}}}}}}))
        mutate_on_show_log.unlink()
    if replace_on_show_log.exists() and source.name == replace_on_show_log.read_text().strip():
        replacement = source.parent / ("." + source.name + ".show-log-replacement")
        replacement.write_bytes(source.read_bytes())
        replacement.chmod(source.stat().st_mode & 0o777)
        replacement.replace(source)
        replace_on_show_log.unlink()
    if mutate_manifest_on_show_log.exists() and source.name == mutate_manifest_on_show_log.read_text().strip():
        manifest = source.parent / "manifest.json"
        with manifest.open("ab") as output:
            output.write(b" ")
        mutate_manifest_on_show_log.unlink()
    if replace_manifest_on_show_log.exists() and source.name == replace_manifest_on_show_log.read_text().strip():
        manifest = source.parent / "manifest.json"
        replacement = manifest.parent / ".manifest.json.show-log-replacement"
        replacement.write_bytes(manifest.read_bytes())
        replacement.chmod(manifest.stat().st_mode & 0o777)
        replacement.replace(manifest)
        replace_manifest_on_show_log.unlink()
    if mutate_earlier_snapshot_on_show_log.exists():
        mutation = json.loads(mutate_earlier_snapshot_on_show_log.read_text())
        if source.name == mutation["trigger"]:
            target = pathlib.Path(mutation["target"])
            with target.open("ab") as output:
                output.write(record({{"_date": 10, "Fixture": {{"late": {{"value": "late"}}}}}}))
            mutate_earlier_snapshot_on_show_log.unlink()
    if final_anchor_race.exists():
        race = json.loads(final_anchor_race.read_text())
        if source.name == race["trigger"]:
            race["remaining"] -= 1
            if race["remaining"] <= 0:
                if race["action"] == "snapshot-append":
                    with source.open("ab") as output:
                        output.write(record({{"_date": 11, "Fixture": {{"final": {{"value": "final"}}}}}}))
                elif race["action"] == "manifest-replace":
                    manifest = source.parent / "manifest.json"
                    replacement = manifest.parent / ".manifest.json.final-anchor-replacement"
                    replacement.write_bytes(manifest.read_bytes())
                    replacement.chmod(manifest.stat().st_mode & 0o777)
                    replacement.replace(manifest)
                else:
                    raise SystemExit(2)
                final_anchor_race.unlink()
            else:
                final_anchor_race.write_text(json.dumps(race))
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
        "/usr/share/pvn/schema/PVN_Control.ovsschema": str(
            schema_paths["PVN_Control"]
        ),
        "/usr/share/ovn/ovn-nb.ovsschema": str(schema_paths["OVN_Northbound"]),
        "/usr/share/ovn/ovn-sb.ovsschema": str(schema_paths["OVN_Southbound"]),
    }
    for old, new in replacements.items():
        source = source.replace(old, new)
    check(
        receipt_root_literal not in source,
        "isolated test did not replace the production restore receipt root",
    )
    tested_script.write_text(source, encoding="utf-8")
    tested_script.chmod(0o755)

    tested_module_name = "pvn_db_backup_isolated_schema_test"
    tested_loader = importlib.machinery.SourceFileLoader(
        tested_module_name, str(tested_script)
    )
    tested_specification = importlib.util.spec_from_loader(
        tested_module_name, tested_loader
    )
    check(tested_specification is not None, "cannot load isolated backup module")
    tested_module = importlib.util.module_from_spec(tested_specification)
    sys.modules[tested_module_name] = tested_module
    tested_loader.exec_module(tested_module)

    parser_root = temporary / "schema-record-parser"
    parser_root.mkdir(mode=0o700)
    parser_database = tested_module.DATABASE_BY_KEY["pvn-control"]

    def record_with_body(body: bytes, *, length: int | None = None, digest: str | None = None) -> bytes:
        actual_digest = digest or hashlib.sha1(
            body, usedforsecurity=False
        ).hexdigest()
        return (
            f"OVSDB JSON {len(body) if length is None else length} {actual_digest}\n".encode(
                "ascii"
            )
            + body
        )

    def expect_schema_record_failure(payload: bytes, expected: str, label: str) -> None:
        path = parser_root / f"{len(list(parser_root.iterdir())):02d}.ovsdb"
        path.write_bytes(payload)
        path.chmod(0o600)
        try:
            tested_module.read_standalone_schema_record(
                path, parser_database, label
            )
        except tested_module.BackupError as error:
            check(
                expected in str(error),
                f"{label} returned the wrong error: {error}",
            )
            return
        raise AssertionError(f"{label} was accepted")

    valid_schema_record = encode_ovsdb_record(normalized_schemas["PVN_Control"])
    valid_with_trailing_transaction = (
        valid_schema_record
        + encode_ovsdb_record(
            {"_date": 3, "Fixture": {"trailing": {"value": "ok"}}}
        )
    )
    valid_parser_path = parser_root / "valid-with-trailing.ovsdb"
    valid_parser_path.write_bytes(valid_with_trailing_transaction)
    valid_parser_path.chmod(0o600)
    tested_module.read_standalone_schema_record(
        valid_parser_path, parser_database, "valid trailing-transaction snapshot"
    )

    valid_body = valid_schema_record.split(b"\n", 1)[1]
    expect_schema_record_failure(
        b"OVSDB JSON " + b"9" * 130 + b"\n{}\n",
        "header is too long",
        "overlong record header",
    )
    expect_schema_record_failure(
        record_with_body(valid_body, digest="0" * 40),
        "SHA-1 mismatch",
        "bad record SHA-1",
    )
    expect_schema_record_failure(
        b"OVSDB JSON 3 " + b"0" * 40 + b"{}\n",
        "invalid OVSDB JSON",
        "missing record-header LF",
    )
    body_without_lf = valid_body[:-1]
    expect_schema_record_failure(
        record_with_body(body_without_lf),
        "does not end with LF",
        "schema body without terminal LF",
    )
    duplicate_body = (
        b'{"name":"PVN_Control","version":"1.0.0","cksum":"",'
        b'"tables":{"Fixture":{"columns":{"value":'
        b'{"type":"string","type":"integer"}}}}}\n'
    )
    expect_schema_record_failure(
        record_with_body(duplicate_body),
        "duplicate JSON key",
        "duplicate nested schema key",
    )
    expect_schema_record_failure(
        record_with_body(valid_body, length=len(valid_body) + 10),
        "truncated",
        "truncated schema record",
    )
    expect_schema_record_failure(
        record_with_body(
            b"{}\n", length=tested_module.MAX_SCHEMA + 1
        ),
        "too large",
        "oversized declared schema record",
    )
    wrong_name = copy.deepcopy(normalized_schemas["PVN_Control"])
    wrong_name["name"] = "Wrong_Control"
    expect_schema_record_failure(
        encode_ovsdb_record(wrong_name),
        "does not contain the PVN_Control schema",
        "wrong schema name",
    )
    invalid_checksum = copy.deepcopy(normalized_schemas["PVN_Control"])
    invalid_checksum["cksum"] = 7
    expect_schema_record_failure(
        encode_ovsdb_record(invalid_checksum),
        "invalid optional schema checksum",
        "non-string optional checksum",
    )

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
    baseline_calls = calls.read_text(encoding="ascii")
    for remote, database in socket_map.items():
        expected = (
            f"client --timeout=10 needs-conversion {remote} "
            f"{schema_paths[database]}"
        )
        check(
            baseline_calls.count(expected) >= 4,
            f"{database} was not compatibility-gated around its live backup",
        )
    check(
        f"tool create" in baseline_calls
        and str(schema_paths["PVN_Control"]) in baseline_calls,
        "PVN installed schema was not normalized through ovsdb-tool create",
    )
    check(
        not any(
            f"tool create" in line and str(schema_paths[name]) in line
            for line in baseline_calls.splitlines()
            for name in ("OVN_Northbound", "OVN_Southbound")
        ),
        "offline installed-schema normalization escaped the PVN_Control scope",
    )

    database_keys = {
        "PVN_Control": "pvn-control",
        "OVN_Northbound": "ovn-nb",
        "OVN_Southbound": "ovn-sb",
    }
    for name, key in database_keys.items():
        conversion_override.write_text(
            json.dumps({name: "yes\n"}), encoding="utf-8"
        )
        rejected_root = temporary / f"conversion-yes-{key}"
        reset_status()
        result = invoke(
            "create", "--output", str(rejected_root), "--database", key
        )
        check(
            result.returncode != 0
            and "not exactly the installed package schema" in result.stderr,
            f"{name} needs-conversion yes was accepted",
        )
        check(
            rejected_root.is_dir() and not list(rejected_root.iterdir()),
            f"{name} conversion rejection published a backup set",
        )
    for index, malformed in enumerate(("no", "maybe\n", "no\nyes\n")):
        conversion_override.write_text(
            json.dumps({"PVN_Control": malformed}), encoding="utf-8"
        )
        rejected_root = temporary / f"conversion-malformed-{index}"
        reset_status()
        result = invoke(
            "create",
            "--output",
            str(rejected_root),
            "--database",
            "pvn-control",
        )
        check(
            result.returncode != 0
            and "not exactly the installed package schema" in result.stderr,
            f"malformed needs-conversion output {malformed!r} was accepted",
        )
        check(
            rejected_root.is_dir() and not list(rejected_root.iterdir()),
            "malformed conversion output published a backup set",
        )
    conversion_override.write_text(
        json.dumps({"PVN_Control": ["no\n", "no\n", "yes\n"]}),
        encoding="utf-8",
    )
    post_snapshot_drift_root = temporary / "post-snapshot-schema-drift"
    reset_status()
    result = invoke(
        "create",
        "--output",
        str(post_snapshot_drift_root),
        "--database",
        "pvn-control",
    )
    check(
        result.returncode != 0
        and "not exactly the installed package schema" in result.stderr,
        "schema conversion immediately after snapshot capture was accepted",
    )
    check(
        post_snapshot_drift_root.is_dir()
        and not list(post_snapshot_drift_root.iterdir()),
        "post-snapshot schema drift published a backup set",
    )
    conversion_override.unlink()

    replace_schema_on_conversion.write_text(
        str(schema_paths["PVN_Control"]), encoding="utf-8"
    )
    conversion_replace_root = temporary / "schema-replaced-during-conversion"
    reset_status()
    result = invoke(
        "create",
        "--output",
        str(conversion_replace_root),
        "--database",
        "pvn-control",
    )
    check(
        result.returncode != 0
        and "schema changed during live validation" in result.stderr,
        "same-bytes installed schema replacement during live validation was accepted",
    )
    check(
        not replace_schema_on_conversion.exists()
        and conversion_replace_root.is_dir()
        and not list(conversion_replace_root.iterdir()),
        "live schema replacement published a backup or left its hook",
    )

    replace_schema_on_create.write_text(
        str(schema_paths["PVN_Control"]), encoding="utf-8"
    )
    normalization_replace_root = temporary / "schema-replaced-during-normalization"
    reset_status()
    result = invoke(
        "create",
        "--output",
        str(normalization_replace_root),
        "--database",
        "pvn-control",
    )
    check(
        result.returncode != 0 and "changed during normalization" in result.stderr,
        "same-bytes installed schema replacement during normalization was accepted",
    )
    check(
        not replace_schema_on_create.exists()
        and normalization_replace_root.is_dir()
        and not list(normalization_replace_root.iterdir()),
        "normalization schema replacement published a backup or left its hook",
    )

    for action in ("snapshot-append", "manifest-replace"):
        final_anchor_race.write_text(
            json.dumps(
                {
                    "trigger": "pvn-control.ovsdb",
                    "remaining": 3,
                    "action": action,
                }
            ),
            encoding="utf-8",
        )
        final_anchor_root = temporary / f"final-anchor-{action}"
        reset_status()
        result = invoke(
            "create",
            "--output",
            str(final_anchor_root),
            "--database",
            "pvn-control",
        )
        check(
            result.returncode != 0 and "changed" in result.stderr,
            f"{action} during the final create anchor was accepted",
        )
        check(
            not final_anchor_race.exists()
            and final_anchor_root.is_dir()
            and not list(final_anchor_root.iterdir()),
            f"{action} during the final create anchor published a backup",
        )

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

    legacy_syntax = clone_backup(legacy, "legacy-equivalent-schema-syntax")
    legacy_syntax_snapshot = legacy_syntax / "pvn-control.ovsdb"
    legacy_syntax_records = decode_ovsdb_records(
        legacy_syntax_snapshot.read_bytes()
    )
    legacy_syntax_records[0]["tables"]["Fixture"]["columns"]["value"][
        "type"
    ] = {"key": "string"}
    legacy_syntax_payload = rewrite_ovsdb_records(
        legacy_syntax_snapshot, legacy_syntax_records
    )
    legacy_syntax_manifest_path = legacy_syntax / "manifest.json"
    legacy_syntax_manifest = json.loads(
        legacy_syntax_manifest_path.read_text(encoding="utf-8")
    )
    legacy_syntax_manifest["databases"][0].update(
        bytes=len(legacy_syntax_payload),
        sha256=hashlib.sha256(legacy_syntax_payload).hexdigest(),
    )
    legacy_syntax_manifest_path.write_text(
        json.dumps(legacy_syntax_manifest, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    require_success(
        invoke("verify", str(legacy_syntax)),
        "legacy equivalent-schema normalization verify",
    )

    installed_pvn_before = schema_paths["PVN_Control"].read_bytes()
    future_installed = copy.deepcopy(installed_schemas["PVN_Control"])
    future_installed["tables"]["Fixture"]["columns"]["future"] = {
        "type": "string"
    }
    schema_paths["PVN_Control"].write_text(
        json.dumps(future_installed, sort_keys=True) + "\n", encoding="utf-8"
    )
    schema_paths["PVN_Control"].chmod(0o644)
    try:
        require_success(
            invoke("verify", str(legacy)),
            "archival verify with a newer installed PVN schema",
        )
        try:
            tested_module.verify_backup_set(
                legacy, require_current_pvn_schema=True
            )
        except tested_module.BackupError as error:
            check(
                "differs from the installed package schema" in str(error),
                f"strict legacy compatibility returned the wrong error: {error}",
            )
        else:
            raise AssertionError(
                "strict compatibility accepted a historical PVN schema"
            )
    finally:
        schema_paths["PVN_Control"].write_bytes(installed_pvn_before)
        schema_paths["PVN_Control"].chmod(0o644)

    same_metadata_drift = clone_backup(backup_set, "same-metadata-pvn-schema-drift")
    drift_snapshot = same_metadata_drift / "pvn-control.ovsdb"
    drift_records = decode_ovsdb_records(drift_snapshot.read_bytes())
    drift_records[0]["tables"]["Fixture"]["columns"]["shadow"] = {
        "type": "string"
    }
    drift_payload = rewrite_ovsdb_records(drift_snapshot, drift_records)
    drift_manifest_path = same_metadata_drift / "manifest.json"
    drift_manifest = json.loads(drift_manifest_path.read_text(encoding="utf-8"))
    drift_manifest["databases"][0].update(
        bytes=len(drift_payload), sha256=hashlib.sha256(drift_payload).hexdigest()
    )
    drift_manifest_path.write_text(
        json.dumps(drift_manifest, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    require_success(
        invoke("verify", str(same_metadata_drift)),
        "archival same-metadata schema verify",
    )
    try:
        tested_module.verify_backup_set(
            same_metadata_drift, require_current_pvn_schema=True
        )
    except tested_module.BackupError as error:
        check(
            "differs from the installed package schema" in str(error),
            f"same-metadata strict gate returned the wrong error: {error}",
        )
    else:
        raise AssertionError("strict gate accepted same-version empty-cksum PVN drift")

    empty_nb_checksum = clone_backup(backup_set, "empty-northbound-checksum")
    empty_nb_snapshot = empty_nb_checksum / "ovn-northbound.ovsdb"
    empty_nb_records = decode_ovsdb_records(empty_nb_snapshot.read_bytes())
    empty_nb_records[0]["cksum"] = ""
    empty_nb_payload = rewrite_ovsdb_records(empty_nb_snapshot, empty_nb_records)
    empty_nb_manifest_path = empty_nb_checksum / "manifest.json"
    empty_nb_manifest = json.loads(
        empty_nb_manifest_path.read_text(encoding="utf-8")
    )
    empty_nb_manifest["databases"][1].update(
        bytes=len(empty_nb_payload),
        sha256=hashlib.sha256(empty_nb_payload).hexdigest(),
        schema_checksum="",
    )
    empty_nb_manifest_path.write_text(
        json.dumps(empty_nb_manifest, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    result = invoke("verify", str(empty_nb_checksum))
    check(
        result.returncode != 0 and "schema checksum is empty for ovn-nb" in result.stderr,
        "empty OVN Northbound schema checksum was accepted",
    )

    raced_backup = clone_backup(backup_set, "snapshot-mutated-during-verify")
    mutate_on_show_log.write_text("pvn-control.ovsdb\n", encoding="ascii")
    result = invoke("verify", str(raced_backup))
    check(
        result.returncode != 0
        and "snapshot changed during verification" in result.stderr,
        "snapshot mutation between structural checks was accepted",
    )
    check(
        not mutate_on_show_log.exists(),
        "snapshot mutation hook was not exercised",
    )
    replaced_backup = clone_backup(backup_set, "snapshot-replaced-during-verify")
    replace_on_show_log.write_text("pvn-control.ovsdb\n", encoding="ascii")
    result = invoke("verify", str(replaced_backup))
    check(
        result.returncode != 0
        and "snapshot changed during verification" in result.stderr,
        "same-bytes snapshot replacement during verify was accepted",
    )
    check(
        not replace_on_show_log.exists(),
        "same-bytes snapshot replacement hook was not exercised",
    )
    mutated_manifest_backup = clone_backup(
        backup_set, "manifest-mutated-during-verify"
    )
    mutate_manifest_on_show_log.write_text(
        "pvn-control.ovsdb\n", encoding="ascii"
    )
    result = invoke("verify", str(mutated_manifest_backup))
    check(
        result.returncode != 0
        and "backup manifest changed during verification" in result.stderr,
        "manifest mutation during verify was accepted",
    )
    check(
        not mutate_manifest_on_show_log.exists(),
        "manifest mutation hook was not exercised",
    )
    replaced_manifest_backup = clone_backup(
        backup_set, "manifest-replaced-during-verify"
    )
    replace_manifest_on_show_log.write_text(
        "pvn-control.ovsdb\n", encoding="ascii"
    )
    result = invoke("verify", str(replaced_manifest_backup))
    check(
        result.returncode != 0 and "changed during verification" in result.stderr,
        "same-bytes manifest replacement during verify was accepted",
    )
    check(
        not replace_manifest_on_show_log.exists(),
        "same-bytes manifest replacement hook was not exercised",
    )
    late_snapshot_backup = clone_backup(
        backup_set, "earlier-snapshot-mutated-by-later-verify"
    )
    mutate_earlier_snapshot_on_show_log.write_text(
        json.dumps(
            {
                "trigger": "ovn-northbound.ovsdb",
                "target": str(late_snapshot_backup / "pvn-control.ovsdb"),
            }
        ),
        encoding="utf-8",
    )
    result = invoke("verify", str(late_snapshot_backup))
    check(
        result.returncode != 0 and "snapshot changed" in result.stderr,
        "an earlier snapshot mutation during later verification was accepted",
    )
    check(
        not mutate_earlier_snapshot_on_show_log.exists(),
        "earlier-snapshot mutation hook was not exercised",
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

    pvn_leader_status = copy.deepcopy(base_status)
    pvn_leader_status["databases"][0].update(role="leader", leader="self")
    reset_status(pvn_leader_status)
    pvn_recovery = require_success(
        invoke(
            "create",
            "--output",
            str(output_root),
            "--database",
            "pvn-control",
            "--recovery-window",
        ),
        "PVN recovery-window create",
    )
    pvn_recovery_path = pathlib.Path(pvn_recovery["backup_set"])
    pvn_recovery_manifest = json.loads(
        (pvn_recovery_path / "manifest.json").read_text(encoding="utf-8")
    )
    pvn_recovery_entry = pvn_recovery_manifest["databases"][0]
    pvn_follower_b = copy.deepcopy(pvn_leader_status)
    pvn_follower_b["databases"][0].update(
        server_id="dddddddd-dddd-4ddd-8ddd-dddddddddd01",
        address="ssl:192.0.2.11:6646",
        role="follower",
        leader="aaaa",
    )
    pvn_follower_c = copy.deepcopy(pvn_leader_status)
    pvn_follower_c["databases"][0].update(
        server_id="eeeeeeee-eeee-4eee-8eee-eeeeeeeeee02",
        address="ssl:192.0.2.12:6646",
        role="follower",
        leader="aaaa",
    )
    pvn_status_paths = {
        source_hostname: write_status_report(
            "pvn-source-status.json", pvn_leader_status
        ),
        "pvn-voter-b": write_status_report(
            "pvn-voter-b-status.json", pvn_follower_b
        ),
        "pvn-voter-c": write_status_report(
            "pvn-voter-c-status.json", pvn_follower_c
        ),
    }
    pvn_current_digest = pvn_recovery_entry["sha256"]
    pvn_current_gate_arguments = [
        "pre-restore",
        str(pvn_recovery_path),
        "--database",
        "pvn-control",
        "--captured-after",
        pvn_recovery_entry["capture_started_at"],
        "--expected-sha256",
        pvn_current_digest,
    ]
    for node, path in pvn_status_paths.items():
        pvn_current_gate_arguments.extend(("--voter-status", f"{node}={path}"))
    reset_status(pvn_leader_status)
    pvn_pre_restore = require_success(
        invoke(*pvn_current_gate_arguments), "current PVN pre-restore"
    )
    check(
        pvn_pre_restore["database_key"] == "pvn-control"
        and pvn_pre_restore["restore_ready"] is True,
        "current PVN pre-restore did not traverse the strict schema gate",
    )
    reset_status(pvn_leader_status)
    pvn_reserved = require_success(
        invoke(
            "reserve-restore",
            *pvn_current_gate_arguments[1:],
            "--confirm",
            f"RESTORE PVN_Control {pvn_current_digest}",
        ),
        "current PVN restore reservation",
    )
    pvn_receipt = pathlib.Path(pvn_reserved["receipt_path"])
    check(
        pvn_reserved["database_key"] == "pvn-control"
        and pvn_reserved["snapshot"] == "pvn-control.ovsdb"
        and pvn_receipt.is_file(),
        "current PVN reservation did not persist the expected receipt",
    )
    pvn_receipt.unlink()
    receipt_root.rmdir()

    replace_on_central_status.write_text(
        json.dumps(
            {"remaining": 2, "path": str(pvn_recovery_path / "pvn-control.ovsdb")}
        ),
        encoding="utf-8",
    )
    reset_status(pvn_leader_status)
    result = invoke(
        "reserve-restore",
        *pvn_current_gate_arguments[1:],
        "--confirm",
        f"RESTORE PVN_Control {pvn_current_digest}",
    )
    check(
        result.returncode != 0 and "recovery snapshot changed" in result.stderr,
        "late same-bytes recovery snapshot replacement created a reservation",
    )
    check(
        not replace_on_central_status.exists() and not receipt_root.exists(),
        "late snapshot replacement left a hook or restore receipt",
    )
    replace_on_central_status.write_text(
        json.dumps(
            {"remaining": 2, "path": str(pvn_recovery_path / "manifest.json")}
        ),
        encoding="utf-8",
    )
    reset_status(pvn_leader_status)
    result = invoke(
        "reserve-restore",
        *pvn_current_gate_arguments[1:],
        "--confirm",
        f"RESTORE PVN_Control {pvn_current_digest}",
    )
    check(
        result.returncode != 0 and "backup manifest changed" in result.stderr,
        "late same-bytes manifest replacement created a reservation",
    )
    check(
        not replace_on_central_status.exists() and not receipt_root.exists(),
        "late manifest replacement left a hook or restore receipt",
    )

    installed_schema_before_switch = schema_paths["PVN_Control"].read_bytes()
    switched_schema = copy.deepcopy(installed_schemas["PVN_Control"])
    switched_schema["tables"]["Fixture"]["columns"]["same_metadata_switch"] = {
        "type": "string"
    }
    switched_schema_payload = json.dumps(switched_schema, sort_keys=True) + "\n"
    replace_on_central_status.write_text(
        json.dumps(
            {
                "remaining": 1,
                "path": str(schema_paths["PVN_Control"]),
                "payload": switched_schema_payload,
            }
        ),
        encoding="utf-8",
    )
    reset_status(pvn_leader_status)
    result = invoke(*pvn_current_gate_arguments)
    check(
        result.returncode != 0
        and "differs from the installed package schema" in result.stderr,
        "pre-restore accepted a same-metadata live/package schema switch",
    )
    check(
        not replace_on_central_status.exists(),
        "pre-restore schema-switch hook was not exercised",
    )
    schema_paths["PVN_Control"].write_bytes(installed_schema_before_switch)
    schema_paths["PVN_Control"].chmod(0o644)

    replace_on_central_status.write_text(
        json.dumps(
            {
                "remaining": 2,
                "path": str(schema_paths["PVN_Control"]),
                "payload": switched_schema_payload,
            }
        ),
        encoding="utf-8",
    )
    reset_status(pvn_leader_status)
    result = invoke(
        "reserve-restore",
        *pvn_current_gate_arguments[1:],
        "--confirm",
        f"RESTORE PVN_Control {pvn_current_digest}",
    )
    check(
        result.returncode != 0
        and "differs from the installed package schema" in result.stderr,
        "restore reservation accepted a final same-metadata schema switch",
    )
    check(
        not replace_on_central_status.exists() and not receipt_root.exists(),
        "final schema switch left a hook or restore receipt",
    )
    schema_paths["PVN_Control"].write_bytes(installed_schema_before_switch)
    schema_paths["PVN_Control"].chmod(0o644)

    pvn_drift_set = clone_backup(
        pvn_recovery_path, "pvn-recovery-same-metadata-schema-drift"
    )
    pvn_drift_snapshot = pvn_drift_set / "pvn-control.ovsdb"
    pvn_drift_records = decode_ovsdb_records(pvn_drift_snapshot.read_bytes())
    pvn_drift_records[0]["tables"]["Fixture"]["columns"]["shadow"] = {
        "type": "string"
    }
    pvn_drift_payload = rewrite_ovsdb_records(
        pvn_drift_snapshot, pvn_drift_records
    )
    pvn_drift_manifest_path = pvn_drift_set / "manifest.json"
    pvn_drift_manifest = json.loads(
        pvn_drift_manifest_path.read_text(encoding="utf-8")
    )
    pvn_drift_manifest["databases"][0].update(
        bytes=len(pvn_drift_payload),
        sha256=hashlib.sha256(pvn_drift_payload).hexdigest(),
    )
    pvn_drift_manifest_path.write_text(
        json.dumps(pvn_drift_manifest, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    pvn_drift_digest = pvn_drift_manifest["databases"][0]["sha256"]
    require_success(
        invoke("verify", str(pvn_drift_set)),
        "archival PVN recovery-window drift verify",
    )
    pvn_gate_arguments = [
        "pre-restore",
        str(pvn_drift_set),
        "--database",
        "pvn-control",
        "--captured-after",
        pvn_recovery_entry["capture_started_at"],
        "--expected-sha256",
        pvn_drift_digest,
    ]
    for node, path in pvn_status_paths.items():
        pvn_gate_arguments.extend(("--voter-status", f"{node}={path}"))
    reset_status(pvn_leader_status)
    result = invoke(*pvn_gate_arguments)
    check(
        result.returncode != 0
        and "differs from the installed package schema" in result.stderr,
        "PVN pre-restore accepted same-version empty-cksum schema drift",
    )
    reset_status(pvn_leader_status)
    result = invoke(
        "reserve-restore",
        *pvn_gate_arguments[1:],
        "--confirm",
        f"RESTORE PVN_Control {pvn_drift_digest}",
    )
    check(
        result.returncode != 0
        and "differs from the installed package schema" in result.stderr,
        "PVN restore reservation accepted same-version empty-cksum schema drift",
    )
    check(not receipt_root.exists(), "rejected PVN schema drift created a receipt")
    reset_status()

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
    distinct_snapshot_records = decode_ovsdb_records(
        distinct_snapshot_path.read_bytes()
    )
    distinct_snapshot_records.append(
        {"_date": 2, "Fixture": {"different-record": {"value": "different"}}}
    )
    rewrite_ovsdb_records(distinct_snapshot_path, distinct_snapshot_records)
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

    for active_node_unit in (
        "pvn-node.target",
        "pvn-node-ready.service",
        "pvn-ovn-host-config.service",
        "pvn-ovn-db-listeners.service",
    ):
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
    unit_override.write_text("pvn-ovn-northd-ready.service active\n", encoding="ascii")
    active_northd_gate_output = temporary / "active-northd-gate-backups"
    result = invoke(
        "create",
        "--output",
        str(active_northd_gate_output),
        "--database",
        "ovn-nb",
        "--recovery-window",
    )
    check(
        result.returncode != 0
        and "pvn-ovn-northd-ready.service inactive" in result.stderr,
        "recovery window accepted an active northd readiness gate",
    )
    check(
        not active_northd_gate_output.exists(),
        "active northd readiness rejection changed the output filesystem",
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
    wrong_records = decode_ovsdb_records(wrong_snapshot.read_bytes())
    wrong_records[0]["name"] = "Wrong_Control"
    encoded = rewrite_ovsdb_records(wrong_snapshot, wrong_records)
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
