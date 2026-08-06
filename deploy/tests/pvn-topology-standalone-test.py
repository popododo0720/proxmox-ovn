#!/usr/bin/env python3
"""End-to-end standalone coverage for pvn-topology's local-only path."""

from __future__ import annotations

import hashlib
from importlib.machinery import SourceFileLoader
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import types


REPO = Path(__file__).resolve().parents[2]
TOPOLOGY = REPO / "deploy/scripts/pvn-topology"
GENEVE = "192.168.100.0/24"
PROVIDER = "192.168.200.0/24"
ACK = "OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP"
CONFIRM = "standalone-prox1"


def sha(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def canonical(value: object) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def fail(message: str) -> None:
    raise AssertionError(message)


def production_remote_namespace() -> dict[str, object]:
    loader = SourceFileLoader("pvn_topology_standalone_test_module", str(TOPOLOGY))
    module = types.ModuleType(loader.name)
    loader.exec_module(module)
    marker = "\ntry:\n    main()\nexcept RemoteFailure as exc:"
    if module.REMOTE_HELPER.count(marker) != 1:
        fail("cannot isolate the production topology remote helper")
    source = module.REMOTE_HELPER.split(marker, 1)[0]
    namespace: dict[str, object] = {
        "__name__": "pvn_topology_standalone_remote_contract_test",
    }
    exec(compile(source, "<pvn-topology-standalone-remote>", "exec"), namespace)
    return namespace


def embedded_database_case(fault: str | None = None) -> list[dict[str, str]] | None:
    """Exercise the shipped DB proof, including adversarial path/schema drift."""
    remote = production_remote_namespace()
    expected_paths = (
        ("PVN_Control", "/var/lib/pvn/control-db/pvn_control.db",
         "/run/pvn-control/pvn-control-db.sock"),
        ("OVN_Northbound", "/var/lib/ovn/ovnnb_db.db",
         "/run/ovn/ovnnb_db.sock"),
        ("OVN_Southbound", "/var/lib/ovn/ovnsb_db.db",
         "/run/ovn/ovnsb_db.sock"),
    )
    if tuple(
        (name, str(database), str(sock))
        for name, database, sock in remote["STANDALONE_DATABASES"]
    ) != expected_paths:
        fail("production standalone DB paths drifted")

    with tempfile.TemporaryDirectory(prefix="pvn-standalone-db-") as raw:
        root = Path(raw)
        rows = []
        sockets: list[socket.socket] = []
        by_name: dict[str, tuple[Path, Path]] = {}
        for index, (name, _database, _socket) in enumerate(expected_paths):
            database = root / f"database-{index}.db"
            sock = root / f"database-{index}.sock"
            database.write_text(name + "\n", encoding="utf-8")
            database.chmod(0o640)
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            listener.bind(str(sock))
            sockets.append(listener)
            rows.append((name, database, sock))
            by_name[name] = (database, sock)
        remote["STANDALONE_DATABASES"] = tuple(rows)

        if fault == "db-symlink":
            database, _sock = by_name["PVN_Control"]
            target = root / "symlink-target"
            target.write_text("PVN_Control\n", encoding="utf-8")
            target.chmod(0o640)
            database.unlink()
            database.symlink_to(target)
        elif fault == "db-hardlink":
            database, _sock = by_name["PVN_Control"]
            os.link(database, root / "database-hardlink")
        elif fault == "db-writable":
            database, _sock = by_name["PVN_Control"]
            database.chmod(0o660)
        elif fault == "socket-regular":
            _database, sock = by_name["PVN_Control"]
            sock.unlink()
            sock.write_text("not a socket\n", encoding="utf-8")

        class Result:
            def __init__(self, returncode: int = 0, stdout: str = "", stderr: str = ""):
                self.returncode = returncode
                self.stdout = stdout
                self.stderr = stderr

        swapped_socket: list[socket.socket] = []

        def fake_run(argv, **_kwargs):
            command = argv[1] if argv and argv[0] == "/usr/bin/ovsdb-tool" else None
            database = Path(argv[-1]) if command and command != "db-is-standalone" else None
            if command == "db-is-standalone":
                if fault == "clustered" and Path(argv[-1]) == by_name["PVN_Control"][0]:
                    return Result(returncode=1)
                return Result()
            expected_name = next(
                name for name, (path, _sock) in by_name.items() if path == database
            ) if database is not None else argv[-1]
            if command == "db-name":
                return Result(stdout=(
                    "Wrong_Control\n"
                    if fault == "offline-name" and expected_name == "PVN_Control"
                    else expected_name + "\n"
                ))
            if command == "db-version":
                return Result(stdout="1.0.0\n")
            if command == "db-cksum":
                return Result(stdout="12345 67890\n")
            if argv and argv[0] == "/usr/bin/ovsdb-client":
                expected_name = argv[-1]
                database, sock = by_name[expected_name]
                if fault == "db-inode-swap" and expected_name == "PVN_Control":
                    database.rename(root / "database-original.db")
                    database.write_text(expected_name + "-replacement\n", encoding="utf-8")
                    database.chmod(0o640)
                if fault == "socket-inode-swap" and expected_name == "PVN_Control":
                    sock.rename(root / "socket-original.sock")
                    replacement = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                    replacement.bind(str(sock))
                    swapped_socket.append(replacement)
                schema = {
                    "name": expected_name,
                    "version": "1.0.0",
                    "cksum": "12345 67890",
                    "tables": {"Identity": {}},
                }
                if fault == "live-name" and expected_name == "PVN_Control":
                    schema["name"] = "Wrong_Control"
                elif fault == "live-version" and expected_name == "PVN_Control":
                    schema["version"] = "2.0.0"
                elif fault == "live-checksum" and expected_name == "PVN_Control":
                    schema["cksum"] = "foreign checksum"
                elif fault == "empty-tables" and expected_name == "PVN_Control":
                    schema["tables"] = {}
                return Result(stdout=json.dumps(schema))
            raise AssertionError(f"unexpected standalone DB proof command: {argv}")

        remote["run"] = fake_run
        try:
            result = remote["standalone_database_readiness"]()
        except remote["RemoteFailure"]:
            if fault is None:
                raise
            result = None
        else:
            if fault is not None:
                fail(f"embedded standalone DB proof accepted {fault}")
        finally:
            for listener in swapped_socket + sockets:
                listener.close()
        return result


def test_embedded_database_readiness() -> None:
    rows = embedded_database_case()
    if rows is None or [row["database"] for row in rows] != [
        "PVN_Control", "OVN_Northbound", "OVN_Southbound",
    ]:
        fail("embedded standalone DB proof rejected its exact healthy boundary")
    for fault in (
        "clustered", "offline-name", "live-name", "live-version",
        "live-checksum", "empty-tables", "db-symlink", "db-hardlink",
        "db-writable", "socket-regular", "db-inode-swap", "socket-inode-swap",
    ):
        embedded_database_case(fault)


def test_embedded_standalone_command_fences() -> None:
    remote = production_remote_namespace()

    class Result:
        def __init__(self, returncode: int = 0, stdout: str = ""):
            self.returncode = returncode
            self.stdout = stdout
            self.stderr = ""

    commands: list[list[str]] = []

    def fake_run(argv, **_kwargs):
        commands.append(list(argv))
        if argv != ["/usr/sbin/pvnctl", "doctor"]:
            fail(f"standalone active readiness invoked forbidden command: {argv}")
        return Result()

    database_rows = [
        {
            "database": name,
            "schema_version": "1.0.0",
            "schema_checksum": "12345 67890",
            "socket": socket_path,
        }
        for name, socket_path in (
            ("PVN_Control", "/run/pvn-control/pvn-control-db.sock"),
            ("OVN_Northbound", "/run/ovn/ovnnb_db.sock"),
            ("OVN_Southbound", "/run/ovn/ovnsb_db.sock"),
        )
    ]
    remote["run"] = fake_run
    remote["installed_package_version"] = lambda package: (
        "0.2.14-1" if package == "pvn-node" else fail("unexpected package query")
    )
    remote["central_restart_pending"] = lambda: "0.2.14-1"
    remote["standalone_database_readiness"] = lambda: database_rows
    readiness = remote["active_pvn_readiness"]("standalone")
    if commands != [["/usr/sbin/pvnctl", "doctor"]] or \
            readiness.get("standalone_databases") != database_rows or \
            "central_status" in readiness:
        fail("standalone readiness did not remain local-only")

    report = {
        "network_state": "desired",
        "interfaces_sha256": "d" * 64,
        "management": {"interface": "vmbr0"},
        "geneve": {"interface": "ens4"},
    }
    journal = {
        "desired_interfaces_sha256": "d" * 64,
        "management": report["management"],
        "geneve": report["geneve"],
    }
    remote["build_probe"] = lambda _request: report
    remote["load_journal"] = lambda optional=False: journal
    remote["ping_geneve"] = lambda _request: None
    remote["cluster_status"] = lambda *_args: fail(
        "standalone verify-network invoked pvecm status"
    )
    remote["check_activation_absent"] = lambda: None
    emitted: dict[str, object] = {}
    remote["emit"] = lambda **value: emitted.update(value)
    remote["verify_network"]({
        "pvn_mode": "standalone", "geneve": report["geneve"],
    })
    if emitted.get("verified") is not True:
        fail("standalone network verification did not complete")

    remote["read_regular"] = lambda _path, _limit=None: json.dumps({
        "nodename": "prox1", "version": 9,
    })
    with tempfile.TemporaryDirectory(prefix="pvn-no-corosync-") as raw:
        remote["COROSYNC"] = Path(raw) / "corosync.conf"
        remote["verify_membership_snapshot"](None, "standalone", "prox1")
        try:
            remote["verify_membership_snapshot"](None, "standalone", "prox2")
        except remote["RemoteFailure"]:
            pass
        else:
            fail("standalone helper accepted a changed local nodename")

    request = {
        "pvn_mode": "standalone", "transaction": "tx", "node": "prox1",
        "spec": {"cluster_name": "standalone-prox1"},
    }
    journal = {
        "transaction": "tx", "node": "prox1",
        "spec": request["spec"], "before_corosync_sha256": None,
    }
    remote["validate_journal_for_request"](journal, request)
    for corrupt in (
        {key: value for key, value in journal.items()
         if key != "before_corosync_sha256"},
        {**journal, "before_corosync_sha256": "f" * 64},
    ):
        try:
            remote["validate_journal_for_request"](corrupt, request)
        except remote["RemoteFailure"]:
            pass
        else:
            fail("embedded helper accepted a non-null/missing standalone journal pin")


LOCAL_HELPER = r'''#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import sys

STATE = Path(os.environ["PVN_STANDALONE_TEST_STATE"])
LOG = Path(os.environ["PVN_STANDALONE_TEST_LOCAL_LOG"])
ACTION = sys.argv[1]
REQUEST = json.load(sys.stdin)
HASH_KEYS = (
    "node", "interfaces_sha256", "pending_interfaces_sha256",
    "corosync_sha256", "corosync_text", "corosync_package_version",
    "corosync_runtime", "management", "geneve", "provider",
    "network_state", "journal", "journal_phase", "activation",
    "control_ledger_text", "control_ledger_sha256", "active_readiness",
)
MARKERS = ("/etc/pvn/node-enabled", "/etc/pvn/central/enabled")
UNITS = (
    "pvn-node.target", "pvn-central.target", "pvn-node-ready.service",
    "pvn-manager.service", "pvn-agent.service", "pvn-control-db.service",
    "pvn-ovn-db-listeners.service", "pvn-ovn-host-config.service",
    "ovn-controller.service", "ovn-northd.service",
    "ovn-ovsdb-server-nb.service", "ovn-ovsdb-server-sb.service",
)
SOCKETS = {
    "PVN_Control": "/run/pvn-control/pvn-control-db.sock",
    "OVN_Northbound": "/run/ovn/ovnnb_db.sock",
    "OVN_Southbound": "/run/ovn/ovnsb_db.sock",
}


def stop(message):
    print(message, file=sys.stderr)
    raise SystemExit(1)


def digest(text):
    return hashlib.sha256(text.encode()).hexdigest()


def save():
    STATE.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")


def emit(**value):
    value["ok"] = True
    print(json.dumps(value, sort_keys=True))


if ACTION not in {
    "probe", "prepare", "stage-network", "apply-network", "verify-network",
    "rollback-network", "discard-stage", "write-ledger", "verify-ledger",
    "restore-ledger", "record-phase",
}:
    stop("standalone helper received forbidden action %s" % ACTION)
if REQUEST.get("pvn_mode") != "standalone":
    stop("local request is not fenced as standalone")
if REQUEST.get("membership_snapshot") is not None:
    stop("standalone request unexpectedly carries clustered membership")
if REQUEST.get("cluster_name") != "standalone-prox1" or REQUEST.get("node") != "prox1":
    stop("standalone request identity drifted")
if REQUEST.get("management_ip") not in (None, "192.168.0.80"):
    stop("standalone management pin drifted")

state = json.loads(STATE.read_text(encoding="utf-8"))
with LOG.open("a", encoding="utf-8") as stream:
    stream.write(json.dumps({
        "action": ACTION,
        "management_ip": REQUEST.get("management_ip"),
        "corosync_package_version": REQUEST.get("corosync_package_version"),
        "pvn_mode": REQUEST.get("pvn_mode"),
    }, sort_keys=True) + "\n")


def report():
    active = state["active"]
    activation = {
        "markers": {name: active for name in MARKERS},
        "units": {name: active for name in UNITS},
    }
    readiness = None
    if active:
        databases = []
        for name, socket in SOCKETS.items():
            databases.append({
                "database": name,
                "schema_version": "1.0.0",
                "schema_checksum": "12345 67890",
                "socket": socket,
            })
        drift = state.get("db_drift")
        if drift == "socket":
            databases[0]["socket"] = "/run/wrong.sock"
        elif drift == "name":
            databases[0]["database"] = "Wrong_Control"
        readiness = {
            "package_version": state["live_version"],
            "central_restart_pending": state["marker"],
            "doctor": state["doctor"],
            "standalone_databases": databases,
        }
    interfaces = state["desired_interfaces_sha256"] \
        if state["network"] == "desired" else state["initial_interfaces_sha256"]
    management = {
        "interface": "vmbr0",
        "address": "192.168.0.80/24",
        "default_routes": [{
            "dst": "default", "gateway": "192.168.0.1", "dev": "vmbr0",
        }],
    }
    if state.get("management_drift") == "missing-address":
        management.pop("address")
    elif state.get("management_drift") == "integer-address":
        management["address"] = 1
    elif state.get("management_drift") == "wrong-address":
        management["address"] = "192.168.0.99/24"
    return {
        "node": "prox1",
        "interfaces_sha256": interfaces,
        "pending_interfaces_sha256": (
            state["desired_interfaces_sha256"] if state["staged"] else None
        ),
        "corosync_sha256": None,
        "corosync_text": None,
        "corosync_package_version": None,
        "corosync_runtime": None,
        "management": management,
        "geneve": {
            "interface": "ens4", "address": "192.168.100.10/24",
            "mac": "02:00:00:00:01:04", "mtu": 1500, "max_mtu": 9000,
            "configured_mtu": 1500,
        },
        "provider": {
            "interface": "ens5", "address": "192.168.200.10/24",
            "mac": "02:00:00:00:01:05", "mtu": 1500, "max_mtu": 9000,
            "configured_mtu": 1500,
        },
        "network_state": state["network"],
        "journal": state["journal"],
        "journal_phase": state["journal"].get("phase") if state["journal"] else None,
        "ledger_text": state["ledger_text"],
        "ledger_sha256": state["ledger_sha256"],
        "control_ledger_text": state["control_ledger_text"],
        "control_ledger_sha256": state["control_ledger_sha256"],
        "active_readiness": readiness,
        "activation": activation,
        "activation_safe": not active,
    }


def proof(value):
    selected = {key: value.get(key) for key in HASH_KEYS}
    return digest(json.dumps(selected, sort_keys=True, separators=(",", ":")))


if ACTION == "probe":
    emit(report=report())
elif ACTION == "prepare":
    if state["active"]:
        stop("prepare attempted while PVN is active")
    current = report()
    expected = REQUEST.get("expected", {})
    if expected.get("interfaces_sha256") != current["interfaces_sha256"] or \
            expected.get("corosync_sha256") is not None:
        stop("prepare did not pin the standalone boundary")
    if state["journal"] is None:
        state["journal"] = {
            "schema": 1,
            "transaction": REQUEST["transaction"],
            "spec": REQUEST["spec"],
            "node": "prox1",
            "management": current["management"],
            "geneve": current["geneve"],
            "provider": current["provider"],
            "effective_underlay_mtu": REQUEST["effective_underlay_mtu"],
            "before_interfaces_sha256": current["interfaces_sha256"],
            "before_corosync_sha256": None,
            "desired_interfaces_sha256": None,
            "phase": "prepared",
        }
        save()
    emit(journal=state["journal"], report=report())
elif ACTION == "stage-network":
    if state["active"]:
        stop("network staging attempted while PVN is active")
    state["staged"] = True
    state["journal"]["desired_interfaces_sha256"] = state["desired_interfaces_sha256"]
    state["journal"]["phase"] = "network-staged"
    save()
    emit(noop=False, desired_interfaces_sha256=state["desired_interfaces_sha256"])
elif ACTION == "apply-network":
    if state["active"]:
        stop("network apply attempted while PVN is active")
    state["network"] = "desired"
    state["staged"] = False
    state["journal"]["phase"] = "network-applied-unverified"
    save()
    emit(noop=False, interfaces_sha256=state["desired_interfaces_sha256"])
elif ACTION == "verify-network":
    if state["network"] != "desired" or REQUEST.get("geneve_peers") != []:
        stop("standalone network verification boundary is invalid")
    emit(verified=True, report=report())
elif ACTION == "record-phase":
    if state["active"]:
        stop("journal mutation attempted while PVN is active")
    if REQUEST.get("phase") != "complete":
        stop("standalone path attempted a Corosync journal phase")
    state["journal"]["phase"] = "complete"
    save()
    emit(phase="complete")
elif ACTION == "write-ledger":
    if state["active"]:
        if REQUEST.get("ledger_upgrade_only") is not True:
            stop("active standalone ledger write lacks upgrade-only fence")
        if REQUEST.get("upgrade_proof_sha256") != proof(report()):
            stop("active standalone ledger proof changed before CAS")
    elif REQUEST.get("ledger_upgrade_only") is not None:
        stop("inactive standalone write has an active-only fence")
    if state["ledger_sha256"] != REQUEST.get("expected_ledger_sha256"):
        stop("standalone ledger CAS boundary changed")
    if digest(REQUEST["ledger"]) != REQUEST.get("ledger_sha256"):
        stop("standalone ledger content hash is invalid")
    state["ledger_text"] = REQUEST["ledger"]
    state["ledger_sha256"] = REQUEST["ledger_sha256"]
    save()
    emit(noop=False, ledger_sha256=state["ledger_sha256"])
elif ACTION == "verify-ledger":
    if state["ledger_sha256"] != REQUEST.get("ledger_sha256"):
        stop("standalone ledger verification failed")
    emit(ledger_sha256=state["ledger_sha256"])
elif ACTION == "rollback-network":
    state["network"] = "initial"
    state["staged"] = False
    save()
    emit(noop=False, report=report())
elif ACTION == "discard-stage":
    state["staged"] = False
    save()
    emit(noop=False)
elif ACTION == "restore-ledger":
    state["ledger_text"] = REQUEST.get("original_ledger")
    state["ledger_sha256"] = REQUEST.get("original_ledger_sha256")
    save()
    emit(noop=False, ledger_sha256=state["ledger_sha256"])
'''


LEASE_HELPER = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

log = Path(os.environ["PVN_STANDALONE_TEST_LEASE_LOG"])
with log.open("a", encoding="utf-8") as stream:
    stream.write(json.dumps(sys.argv[1:]) + "\n")
if sys.argv[1] == "acquire":
    json.load(sys.stdin)
raise SystemExit(0)
'''


SSH_TRAP = r'''#!/bin/sh
echo invoked >> "$PVN_STANDALONE_TEST_SSH_LOG"
exit 97
'''


def initial_state() -> dict[str, object]:
    return {
        "active": False,
        "network": "initial",
        "staged": False,
        "journal": None,
        "ledger_text": None,
        "ledger_sha256": None,
        "control_ledger_text": None,
        "control_ledger_sha256": None,
        "initial_interfaces_sha256": sha("standalone-interfaces-initial"),
        "desired_interfaces_sha256": sha("standalone-interfaces-desired"),
        "live_version": "0.2.14-1",
        "marker": None,
        "doctor": True,
        "db_drift": None,
        "management_drift": None,
    }


def read_actions(path: Path) -> list[str]:
    if not path.exists():
        return []
    return [json.loads(line)["action"] for line in path.read_text().splitlines()]


def run_topology(env: dict[str, str], *arguments: str, ok: bool = True) -> subprocess.CompletedProcess[str]:
    command = [sys.executable, str(TOPOLOGY), *arguments]
    result = subprocess.run(
        command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        env=env, timeout=30, check=False,
    )
    if ok and result.returncode != 0:
        fail(f"{' '.join(command)} failed:\n{result.stdout}\n{result.stderr}")
    if not ok and result.returncode == 0:
        fail(f"{' '.join(command)} unexpectedly succeeded")
    return result


def arguments(operation: str = "plan") -> list[str]:
    values = [operation, "--geneve-cidr", GENEVE, "--provider-cidr", PROVIDER]
    if operation == "apply":
        values.extend([
            "--provider-port-ready", ACK, "--confirm", CONFIRM,
        ])
    return values


def seed_active_schema1(state_path: Path) -> tuple[str, str]:
    state = json.loads(state_path.read_text())
    topology = json.loads(state["ledger_text"])
    topology["schema"] = 1
    topology.pop("corosync", None)
    legacy_text = json.dumps(topology, sort_keys=True, indent=2) + "\n"
    legacy_sha = sha(legacy_text)
    node = topology["nodes"][0]
    pin = "a" * 64
    snapshot_node = {
        "name": node["name"],
        "node_id": 0,
        "control_ip": node["control_ip"],
        "geneve_ip": node["geneve_ip"],
        "geneve_interface": node["geneve_interface"],
        "provider_interface": node["provider_interface"],
        "provider_bridge": topology["provider_bridge"],
        "package_version": "0.2.13-1",
        "geneve_mtu": 1500,
    }
    control = {
        "version": 1,
        "cluster_uuid": "11111111-1111-4111-8111-111111111111",
        "snapshot": {
            "mode": "standalone",
            "confirmation": CONFIRM,
            "cluster_name": CONFIRM,
            "members_version": 1,
            "cluster_version": 0,
            "topology_sha256": legacy_sha,
            "guest_mtu": topology["guest_mtu"],
            "physnet": topology["physnet"],
            "nodes": [snapshot_node],
        },
        "phase": "complete",
        "central_complete": 1,
        "nodes_complete": 1,
        "cert_fingerprints": {
            "ca_certificate_sha256": pin,
            "nodes": {
                "prox1": {
                    "certificate_sha256": "b" * 64,
                    "public_key_sha256": "c" * 64,
                },
            },
        },
        "control_db_cluster_id": None,
        "db_cluster_ids": {},
    }
    control_text = json.dumps(control, sort_keys=True, indent=2) + "\n"
    state.update({
        "active": True,
        "ledger_text": legacy_text,
        "ledger_sha256": legacy_sha,
        "control_ledger_text": control_text,
        "control_ledger_sha256": sha(control_text),
        "live_version": "0.2.14-1",
        "marker": "0.2.14-1",
        "doctor": True,
        "db_drift": None,
    })
    state_path.write_text(json.dumps(state, sort_keys=True))
    return legacy_sha, control_text


def repin_control_to_candidate(state_path: Path) -> None:
    state = json.loads(state_path.read_text())
    control = json.loads(state["control_ledger_text"])
    control["snapshot"]["topology_sha256"] = state["ledger_sha256"]
    control["snapshot"]["nodes"][0]["package_version"] = state["live_version"]
    text = json.dumps(control, sort_keys=True, indent=2) + "\n"
    state["control_ledger_text"] = text
    state["control_ledger_sha256"] = sha(text)
    state["marker"] = None
    state_path.write_text(json.dumps(state, sort_keys=True))


def main() -> None:
    test_embedded_database_readiness()
    test_embedded_standalone_command_fences()
    with tempfile.TemporaryDirectory(prefix="pvn-topology-standalone-") as raw:
        work = Path(raw)
        members = work / "members.json"
        corosync = work / "corosync.conf"
        local_helper = work / "local-helper.py"
        lease_helper = work / "lease-helper.py"
        ssh_trap = work / "ssh-trap"
        state_path = work / "state.json"
        local_log = work / "local.log"
        lease_log = work / "lease.log"
        ssh_log = work / "ssh.log"
        members.write_text('{"nodename":"prox1","version":1}\n')
        state_path.write_text(json.dumps(initial_state(), sort_keys=True))
        local_helper.write_text(LOCAL_HELPER)
        lease_helper.write_text(LEASE_HELPER)
        ssh_trap.write_text(SSH_TRAP)
        for executable in (local_helper, lease_helper, ssh_trap):
            executable.chmod(0o755)

        env = dict(os.environ)
        env.update({
            "PVN_TOPOLOGY_MEMBERS_FILE": str(members),
            "PVN_TOPOLOGY_COROSYNC_FILE": str(corosync),
            "PVN_TOPOLOGY_LOCAL_HELPER_BIN": str(local_helper),
            "PVN_TOPOLOGY_LEASE_BIN": str(lease_helper),
            "PVN_TOPOLOGY_SSH_BIN": str(ssh_trap),
            "PVN_TOPOLOGY_IDENTITY_FILE": str(work / "must-not-read-id"),
            "PVN_TOPOLOGY_NODES_DIR": str(work / "must-not-read-nodes"),
            "PVN_TOPOLOGY_LOCK_FILE": str(work / "topology.lock"),
            "PVN_TOPOLOGY_TEST_FAST": "1",
            "PVN_STANDALONE_TEST_STATE": str(state_path),
            "PVN_STANDALONE_TEST_LOCAL_LOG": str(local_log),
            "PVN_STANDALONE_TEST_LEASE_LOG": str(lease_log),
            "PVN_STANDALONE_TEST_SSH_LOG": str(ssh_log),
        })

        plan = run_topology(env, *arguments())
        if "management=192.168.0.80" not in plan.stdout or \
                "Corosync: standalone (not configured)" not in plan.stdout:
            fail("standalone plan did not expose the pinned local management boundary")
        if read_actions(local_log) != ["probe"]:
            fail("standalone read-only plan did more than one local probe")

        run_topology(env, *arguments("apply"))
        state = json.loads(state_path.read_text())
        ledger = json.loads(state["ledger_text"])
        if ledger.get("schema") != 2 or "corosync" not in ledger or \
                ledger["corosync"] is not None:
            fail("standalone apply did not publish explicit schema-2 corosync:null")
        if ledger["nodes"] != [{
            "name": "prox1", "node_id": 0, "management_ip": "192.168.0.80",
            "control_ip": "192.168.0.80", "geneve_ip": "192.168.100.10",
            "geneve_interface": "ens4", "provider_interface": "ens5",
        }]:
            fail("standalone topology ledger node identity is not canonical")
        forbidden = {
            "validate-corosync", "apply-corosync", "reload-corosync",
            "restart-corosync", "verify-cluster",
        }
        if forbidden.intersection(read_actions(local_log)):
            fail("standalone apply invoked a Corosync action")
        if ssh_log.exists():
            fail("standalone topology invoked SSH")

        # Inactive rerun remains resumable and preserves the exact candidate.
        previous_ledger = state["ledger_text"]
        run_topology(env, *arguments("apply"))
        if json.loads(state_path.read_text())["ledger_text"] != previous_ledger:
            fail("standalone inactive rerun changed the canonical ledger")

        seed_active_schema1(state_path)
        active_plan = run_topology(env, *arguments())
        if "Upgrade readiness: READY" not in active_plan.stdout or \
                "exact standalone databases" not in active_plan.stdout:
            fail("active standalone schema-1 upgrade was not ready")
        before = len(read_actions(local_log))
        run_topology(env, *arguments("apply"))
        active_actions = read_actions(local_log)[before:]
        if active_actions.count("write-ledger") != 1 or \
                set(active_actions) - {"probe", "write-ledger"}:
            fail("active standalone upgrade mutated more than its one ledger CAS")
        state = json.loads(state_path.read_text())
        upgraded = json.loads(state["ledger_text"])
        if upgraded.get("schema") != 2 or upgraded.get("corosync", "missing") is not None:
            fail("active standalone upgrade did not publish corosync:null")

        # Exact active rerun is a probe-only no-op.
        before = len(read_actions(local_log))
        run_topology(env, *arguments("apply"))
        if set(read_actions(local_log)[before:]) != {"probe"}:
            fail("active standalone schema-2 rerun was not a probe-only no-op")

        repin_control_to_candidate(state_path)
        repinned = run_topology(env, *arguments())
        if "Upgrade readiness: READY" not in repinned.stdout:
            fail("equal-package consumed-marker standalone state was not ready")

        healthy_state_text = state_path.read_text()

        # Schema 2 must carry an explicit null pin, never an omitted key.
        state = json.loads(healthy_state_text)
        malformed_ledger = json.loads(state["ledger_text"])
        malformed_ledger.pop("corosync")
        state["ledger_text"] = json.dumps(
            malformed_ledger, sort_keys=True, indent=2,
        ) + "\n"
        state["ledger_sha256"] = sha(state["ledger_text"])
        state_path.write_text(json.dumps(state, sort_keys=True))
        before_count = len(read_actions(local_log))
        run_topology(env, *arguments("apply"), ok=False)
        if "write-ledger" in read_actions(local_log)[before_count:]:
            fail("schema-2 ledger without corosync:null reached a ledger write")
        state_path.write_text(healthy_state_text)

        # Completed standalone journals must explicitly pin no Corosync state.
        for journal_fault in ("missing", "non-null"):
            state = json.loads(healthy_state_text)
            if journal_fault == "missing":
                state["journal"].pop("before_corosync_sha256")
            else:
                state["journal"]["before_corosync_sha256"] = "e" * 64
            state_path.write_text(json.dumps(state, sort_keys=True))
            before_count = len(read_actions(local_log))
            run_topology(env, *arguments("apply"), ok=False)
            if "write-ledger" in read_actions(local_log)[before_count:]:
                fail(f"standalone journal {journal_fault} null pin allowed a ledger write")
        state_path.write_text(healthy_state_text)

        # Standalone control ledgers must never carry clustered database IDs.
        state = json.loads(healthy_state_text)
        control = json.loads(state["control_ledger_text"])
        control["control_db_cluster_id"] = "22222222-2222-4222-8222-222222222222"
        control["db_cluster_ids"] = {
            "PVN_Control": control["control_db_cluster_id"],
        }
        control_text = json.dumps(control, sort_keys=True, indent=2) + "\n"
        state["control_ledger_text"] = control_text
        state["control_ledger_sha256"] = sha(control_text)
        state_path.write_text(json.dumps(state, sort_keys=True))
        before_count = len(read_actions(local_log))
        run_topology(env, *arguments("apply"), ok=False)
        if "write-ledger" in read_actions(local_log)[before_count:]:
            fail("standalone control ledger with Raft IDs reached a ledger write")
        state_path.write_text(healthy_state_text)

        # A schema-1 rollout needs the exact newer-package restart marker.
        seed_active_schema1(state_path)
        state = json.loads(state_path.read_text())
        state["marker"] = None
        state_path.write_text(json.dumps(state, sort_keys=True))
        missing_marker = run_topology(env, *arguments())
        if "Upgrade readiness: NOT READY" not in missing_marker.stdout or \
                "restart marker" not in missing_marker.stdout:
            fail("schema-1 standalone missing marker was not reported")
        before_count = len(read_actions(local_log))
        run_topology(env, *arguments("apply"), ok=False)
        if "write-ledger" in read_actions(local_log)[before_count:]:
            fail("schema-1 standalone missing marker allowed a ledger write")
        state_path.write_text(healthy_state_text)

        # Fresh doctor drift must make apply fail without a ledger write.
        state = json.loads(state_path.read_text())
        state["doctor"] = False
        state_path.write_text(json.dumps(state, sort_keys=True))
        drift_plan = run_topology(env, *arguments())
        if "Upgrade readiness: NOT READY" not in drift_plan.stdout or \
                "fresh pvnctl doctor failed" not in drift_plan.stdout:
            fail("standalone doctor drift was not reported")
        before_actions = read_actions(local_log)
        run_topology(env, *arguments("apply"), ok=False)
        if "write-ledger" in read_actions(local_log)[len(before_actions):]:
            fail("doctor drift allowed an active ledger write")

        # A malformed local DB identity is rejected before any mutation.
        state = json.loads(state_path.read_text())
        state["doctor"] = True
        state["db_drift"] = "socket"
        state_path.write_text(json.dumps(state, sort_keys=True))
        before_count = len(read_actions(local_log))
        run_topology(env, *arguments("apply"), ok=False)
        if "write-ledger" in read_actions(local_log)[before_count:]:
            fail("standalone DB identity drift allowed a ledger write")

        state = json.loads(state_path.read_text())
        state["db_drift"] = None
        for management_fault in ("missing-address", "integer-address"):
            state["management_drift"] = management_fault
            state_path.write_text(json.dumps(state, sort_keys=True))
            malformed_management = run_topology(env, *arguments(), ok=False)
            if "invalid management/default-route report" not in malformed_management.stderr:
                fail(f"standalone management {management_fault} did not fail closed")

        # Strict standalone membership and absent Corosync are outer local gates.
        state["management_drift"] = None
        state["db_drift"] = None
        state_path.write_text(json.dumps(state, sort_keys=True))
        good_members = {"nodename": "prox1", "version": 1}
        for malformed in (
            {**good_members, "extra": True},
            {**good_members, "cluster": {}},
            {"nodename": "prox1", "version": -1},
        ):
            members.write_text(json.dumps(malformed) + "\n")
            before_count = len(read_actions(local_log))
            run_topology(env, *arguments(), ok=False)
            if len(read_actions(local_log)) != before_count:
                fail("malformed standalone membership reached the local helper")
        members.write_text(json.dumps(good_members) + "\n")
        corosync.write_text("forbidden\n")
        before_count = len(read_actions(local_log))
        run_topology(env, *arguments(), ok=False)
        if len(read_actions(local_log)) != before_count:
            fail("standalone corosync.conf drift reached the local helper")

        if ssh_log.exists():
            fail("standalone tests invoked SSH or consulted its identity files")
        print("pvn-topology standalone tests passed")


if __name__ == "__main__":
    main()
