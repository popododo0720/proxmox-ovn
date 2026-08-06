#!/usr/bin/python3
"""Focused orchestration tests for the rolling cluster updater."""

from __future__ import annotations

import contextlib
import importlib.machinery
import importlib.util
import io
import os
import pathlib
import shlex
import socket
import subprocess
import sys
import tempfile
import time


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
embedded_python = module.REMOTE_SCRIPT.split(
    "python3 - <<'PVN_EMBEDDED_COROSYNC'\n", 1
)[1].split("\nPVN_EMBEDDED_COROSYNC", 1)[0]
compile(embedded_python, "pvn-cluster-update:embedded-corosync", "exec")


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
    northd_role=None,
) -> str:
    pids = "101,102,103,104" if central == "active" else "none"
    if central_mode is None:
        central_mode = "raft" if central == "active" else "inactive"
    if northd_role is None:
        northd_role = "active" if central == "active" else "inactive"
    return (
        "PVN_UPDATE "
        f"mode={snapshot.mode} cluster={snapshot.deployment} "
        f"config={snapshot.config_version} fingerprint={snapshot.fingerprint} "
        f"nodes={len(snapshot.nodes)} pve=9.2.2 arch=amd64 version={version} "
        f"hostname={node.name} nodeid={node.node_id} node={node_state} "
        f"central={central} centralmode={central_mode} centralpids={pids} "
        f"centralpending={central_pending} northdrole={northd_role}\n"
    )


def embedded_corosync_line(
    snapshot,
    node,
    *,
    corosync_package="3.1.10-pve2",
    config_sha="c" * 64,
    config_version=None,
) -> str:
    if snapshot.mode == "standalone":
        corosync = None
    else:
        if config_version is None:
            config_version = snapshot.config_version
        corosync = {
            "sha256": config_sha,
            "config_version": config_version,
            "nodes": [
                {
                    "name": member.name,
                    "node_id": member.node_id,
                    "rings": {"0": member.address},
                }
                for member in sorted(snapshot.nodes, key=lambda value: value.name)
            ],
        }
    payload = {
        "schema": 1,
        "mode": snapshot.mode,
        "deployment": snapshot.deployment,
        "membership_config_version": snapshot.config_version,
        "membership_fingerprint": snapshot.fingerprint,
        "local_name": node.name,
        "local_node_id": node.node_id,
        "corosync_package_version": corosync_package,
        "corosync": corosync,
    }
    return "PVN_EMBEDDED_COROSYNC " + module.json.dumps(
        payload, sort_keys=True, separators=(",", ":")
    ) + "\n"


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
standalone_embedded = module.parse_embedded_corosync_snapshot(
    standalone_node,
    embedded_corosync_line(standalone_snapshot, standalone_node),
    standalone_snapshot,
)
check(
    standalone_embedded.config_sha256 is None and standalone_embedded.config_version is None,
    "real minimal standalone membership did not produce an absent Corosync snapshot",
)
invalid_standalone_payload = embedded_corosync_line(snapshot, nodes[0]).replace(
    '"deployment":"lab-cluster"', '"deployment":"standalone-pve-one"'
).replace('"mode":"cluster"', '"mode":"standalone"').replace(
    f'"local_name":"{nodes[0].name}"', '"local_name":"pve-one"'
).replace(f'"local_node_id":{nodes[0].node_id}', '"local_node_id":0').replace(
    f'"membership_config_version":{snapshot.config_version}', '"membership_config_version":0'
).replace(snapshot.fingerprint, standalone_snapshot.fingerprint)
try:
    module.parse_embedded_corosync_snapshot(
        standalone_node, invalid_standalone_payload, standalone_snapshot
    )
except module.UpdateError:
    pass
else:
    raise AssertionError("standalone preflight accepted persisted Corosync state")
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

