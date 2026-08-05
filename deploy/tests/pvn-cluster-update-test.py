#!/usr/bin/python3
"""Focused orchestration tests for the rolling cluster updater."""

from __future__ import annotations

import contextlib
import importlib.machinery
import importlib.util
import io
import os
import pathlib
import socket
import subprocess
import sys
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = REPO / "deploy/scripts/pvn-cluster-update"
sys.dont_write_bytecode = True
loader = importlib.machinery.SourceFileLoader("pvn_cluster_update_tested", str(SCRIPT))
spec = importlib.util.spec_from_loader(loader.name, loader)
assert spec is not None
module = importlib.util.module_from_spec(spec)
sys.modules[loader.name] = module
loader.exec_module(module)

remote_syntax = subprocess.run(
    ["sh", "-n"], input=module.REMOTE_SCRIPT, text=True, capture_output=True, check=False
)
if remote_syntax.returncode != 0:
    raise AssertionError(f"embedded remote updater is not valid shell: {remote_syntax.stderr}")


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def probe_line(
    snapshot,
    node,
    version,
    *,
    node_state="active",
    central="inactive",
    central_mode=None,
    central_pending="none",
) -> str:
    pids = "101,102,103,104" if central == "active" else "none"
    if central_mode is None:
        central_mode = "raft" if central == "active" else "inactive"
    return (
        "PVN_UPDATE "
        f"mode={snapshot.mode} cluster={snapshot.deployment} "
        f"config={snapshot.config_version} fingerprint={snapshot.fingerprint} "
        f"nodes={len(snapshot.nodes)} pve=9.2.2 arch=amd64 version={version} "
        f"hostname={node.name} nodeid={node.node_id} node={node_state} "
        f"central={central} centralmode={central_mode} centralpids={pids} "
        f"centralpending={central_pending}\n"
    )


nodes = (
    module.Node(1, "pve-a", "192.0.2.10", True),
    module.Node(2, "pve-b", "192.0.2.11", False),
)
fingerprint = module.membership_fingerprint("cluster", "lab-cluster", 7, nodes)
snapshot = module.Snapshot("cluster", "lab-cluster", 7, "pve-a", nodes, fingerprint)


parsed = module.parse_probe(nodes[0], probe_line(snapshot, nodes[0], "0.1.0"), snapshot)
check(parsed.version == "0.1.0" and parsed.node_state == "active", "valid probe was not parsed")
standalone_node = module.Node(0, "pve-one", "local", True)
standalone_fingerprint = module.membership_fingerprint(
    "standalone", "standalone-pve-one", 0, (standalone_node,)
)
standalone_snapshot = module.Snapshot(
    "standalone", "standalone-pve-one", 0, "pve-one", (standalone_node,), standalone_fingerprint
)
standalone = module.parse_probe(
    standalone_node,
    probe_line(
        standalone_snapshot,
        standalone_node,
        "0.1.0",
        central="active",
        central_mode="standalone",
    ),
    standalone_snapshot,
)
check(standalone.central_mode == "standalone", "standalone active central was not parsed")
try:
    module.parse_probe(
        nodes[0],
        probe_line(snapshot, nodes[0], "0.1.0", central="active", central_mode="standalone"),
        snapshot,
    )
except module.UpdateError:
    pass
else:
    raise AssertionError("clustered PVE deployment accepted standalone central databases")
inactive = module.parse_probe(
    nodes[1], probe_line(snapshot, nodes[1], "0.1.0", node_state="inactive"), snapshot
)
check(inactive.central_mode == "inactive", "inactive central was not preserved")
try:
    module.parse_probe(
        nodes[1],
        probe_line(
            snapshot,
            nodes[1],
            "0.1.0",
            node_state="inactive",
            central="inactive",
            central_mode="standalone",
        ),
        snapshot,
    )
except module.UpdateError:
    pass
else:
    raise AssertionError("inactive central accepted a live database mode")