active_probe = module.parse_probe(
    nodes[0],
    probe_line(snapshot, nodes[0], "0.1.0", central="active", northd_role="active"),
    snapshot,
)
standby_probe = module.parse_probe(
    nodes[1],
    probe_line(snapshot, nodes[1], "0.1.0", central="active", northd_role="standby"),
    snapshot,
)
module.gate_northd_roles({nodes[0].name: active_probe, nodes[1].name: standby_probe})
second_active = module.parse_probe(
    nodes[1],
    probe_line(snapshot, nodes[1], "0.1.0", central="active", northd_role="active"),
    snapshot,
)
try:
    module.gate_northd_roles({nodes[0].name: active_probe, nodes[1].name: second_active})
except module.UpdateError:
    pass
else:
    raise AssertionError("all-node health accepted two active northd roles")
pending_active = module.parse_probe(
    nodes[0],
    probe_line(
        snapshot,
        nodes[0],
        "0.2.1",
        central="active",
        central_pending="0.2.1",
        northd_role="active",
    ),
    snapshot,
)
pending_second = module.parse_probe(
    nodes[1],
    probe_line(
        snapshot,
        nodes[1],
        "0.2.1",
        central="active",
        central_pending="0.2.1",
        northd_role="active",
    ),
    snapshot,
)
module.gate_northd_roles({nodes[0].name: pending_active, nodes[1].name: pending_second})


def exercise_embedded_remote(
    *,
    standalone_mode: bool,
    stale_runtime: bool = False,
    unbounded_runtime: str | None = None,
    timed_out_runtime: bool = False,
    hardlinked_members: bool = False,
):
    with tempfile.TemporaryDirectory() as temporary:
        root = pathlib.Path(temporary)
        members_path = root / "members"
        corosync_path = root / "corosync.conf"
        command_dir = root / "bin"
        command_dir.mkdir()
        if standalone_mode:
            members_path.write_text('{"nodename":"pve-one","version":7}\n', encoding="utf-8")
        else:
            members_path.write_text(
                module.json.dumps({
                    "nodename": "pve-a",
                    "version": 7,
                    "cluster": {
                        "name": "lab-cluster", "version": 3, "nodes": 3, "quorate": 1,
                    },
                    "nodelist": {
                        "pve-a": {"id": 1, "online": 1, "ip": "192.0.2.10"},
                        "pve-b": {"id": 2, "online": 1, "ip": "192.0.2.11"},
                        "pve-c": {"id": 3, "online": 1, "ip": "192.0.2.12"},
                    },
                }, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            corosync_path.write_text(
                "totem {\n    cluster_name: lab-cluster\n    config_version: 3\n}\n"
                "nodelist {\n"
                "  node {\n    name: pve-a\n    nodeid: 1\n    ring0_addr: 192.0.2.10\n  }\n"
                "  node {\n    name: pve-b\n    nodeid: 2\n    ring0_addr: 192.0.2.11\n  }\n"
                "  node {\n    name: pve-c\n    nodeid: 3\n    ring0_addr: 192.0.2.12\n  }\n"
                "}\n",
                encoding="utf-8",
            )
        if hardlinked_members:
            os.link(members_path, root / "members-hardlink")

        dpkg_query = command_dir / "dpkg-query"
        dpkg_query.write_text(
            "#!/bin/sh\ncase \"$*\" in *Status-Status*) printf 'installed\\n' ;; "
            "*Version*) printf '3.1.10-pve2\\n' ;; *) exit 2 ;; esac\n",
            encoding="ascii",
        )
        pvecm = command_dir / "pvecm"
        if standalone_mode:
            pvecm.write_text("#!/bin/sh\nexit 1\n", encoding="ascii")
        else:
            pvecm.write_text(
                "#!/bin/sh\ncase \"$1\" in\n"
                "status) printf 'Name: lab-cluster\\nNodes: 3\\nQuorate: Yes\\n' ;;\n"
                "nodes) printf '1 1 pve-a (local)\\n2 1 pve-b\\n3 1 pve-c\\n' ;;\n"
                "*) exit 2 ;;\nesac\n",
                encoding="ascii",
            )
        runtime_version = 2 if stale_runtime else 3
        cmapctl = command_dir / "corosync-cmapctl"
        if standalone_mode:
            cmapctl.write_text("#!/bin/sh\nexit 1\n", encoding="ascii")
        elif timed_out_runtime:
            cmapctl.write_text("#!/bin/sh\nexec sleep 5\n", encoding="ascii")
        elif unbounded_runtime:
            redirect = " >&2" if unbounded_runtime == "stderr" else ""
            cmapctl.write_text(
                f"#!/bin/sh\nwhile :; do printf '%065536d' 0{redirect}; done\n",
                encoding="ascii",
            )
        else:
            cmap_lines = [
                "totem.cluster_name (str) = lab-cluster",
                f"totem.config_version (u64) = {runtime_version}",
                "nodelist.local_node_pos (u32) = 0",
                "totem.interface.0.bindnetaddr (str) = 192.0.2.10",
            ]
            for position, (name, node_id, address) in enumerate((
                ("pve-a", 1, "192.0.2.10"),
                ("pve-b", 2, "192.0.2.11"),
                ("pve-c", 3, "192.0.2.12"),
            )):
                cmap_lines.extend((
                    f"nodelist.node.{position}.name (str) = {name}",
                    f"nodelist.node.{position}.nodeid (u32) = {node_id}",
                    f"nodelist.node.{position}.ring0_addr (str) = {address}",
                    f"runtime.members.{node_id}.config_version (u64) = {runtime_version}",
                    f"runtime.members.{node_id}.status (str) = joined",
                    f"runtime.members.{node_id}.ip (str) = r(0) ip({address})",
                ))
            cmapctl.write_text(
                "#!/bin/sh\nprintf '%s\\n' "
                + " ".join(shlex.quote(line) for line in cmap_lines)
                + "\n",
                encoding="ascii",
            )
        cfgtool = command_dir / "corosync-cfgtool"
        if standalone_mode:
            cfgtool.write_text("#!/bin/sh\nexit 1\n", encoding="ascii")
        else:
            cfgtool.write_text(
                "#!/bin/sh\ncat <<'EOF'\nLocal node ID 1, transport knet\n"
                "LINK ID 0\n    addr = 192.0.2.10\n"
                "    nodeid: 1: localhost\n    nodeid: 2: connected\n    nodeid: 3: connected\nEOF\n",
                encoding="ascii",
            )
        systemctl = command_dir / "systemctl"
        systemctl.write_text("#!/bin/sh\nexit 3\n", encoding="ascii")
        for command_path in (dpkg_query, pvecm, cmapctl, cfgtool, systemctl):
            command_path.chmod(0o755)

        function = module.REMOTE_SCRIPT[
            module.REMOTE_SCRIPT.index("embedded_corosync_snapshot()"):
            module.REMOTE_SCRIPT.index("\ncorosync_doctor_gate()")
        ].replace("st_uid != 0", f"st_uid != {os.getuid()}")
        if timed_out_runtime:
            function = function.replace(
                "def run(arguments, *, check=True, timeout=20):",
                "def run(arguments, *, check=True, timeout=0.2):",
            )
        environment = os.environ.copy()
        environment.update({
            "PVN_PVE_MEMBERS_FILE": str(members_path),
            "PVN_PVE_COROSYNC_CONF": str(corosync_path),
            "PVN_PVECM_BIN": str(pvecm),
            "PVN_COROSYNC_CFGTOOL_BIN": str(cfgtool),
            "PVN_COROSYNC_CMAPCTL_BIN": str(cmapctl),
            "PVN_SYSTEMCTL_BIN": str(systemctl),
            "PVN_DPKG_QUERY_BIN": str(dpkg_query),
        })
        return subprocess.run(
            ["sh", "-s"],
            input="set -eu\n" + function + "\nembedded_corosync_snapshot\n",
            text=True,
            capture_output=True,
            check=False,
            env=environment,
        )


embedded_standalone = exercise_embedded_remote(standalone_mode=True)
check(
    embedded_standalone.returncode == 0 and '"corosync":null' in embedded_standalone.stdout,
    f"embedded checker rejected real minimal standalone membership: {embedded_standalone.stderr}",
)
embedded_cluster = exercise_embedded_remote(standalone_mode=False)
check(
    embedded_cluster.returncode == 0 and '"config_version":3' in embedded_cluster.stdout,
    f"embedded checker rejected exact clustered runtime: {embedded_cluster.stderr}",
)
embedded_stale = exercise_embedded_remote(standalone_mode=False, stale_runtime=True)
check(embedded_stale.returncode != 0, "embedded checker accepted stale clustered runtime")
for overflow_stream in ("stdout", "stderr"):
    overflow_started = time.monotonic()
    embedded_overflow = exercise_embedded_remote(
        standalone_mode=False, unbounded_runtime=overflow_stream
    )
    check(
        embedded_overflow.returncode != 0
        and f"returned unbounded {overflow_stream}" in embedded_overflow.stderr
        and time.monotonic() - overflow_started < 5,
        f"embedded checker did not terminate live {overflow_stream} overflow at its bound",
    )
timeout_started = time.monotonic()
embedded_timeout = exercise_embedded_remote(
    standalone_mode=False, timed_out_runtime=True
)
check(
    embedded_timeout.returncode != 0
    and "timed out" in embedded_timeout.stderr
    and time.monotonic() - timeout_started < 2,
    "embedded checker did not terminate and reap a timed-out command",
)
embedded_hardlink = exercise_embedded_remote(
    standalone_mode=True, hardlinked_members=True
)
check(
    embedded_hardlink.returncode != 0
    and "unsafe ownership or permissions" in embedded_hardlink.stderr,
    "embedded checker accepted a hardlinked PVE membership file",
)
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
    restart_pending: bool = False,
    helper_failure: bool = False,
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
        restart_directory = root / "pvn-node"
        restart_directory.mkdir(mode=0o700)
        if restart_pending:
            marker = restart_directory / "central-restart-pending"
            marker.write_text("0.2.1\n", encoding="ascii")
            marker.chmod(0o600)
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

        appctl = root / "ovn-appctl"
        appctl.write_text(
            f"#!/bin/sh\nprintf 'northd-role\\n' >> '{calls}'\n[ \"$1 $2 $3\" = '-t ovn-northd status' ]\nprintf 'Status: active\\n'\n",
            encoding="ascii",
        )
        appctl.chmod(0o755)

        northd_helper = root / "pvn-ovn-northd"
        northd_helper.write_text(
            f"#!/bin/sh\nprintf 'northd-helper\\n' >> '{calls}'\n[ \"$1\" = status ]\n"
            + ("exit 1\n" if helper_failure else "exit 0\n"),
            encoding="ascii",
        )
        northd_helper.chmod(0o755)

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
                "/usr/bin/ovn-appctl": str(appctl),
                "/usr/lib/pvn/pvn-ovn-northd": str(northd_helper),
                "/var/lib/pvn-node": str(restart_directory),
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
helper_drift = exercise_remote_central_health("raft", helper_failure=True)
check(helper_drift.returncode != 0, "updater accepted failed clustered northd health")
pending_helper = exercise_remote_central_health(
    "raft", restart_pending=True, helper_failure=True
)
check(
    pending_helper.returncode == 0,
    "pending package/schema repin was made dependent on restarting northd",
)
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
corosync_gate_fail: set[str] = set()
embedded_corosync_fail: set[str] = set()
embedded_corosync_package = {"pve-a": "3.1.10-pve2", "pve-b": "3.1.10-pve2"}
embedded_corosync_sha = {"pve-a": "c" * 64, "pve-b": "c" * 64}


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
        if action == "embedded-corosync":
            if node.name in embedded_corosync_fail:
                raise module.UpdateError(f"injected embedded Corosync failure on {node.name}")
            return subprocess.CompletedProcess(
                [],
                0,
                embedded_corosync_line(
                    self.snapshot,
                    node,
                    corosync_package=embedded_corosync_package[node.name],
                    config_sha=embedded_corosync_sha[node.name],
                ),
                "",
            )
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
        if action == "corosync-gate":
            if node.name in corosync_gate_fail:
                raise module.UpdateError(f"injected Corosync doctor failure on {node.name}")
            return subprocess.CompletedProcess(
                [], 0, f"PVN_COROSYNC_GATE version={versions[node.name]}\n", ""
            )
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
    first_prepare = min(index for index, event in enumerate(events) if event.startswith("prepare:"))
    for expected in ("embedded-corosync:pve-a", "embedded-corosync:pve-b"):
        check(events.index(expected) < first_prepare, "embedded Corosync gate ran after remote staging")
    for expected in ("verify:pve-a", "verify:pve-b"):
        check(events.index(expected) < first_apply, "every pending DEB was not verified before the first apply")
    check(events.index("apply:pve-a") < events.index("apply:pve-b"), "nodes were not updated sequentially")
    for expected in ("cleanup:pve-a", "cleanup:pve-b"):
        check(events.count(expected) == 1, "successful update left a staged DEB behind")
    check(events.count("membership-revalidate") >= 4, "membership was not repeatedly revalidated")
    check(
        events.index("corosync-gate:pve-a") < events.index("apply:pve-b"),
        "updated node was not Corosync-gated before the next node mutation",
    )
    success_text = output.getvalue()
    schema_step = success_text.index("pvn-topology active schema migration")
    control_step = success_text.index("pvn-control-plane plan")
    restart_step = success_text.index("only after both succeed may central voters be restarted")
    check(schema_step < control_step < restart_step, "success guidance printed an unsafe restart order")

    plan_output = io.StringIO()
    with contextlib.redirect_stdout(plan_output):
        module.plan(snapshot, deb, "0.2.1", "amd64", "a" * 64)
    restart_lines = [
        line for line in plan_output.getvalue().splitlines()
        if "restart-required marker" in line
    ]
    check(len(restart_lines) == 1 and "pve-a" in restart_lines[0], "plan omitted active central restart work")
    check("pve-b" not in restart_lines[0], "plan treated an inactive central target as restartable")
    check(
        plan_output.getvalue().index("pvn-topology active schema migration")
        < plan_output.getvalue().index("pvn-control-plane combined package/schema repin")
        < plan_output.getvalue().index("only then a guarded central restart"),
        "plan printed an unsafe schema/repin/restart order",
    )

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


def exercise_inactive_odd_rollout(
    count: int,
    *,
    initially_updated: int = 0,
    failing_gate: str = "",
    failing_embedded_gate: str = "",
    embedded_package_overrides: dict[str, str] | None = None,
    embedded_sha_overrides: dict[str, str] | None = None,
    embedded_drift_after_first: str = "",
    embedded_drift_on_call: int = 2,
):
    scenario_nodes = tuple(
        module.Node(index, f"pve-{index}", f"192.0.2.{index + 9}", index == 1)
        for index in range(1, count + 1)
    )
    scenario_fingerprint = module.membership_fingerprint(
        "cluster", f"odd-{count}", 11, scenario_nodes
    )
    scenario_snapshot = module.Snapshot(
        "cluster", f"odd-{count}", 11, "pve-1", scenario_nodes, scenario_fingerprint
    )
    scenario_versions = {
        node.name: "0.2.1" if index < initially_updated else "0.1.0"
        for index, node in enumerate(scenario_nodes)
    }
    scenario_events: list[str] = []
    scenario_embedded_calls = {node.name: 0 for node in scenario_nodes}
    scenario_packages = {node.name: "3.1.10-pve2" for node in scenario_nodes}
    scenario_shas = {node.name: "c" * 64 for node in scenario_nodes}
    scenario_packages.update(embedded_package_overrides or {})
    scenario_shas.update(embedded_sha_overrides or {})

    class ScenarioLease:
        def __init__(self, used_snapshot):
            self.snapshot = used_snapshot

        def acquire(self):
            scenario_events.append("lease-acquire")

        def release(self):
            scenario_events.append("lease-release")

    class ScenarioTransport:
        def __init__(self, used_snapshot, deb):
            self.snapshot = used_snapshot
            self.deb = deb

        def run(self, node, action, *arguments, check=True):
            scenario_events.append(f"{action}:{node.name}")
            if action == "embedded-corosync":
                scenario_embedded_calls[node.name] += 1
                if node.name == failing_embedded_gate:
                    raise module.UpdateError(f"stale v0.2.13 Corosync runtime on {node.name}")
                config_sha = scenario_shas[node.name]
                if node.name == embedded_drift_after_first and \
                        scenario_embedded_calls[node.name] >= embedded_drift_on_call:
                    config_sha = "d" * 64
                return subprocess.CompletedProcess(
                    [],
                    0,
                    embedded_corosync_line(
                        self.snapshot,
                        node,
                        corosync_package=scenario_packages[node.name],
                        config_sha=config_sha,
                    ),
                    "",
                )
            if action == "probe":
                return subprocess.CompletedProcess(
                    [],
                    0,
                    probe_line(
                        self.snapshot,
                        node,
                        scenario_versions[node.name],
                        node_state="inactive",
                        central="inactive",
                    ),
                    "",
                )
            if action == "prepare":
                return subprocess.CompletedProcess(
                    [], 0, f"/var/tmp/pvn-node-update.pve{node.node_id}.deb\n", ""
                )
            if action == "corosync-gate":
                if node.name == failing_gate:
                    raise module.UpdateError(f"stale Corosync runtime on {node.name}")
                return subprocess.CompletedProcess(
                    [], 0, f"PVN_COROSYNC_GATE version={scenario_versions[node.name]}\n", ""
                )
            if action in {"verify", "cleanup"}:
                return subprocess.CompletedProcess([], 0, "", "")
            if action == "apply":
                scenario_versions[node.name] = arguments[2]
                return subprocess.CompletedProcess(
                    [], 0, f"PVN_UPDATED version={arguments[2]} central-restart=none\n", ""
                )
            raise AssertionError(f"unexpected scenario action {action}")

        def copy(self, node, destination):
            scenario_events.append(f"copy:{node.name}")

    saved_transport = module.Transport
    saved_lease = module.UpdateLease
    saved_revalidate = module.revalidate
    saved_compare = module.compare_versions
    module.Transport = ScenarioTransport
    module.UpdateLease = ScenarioLease
    module.revalidate = lambda ignored: scenario_events.append("membership-revalidate")
    module.compare_versions = (
        lambda left, operator, right: operator == "lt" and left == "0.1.0" and right == "0.2.1"
    )
    error = None
    try:
        with tempfile.TemporaryDirectory() as temporary:
            deb = pathlib.Path(temporary) / "pvn-node.deb"
            deb.write_bytes(b"test")
            with contextlib.redirect_stdout(io.StringIO()):
                module.update_cluster(scenario_snapshot, deb, "0.2.1", "amd64", "a" * 64)
    except module.UpdateError as caught:
        error = caught
    finally:
        module.Transport = saved_transport
        module.UpdateLease = saved_lease
        module.revalidate = saved_revalidate
        module.compare_versions = saved_compare
    return scenario_nodes, scenario_versions, scenario_events, error


# Every supported odd cluster size can roll while PVN is topology-only and
# inactive. Each completed node is gated before the following package mutation.
for odd_count in (1, 3, 5):
    odd_nodes, odd_versions, odd_events, odd_error = exercise_inactive_odd_rollout(odd_count)
    check(odd_error is None, f"inactive {odd_count}-node rollout failed: {odd_error}")
    check(
        set(odd_versions.values()) == {"0.2.1"},
        f"inactive {odd_count}-node rollout did not finish",
    )
    for index in range(len(odd_nodes) - 1):
        current = odd_nodes[index].name
        following = odd_nodes[index + 1].name
        current_apply = odd_events.index(f"apply:{current}")
        following_apply = odd_events.index(f"apply:{following}")
        check(
            any(
                event == f"corosync-gate:{current}"
                for event in odd_events[current_apply + 1 : following_apply]
            ),
            f"{odd_count}-node rollout mutated {following} before gating {current}",
        )


# A stale v0.2.13 runtime is rejected by the embedded checker before even a
# remote staging file or first dpkg transaction can be created.
_, _, stale_events, stale_error = exercise_inactive_odd_rollout(
    3, failing_embedded_gate="pve-1"
)
check(stale_error is not None, "stale Corosync runtime did not fail the rollout")
check(not any(event.startswith("prepare:") for event in stale_events), "stale runtime reached remote staging")
check(not any(event.startswith("apply:") for event in stale_events), "stale runtime reached dpkg")


# Persisted config/package disagreement across nodes is rejected during the
# same first sweep, also with zero package/staging mutation.
for label, options in (
    ("mixed Corosync package", {"embedded_package_overrides": {"pve-3": "3.1.11-pve1"}}),
    ("mixed Corosync runtime", {"embedded_sha_overrides": {"pve-3": "e" * 64}}),
):
    _, _, mismatch_events, mismatch_error = exercise_inactive_odd_rollout(3, **options)
    check(mismatch_error is not None, f"{label} unexpectedly passed")
    check(
        not any(event.startswith(("prepare:", "copy:", "apply:")) for event in mismatch_events),
        f"{label} was detected after package staging/mutation",
    )


# A state change between the baseline and the immediate confirmation sweep is
# a TOCTOU failure before staging or dpkg.
_, _, toctou_events, toctou_error = exercise_inactive_odd_rollout(
    3, embedded_drift_after_first="pve-1"
)
check(toctou_error is not None, "Corosync TOCTOU drift unexpectedly passed")
check(
    not any(event.startswith(("prepare:", "copy:", "apply:")) for event in toctou_events),
    "Corosync TOCTOU drift was detected after staging/dpkg",
)

# Drift after checksum-verified staging is caught by the all-node sweep placed
# immediately before the first dpkg transaction.
_, _, staged_toctou_events, staged_toctou_error = exercise_inactive_odd_rollout(
    3, embedded_drift_after_first="pve-1", embedded_drift_on_call=3
)
check(staged_toctou_error is not None, "post-staging Corosync TOCTOU drift passed")
check(any(event.startswith("prepare:") for event in staged_toctou_events), "TOCTOU fixture did not stage")
check(not any(event.startswith("apply:") for event in staged_toctou_events), "TOCTOU drift reached dpkg")


# The installed target's doctor remains an independent post-update gate and
# still stops before the second package transaction.
_, _, postinstall_events, postinstall_error = exercise_inactive_odd_rollout(
    3, failing_gate="pve-1"
)
check(postinstall_error is not None, "post-update Corosync doctor failure was ignored")
check("apply:pve-1" in postinstall_events, "post-update doctor was not exercised")
check("apply:pve-2" not in postinstall_events, "failed post-update doctor did not stop rollout")


# On a resumed rollout an already-updated node's doctor failure is checked
# before staging or applying the remaining nodes.
_, _, resume_events, resume_error = exercise_inactive_odd_rollout(
    3, initially_updated=1, failing_gate="pve-1"
)
check(resume_error is not None, "resumed rollout ignored a failed Corosync doctor")
check(not any(event.startswith("prepare:") for event in resume_events), "doctor gate ran after staging")
check(not any(event.startswith("apply:") for event in resume_events), "doctor gate ran after mutation")


source = SCRIPT.read_text(encoding="utf-8")
postinst_source = (REPO / "packaging/debian/pvn-node.postinst").read_text(encoding="utf-8")
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
    "pvnctl doctor --check corosync-runtime-config",
    'transport.run(node, "corosync-gate")',
    'transport.run(node, "embedded-corosync")',
    "embedded_corosync_snapshot",
    "pvn-topology active schema migration",
    "combined package/schema repin",
):
    check(required in source, f"missing fail-closed updater behavior: {required}")

postinst_gate = postinst_source.index("\nverify_corosync_runtime\n")
daemon_reload = postinst_source.index("\nsystemctl daemon-reload")
node_restart = postinst_source.index("\n    if ! systemctl restart")
check(postinst_gate < daemon_reload < node_restart, "postinst Corosync gate runs after service mutation")
check(
    "/usr/sbin/pvnctl doctor --check corosync-runtime-config" in postinst_source,
    "postinst does not use the exact configuration-independent doctor check",
)
embedded_source = source[
    source.index("embedded_corosync_snapshot()") : source.index("\ncorosync_doctor_gate()")
]
check("/usr/sbin/pvnctl" not in embedded_source, "pre-dpkg gate depends on installed pvnctl")

print("pvn-cluster-update tests passed")