def exercise_remote_central_health(
    configured_mode: str,
    *,
    database_modes: tuple[str, str, str] | None = None,
    schema_drift: bool = False,
) -> subprocess.CompletedProcess[str]:
    if database_modes is None:
        database_modes = (configured_mode, configured_mode, configured_mode)
    with tempfile.TemporaryDirectory() as temporary:
        root = pathlib.Path(temporary)
        control_environment = root / "control-db.env"
        ovn_environment = root / "ovn-central.env"
        control_environment.write_text(f"PVN_CONTROL_MODE={configured_mode}\n", encoding="ascii")
        ovn_bootstrap = "standalone" if configured_mode == "standalone" else "seed"
        ovn_environment.write_text(
            f"PVN_OVN_BOOTSTRAP={ovn_bootstrap}\nOVN_CTL_OPTS=--one --two\n",
            encoding="ascii",
        )
        control_environment.chmod(0o640)
        ovn_environment.chmod(0o640)

        records = (
            (root / "control.db", "PVN_Control", database_modes[0]),
            (root / "northbound.db", "OVN_Northbound", database_modes[1]),
            (root / "southbound.db", "OVN_Southbound", database_modes[2]),
        )
        for path, name, mode in records:
            path.write_text(f"{name}|{mode}\n", encoding="ascii")

        calls = root / "calls"
        tool = root / "ovsdb-tool"
        tool.write_text(
            """#!/bin/sh
set -eu
IFS='|' read -r name mode < "$2"
case "$1" in
  db-name) printf '%s\\n' "$name" ;;
  db-is-standalone) [ "$mode" = standalone ] ;;
  db-is-clustered) [ "$mode" = raft ] ;;
  *) exit 2 ;;
esac
""",
            encoding="ascii",
        )
        tool.chmod(0o755)

        client = root / "ovsdb-client"
        drift_name = "Wrong_Southbound" if schema_drift else "OVN_Southbound"
        client.write_text(
            f"""#!/bin/sh
set -eu
[ "$1" = --timeout=10 ] && [ "$2" = get-schema ]
case "$3" in
  unix:*control.sock) name=PVN_Control ;;
  unix:*northbound.sock) name=OVN_Northbound ;;
  unix:*southbound.sock) name={drift_name} ;;
  *) exit 2 ;;
esac
printf 'client:%s\\n' "$4" >> "{calls}"
printf '{{"name":"%s","tables":{{}}}}\\n' "$name"
""",
            encoding="ascii",
        )
        client.chmod(0o755)

        pvnctl = root / "pvnctl"
        pvnctl.write_text(
            f"#!/bin/sh\nprintf 'raft\\n' >> '{calls}'\n[ \"$1 $2\" = 'central status' ]\n",
            encoding="ascii",
        )
        pvnctl.chmod(0o755)

        socket_paths = (root / "control.sock", root / "northbound.sock", root / "southbound.sock")
        sockets = []
        try:
            for path in socket_paths:
                listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                listener.bind(str(path))
                sockets.append(listener)

            helpers = module.REMOTE_SCRIPT[
                module.REMOTE_SCRIPT.index("remote_fail()") : module.REMOTE_SCRIPT.index("\ncollect_state()")
            ]
            replacements = {
                "/etc/pvn/central/control-db.env": str(control_environment),
                "/etc/pvn/central/ovn-central.env": str(ovn_environment),
                "/var/lib/pvn/control-db/pvn_control.db": str(records[0][0]),
                "/var/lib/ovn/ovnnb_db.db": str(records[1][0]),
                "/var/lib/ovn/ovnsb_db.db": str(records[2][0]),
                "/run/pvn-control/pvn-control-db.sock": str(socket_paths[0]),
                "/run/ovn/ovnnb_db.sock": str(socket_paths[1]),
                "/run/ovn/ovnsb_db.sock": str(socket_paths[2]),
                "/usr/bin/ovsdb-tool": str(tool),
                "/usr/bin/ovsdb-client": str(client),
                "/usr/sbin/pvnctl": str(pvnctl),
                'pvn_gid = grp.getgrnam("pvn").gr_gid': f"pvn_gid = {os.getgid()}",
                "metadata.st_uid != 0": f"metadata.st_uid != {os.getuid()}",
            }
            for old, new in replacements.items():
                helpers = helpers.replace(old, new)
            script = "set -eu\n" + helpers + "\nactive_central_health\nprintf '%s\\n' \"$pvn_central_mode\"\n"
            return subprocess.run(["sh", "-s"], input=script, text=True, capture_output=True, check=False)
        finally:
            for listener in sockets:
                listener.close()


standalone_health = exercise_remote_central_health("standalone")
check(standalone_health.returncode == 0, f"standalone health failed: {standalone_health.stderr}")
check(standalone_health.stdout.strip() == "standalone", "standalone mode was not selected")
raft_health = exercise_remote_central_health("raft")
check(raft_health.returncode == 0, f"Raft health failed: {raft_health.stderr}")
check(raft_health.stdout.strip() == "raft", "Raft mode was not selected")
mode_drift = exercise_remote_central_health(
    "standalone", database_modes=("standalone", "standalone", "raft")
)
check(mode_drift.returncode != 0, "standalone configuration accepted a clustered database")
identity_drift = exercise_remote_central_health("standalone", schema_drift=True)
check(identity_drift.returncode != 0, "standalone socket accepted the wrong database schema")
try:
    module.parse_probe(
        nodes[0],
        probe_line(snapshot, nodes[0], "0.1.0").replace(fingerprint, "0" * 64),
        snapshot,
    )
except module.UpdateError:
    pass
else:
    raise AssertionError("membership fingerprint mismatch was accepted")


events: list[str] = []
versions = {"pve-a": "0.1.0", "pve-b": "0.1.0"}
node_states = {"pve-a": "active", "pve-b": "inactive"}
central_states = {"pve-a": "active", "pve-b": "inactive"}
central_modes = {"pve-a": "raft", "pve-b": "inactive"}
central_pending = {"pve-a": "none", "pve-b": "none"}
fail_apply = ""


class FakeLease:
    def __init__(self, ignored_snapshot):
        self.snapshot = ignored_snapshot

    def acquire(self):
        events.append("lease-acquire")

    def release(self):
        events.append("lease-release")


class FakeTransport:
    def __init__(self, used_snapshot, deb):
        self.snapshot = used_snapshot
        self.deb = deb

    def run(self, node, action, *arguments, check=True):
        events.append(f"{action}:{node.name}")
        if action == "probe":
            return subprocess.CompletedProcess(
                [],
                0,
                probe_line(
                    self.snapshot,
                    node,
                    versions[node.name],
                    node_state=node_states[node.name],
                    central=central_states[node.name],
                    central_mode=central_modes[node.name],
                    central_pending=central_pending[node.name],
                ),
                "",
            )
        if action == "prepare":
            token = node.name.replace("-", "")
            return subprocess.CompletedProcess([], 0, f"/var/tmp/pvn-node-update.{token}.deb\n", "")
        if action in {"verify", "cleanup"}:
            return subprocess.CompletedProcess([], 0, "", "")
        if action == "apply":
            if node.name == fail_apply:
                raise module.UpdateError(f"injected update failure on {node.name}")
            if arguments[4] != versions[node.name]:
                raise AssertionError("apply did not pin the current version")
            if arguments[8] != str(snapshot.config_version):
                raise AssertionError("apply did not pin cluster config version")
            if arguments[9] != snapshot.fingerprint:
                raise AssertionError("apply did not pin membership fingerprint")
            versions[node.name] = arguments[2]
            if central_states[node.name] == "active":
                central_pending[node.name] = arguments[2]
            restart = "pending" if central_states[node.name] == "active" else "none"
            return subprocess.CompletedProcess(
                [], 0, f"PVN_UPDATED version={arguments[2]} central-restart={restart}\n", ""
            )
        raise AssertionError(f"unexpected fake action {action}")

    def copy(self, node, destination):
        events.append(f"copy:{node.name}")


original_transport = module.Transport
original_lease = module.UpdateLease
original_revalidate = module.revalidate
original_compare = module.compare_versions
module.Transport = FakeTransport
module.UpdateLease = FakeLease
module.revalidate = lambda ignored: events.append("membership-revalidate")
module.compare_versions = lambda left, operator, right: operator == "lt" and left == "0.1.0" and right == "0.2.1"

try:
    with tempfile.TemporaryDirectory() as temporary:
        deb = pathlib.Path(temporary) / "pvn-node.deb"
        deb.write_bytes(b"test")
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            module.update_cluster(snapshot, deb, "0.2.1", "amd64", "a" * 64)
    check(versions == {"pve-a": "0.2.1", "pve-b": "0.2.1"}, "not every node was updated")
    check(central_pending == {"pve-a": "0.2.1", "pve-b": "none"}, "mixed central markers were not preserved")
    check("RESTART REQUIRED" in output.getvalue() and "pve-a" in output.getvalue(), "active central restart was not reported")
    check("Persistent /etc/pvn configuration" in output.getvalue(), "configuration preservation was not reported")
    check("restart-central" not in " ".join(events), "updater unexpectedly restarted a central target")
    check(events[0] == "lease-acquire" and events[-1] == "lease-release", "mutation lease did not bracket rollout")
    first_apply = min(index for index, event in enumerate(events) if event.startswith("apply:"))
    for expected in ("verify:pve-a", "verify:pve-b"):
        check(events.index(expected) < first_apply, "every pending DEB was not verified before the first apply")
    check(events.index("apply:pve-a") < events.index("apply:pve-b"), "nodes were not updated sequentially")
    for expected in ("cleanup:pve-a", "cleanup:pve-b"):
        check(events.count(expected) == 1, "successful update left a staged DEB behind")
    check(events.count("membership-revalidate") >= 4, "membership was not repeatedly revalidated")

    plan_output = io.StringIO()
    with contextlib.redirect_stdout(plan_output):
        module.plan(snapshot, deb, "0.2.1", "amd64", "a" * 64)
    restart_lines = [
        line for line in plan_output.getvalue().splitlines()
        if "restart-required marker" in line
    ]
    check(len(restart_lines) == 1 and "pve-a" in restart_lines[0], "plan omitted active central restart work")
    check("pve-b" not in restart_lines[0], "plan treated an inactive central target as restartable")

    events.clear()
    versions.update({"pve-a": "0.1.0", "pve-b": "0.1.0"})
    central_pending.update({"pve-a": "none", "pve-b": "none"})
    fail_apply = "pve-a"
    try:
        with tempfile.TemporaryDirectory() as temporary:
            deb = pathlib.Path(temporary) / "pvn-node.deb"
            deb.write_bytes(b"test")
            with contextlib.redirect_stdout(io.StringIO()):
                module.update_cluster(snapshot, deb, "0.2.1", "amd64", "a" * 64)
    except module.UpdateError:
        pass
    else:
        raise AssertionError("injected node failure did not stop the rollout")
    check("apply:pve-a" in events and "apply:pve-b" not in events, "rollout did not fail-stop")
    check("cleanup:pve-a" in events and "cleanup:pve-b" in events, "staged files were not cleaned after failure")
    check(events[-1] == "lease-release", "mutation lease was not released after failure")
finally:
    module.Transport = original_transport
    module.UpdateLease = original_lease
    module.revalidate = original_revalidate
    module.compare_versions = original_compare


source = SCRIPT.read_text(encoding="utf-8")
for required in (
    "apt-get install -y --only-upgrade --no-remove",
    "/usr/sbin/pvnctl central status",
    "/usr/bin/ovsdb-tool db-is-standalone",
    "/usr/bin/ovsdb-tool db-is-clustered",
    "/usr/bin/ovsdb-client --timeout=10 get-schema",
    "PVN_CONTROL_MODE",
    "PVN_OVN_BOOTSTRAP",
    "central service restarted unexpectedly; stop the rollout",
    "persistent /etc/pvn or shared PVN configuration changed during update",
    "central-restart-pending",
    "RESTART REQUIRED: active central processes were preserved",
    '"domain": "mutation"',
):
    check(required in source, f"missing fail-closed updater behavior: {required}")

print("pvn-cluster-update tests passed")
