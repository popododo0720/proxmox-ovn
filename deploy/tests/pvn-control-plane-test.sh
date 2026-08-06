#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
SCRIPT=$REPO/deploy/scripts/pvn-control-plane

"$SCRIPT" --help >/dev/null

PVN_CONTROL_PLANE_SCRIPT=$SCRIPT python3 <<'PY'
import base64
import ast
import copy
import contextlib
import hashlib
import io
import ipaddress
import json
import os
import pathlib
import re
import subprocess
import tempfile
import sys
import types
import uuid

script_path = pathlib.Path(os.environ["PVN_CONTROL_PLANE_SCRIPT"])
source = script_path.read_text()
marker = "__PVN_CONTROL_PLANE_PYTHON__"
payload = source.split(marker + "'\n", 1)[1].rsplit("\n" + marker, 1)[0]
loaded = types.ModuleType("pvn_control_plane_module")
loaded.__file__ = str(script_path)
sys.modules[loaded.__name__] = loaded
exec(compile(payload, str(script_path), "exec"), loaded.__dict__)
module = loaded.__dict__
ControlPlane = module["ControlPlane"]
ControlPlaneError = module["ControlPlaneError"]
Discovery = module["Discovery"]
LedgerStore = module["LedgerStore"]
Node = module["Node"]
SystemBackend = module["SystemBackend"]
DATABASES = module["DATABASES"]
MAX_KNOWN_HOSTS_BYTES = module["MAX_KNOWN_HOSTS_BYTES"]


def discovery(count=3, package="0.1.1"):
    records = tuple(
        (
            f"pve-{chr(ord('a') + index)}",
            index + 1,
            f"192.0.2.{11 + index}",
            f"198.51.100.{11 + index}",
        )
        for index in range(count)
    )
    nodes = tuple(Node(name, node_id, control, geneve, "ens4", "ens5", "br-provider",
                       package, 1450, index == 0)
                  for index, (name, node_id, control, geneve) in enumerate(records))
    mode = "standalone" if count == 1 else "raft"
    confirm = "standalone-pve-a" if count == 1 else "test-cluster"
    return Discovery(mode, confirm, confirm, "pve-a", 7, 3 if count > 1 else 0,
                     "a" * 64, None, 1380, "provider", nodes)


class FakeBackend:
    def __init__(self, found):
        self.found = found
        self.log = []
        self.shared = None
        self.staged = {}
        self.control_dbs = {}
        self.pending_control_dbs = set()
        self.pending_control_cids = {}
        self.pending_control_remotes = {}
        self.pending_control_server_ids = {}
        self.central = []
        self.central_markers = []
        self.nodes = []
        self.node_markers = []
        self.node_targets = []
        self.restart_pending = {}
        self.cids = {name: f"cid-{index}" for index, name in enumerate(DATABASES, 1)}
        self.block_at = None
        self.pki_variant = "one"
        self.pki_installed = set()
        self.pki_pin_check = None
        self.pristine = True
        self.crash_after_init = None
        self.discoveries = 0
        self.discover_hook = None
        self.staged_report_hook = None
        self.complete_report_hook = None
        self.doctor_fail = set()

    def discover(self):
        self.discoveries += 1
        if self.discover_hook is not None:
            self.discover_hook(self)
        return self.found

    def assert_pristine(self, found):
        self.log.append("pristine")
        if not self.pristine:
            raise ControlPlaneError("unledgered state")

    def assert_planned_package_repin_safe(self, found):
        if self.central or self.central_markers or self.nodes or self.node_markers or \
                self.node_targets or self.restart_pending or self.control_dbs or \
                self.pending_control_dbs or self.pending_control_remotes or \
                self.pending_control_server_ids or self.pending_control_cids:
            raise ControlPlaneError(
                "planned ledger package repin refused: activation or central database state"
            )

    def assert_staged_seed_package_repin_safe(self, found, ledger):
        seed = found.nodes[0]
        if self.central != [seed.name] or self.central_markers != [seed.name] or \
                self.nodes or self.node_markers or self.node_targets or \
                self.restart_pending != {seed.name: seed.package_version} or \
                set(self.control_dbs) != {seed.name} or self.pending_control_dbs or \
                self.pending_control_remotes or self.pending_control_server_ids or \
                self.pending_control_cids:
            raise ControlPlaneError("staged ledger package repin refused: runtime shape")
        if self.control_dbs[seed.name] != ledger["control_db_cluster_id"]:
            raise ControlPlaneError("staged ledger package repin refused: wrong Control CID")
        report = self.central_status(seed, found.mode)
        report["offline"] = {
            name: {
                "exists": True,
                "name": name,
                "clustered": True,
                "local_address": f"ssl:{seed.control_ip}:{port}",
                "cluster_id": self.cids[name],
                "server_id": f"sid-{name}-{seed.name}",
            }
            for name, port in DATABASES.items()
        }
        if self.staged_report_hook is not None:
            self.staged_report_hook(report)
        return report

    def assert_partial_central_package_repin_safe(self, found, ledger):
        complete = ledger["central_complete"]
        active = [node.name for node in found.nodes[:complete]]
        next_node = found.nodes[complete].name
        expected_remote = [f"ssl:{found.nodes[0].control_ip}:6646"]
        active_server_ids = {f"sid-PVN_Control-{name}" for name in active}
        if self.central != active or self.central_markers != active or \
                self.nodes or self.node_markers or self.node_targets or \
                set(self.control_dbs) != set(active) or \
                self.pending_control_dbs != {next_node} or \
                set(self.pending_control_cids) != {next_node} or \
                self.pending_control_cids[next_node] not in {
                    None, ledger["control_db_cluster_id"]
                } or \
                self.pending_control_remotes != {next_node: expected_remote} or \
                set(self.pending_control_server_ids) != {next_node} or \
                not self.pending_control_server_ids[next_node] or \
                self.pending_control_server_ids[next_node] in active_server_ids or \
                self.restart_pending != {
                    node.name: node.package_version for node in found.nodes[:complete]
                }:
            raise ControlPlaneError(
                f"central-{complete} ledger package repin refused: runtime shape"
            )
        if any(self.control_dbs[name] != ledger["control_db_cluster_id"] for name in active):
            raise ControlPlaneError(
                f"central-{complete} ledger package repin refused: wrong Control CID"
            )
        return [(node, self.central_status(node, found.mode))
                for node in found.nodes[:complete]]

    def assert_complete_package_repin_safe(self, found, ledger):
        names = [node.name for node in found.nodes]
        expected_restart = {
            node.name: node.package_version for node in found.nodes
        }
        if self.central != names or self.central_markers != names or \
                self.nodes != names or self.node_markers != names or \
                self.node_targets != names or set(self.control_dbs) != set(names) or \
                self.pending_control_dbs or self.pending_control_remotes or \
                self.pending_control_server_ids or self.pending_control_cids or \
                self.restart_pending != expected_restart:
            raise ControlPlaneError("complete ledger package repin refused: runtime shape")
        if found.mode == "raft" and any(
                self.control_dbs[name] != ledger["control_db_cluster_id"]
                for name in names):
            raise ControlPlaneError(
                "complete ledger package repin refused: wrong Control CID"
            )
        reports = [(node, self.central_status(node, found.mode)) for node in found.nodes]
        if self.complete_report_hook is not None:
            self.complete_report_hook(reports)
        for node in found.nodes:
            status = self.node_status(node)
            if not all(status.get(key) is True for key in
                       ("ready", "target_active", "service_ready", "doctor", "marker")):
                raise ControlPlaneError(
                    f"complete ledger package repin refused: doctor failed on {node.name}"
                )
        return reports

    def ensure_shared_config(self, config):
        encoded = json.dumps(config, sort_keys=True)
        if self.shared is None:
            self.shared = encoded
            self.log.append("config")
        elif self.shared != encoded:
            raise ControlPlaneError("config drift")

    def prepare_pki(self, cluster_id, nodes, expected_fingerprints):
        fingerprint = lambda value: hashlib.sha256(value.encode()).hexdigest()
        fingerprints = {
            "ca_certificate_sha256": fingerprint("ca-" + self.pki_variant),
            "nodes": {},
        }
        for node in nodes:
            fingerprints["nodes"][node.name] = {
                "certificate_sha256": fingerprint(
                    "cert-" + self.pki_variant + "-" + node.name
                ),
                "public_key_sha256": fingerprint("public-key-" + node.name),
            }
        if not any(entry.startswith("pki:") for entry in self.log):
            self.log.append("pki:" + cluster_id)
        installations = {
            node.name: {
                "name": node.name,
                "ca": "public-ca",
                "certificate": "public-certificate-" + node.name,
            }
            for node in nodes
        }
        return fingerprints, installations

    def install_pki(self, nodes, installations):
        if self.pki_pin_check is not None:
            assert self.pki_pin_check(), "public certificates installed before ledger PKI pin"
        encoded = json.dumps(installations, sort_keys=True)
        assert "PRIVATE KEY" not in encoded and "node-key.pem" not in encoded
        for node in nodes:
            if node.name not in self.pki_installed:
                self.pki_installed.add(node.name)
                self.log.append("pki-install:" + node.name)

    def stage(self, node, files):
        snapshot = json.dumps(files, sort_keys=True)
        if node.name not in self.staged:
            self.staged[node.name] = snapshot
            self.log.append("stage:" + node.name)
        elif self.staged[node.name] != snapshot:
            raise ControlPlaneError("stage drift")

    def init_control(self, node, mode, cluster_id, seed, expected_cluster_id):
        if node.name in self.pending_control_dbs:
            if node == seed or mode != "raft" or expected_cluster_id is None:
                raise ControlPlaneError("pending Control DB is not pinned")
            control_cid = self.pending_control_cids[node.name]
            return {
                "cluster_id": control_cid,
                "cluster_id_pending": control_cid is None,
                "preactivation_join": True,
                "remote_addresses": self.pending_control_remotes[node.name],
                "server_id": self.pending_control_server_ids[node.name],
                "created": False,
            }
        actual = self.control_dbs.get(node.name)
        if actual is not None:
            if mode == "raft" and expected_cluster_id is None:
                raise ControlPlaneError("existing Control DB cluster ID is not pinned")
            if mode == "raft" and actual != expected_cluster_id:
                raise ControlPlaneError("existing Control DB cluster ID differs from the ledger")
            return {"cluster_id": actual if mode == "raft" else None, "created": False}
        actual = self.cids["PVN_Control"] if mode == "raft" else "standalone"
        if mode == "raft" and expected_cluster_id is not None and actual != expected_cluster_id:
            raise ControlPlaneError("new Control DB joined a different cluster ID")
        if node.name not in self.control_dbs:
            self.control_dbs[node.name] = actual
            self.log.append("init:" + node.name)
        if self.crash_after_init == node.name:
            self.crash_after_init = None
            raise ControlPlaneError("simulated crash after Control DB initialization")
        return {"cluster_id": actual if mode == "raft" else None, "created": True}

    def activate_central(self, node):
        if node.name in self.pending_control_dbs:
            self.pending_control_dbs.remove(node.name)
            self.pending_control_cids.pop(node.name, None)
            self.pending_control_remotes.pop(node.name, None)
            self.pending_control_server_ids.pop(node.name, None)
            self.control_dbs[node.name] = self.cids["PVN_Control"]
        if node.name not in self.central_markers:
            self.central_markers.append(node.name)
        if node.name not in self.central:
            self.central.append(node.name)
            self.log.append("central:" + node.name)

    def central_status(self, node, mode):
        count = len(self.central)
        healthy = self.block_at != count
        rows = []
        for name, port in DATABASES.items():
            rows.append({
                "database": name,
                "healthy": healthy,
                "member_count": count,
                "connected_members": count,
                "membership_change": False,
                "cluster_id": self.cids[name] if mode == "raft" else None,
                "server_id": f"sid-{name}-{node.name}",
                "address": f"ssl:{node.control_ip}:{port}" if mode == "raft" else None,
            })
        return {"healthy": healthy, "target_active": node.name in self.central,
                "databases": rows}

    def activate_node(self, node):
        if len(self.central) != len(self.found.nodes):
            raise AssertionError("transport activation preceded central convergence")
        if node.name not in self.nodes:
            self.nodes.append(node.name)
            self.node_markers.append(node.name)
            self.node_targets.append(node.name)
            self.log.append("node:" + node.name)

    def node_status(self, node):
        target_active = node.name in self.node_targets
        service_ready = node.name in self.nodes
        doctor = node.name not in self.doctor_fail
        return {
            "ready": target_active and service_ready and doctor,
            "target_active": target_active,
            "service_ready": service_ready,
            "doctor": doctor,
            "marker": node.name in self.node_markers,
        }


def expect_error(action, text):
    try:
        action()
    except ControlPlaneError as error:
        assert text in str(error), (text, str(error))
    else:
        raise AssertionError("expected ControlPlaneError")


def remote_db_info_runner(cluster_id_result, show_log=None):
    if show_log is None:
        show_log = (
            'record 0:\n name: "PVN_Control\'\n'
            ' local address: "ssl:192.0.2.12:6646"\n'
            ' server_id: serv\n'
            ' remote_addresses: ssl:192.0.2.11:6646\n\n'
        )
    def fake_run(arguments, check=True):
        operation = arguments[2] if arguments[1] == "--more" else arguments[1]
        values = {
            "db-name": types.SimpleNamespace(returncode=0, stdout="PVN_Control\n", stderr=""),
            "db-is-clustered": types.SimpleNamespace(returncode=0, stdout="", stderr=""),
            "db-local-address": types.SimpleNamespace(
                returncode=0, stdout="ssl:192.0.2.12:6646\n", stderr=""
            ),
            "db-sid": types.SimpleNamespace(returncode=0, stdout="server-id\n", stderr=""),
            "db-cid": cluster_id_result,
            "show-log": types.SimpleNamespace(
                returncode=0,
                stdout=show_log,
                stderr="",
            ),
        }
        result = values[operation]
        if check and result.returncode != 0:
            raise AssertionError(f"unexpected checked failure: {operation}")
        return result
    return fake_run


def load_remote_db_info(run_function):
    tree = ast.parse(module["REMOTE_HELPER"])
    selected = [
        node for node in tree.body
        if isinstance(node, ast.FunctionDef) and node.name in {
            "fail", "ipv4", "join_stub_remote", "db_info"
        }
    ]
    namespace = {
        "ipaddress": ipaddress, "os": os, "re": re, "sys": sys,
        "run": run_function,
    }
    exec(compile(ast.Module(body=selected, type_ignores=[]), "remote-db-info", "exec"),
         namespace)
    return namespace["db_info"]


# Only ovsdb-tool's exact pre-activation join sentinel is represented as a
# pending CID. A known CID remains strict, and any lookalike command failure
# still aborts the remote probe.
with tempfile.TemporaryDirectory() as temporary:
    database = pathlib.Path(temporary) / "control.db"
    database.touch()
    pending = types.SimpleNamespace(
        returncode=2, stdout="", stderr=f"{database}: cluster ID not yet known\n"
    )
    info = load_remote_db_info(remote_db_info_runner(pending))(str(database))
    assert info["cluster_id"] is None and info["cluster_id_pending"] is True
    assert info["server_id"] == "server-id"
    assert info["remote_addresses"] == ["ssl:192.0.2.11:6646"]

    known = types.SimpleNamespace(returncode=0, stdout="expected-cid\n", stderr="")
    info = load_remote_db_info(remote_db_info_runner(known))(str(database), True)
    assert info["cluster_id"] == "expected-cid"
    assert info["cluster_id_pending"] is False
    assert info["preactivation_join"] is True
    assert info["remote_addresses"] == ["ssl:192.0.2.11:6646"]

    for rejected in (
        types.SimpleNamespace(
            returncode=1, stdout="", stderr=f"{database}: cluster ID not yet known\n"
        ),
        types.SimpleNamespace(returncode=2, stdout="", stderr="database corrupt\n"),
    ):
        error = io.StringIO()
        with contextlib.redirect_stderr(error):
            try:
                load_remote_db_info(remote_db_info_runner(rejected))(str(database))
            except SystemExit:
                pass
            else:
                raise AssertionError("non-exact db-cid failure was accepted")
        assert "ovsdb-tool db-cid" in error.getvalue()

    malformed_logs = (
        "record 0:\n remote_addresses: ssl:192.0.2.11:6646\n"
        "record 1:\n remote_addresses: ssl:192.0.2.11:6646\n",
        "record 0:\n",
        "record 0:\n remote_addresses: ssl:192.0.2.11:6646 ssl:192.0.2.99:6646\n",
    )
    for malformed in malformed_logs:
        error = io.StringIO()
        with contextlib.redirect_stderr(error):
            try:
                load_remote_db_info(
                    remote_db_info_runner(pending, malformed)
                )(str(database))
            except SystemExit:
                pass
            else:
                raise AssertionError("malformed join-stub log was accepted")
        assert "clustered database" in error.getvalue()


# Corosync probe parsing and file access preserve the exact shared-file bytes,
# membership/ring mapping, and root-owned no-link/no-write boundary.
def load_remote_corosync_helpers():
    tree = ast.parse(module["REMOTE_HELPER"])
    selected = [
        node for node in tree.body
        if isinstance(node, ast.FunctionDef) and node.name in {
            "fail", "ipv4", "secure_file_bytes", "corosync_snapshot",
        }
    ]
    namespace = {
        "hashlib": hashlib,
        "ipaddress": ipaddress,
        "os": os,
        "re": re,
        "stat": module["stat"],
        "sys": sys,
        "SAFE_NAME": module["SAFE_NAME"],
        "NODE_BLOCK": re.compile(r"(?ms)^\s*node\s*\{.*?^\s*\}"),
    }
    exec(compile(ast.Module(body=selected, type_ignores=[]),
                 "remote-corosync", "exec"), namespace)
    return namespace


def expect_remote_failure(callback, message):
    error = io.StringIO()
    with contextlib.redirect_stderr(error):
        try:
            callback()
        except SystemExit:
            pass
        else:
            raise AssertionError("remote Corosync guard unexpectedly passed")
    assert message in error.getvalue(), error.getvalue()


with tempfile.TemporaryDirectory() as temporary:
    helpers = load_remote_corosync_helpers()
    root = pathlib.Path(temporary)
    config = root / "corosync.conf"
    content = b"""totem {
    cluster_name: test-cluster
    config_version: 9
}
nodelist {
    node {
        name: pve-a
        nodeid: 1
        ring1_addr: 192.0.2.11
    }
    node {
        name: pve-b
        nodeid: 2
        ring1_addr: 192.0.2.12
    }
    node {
        name: pve-c
        nodeid: 3
        ring1_addr: 192.0.2.13
    }
}
"""
    config.write_bytes(content)
    config.chmod(0o640)
    opened = helpers["secure_file_bytes"](config, "corosync.conf", 4096)
    assert opened == content
    snapshot = helpers["corosync_snapshot"](
        opened, "test-cluster", {"pve-a": 1, "pve-b": 2, "pve-c": 3}
    )
    assert snapshot == {
        "sha256": hashlib.sha256(content).hexdigest(),
        "config_version": 9,
        "rings": {"1": {
            "pve-a": "192.0.2.11",
            "pve-b": "192.0.2.12",
            "pve-c": "192.0.2.13",
        }},
    }
    expect_remote_failure(
        lambda: helpers["corosync_snapshot"](
            opened, "wrong-cluster", {"pve-a": 1, "pve-b": 2, "pve-c": 3}
        ),
        "cluster name/config_version",
    )
    expect_remote_failure(
        lambda: helpers["corosync_snapshot"](
            opened, "test-cluster", {"pve-a": 1, "pve-b": 2, "pve-c": 4}
        ),
        "node identity differs",
    )
    hardlink = root / "corosync-hardlink"
    os.link(config, hardlink)
    expect_remote_failure(
        lambda: helpers["secure_file_bytes"](config, "corosync.conf", 4096),
        "owner/link/mode",
    )
    hardlink.unlink()
    config.chmod(0o660)
    expect_remote_failure(
        lambda: helpers["secure_file_bytes"](config, "corosync.conf", 4096),
        "owner/link/mode",
    )
    config.chmod(0o640)
    symlink = root / "corosync-link"
    symlink.symlink_to(config)
    expect_remote_failure(
        lambda: helpers["secure_file_bytes"](symlink, "corosync.conf", 4096),
        "non-symlink",
    )
    expect_remote_failure(
        lambda: helpers["secure_file_bytes"](config, "corosync.conf", 8),
        "size limit",
    )

remote_helper_source = module["REMOTE_HELPER"]
assert remote_helper_source.count(
    'secure_file_bytes(COROSYNC_CONFIG, "corosync.conf", MAX_COROSYNC_BYTES)'
) == 2
assert '[PVNCTL_BIN, "doctor", "--check", "corosync-runtime-config"], 30' in \
    remote_helper_source
assert "stdout=subprocess.DEVNULL" in remote_helper_source
assert remote_helper_source.count("os.path.lexists(COROSYNC_CONFIG)") == 2

remote_tree = ast.parse(remote_helper_source)
standalone_guard_nodes = [
    node for node in remote_tree.body
    if isinstance(node, ast.FunctionDef) and node.name in {
        "fail", "probe_standalone_corosync",
    }
]

class StandaloneCorosyncPath:
    def __init__(self, states):
        self.states = iter(states)

    def lexists(self, _path):
        return next(self.states)


def standalone_guard(states, doctor_status=0):
    calls = []
    namespace = {
        "os": types.SimpleNamespace(path=StandaloneCorosyncPath(states)),
        "sys": sys,
        "COROSYNC_CONFIG": pathlib.Path("/etc/pve/corosync.conf"),
        "PVNCTL_BIN": "/usr/sbin/pvnctl",
        "run_quiet": lambda arguments, timeout: (
            calls.append((arguments, timeout)) or doctor_status
        ),
    }
    exec(compile(ast.Module(body=standalone_guard_nodes, type_ignores=[]),
                 "standalone-corosync", "exec"), namespace)
    return namespace["probe_standalone_corosync"], calls


guard, calls = standalone_guard([False, False])
assert guard() is True
assert calls == [([
    "/usr/sbin/pvnctl", "doctor", "--check", "corosync-runtime-config",
], 30)]
guard, _ = standalone_guard([False, False], doctor_status=1)
assert guard() is False
guard, _ = standalone_guard([True])
expect_remote_failure(guard, "unexpectedly has")
guard, _ = standalone_guard([False, True])
expect_remote_failure(guard, "appeared during")


# Exercise the production init-control dispatch with fake ovsdb-tool,
# pvnctl, and systemctl processes. This covers the legacy unknown-CID stub,
# new --cid-pinned stub, and the post-activation/pre-ledger-write resume state.
with tempfile.TemporaryDirectory() as temporary:
    root = pathlib.Path(temporary)
    database = root / "pvn_control.db"
    marker_path = root / "central-enabled"
    command_log = root / "pvnctl-command.json"
    fake_bin = root / "bin"
    fake_bin.mkdir()
    fake_ovsdb = fake_bin / "ovsdb-tool"
    fake_pvnctl = fake_bin / "pvnctl"
    fake_systemctl = fake_bin / "systemctl"
    fake_ovsdb.write_text(r'''#!/usr/bin/python3
import json
import os
import sys

arguments = sys.argv[1:]
operation = arguments[1] if arguments[0] == "--more" else arguments[0]
database = arguments[-1]
state = json.loads(os.environ["PVN_TEST_DB_STATE"])
if operation == "db-name":
    print("PVN_Control")
elif operation == "db-is-clustered":
    raise SystemExit(0)
elif operation == "db-local-address":
    print(state["local"])
elif operation == "db-sid":
    print(state["sid"])
elif operation == "db-cid":
    if state["cid"] is None:
        print(f"{database}: cluster ID not yet known", file=sys.stderr)
        raise SystemExit(2)
    print(state["cid"])
elif operation == "show-log":
    sys.stdout.write(state["log"])
else:
    print(f"unexpected ovsdb-tool operation: {operation}", file=sys.stderr)
    raise SystemExit(9)
''')
    fake_pvnctl.write_text(r'''#!/usr/bin/python3
import json
import os
import pathlib
import sys

pathlib.Path(os.environ["PVN_TEST_CONTROL_DB"]).touch()
pathlib.Path(os.environ["PVN_TEST_PVNCTL_LOG"]).write_text(json.dumps(sys.argv[1:]))
''')
    fake_systemctl.write_text(r'''#!/usr/bin/python3
import os
import sys

if sys.argv[1:] != ["is-active", "pvn-central.target"]:
    raise SystemExit(9)
active = os.environ.get("PVN_TEST_CENTRAL_ACTIVE") == "1"
print("active" if active else "inactive")
raise SystemExit(0 if active else 3)
''')
    for executable in (fake_ovsdb, fake_pvnctl, fake_systemctl):
        executable.chmod(0o755)

    helper_source = module["REMOTE_HELPER"].replace(
        'database = "/var/lib/pvn/control-db/pvn_control.db"',
        f"database = {str(database)!r}",
        1,
    ).replace(
        '"/etc/pvn/central/enabled"', repr(str(marker_path)),
    ).replace(
        '"/usr/bin/ovsdb-tool"', repr(str(fake_ovsdb)),
    ).replace(
        '"/usr/sbin/pvnctl"', repr(str(fake_pvnctl)),
    )
    expected_cid = "11111111-2222-3333-4444-555555555555"
    request = {
        "mode": "raft",
        "cluster_id": str(uuid.uuid4()),
        "expected_cluster_id": expected_cid,
        "local": "ssl:192.0.2.12:6646",
        "join": "ssl:192.0.2.11:6646",
    }

    def control_helper(state, *, exists=True, marked=False, active=False):
        if exists:
            database.touch()
        elif database.exists():
            database.unlink()
        if marked:
            marker_path.touch()
        elif marker_path.exists():
            marker_path.unlink()
        if command_log.exists():
            command_log.unlink()
        environment = {
            **os.environ,
            "PATH": str(fake_bin) + os.pathsep + os.environ.get("PATH", ""),
            "PVN_TEST_DB_STATE": json.dumps(state),
            "PVN_TEST_CONTROL_DB": str(database),
            "PVN_TEST_PVNCTL_LOG": str(command_log),
            "PVN_TEST_CENTRAL_ACTIVE": "1" if active else "0",
        }
        return subprocess.run(
            [sys.executable, "-c", helper_source, "init-control"],
            input=json.dumps(request), text=True, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, env=environment, check=False,
        )

    def record_zero(remote="ssl:192.0.2.11:6646"):
        return (
            'record 0:\n name: "PVN_Control\'\n'
            ' local address: "ssl:192.0.2.12:6646"\n'
            ' server_id: stub\n'
            f' remote_addresses: {remote}\n\n'
        )

    legacy = {"cid": None, "local": request["local"], "sid": "legacy-sid",
              "log": record_zero()}
    result = control_helper(legacy)
    assert result.returncode == 0, result.stderr
    legacy_info = json.loads(result.stdout)
    assert legacy_info["cluster_id_pending"] is True
    assert legacy_info["preactivation_join"] is True

    foreign_remote = {**legacy, "log": record_zero("ssl:192.0.2.99:6646")}
    result = control_helper(foreign_remote)
    assert result.returncode != 0 and "exact inactive pre-activation join" in result.stderr
    assert not command_log.exists()

    pinned_stub = {**legacy, "cid": expected_cid, "sid": "pinned-stub-sid"}
    result = control_helper(pinned_stub)
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["cluster_id"] == expected_cid

    joined = {
        **pinned_stub,
        "sid": "joined-sid",
        "log": record_zero() + "record 1:\n term: 1\n\n",
    }
    result = control_helper(joined, marked=True, active=True)
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["preactivation_join"] is False
    for marked, active in ((False, True), (True, False), (False, False)):
        result = control_helper(joined, marked=marked, active=active)
        assert result.returncode != 0
        assert "marker/runtime state" in result.stderr

    result = control_helper(pinned_stub, exists=False)
    assert result.returncode == 0, result.stderr
    invoked = json.loads(command_log.read_text())
    assert "--cid" in invoked and invoked[invoked.index("--cid") + 1] == expected_cid


def drift_topology(found_backend):
    found_backend.found = Discovery(**{
        **found_backend.found.__dict__,
        "topology_sha256": "b" * 64,
    })
    found_backend.discover_hook = None


def planned_ledger(found, **overrides):
    ledger = {
        "version": module["LEDGER_VERSION"],
        "cluster_uuid": str(uuid.uuid4()),
        "snapshot": found.snapshot(),
        "phase": "planned",
        "central_complete": 0,
        "nodes_complete": 0,
        "cert_fingerprints": {},
        "control_db_cluster_id": None,
        "db_cluster_ids": {},
    }
    ledger.update(overrides)
    return ledger


def complete_fingerprints(found, variant="one"):
    fingerprint = lambda value: hashlib.sha256(value.encode()).hexdigest()
    return {
        "ca_certificate_sha256": fingerprint("ca-" + variant),
        "nodes": {
            node.name: {
                "certificate_sha256": fingerprint(
                    "cert-" + variant + "-" + node.name
                ),
                "public_key_sha256": fingerprint("public-key-" + node.name),
            }
            for node in found.nodes
        },
    }


def staged_seed_ledger(found, control_cid, **overrides):
    ledger = planned_ledger(
        found,
        phase="staged",
        cert_fingerprints=complete_fingerprints(found),
        control_db_cluster_id=control_cid,
    )
    ledger.update(overrides)
    return ledger


def partial_central_ledger(found, cids, complete=1, **overrides):
    ledger = planned_ledger(
        found,
        phase=f"central-{complete}",
        central_complete=complete,
        cert_fingerprints=complete_fingerprints(found),
        control_db_cluster_id=cids["PVN_Control"],
        db_cluster_ids=copy.deepcopy(cids),
    )
    ledger.update(overrides)
    return ledger


def complete_ledger(found, cids, **overrides):
    count = len(found.nodes)
    ledger = planned_ledger(
        found,
        phase="complete",
        central_complete=count,
        nodes_complete=count,
        cert_fingerprints=complete_fingerprints(found),
        control_db_cluster_id=cids["PVN_Control"] if found.mode == "raft" else None,
        db_cluster_ids=copy.deepcopy(cids) if found.mode == "raft" else {},
    )
    ledger.update(overrides)
    return ledger


def enter_seed_activation_crash(backend):
    seed = backend.found.nodes[0].name
    backend.control_dbs[seed] = backend.cids["PVN_Control"]
    backend.central = [seed]
    backend.central_markers = [seed]
    backend.restart_pending = {seed: backend.found.nodes[0].package_version}


def enter_partial_join_crash(backend, complete=1, cid_known=False):
    active = backend.found.nodes[:complete]
    backend.control_dbs = {
        node.name: backend.cids["PVN_Control"] for node in active
    }
    backend.central = [node.name for node in active]
    backend.central_markers = [node.name for node in active]
    backend.restart_pending = {
        node.name: node.package_version for node in active
    }
    backend.pending_control_dbs = {backend.found.nodes[complete].name}
    pending = backend.found.nodes[complete]
    backend.pending_control_cids = {
        pending.name: backend.cids["PVN_Control"] if cid_known else None
    }
    backend.pending_control_remotes = {
        pending.name: [f"ssl:{backend.found.nodes[0].control_ip}:6646"]
    }
    backend.pending_control_server_ids = {
        pending.name: f"pending-sid-{pending.name}"
    }


def enter_complete_update(backend):
    names = [node.name for node in backend.found.nodes]
    backend.central = list(names)
    backend.central_markers = list(names)
    backend.nodes = list(names)
    backend.node_markers = list(names)
    backend.node_targets = list(names)
    backend.control_dbs = {
        name: backend.cids["PVN_Control"] for name in names
    }
    backend.restart_pending = {
        node.name: node.package_version for node in backend.found.nodes
    }


lease_temporary = tempfile.TemporaryDirectory()
lease_root = pathlib.Path(lease_temporary.name)
lease_helper = lease_root / "pvn-cluster-lease"
lease_state = lease_root / "mutation.lease"
lease_helper.write_text(r'''#!/usr/bin/python3
import json
import os
import pathlib
import sys

state = pathlib.Path(os.environ["PVN_TEST_CP_LEASE"])
action, domain, token = sys.argv[1:]
if domain != "mutation":
    raise SystemExit("unexpected lease domain")
if action == "acquire":
    owner = json.load(sys.stdin)
    if owner.get("domain") != domain or owner.get("token") != token:
        raise SystemExit("owner mismatch")
    try:
        descriptor = os.open(state, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        raise SystemExit("active lease exists")
    with os.fdopen(descriptor, "w") as stream:
        json.dump(owner, stream)
elif action == "release":
    try:
        owner = json.loads(state.read_text())
    except FileNotFoundError:
        raise SystemExit("no active lease")
    if owner.get("domain") != domain or owner.get("token") != token:
        raise SystemExit("wrong lease owner")
    state.unlink()
else:
    raise SystemExit("unsupported fake lease action")
''')
lease_helper.chmod(0o755)
os.environ["PVN_CP_LEASE_BIN"] = str(lease_helper)
os.environ["PVN_TEST_CP_LEASE"] = str(lease_state)


def canonical_topology_fixture(root, count=3, standalone=False):
    if standalone and count != 1:
        raise AssertionError("standalone fixture must contain exactly one node")
    records = tuple(
        (
            f"pve-{chr(ord('a') + index)}",
            index + 1,
            f"192.0.2.{11 + index}",
            f"198.51.100.{11 + index}",
        )
        for index in range(count)
    )
    cluster_name = "standalone-pve-a" if standalone else "test-cluster"
    members = {
        "nodename": "pve-a",
        "version": 7,
    }
    if not standalone:
        members["nodelist"] = {
            name: {"id": node_id, "online": 1, "ip": management}
            for name, node_id, management, _ in records
        }
        members["cluster"] = {
            "name": "test-cluster", "version": 3, "nodes": count, "quorate": 1,
        }
    membership = {
        "cluster_name": cluster_name,
        "nodes": [
            {"name": name, "node_id": 0 if standalone else node_id,
             "management_ip": management}
            for name, node_id, management, _ in records
        ],
    }
    corosync = None if standalone else {
        "sha256": hashlib.sha256(b"canonical-corosync-conf").hexdigest(),
        "config_version": 3,
        "rings": {
            "1": {
                name: management
                for name, _, management, _ in records
            },
        },
    }
    topology = {
        "schema": 2,
        "phase": "complete",
        "cluster_name": cluster_name,
        "corosync": corosync,
        "membership_snapshot": membership,
        "membership_hash": module["sha256"](
            json.dumps(membership, sort_keys=True, separators=(",", ":")).encode()
        ),
        "nodes": [
            {
                "name": name,
                "node_id": 0 if standalone else node_id,
                "management_ip": management,
                "control_ip": management,
                "geneve_ip": geneve,
                "geneve_interface": "ens4",
                "provider_interface": "ens5",
            }
            for name, node_id, management, geneve in records
        ],
        "guest_mtu": 1300,
        "provider_bridge": "br-provider",
        "physnet": "provider",
        "provider_readiness": {
            "operator_ack": True,
            "ack_phrase": "OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP",
            "live_arbitrary_mac_l2_verified": False,
            "basis": "operator-ack-only",
        },
    }
    members_path = root / "members.json"
    topology_path = root / "topology.json"
    members_path.write_text(json.dumps(members))
    topology_path.write_text(json.dumps(topology))
    args = types.SimpleNamespace(
        members=str(members_path),
        topology_ledger=str(topology_path),
        config=str(root / "config.json"),
        private_dir=str(root / "private"),
        ssh_key=str(root / "ssh-key"),
        nodes_dir=str(root / "nodes"),
        python="python3",
    )
    backend = SystemBackend(args)
    backend._validate_ssh = lambda _records: None
    probes = {
        name: {
            "hostname": name,
            "package_version": "0.1.1",
            "pve_version": "9.2.2",
            "addresses": [
                {"ip": management, "interface": "vmbr0", "mtu": 1500},
                {"ip": geneve, "interface": "ens4", "mtu": 1442},
            ],
            "bridges": {"br-int": True, "br-provider": True},
            "corosync": copy.deepcopy(corosync),
            "corosync_runtime_consistent": True,
        }
        for name, _, management, geneve in records
    }
    backend.test_probes = probes

    def remote(name, action, payload):
        assert action == "probe"
        assert set(payload) == {"bridges", "corosync"}
        if standalone:
            assert payload["corosync"] is None
        else:
            assert payload["corosync"] == {
                "cluster_name": cluster_name,
                "nodes": {
                    node_name: node_id
                    for node_name, node_id, _, _ in records
                },
            }
        return copy.deepcopy(backend.test_probes[name])

    backend._remote = remote
    return backend, topology_path, topology


# Control-plane discovery consumes the exact completed topology ledger contract.
with tempfile.TemporaryDirectory() as temporary:
    backend, topology_path, topology = canonical_topology_fixture(pathlib.Path(temporary))
    found = backend.discover()
    assert found.guest_mtu == 1300
    assert found.nodes[0].geneve_interface == "ens4"
    assert found.nodes[0].provider_interface == "ens5"
    legacy_projection = copy.deepcopy(topology)
    legacy_projection["schema"] = 1
    legacy_projection.pop("corosync")
    assert found.legacy_topology_sha256 == module["sha256"](
        module["canonical_json"](legacy_projection)
    )

    invalid = copy.deepcopy(topology)
    invalid["schema"] = 1
    topology_path.write_text(json.dumps(invalid))
    expect_error(backend.discover, "schema 2")

    invalid = copy.deepcopy(topology)
    invalid["phase"] = "network-staged"
    topology_path.write_text(json.dumps(invalid))
    expect_error(backend.discover, "phase complete")

    invalid = copy.deepcopy(topology)
    invalid["membership_hash"] = "0" * 64
    topology_path.write_text(json.dumps(invalid))
    expect_error(backend.discover, "snapshot/hash")

    invalid = copy.deepcopy(topology)
    invalid["provider_readiness"]["operator_ack"] = False
    topology_path.write_text(json.dumps(invalid))
    expect_error(backend.discover, "operator acknowledgement")

    invalid = copy.deepcopy(topology)
    invalid["guest_mtu"] = 1400
    topology_path.write_text(json.dumps(invalid))
    expect_error(backend.discover, "exceeds effective Geneve MTU")

    malformed_corosync = []
    invalid = copy.deepcopy(topology)
    invalid.pop("corosync")
    malformed_corosync.append((invalid, "exactly sha256"))
    invalid = copy.deepcopy(topology)
    invalid["corosync"]["extra"] = True
    malformed_corosync.append((invalid, "exactly sha256"))
    invalid = copy.deepcopy(topology)
    invalid["corosync"]["sha256"] = "A" * 64
    malformed_corosync.append((invalid, "sha256 is invalid"))
    invalid = copy.deepcopy(topology)
    invalid["corosync"]["config_version"] = True
    malformed_corosync.append((invalid, "config_version is invalid"))
    invalid = copy.deepcopy(topology)
    invalid["corosync"]["rings"] = {}
    malformed_corosync.append((invalid, "1..8 KNET links"))
    invalid = copy.deepcopy(topology)
    invalid["corosync"]["rings"] = {"8": invalid["corosync"]["rings"]["1"]}
    malformed_corosync.append((invalid, "invalid KNET ring key"))
    invalid = copy.deepcopy(topology)
    invalid["corosync"]["rings"]["1"].pop("pve-c")
    malformed_corosync.append((invalid, "node set differs"))
    invalid = copy.deepcopy(topology)
    invalid["corosync"]["rings"]["1"]["pve-c"] = "not-an-address"
    malformed_corosync.append((invalid, "not an IP address"))
    for invalid, message in malformed_corosync:
        topology_path.write_text(json.dumps(invalid))
        expect_error(backend.discover, message)

    topology_path.write_text(json.dumps(topology))
    baseline_probes = copy.deepcopy(backend.test_probes)

    def reject_corosync_probe(mutate, message):
        backend.test_probes = copy.deepcopy(baseline_probes)
        mutate(backend.test_probes["pve-b"])
        expect_error(backend.discover, message)

    reject_corosync_probe(
        lambda probe: probe["corosync"].update(extra=True), "exactly sha256"
    )
    reject_corosync_probe(
        lambda probe: probe.update(corosync_runtime_consistent=False),
        "persisted/runtime state differs",
    )
    reject_corosync_probe(
        lambda probe: probe["corosync"].update(sha256="f" * 64),
        "differs from the topology ledger pin",
    )
    backend.test_probes = baseline_probes


# Discovery accepts exactly one standalone node and every clustered odd size
# from three upward. Clustered one-node and even-sized layouts stop before any
# configuration, database, or service mutation can begin.
with tempfile.TemporaryDirectory() as temporary:
    backend, _, _ = canonical_topology_fixture(
        pathlib.Path(temporary), count=1, standalone=True
    )
    found = backend.discover()
    assert len(found.nodes) == 1 and found.mode == "standalone"
    assert found.nodes[0].node_id == 0
    assert json.loads(backend.members_path.read_text()) == {
        "nodename": "pve-a", "version": 7,
    }
    topology = json.loads(backend.topology_path.read_text())
    invalid = copy.deepcopy(topology)
    invalid.pop("corosync")
    backend.topology_path.write_text(json.dumps(invalid))
    expect_error(backend.discover, "explicit corosync null")
    invalid = copy.deepcopy(topology)
    invalid["corosync"] = {
        "sha256": "a" * 64, "config_version": 1,
        "rings": {"0": {"pve-a": "192.0.2.11"}},
    }
    backend.topology_path.write_text(json.dumps(invalid))
    expect_error(backend.discover, "explicit corosync null")
    backend.topology_path.write_text(json.dumps(topology))
    backend.test_probes["pve-a"]["corosync_runtime_consistent"] = False
    expect_error(backend.discover, "unexpected Corosync state")

for supported_count in (3, 5, 7):
    with tempfile.TemporaryDirectory() as temporary:
        backend, _, _ = canonical_topology_fixture(
            pathlib.Path(temporary), count=supported_count
        )
        found = backend.discover()
        assert len(found.nodes) == supported_count and found.mode == "raft"

for unsupported_count in (1, 2, 4, 6):
    with tempfile.TemporaryDirectory() as temporary:
        backend, _, _ = canonical_topology_fixture(
            pathlib.Path(temporary), count=unsupported_count
        )
        expect_error(backend.discover, "odd clustered node count of at least three")


def complete_system_backend_state(root):
    backend, _, _ = canonical_topology_fixture(root)
    found = backend.discover()
    cids = {name: f"production-cid-{index}" for index, name in enumerate(DATABASES, 1)}
    ledger = complete_ledger(found, cids)
    pki = ledger["cert_fingerprints"]
    reports = {}
    statuses = {}
    probes = {}
    for index, node in enumerate(found.nodes):
        databases = {
            name: {
                "exists": True,
                "name": name,
                "clustered": True,
                "local_address": f"ssl:{node.control_ip}:{port}",
                "cluster_id": cids[name],
                "cluster_id_pending": False,
                "server_id": f"production-sid-{name}-{node.name}",
            }
            for name, port in DATABASES.items()
        }
        probes[node.name] = {
            "cert_hashes": {
                "ca": pki["ca_certificate_sha256"],
                "cert": pki["nodes"][node.name]["certificate_sha256"],
                "public_key": pki["nodes"][node.name]["public_key_sha256"],
            },
            "pki_owner_present": True,
            "seed_ca_present": index == 0,
            "central_marker": True,
            "central_active": True,
            "node_marker": True,
            "node_active": True,
            "node_ready": True,
            "central_restart_pending": node.package_version,
            "databases": databases,
        }
        reports[node.name] = {
            "healthy": True,
            "target_active": True,
            "offline": copy.deepcopy(databases),
            "databases": [
                {
                    "database": name,
                    "healthy": True,
                    "member_count": len(found.nodes),
                    "connected_members": len(found.nodes),
                    "membership_change": False,
                    "cluster_id": cids[name],
                    "server_id": databases[name]["server_id"],
                    "address": databases[name]["local_address"],
                }
                for name in DATABASES
            ],
        }
        statuses[node.name] = {
            "ready": True,
            "target_active": True,
            "service_ready": True,
            "doctor": True,
            "marker": True,
        }
    backend.probes = probes
    backend.central_status = lambda node, _mode: copy.deepcopy(reports[node.name])
    backend.node_status = lambda node: copy.deepcopy(statuses[node.name])
    return backend, found, ledger, reports, statuses


# Exercise the production complete-repin proof, rather than only the fake
# orchestration model. Online rows must identify the same local DB files, and
# a fresh target/service/doctor check is required at proof time.
with tempfile.TemporaryDirectory() as temporary:
    backend, found, ledger, _, _ = complete_system_backend_state(
        pathlib.Path(temporary)
    )
    ControlPlane(backend, None)._assert_package_repin_safe(found, ledger)


def production_complete_repin_rejected(case):
    with tempfile.TemporaryDirectory() as temporary:
        backend, found, ledger, reports, statuses = complete_system_backend_state(
            pathlib.Path(temporary)
        )
        first, second, third = found.nodes
        if case == "pki":
            backend.probes[first.name]["cert_hashes"]["cert"] = "0" * 64
        elif case == "seed-ca":
            backend.probes[second.name]["seed_ca_present"] = True
        elif case == "central-target":
            backend.probes[first.name]["central_active"] = False
        elif case == "node-marker":
            backend.probes[second.name]["node_marker"] = False
        elif case == "restart-marker":
            backend.probes[third.name]["central_restart_pending"] = None
        elif case == "offline-cid":
            backend.probes[first.name]["databases"]["OVN_Northbound"][
                "cluster_id"
            ] = "foreign-cid"
        elif case == "offline-sid":
            backend.probes[first.name]["databases"]["OVN_Southbound"][
                "server_id"
            ] = ""
        elif case == "member-count":
            reports[second.name]["databases"][0]["member_count"] = 2
        elif case == "online-cid":
            reports[second.name]["databases"][1]["cluster_id"] = "foreign-cid"
        elif case == "online-sid":
            reports[third.name]["databases"][2]["server_id"] = "foreign-sid"
        elif case == "doctor":
            statuses[first.name]["doctor"] = False
            statuses[first.name]["ready"] = False
        elif case == "live-node-target":
            statuses[second.name]["target_active"] = False
            statuses[second.name]["ready"] = False
        else:
            raise AssertionError(case)
        control = ControlPlane(backend, None)
        expect_error(
            lambda: control._assert_package_repin_safe(found, ledger),
            "repin refused",
        )


for production_case in (
    "pki", "seed-ca", "central-target", "node-marker", "restart-marker",
    "offline-cid", "offline-sid", "member-count", "online-cid", "online-sid",
    "doctor", "live-node-target",
):
    production_complete_repin_rejected(production_case)


# A wedged doctor command fails closed inside the remote helper instead of
# holding a read-only plan or the cluster mutation lease indefinitely.
assert re.search(
    r'\["/usr/sbin/pvnctl", "doctor"\], check=False, timeout=30',
    module["REMOTE_HELPER"],
)
remote_tree = ast.parse(module["REMOTE_HELPER"])
remote_run_nodes = [
    node for node in remote_tree.body
    if isinstance(node, ast.FunctionDef) and node.name in {"fail", "run"}
]

class TimeoutSubprocess:
    PIPE = subprocess.PIPE
    TimeoutExpired = subprocess.TimeoutExpired

    @staticmethod
    def run(arguments, **_kwargs):
        raise subprocess.TimeoutExpired(arguments, 30)

timeout_namespace = {
    "os": os,
    "subprocess": TimeoutSubprocess,
    "sys": sys,
}
exec(compile(ast.Module(body=remote_run_nodes, type_ignores=[]),
             "remote-timeout", "exec"), timeout_namespace)
timeout_error = io.StringIO()
with contextlib.redirect_stderr(timeout_error):
    try:
        timeout_namespace["run"](["/usr/sbin/pvnctl", "doctor"], timeout=30)
    except SystemExit:
        pass
    else:
        raise AssertionError("remote helper accepted a timed-out doctor command")
assert "timed out after 30 seconds" in timeout_error.getvalue()


# The real backend's staged-recovery probe accepts only the sole active seed,
# pinned live PKI, exact offline database identities, no transport target, and
# no database/activation state on either non-seed.
with tempfile.TemporaryDirectory() as temporary:
    backend, _, _ = canonical_topology_fixture(pathlib.Path(temporary))
    found = backend.discover()
    cids = {name: f"probe-cid-{index}" for index, name in enumerate(DATABASES, 1)}
    ledger = staged_seed_ledger(found, cids["PVN_Control"])
    seed = found.nodes[0]
    seed_databases = {
        name: {
            "exists": True,
            "name": name,
            "clustered": True,
            "local_address": f"ssl:{seed.control_ip}:{port}",
            "cluster_id": cids[name],
            "server_id": f"probe-sid-{name}",
        }
        for name, port in DATABASES.items()
    }
    for node in found.nodes:
        pins = ledger["cert_fingerprints"]["nodes"][node.name]
        backend.probes[node.name].update({
            "cert_hashes": {
                "ca": ledger["cert_fingerprints"]["ca_certificate_sha256"],
                "cert": pins["certificate_sha256"],
                "public_key": pins["public_key_sha256"],
            },
            "pki_owner_present": True,
            "seed_ca_present": node == seed,
            "central_marker": node == seed,
            "central_active": node == seed,
            "node_marker": False,
            "node_active": False,
            "node_ready": False,
            "central_restart_pending": node.package_version if node == seed else None,
            "databases": copy.deepcopy(seed_databases) if node == seed else {
                name: {"exists": False} for name in DATABASES
            },
        })
    report = {
        "healthy": True,
        "target_active": True,
        "databases": [
            {
                "database": name,
                "healthy": True,
                "member_count": 1,
                "connected_members": 1,
                "membership_change": False,
                "cluster_id": cids[name],
                "server_id": f"probe-sid-{name}",
                "address": f"ssl:{seed.control_ip}:{port}",
            }
            for name, port in DATABASES.items()
        ],
        "offline": copy.deepcopy(seed_databases),
    }
    baseline_probes = copy.deepcopy(backend.probes)
    baseline_report = copy.deepcopy(report)
    backend.central_status = lambda _node, _mode: copy.deepcopy(report)
    accepted = backend.assert_staged_seed_package_repin_safe(found, ledger)
    accepted_cids = ControlPlane._verify_reports(found, [(seed, accepted)], 1, {})
    assert accepted_cids == cids

    def expect_real_probe_rejected(mutate_probe=None, mutate_report=None):
        backend.probes = copy.deepcopy(baseline_probes)
        report.clear()
        report.update(copy.deepcopy(baseline_report))
        if mutate_probe is not None:
            mutate_probe(backend.probes)
        if mutate_report is not None:
            mutate_report(report)
        expect_error(
            lambda: backend.assert_staged_seed_package_repin_safe(found, ledger),
            "repin refused",
        )

    probe_mutations = [
        lambda probes: probes[seed.name].update(central_restart_pending=None),
        lambda probes: probes[seed.name].update(central_restart_pending="0.0.0"),
        lambda probes: probes[seed.name]["cert_hashes"].update(public_key="0" * 64),
        lambda probes: probes[seed.name].update(central_marker=False),
        lambda probes: probes[seed.name].update(central_active=False),
        lambda probes: probes[seed.name].update(node_active=True),
        lambda probes: probes[seed.name].update(node_marker=True),
        lambda probes: probes[seed.name].update(node_ready=True),
        lambda probes: probes[found.nodes[1].name].update(central_marker=True),
        lambda probes: probes[found.nodes[1].name].update(
            central_restart_pending=found.nodes[1].package_version
        ),
        lambda probes: probes[found.nodes[1].name]["databases"]["PVN_Control"].update(
            exists=True
        ),
        lambda probes: probes[seed.name]["databases"]["OVN_Northbound"].update(
            name="Wrong_Northbound"
        ),
        lambda probes: probes[seed.name]["databases"]["OVN_Southbound"].update(
            local_address="ssl:192.0.2.99:6644"
        ),
        lambda probes: probes[seed.name]["databases"]["PVN_Control"].update(
            cluster_id="foreign-control-cid"
        ),
    ]
    for probe_mutation in probe_mutations:
        expect_real_probe_rejected(mutate_probe=probe_mutation)

    report_mutations = [
        lambda current: current["offline"]["OVN_Northbound"].update(
            name="Wrong_Northbound"
        ),
        lambda current: current["offline"]["OVN_Southbound"].update(
            local_address="ssl:192.0.2.99:6644"
        ),
        lambda current: current["offline"]["PVN_Control"].update(
            cluster_id="foreign-control-cid"
        ),
    ]
    for report_mutation in report_mutations:
        expect_real_probe_rejected(mutate_report=report_mutation)


# A central-N package repin proves each completed voter against the durable
# N/N CID set and accepts exactly one inactive record-0 Control join stub.
# Both legacy unknown-CID and new --cid-pinned stubs use the same seed-remote
# and unique-server-ID gate.
with tempfile.TemporaryDirectory() as temporary:
    backend, _, _ = canonical_topology_fixture(pathlib.Path(temporary))
    found = backend.discover()
    cids = {name: f"partial-cid-{index}" for index, name in enumerate(DATABASES, 1)}
    ledger = partial_central_ledger(found, cids)
    seed, pending_node = found.nodes[:2]
    active_databases = {
        name: {
            "exists": True,
            "name": name,
            "clustered": True,
            "local_address": f"ssl:{seed.control_ip}:{port}",
            "cluster_id": cids[name],
            "cluster_id_pending": False,
            "server_id": f"active-sid-{name}",
        }
        for name, port in DATABASES.items()
    }
    pending_control = {
        "exists": True,
        "name": "PVN_Control",
        "clustered": True,
        "local_address": f"ssl:{pending_node.control_ip}:6646",
        "cluster_id": None,
        "cluster_id_pending": True,
        "preactivation_join": True,
        "remote_addresses": [f"ssl:{seed.control_ip}:6646"],
        "server_id": "pending-control-sid",
    }
    for index, node in enumerate(found.nodes):
        pins = ledger["cert_fingerprints"]["nodes"][node.name]
        databases = {name: {"exists": False} for name in DATABASES}
        if index == 0:
            databases = copy.deepcopy(active_databases)
        elif index == 1:
            databases["PVN_Control"] = copy.deepcopy(pending_control)
        backend.probes[node.name].update({
            "cert_hashes": {
                "ca": ledger["cert_fingerprints"]["ca_certificate_sha256"],
                "cert": pins["certificate_sha256"],
                "public_key": pins["public_key_sha256"],
            },
            "pki_owner_present": True,
            "seed_ca_present": index == 0,
            "central_marker": index == 0,
            "central_active": index == 0,
            "node_marker": False,
            "node_active": False,
            "node_ready": False,
            "central_restart_pending": node.package_version if index == 0 else None,
            "databases": databases,
        })
    report = {
        "healthy": True,
        "target_active": True,
        "databases": [
            {
                "database": name,
                "healthy": True,
                "member_count": 1,
                "connected_members": 1,
                "membership_change": False,
                "cluster_id": cids[name],
                "server_id": f"active-sid-{name}",
                "address": f"ssl:{seed.control_ip}:{port}",
            }
            for name, port in DATABASES.items()
        ],
        "offline": copy.deepcopy(active_databases),
    }
    backend.central_status = lambda _node, _mode: copy.deepcopy(report)
    baseline = copy.deepcopy(backend.probes)

    reports = backend.assert_partial_central_package_repin_safe(found, ledger)
    assert ControlPlane._verify_reports(found, reports, 1, cids) == cids
    backend.probes[pending_node.name]["databases"]["PVN_Control"].update({
        "cluster_id": cids["PVN_Control"], "cluster_id_pending": False,
    })
    reports = backend.assert_partial_central_package_repin_safe(found, ledger)
    assert ControlPlane._verify_reports(found, reports, 1, cids) == cids

    def reject_partial_probe(mutate):
        backend.probes = copy.deepcopy(baseline)
        mutate(backend.probes)
        expect_error(
            lambda: backend.assert_partial_central_package_repin_safe(found, ledger),
            "repin refused",
        )

    partial_mutations = (
        lambda probes: probes[pending_node.name]["databases"]["PVN_Control"].update(
            remote_addresses=["ssl:192.0.2.99:6646"]
        ),
        lambda probes: probes[pending_node.name]["databases"]["PVN_Control"].update(
            remote_addresses=[]
        ),
        lambda probes: probes[pending_node.name]["databases"]["PVN_Control"].update(
            remote_addresses=[f"ssl:{seed.control_ip}:6646", "ssl:192.0.2.99:6646"]
        ),
        lambda probes: probes[pending_node.name]["databases"]["PVN_Control"].update(
            server_id="active-sid-PVN_Control"
        ),
        lambda probes: probes[pending_node.name]["databases"]["PVN_Control"].update(
            cluster_id="foreign-cid", cluster_id_pending=False
        ),
        lambda probes: probes[pending_node.name]["databases"]["OVN_Northbound"].update(
            exists=True
        ),
        lambda probes: probes[seed.name].update(central_restart_pending=None),
        lambda probes: probes[found.nodes[2].name]["databases"]["PVN_Control"].update(
            exists=True
        ),
    )
    for partial_mutation in partial_mutations:
        reject_partial_probe(partial_mutation)


# Legacy pmxcfs key material is never adopted or copied automatically.
with tempfile.TemporaryDirectory() as temporary:
    backend, _, _ = canonical_topology_fixture(pathlib.Path(temporary))
    found = backend.discover()
    (backend.private_dir / "pki").mkdir(parents=True)
    expect_error(
        lambda: backend.prepare_pki(str(uuid.uuid4()), found.nodes, {}),
        "legacy shared PKI",
    )


# Native PVE SSH trust files and the SSH process are both tightly isolated.
with tempfile.TemporaryDirectory() as temporary:
    root = pathlib.Path(temporary)
    backend, _, _ = canonical_topology_fixture(root)
    backend.local_name = "pve-a"
    backend.node_addresses = {
        "pve-a": "192.0.2.11", "pve-b": "192.0.2.12", "pve-c": "192.0.2.13",
    }
    backend.ssh_key.write_text("test-private-identity\n")
    backend.ssh_key.chmod(0o600)
    records = [
        (1, "pve-a", "192.0.2.11", "198.51.100.11", "ens4", "ens5", "br-provider"),
        (2, "pve-b", "192.0.2.12", "198.51.100.12", "ens4", "ens5", "br-provider"),
        (3, "pve-c", "192.0.2.13", "198.51.100.13", "ens4", "ens5", "br-provider"),
    ]

    def write_known(name, data=None):
        directory = backend.nodes_dir / name
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / "ssh_known_hosts"
        if os.path.lexists(path):
            path.unlink()
        path.write_bytes(data if data is not None else (
            f"# PVE pin\n{name} ssh-ed25519 AAAAC3NzaTest{name}\n".encode()
        ))
        path.chmod(0o640)
        return path

    pve_b_known = write_known("pve-b")
    write_known("pve-c")
    SystemBackend._validate_ssh(backend, records)

    real_fstat = module["os"].fstat

    def writable_fstat(descriptor):
        opened = real_fstat(descriptor)
        return types.SimpleNamespace(
            st_dev=opened.st_dev, st_ino=opened.st_ino, st_mode=opened.st_mode | 0o022,
            st_nlink=opened.st_nlink, st_uid=opened.st_uid, st_size=opened.st_size,
        )

    module["os"].fstat = writable_fstat
    try:
        expect_error(
            lambda: SystemBackend._validate_ssh(backend, records),
            "opened native known_hosts",
        )
    finally:
        module["os"].fstat = real_fstat

    pve_b_known.chmod(0o662)
    expect_error(lambda: SystemBackend._validate_ssh(backend, records), "owner/link/mode")
    pve_b_known.chmod(0o640)
    hardlink = root / "known-hosts-hardlink"
    os.link(pve_b_known, hardlink)
    expect_error(lambda: SystemBackend._validate_ssh(backend, records), "owner/link/mode")
    hardlink.unlink()
    pve_b_known = write_known("pve-b", b"wrong ssh-ed25519 AAAAC3NzaWrong\n")
    expect_error(lambda: SystemBackend._validate_ssh(backend, records), "exclusively pin")
    pve_b_known = write_known("pve-b", b"x" * (MAX_KNOWN_HOSTS_BYTES + 1))
    expect_error(lambda: SystemBackend._validate_ssh(backend, records), "exceeds 1 MiB")
    real_pin = root / "real-known-hosts"
    real_pin.write_text("pve-b ssh-ed25519 AAAAC3NzaReal\n")
    real_pin.chmod(0o640)
    pve_b_known.unlink()
    pve_b_known.symlink_to(real_pin)
    expect_error(lambda: SystemBackend._validate_ssh(backend, records), "non-symlink")
    pve_b_known = write_known("pve-b")
    SystemBackend._validate_ssh(backend, records)

    captured = []
    real_run = module["subprocess"].run

    def capture_run(command, **_kwargs):
        captured.append(command)
        return types.SimpleNamespace(returncode=0, stdout="{}\n", stderr="")

    module["subprocess"].run = capture_run
    try:
        assert SystemBackend._remote(backend, "pve-b", "node-status", {}) == {}
    finally:
        module["subprocess"].run = real_run
    command = captured[0]
    assert command[:6] == ["ssh", "-F", "/dev/null", "-e", "none", "-i"]
    command_text = " ".join(command)
    for option in (
        "BatchMode=yes", "PasswordAuthentication=no", "KbdInteractiveAuthentication=no",
        "PubkeyAuthentication=yes", "PreferredAuthentications=publickey",
        "IdentitiesOnly=yes", "NumberOfPasswordPrompts=0", "StrictHostKeyChecking=yes",
        "CheckHostIP=no", "VerifyHostKeyDNS=no", "UpdateHostKeys=no",
        "GlobalKnownHostsFile=/dev/null", "ConnectionAttempts=1",
        f"UserKnownHostsFile={pve_b_known}", "HostKeyAlias=pve-b",
    ):
        assert option in command_text, option


# Exercise the real remote PKI helper in isolated roots. Private keys never
# enter a request, response, shared ledger, or generic staged-file payload.
with tempfile.TemporaryDirectory() as temporary:
    root = pathlib.Path(temporary)
    cluster_id = str(uuid.uuid4())
    pvnctl = script_path.parents[2] / "bin" / "pvnctl"
    assert pvnctl.is_file() and os.access(pvnctl, os.X_OK), "build bin/pvnctl before package tests"
    config_path = root / "config.json"
    config_path.write_text(json.dumps(ControlPlane._render_config(discovery(), cluster_id)))
    fake_bin = root / "bin"
    fake_bin.mkdir(mode=0o700)
    fake_hostname = fake_bin / "hostname"
    fake_hostname.write_text('#!/bin/sh\nprintf "%s\\n" "$PVN_TEST_HOSTNAME"\n')
    fake_hostname.chmod(0o755)
    transcripts = []

    node_roots = {}
    ca_dirs = {}
    stage_roots = {}
    helper_sources = {}
    for node in discovery().nodes:
        node_root = root / "nodes" / node.name
        node_root.mkdir(mode=0o700, parents=True)
        pki_dir = node_root / "pki"
        stage_root = node_root / "etc-pvn"
        ca_parent = root / "ca-roots" / node.name
        ca_parent.mkdir(mode=0o700, parents=True)
        ca_dir = ca_parent / "pvn-ca"
        source = module["REMOTE_HELPER"]
        replacements = {
            'PKI_DIR = pathlib.Path("/etc/pvn/pki")': f"PKI_DIR = pathlib.Path({str(pki_dir)!r})",
            'CA_DIR = pathlib.Path("/var/lib/pvn-ca")': f"CA_DIR = pathlib.Path({str(ca_dir)!r})",
            'PVN_CONFIG = "/etc/pve/pvn/config.json"': f"PVN_CONFIG = {str(config_path)!r}",
            'PVNCTL_BIN = "/usr/sbin/pvnctl"': f"PVNCTL_BIN = {str(pvnctl)!r}",
            'PVN_GROUP = "pvn"': 'PVN_GROUP = "root"',
            'pathlib.Path("/etc/pvn") not in path.parents':
                f"pathlib.Path({str(stage_root)!r}) not in path.parents",
        }
        for old, new in replacements.items():
            assert old in source
            source = source.replace(old, new, 1)
        node_roots[node.name] = node_root
        ca_dirs[node.name] = ca_dir
        stage_roots[node.name] = stage_root
        helper_sources[node.name] = source

    def helper_call(node_name, action, request, succeeds=True, source=None):
        raw = json.dumps(request, sort_keys=True)
        command = [sys.executable, "-c", source or helper_sources[node_name], action]
        environment = {
            **os.environ,
            "PATH": str(fake_bin) + os.pathsep + os.environ.get("PATH", ""),
            "PVN_TEST_HOSTNAME": node_name,
        }
        result = subprocess.run(command, input=raw, text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, env=environment, check=False)
        transcript = "\n".join((" ".join(command), raw, result.stdout, result.stderr))
        transcripts.append(transcript)
        assert "-----BEGIN PRIVATE KEY-----" not in transcript
        if not succeeds:
            assert result.returncode != 0, (action, result.stdout, result.stderr)
            return (result.stderr or result.stdout).strip()
        assert result.returncode == 0, (action, result.stdout, result.stderr)
        return json.loads(result.stdout)

    # A fresh package install owns one byte-exact inert default. The first
    # control-plane stage must adopt it without weakening drift rejection for
    # any operator-modified content.
    rendered = ControlPlane(None, None)._render_node_files(
        discovery(), discovery().nodes[0], discovery().nodes[0]
    )
    rendered_host = next(item for item in rendered if item["path"] == "/etc/pvn/ovn-host.env")
    package_default = (script_path.parent.parent / "examples" / "ovn-host.env").read_bytes()
    assert base64.b64decode(rendered_host["content"]) == package_default
    staged_host = stage_roots["pve-a"] / "ovn-host.env"
    staged_host.parent.mkdir(mode=0o750, parents=True)
    staged_host.write_bytes(package_default)
    staged_host.chmod(0o640)
    stage_request = {**rendered_host, "path": str(staged_host)}
    assert helper_call("pve-a", "stage", {"files": [stage_request]}) == {"staged": True}
    assert staged_host.read_bytes() == package_default
    staged_host.write_text("operator-modified\n")
    stage_error = helper_call(
        "pve-a", "stage", {"files": [stage_request]}, succeeds=False,
    )
    assert "staged file drift" in stage_error

    requests = []
    public_keys = {}
    for node in discovery().nodes:
        identity = {
            "cluster_id": cluster_id,
            "seed_name": "pve-a",
            "name": node.name,
            "addresses": [node.control_ip, node.geneve_ip],
            "expected_public_key_sha256": None,
        }
        response = helper_call(node.name, "pki-csr", identity)
        public_keys[node.name] = response["public_key_sha256"]
        requests.append({
            "name": node.name,
            "addresses": identity["addresses"],
            "csr": response["csr"],
            "public_key_sha256": response["public_key_sha256"],
            "expected_certificate_sha256": None,
        })
    assert len(set(public_keys.values())) == 3
    signed = helper_call("pve-a", "pki-sign", {
        "cluster_id": cluster_id,
        "seed_name": "pve-a",
        "expected_ca_sha256": None,
        "requests": requests,
    })
    assert set(signed["nodes"]) == set(public_keys)
    assert "-----BEGIN PRIVATE KEY-----" not in json.dumps(signed)
    ca_key = ca_dirs["pve-a"] / "ca-key.pem"
    ca_key_stat = ca_key.stat()
    assert (ca_key_stat.st_uid, ca_key_stat.st_gid,
            ca_key_stat.st_mode & 0o777, ca_key_stat.st_nlink) == (0, 0, 0o600, 1)
    assert all(not os.path.lexists(ca_dirs[name]) for name in ("pve-b", "pve-c"))
    for node in discovery().nodes:
        key = node_roots[node.name] / "pki" / "node-key.pem"
        key_stat = key.stat()
        assert (key_stat.st_uid, key_stat.st_gid,
                key_stat.st_mode & 0o777, key_stat.st_nlink) == (0, 0, 0o640, 1)
        assert not (node_roots[node.name] / "pki" / "node.pem").exists()
        certificate = signed["nodes"][node.name]
        installed = helper_call(node.name, "pki-install", {
            "name": node.name,
            "addresses": [node.control_ip, node.geneve_ip],
            "ca": signed["ca"],
            "ca_sha256": signed["ca_sha256"],
            **certificate,
        })
        assert installed["public_key_sha256"] == public_keys[node.name]

    # Exact pinned rerun is idempotent.
    pinned_requests = []
    for node in discovery().nodes:
        response = helper_call(node.name, "pki-csr", {
            "cluster_id": cluster_id,
            "seed_name": "pve-a",
            "name": node.name,
            "addresses": [node.control_ip, node.geneve_ip],
            "expected_public_key_sha256": public_keys[node.name],
        })
        pinned_requests.append({
            "name": node.name,
            "addresses": [node.control_ip, node.geneve_ip],
            "csr": response["csr"],
            "public_key_sha256": public_keys[node.name],
            "expected_certificate_sha256": signed["nodes"][node.name]["certificate_sha256"],
        })
    rerun = helper_call("pve-a", "pki-sign", {
        "cluster_id": cluster_id,
        "seed_name": "pve-a",
        "expected_ca_sha256": signed["ca_sha256"],
        "requests": pinned_requests,
    })
    assert rerun["ca_sha256"] == signed["ca_sha256"]
    assert {name: item["certificate_sha256"] for name, item in rerun["nodes"].items()} == {
        name: item["certificate_sha256"] for name, item in signed["nodes"].items()
    }

    # Identity, signature, seed-only, and hardlink guards fail closed.
    wrong_pin = copy.deepcopy({
        "cluster_id": cluster_id, "seed_name": "pve-a", "name": "pve-b",
        "addresses": ["192.0.2.12", "198.51.100.12"],
        "expected_public_key_sha256": "0" * 64,
    })
    assert "ledger pin" in helper_call("pve-b", "pki-csr", wrong_pin, False)
    altered = copy.deepcopy(pinned_requests[0])
    damaged = bytearray(base64.b64decode(altered["csr"]))
    damaged[-24] ^= 1
    altered["csr"] = base64.b64encode(damaged).decode()
    assert "CSR" in helper_call("pve-a", "pki-sign", {
        "cluster_id": cluster_id, "seed_name": "pve-a",
        "expected_ca_sha256": signed["ca_sha256"], "requests": [altered],
    }, False)
    assert "pinned seed" in helper_call("pve-b", "pki-sign", {
        "cluster_id": cluster_id, "seed_name": "pve-a",
        "expected_ca_sha256": signed["ca_sha256"], "requests": pinned_requests,
    }, False, helper_sources["pve-a"])
    ca_dirs["pve-b"].mkdir(mode=0o700)
    assert "forbidden seed CA" in helper_call("pve-b", "pki-csr", wrong_pin, False)
    os.link(ca_key, ca_dirs["pve-a"] / "ca-key-hardlink")
    assert "hard link" in helper_call("pve-a", "pki-sign", {
        "cluster_id": cluster_id, "seed_name": "pve-a",
        "expected_ca_sha256": signed["ca_sha256"], "requests": pinned_requests,
    }, False)
    os.link(node_roots["pve-a"] / "pki" / "owner.json",
            node_roots["pve-a"] / "node-owner-hardlink")
    assert "hard link" in helper_call("pve-a", "pki-csr", {
        "cluster_id": cluster_id, "seed_name": "pve-a", "name": "pve-a",
        "addresses": ["192.0.2.11", "198.51.100.11"],
        "expected_public_key_sha256": public_keys["pve-a"],
    }, False)
    pve_b_certificate = signed["nodes"]["pve-b"]
    os.link(node_roots["pve-b"] / "pki" / "node.pem",
            node_roots["pve-b"] / "node-certificate-hardlink")
    assert "hard link" in helper_call("pve-b", "pki-install", {
        "name": "pve-b", "addresses": ["192.0.2.12", "198.51.100.12"],
        "ca": signed["ca"], "ca_sha256": signed["ca_sha256"],
        **pve_b_certificate,
    }, False)
    extra_link = node_roots["pve-c"] / "node-key-hardlink"
    os.link(node_roots["pve-c"] / "pki" / "node-key.pem", extra_link)
    assert "hard link" in helper_call("pve-c", "pki-csr", {
        "cluster_id": cluster_id, "seed_name": "pve-a", "name": "pve-c",
        "addresses": ["192.0.2.13", "198.51.100.13"],
        "expected_public_key_sha256": public_keys["pve-c"],
    }, False)

    # Owner-intent-only crash states resume without adopting unowned keys.
    crash_node_root = root / "nodes" / "pve-d"
    crash_node_root.mkdir(mode=0o700)
    crash_pki = crash_node_root / "pki"
    crash_pki.mkdir(mode=0o750)
    crash_owner = {
        "schema": 1, "cluster_uuid": cluster_id, "name": "pve-d",
        "addresses": ["192.0.2.14", "198.51.100.14"],
    }
    (crash_pki / "owner.json").write_text(json.dumps(crash_owner) + "\n")
    (crash_pki / "owner.json").chmod(0o600)
    crash_ca_parent = root / "ca-roots" / "pve-d"
    crash_ca_parent.mkdir(mode=0o700)
    crash_source = helper_sources["pve-a"].replace(
        f"PKI_DIR = pathlib.Path({str(node_roots['pve-a'] / 'pki')!r})",
        f"PKI_DIR = pathlib.Path({str(crash_pki)!r})",
    ).replace(
        f"CA_DIR = pathlib.Path({str(ca_dirs['pve-a'])!r})",
        f"CA_DIR = pathlib.Path({str(crash_ca_parent / 'pvn-ca')!r})",
    )
    resumed = helper_call("pve-d", "pki-csr", {
        "cluster_id": cluster_id, "seed_name": "pve-a", "name": "pve-d",
        "addresses": crash_owner["addresses"], "expected_public_key_sha256": None,
    }, source=crash_source)
    assert re.fullmatch(r"[0-9a-f]{64}", resumed["public_key_sha256"])

    crash_seed_parent = root / "crash-seed"
    crash_seed_parent.mkdir(mode=0o700)
    crash_seed_ca = crash_seed_parent / "pvn-ca"
    crash_seed_ca.mkdir(mode=0o700)
    (crash_seed_ca / "owner.json").write_text(json.dumps({
        "schema": 1, "cluster_uuid": cluster_id,
    }) + "\n")
    (crash_seed_ca / "owner.json").chmod(0o600)
    crash_issued = crash_seed_ca / "issued"
    crash_issued.mkdir(mode=0o700)
    issued_owner = {
        "schema": 1,
        "cluster_uuid": cluster_id,
        "name": "pve-a",
        "addresses": ["192.0.2.11", "198.51.100.11"],
        "public_key_sha256": public_keys["pve-a"],
    }
    (crash_issued / "pve-a.json").write_text(json.dumps(issued_owner) + "\n")
    (crash_issued / "pve-a.json").chmod(0o600)
    crash_seed_source = helper_sources["pve-a"].replace(
        f"CA_DIR = pathlib.Path({str(ca_dirs['pve-a'])!r})",
        f"CA_DIR = pathlib.Path({str(crash_seed_ca)!r})",
    )
    crash_request = copy.deepcopy(pinned_requests[0])
    crash_request["expected_certificate_sha256"] = None
    crash_signed = helper_call("pve-a", "pki-sign", {
        "cluster_id": cluster_id, "seed_name": "pve-a",
        "expected_ca_sha256": None, "requests": [crash_request],
    }, source=crash_seed_source)
    assert crash_signed["nodes"]["pve-a"]["public_key_sha256"] == public_keys["pve-a"]
    crash_ca_key_stat = (crash_seed_ca / "ca-key.pem").stat()
    assert (crash_ca_key_stat.st_mode & 0o777, crash_ca_key_stat.st_nlink) == (0o600, 1)


with tempfile.TemporaryDirectory() as temporary:
    root = pathlib.Path(temporary)
    store = LedgerStore(root / "private")
    backend = FakeBackend(discovery())
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    backend.pki_pin_check = lambda: bool(store.load()["cert_fingerprints"])

    # A plan is read-only and exposes the exact apply confirmation.
    plan = control.plan()
    assert plan["read_only"] and plan["confirmation"] == "test-cluster"
    assert not store.private_dir.exists()
    assert backend.log == []

    expect_error(lambda: control.apply("wrong"), "must exactly match")
    assert not store.private_dir.exists() and backend.log == []

    result = control.apply("test-cluster")
    assert result["complete"] and result["nodes"] == 3
    ordered = [entry for entry in backend.log if entry.startswith(("init:", "central:", "node:"))]
    assert ordered == [
        "init:pve-a", "central:pve-a",
        "init:pve-b", "central:pve-b",
        "init:pve-c", "central:pve-c",
        "node:pve-a", "node:pve-b", "node:pve-c",
    ], ordered
    for name, staged in backend.staged.items():
        decoded = json.loads(staged)
        assert all(not item["path"].startswith("/etc/pvn/pki/") for item in decoded)
        listener = next(item for item in decoded if item["path"].endswith("ovn-listeners.env"))
        text = base64.b64decode(listener["content"]).decode()
        expected_ip = next(node.control_ip for node in backend.found.nodes if node.name == name)
        assert f"PVN_OVN_LISTEN={expected_ip}\n" in text
    ledger = store.load()
    assert ledger["phase"] == "complete"
    assert ledger["db_cluster_ids"] == backend.cids
    assert ledger["control_db_cluster_id"] == backend.cids["PVN_Control"]
    assert ledger["snapshot"]["nodes"][0]["geneve_ip"] == "198.51.100.11"

    # Exact rerun verifies live state and performs no new mutation.
    before = list(backend.log)
    control.apply("test-cluster")
    assert backend.log == before

    # pmxcfs increments /etc/pve/.members "version" for a new membership
    # view even when the exact durable nodelist/config/topology is unchanged.
    # That epoch is ignored across runs, is not rewritten into the ledger, and
    # does not masquerade as a package repin.
    old_epoch = store.load()["snapshot"]["members_version"]
    backend.found = Discovery(**{
        **backend.found.__dict__, "members_version": old_epoch + 2,
    })
    epoch_plan = control.plan()
    assert epoch_plan["read_only"] and not epoch_plan["package_repin_required"]
    control.apply("test-cluster")
    assert store.load()["snapshot"]["members_version"] == old_epoch
    assert backend.log == before

    # Frozen package/membership/topology drift fails before mutation.
    original = backend.found
    changed_nodes = list(original.nodes)
    changed_nodes[0] = Node(**{**changed_nodes[0].__dict__, "package_version": "0.1.2"})
    backend.found = Discovery(**{**original.__dict__, "nodes": tuple(changed_nodes)})
    expect_error(control.plan, "drift")
    backend.found = original
    backend.pki_variant = "two"
    expect_error(lambda: control.apply("test-cluster"), "PKI fingerprint drift")


# A five-node cluster bootstraps every PVE member as a voter in deterministic
# order. Each join points at the seed, every intermediate N/N membership gate
# passes, and transport activation waits for all five central voters.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    backend = FakeBackend(discovery(5))
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    result = control.apply("test-cluster")
    assert result["complete"] and result["nodes"] == 5
    ordered = [entry for entry in backend.log if entry.startswith(("init:", "central:", "node:"))]
    assert ordered == [
        "init:pve-a", "central:pve-a",
        "init:pve-b", "central:pve-b",
        "init:pve-c", "central:pve-c",
        "init:pve-d", "central:pve-d",
        "init:pve-e", "central:pve-e",
        "node:pve-a", "node:pve-b", "node:pve-c", "node:pve-d", "node:pve-e",
    ], ordered
    for index, node in enumerate(backend.found.nodes):
        decoded = json.loads(backend.staged[node.name])
        node_env = base64.b64decode(next(
            item["content"] for item in decoded if item["path"].endswith("node.env")
        )).decode()
        central_env = base64.b64decode(next(
            item["content"] for item in decoded
            if item["path"].endswith("ovn-central.env")
        )).decode()
        assert "PVN_NODE_ROLES=compute,gateway,central\n" in node_env
        if index == 0:
            assert "PVN_OVN_BOOTSTRAP=seed\n" in central_env
        else:
            assert "PVN_OVN_BOOTSTRAP=join\n" in central_env
            assert "--db-nb-cluster-remote-addr=192.0.2.11 " in central_env
            assert "--db-sb-cluster-remote-addr=192.0.2.11 " in central_env
    ledger = store.load()
    assert ledger["phase"] == "complete"
    assert ledger["central_complete"] == 5 and ledger["nodes_complete"] == 5


# A wholly unactivated planned ledger may follow one uniform forward package
# upgrade. Plan remains read-only; apply performs the atomic snapshot repin
# under the cluster-global mutation lease before any staging or activation.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    pinned = discovery(package="0.1.1")
    store.create(planned_ledger(pinned))
    live = discovery(package="0.1.2")
    live = Discovery(**{**live.__dict__, "members_version": live.members_version + 4})
    backend = FakeBackend(live)
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    before = copy.deepcopy(store.load())
    plan = control.plan()
    assert plan["read_only"] and plan["package_repin_required"] is True
    assert store.load() == before
    result = control.apply("test-cluster")
    assert result["complete"]
    assert {node["package_version"] for node in store.load()["snapshot"]["nodes"]} == {
        "0.1.2"
    }


# A package rollout may land after the seed was activated and its Control CID
# was durably pinned, but before the seed's 1/1 three-DB result was written.
# This exact crash window is read-only in plan and is repinned atomically under
# the mutation lease before normal forward convergence resumes.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    pinned = discovery(package="0.1.1")
    live = discovery(package="0.1.2")
    live = Discovery(**{**live.__dict__, "members_version": live.members_version + 4})
    backend = FakeBackend(live)
    enter_seed_activation_crash(backend)
    store.create(staged_seed_ledger(pinned, backend.cids["PVN_Control"]))
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    before = copy.deepcopy(store.load())
    plan = control.plan()
    assert plan["read_only"] and plan["package_repin_required"] is True
    assert plan["package_repin_source_phase"] == "staged"
    assert "staged seed-activation crash window" in plan["actions"][0]
    assert store.load() == before

    original_update = store.update
    repin_was_leased = []

    def observe_update(value):
        previous = store.load()
        if previous["snapshot"] != value["snapshot"]:
            assert lease_state.exists(), "staged package snapshot repin escaped the lease"
            repin_was_leased.append(True)
        original_update(value)

    store.update = observe_update
    result = control.apply("test-cluster")
    assert result["complete"] and repin_was_leased == [True]
    ledger = store.load()
    assert {node["package_version"] for node in ledger["snapshot"]["nodes"]} == {
        "0.1.2"
    }
    assert ledger["db_cluster_ids"] == backend.cids
    assert backend.log.count("central:pve-a") == 0


# A rolling package update may also land at central-N after the next voter has
# created, but not activated, its Control join stub. Plan is read-only and
# apply repins only under the lease, then proves the ledger CID after startup.
for cid_known in (False, True):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        pinned = discovery(package="0.1.1")
        live = discovery(package="0.1.2")
        live = Discovery(**{**live.__dict__, "members_version": live.members_version + 4})
        backend = FakeBackend(live)
        enter_partial_join_crash(backend, cid_known=cid_known)
        store.create(partial_central_ledger(pinned, backend.cids))
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        before = copy.deepcopy(store.load())
        plan = control.plan()
        assert plan["read_only"] and plan["package_repin_required"] is True
        assert plan["package_repin_source_phase"] == "central-1"
        assert "central-1 pending-join crash window" in plan["actions"][0]
        assert store.load() == before

        original_update = store.update
        repin_was_leased = []

        def observe_partial_update(value):
            previous = store.load()
            if previous["snapshot"] != value["snapshot"]:
                assert lease_state.exists(), "partial package snapshot repin escaped the lease"
                repin_was_leased.append(True)
            original_update(value)

        store.update = observe_partial_update
        result = control.apply("test-cluster")
        assert result["complete"] and repin_was_leased == [True]
        assert backend.central == ["pve-a", "pve-b", "pve-c"]
        assert "init:pve-b" not in backend.log
        assert store.load()["db_cluster_ids"] == backend.cids


# The same recovery proof is count-independent: a five-voter central-3 ledger
# resumes its pinned fourth-voter stub before creating the fifth voter.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    pinned = discovery(5, package="0.1.1")
    backend = FakeBackend(discovery(5, package="0.1.2"))
    enter_partial_join_crash(backend, complete=3, cid_known=True)
    store.create(partial_central_ledger(pinned, backend.cids, complete=3))
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    plan = control.plan()
    assert plan["package_repin_required"] and plan["package_repin_source_phase"] == "central-3"
    result = control.apply("test-cluster")
    assert result["complete"] and result["nodes"] == 5
    assert backend.central == ["pve-a", "pve-b", "pve-c", "pve-d", "pve-e"]
    assert "init:pve-d" not in backend.log and backend.log.count("init:pve-e") == 1


# The exact live crash state exercised in the lab is also resumable without a
# package repin: central-1 is durable, the second voter is already active and
# healthy but its ledger write was missed, and only the volatile PVE membership
# epoch advanced. Apply adopts no foreign state; init verifies the pinned CID,
# the 2/2 health gate records voter two, and convergence continues forward.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    pinned = discovery(package="0.1.1")
    live = Discovery(**{**pinned.__dict__, "members_version": 11})
    backend = FakeBackend(live)
    first, second, _third = live.nodes
    backend.control_dbs = {
        first.name: backend.cids["PVN_Control"],
        second.name: backend.cids["PVN_Control"],
    }
    backend.central = [first.name, second.name]
    backend.central_markers = [first.name, second.name]
    store.create(partial_central_ledger(pinned, backend.cids))
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    before = copy.deepcopy(store.load())
    plan = control.plan()
    assert plan["read_only"] and not plan["package_repin_required"]
    assert store.load() == before
    result = control.apply("test-cluster")
    assert result["complete"] and backend.central == [
        "pve-a", "pve-b", "pve-c"
    ]
    assert "init:pve-b" not in backend.log and "central:pve-b" not in backend.log


# A fully converged cluster may adopt one uniform forward package rollout only
# while every active voter has an unconsumed restart marker for that exact
# package, all three databases remain exact N/N, and a fresh node doctor passes.
for count in (1, 3, 5):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        pinned = discovery(count, package="0.1.1")
        live = discovery(count, package="0.1.2")
        live = Discovery(**{**live.__dict__, "members_version": live.members_version + 4})
        backend = FakeBackend(live)
        enter_complete_update(backend)
        store.create(complete_ledger(pinned, backend.cids))
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        before = copy.deepcopy(store.load())
        plan = control.plan()
        assert plan["read_only"] and plan["package_repin_required"] is True
        assert plan["package_repin_source_phase"] == "complete"
        assert "fully converged restart-pending cluster" in plan["actions"][0]
        assert store.load() == before

        original_update = store.update
        repin_was_leased = []

        def observe_complete_update(value):
            previous = store.load()
            if previous["snapshot"] != value["snapshot"]:
                assert lease_state.exists(), "complete package snapshot repin escaped the lease"
                repin_was_leased.append(True)
            original_update(value)

        store.update = observe_complete_update
        result = control.apply(backend.found.confirmation)
        assert result["complete"] and repin_was_leased == [True]
        assert {node["package_version"] for node in store.load()["snapshot"]["nodes"]} == {
            "0.1.2"
        }
        assert not any(entry.startswith(("init:", "central:", "node:"))
                       for entry in backend.log)


def complete_runtime_repin_rejected(case):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        pinned = discovery(package="0.1.1")
        backend = FakeBackend(discovery(package="0.1.2"))
        enter_complete_update(backend)
        store.create(complete_ledger(pinned, backend.cids))
        if case == "restart-marker-consumed":
            backend.restart_pending.pop(backend.found.nodes[0].name)
        elif case == "node-target-inactive":
            backend.node_targets.remove(backend.found.nodes[1].name)
        elif case == "doctor-failed":
            backend.doctor_fail.add(backend.found.nodes[2].name)
        elif case == "database-disconnected":
            backend.complete_report_hook = lambda reports: \
                reports[0][1]["databases"][0].update(connected_members=2)
        elif case == "database-cid-drift":
            backend.complete_report_hook = lambda reports: \
                reports[1][1]["databases"][1].update(cluster_id="foreign-cid")
        elif case == "control-cid-drift":
            backend.control_dbs[backend.found.nodes[1].name] = "foreign-control-cid"
        else:
            raise AssertionError(case)
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        before = copy.deepcopy(store.load())
        expect_error(control.plan, "repin refused")
        assert store.load() == before


for complete_case in (
    "restart-marker-consumed",
    "node-target-inactive",
    "doctor-failed",
    "database-disconnected",
    "database-cid-drift",
    "control-cid-drift",
):
    complete_runtime_repin_rejected(complete_case)


def partial_runtime_repin_rejected(case):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        pinned = discovery(package="0.1.1")
        backend = FakeBackend(discovery(package="0.1.2"))
        enter_partial_join_crash(backend)
        ledger = partial_central_ledger(pinned, backend.cids)
        store.create(ledger)
        pending = backend.found.nodes[1].name
        if case == "wrong-remote":
            backend.pending_control_remotes[pending] = ["ssl:192.0.2.99:6646"]
        elif case == "missing-remote":
            backend.pending_control_remotes[pending] = []
        elif case == "extra-remote":
            backend.pending_control_remotes[pending].append("ssl:192.0.2.99:6646")
        elif case == "duplicate-sid":
            backend.pending_control_server_ids[pending] = "sid-PVN_Control-pve-a"
        elif case == "foreign-known-cid":
            backend.pending_control_cids[pending] = "foreign-control-cid"
        elif case == "missing-stub":
            backend.pending_control_dbs.clear()
        elif case == "next-marker":
            backend.central_markers.append(pending)
        elif case == "restart-marker-missing":
            backend.restart_pending.clear()
        elif case == "transport-active":
            backend.node_targets.append("pve-a")
        else:
            raise AssertionError(case)
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        before = copy.deepcopy(store.load())
        expect_error(control.plan, "repin refused")
        assert store.load() == before
        assert backend.central == ["pve-a"]


for partial_case in (
    "wrong-remote", "missing-remote", "extra-remote", "duplicate-sid",
    "foreign-known-cid", "missing-stub", "next-marker",
    "restart-marker-missing", "transport-active",
):
    partial_runtime_repin_rejected(partial_case)


def staged_runtime_repin_rejected(case):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        pinned = discovery(package="0.1.1")
        backend = FakeBackend(discovery(package="0.1.2"))
        enter_seed_activation_crash(backend)
        control_cid = backend.cids["PVN_Control"]
        if case == "wrong-control-cid":
            control_cid = "foreign-control-cid"
        elif case == "nonseed-db":
            backend.control_dbs["pve-b"] = backend.cids["PVN_Control"]
        elif case == "nonseed-marker":
            backend.central_markers.append("pve-b")
        elif case == "node-active":
            backend.node_targets.append("pve-a")
        elif case == "node-marker":
            backend.node_markers.append("pve-a")
        elif case == "node-ready":
            backend.nodes.append("pve-a")
        elif case == "seed-marker-missing":
            backend.central_markers.clear()
        elif case == "seed-inactive":
            backend.central.clear()
        elif case == "restart-marker-missing":
            backend.restart_pending.clear()
        elif case == "restart-marker-wrong":
            backend.restart_pending["pve-a"] = "0.1.1"
        else:
            raise AssertionError(case)
        store.create(staged_seed_ledger(pinned, control_cid))
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        expect_error(control.plan, "repin refused")


for runtime_case in (
    "wrong-control-cid", "nonseed-db", "nonseed-marker", "node-active",
    "node-marker", "node-ready", "seed-marker-missing", "seed-inactive",
    "restart-marker-missing", "restart-marker-wrong",
):
    staged_runtime_repin_rejected(runtime_case)


def staged_report_repin_rejected(mutate):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        pinned = discovery(package="0.1.1")
        backend = FakeBackend(discovery(package="0.1.2"))
        enter_seed_activation_crash(backend)
        backend.staged_report_hook = mutate
        store.create(staged_seed_ledger(pinned, backend.cids["PVN_Control"]))
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        expect_error(control.plan, "repin refused")


def report_row(report, name):
    return next(row for row in report["databases"] if row["database"] == name)


# The live seed proof is exactly healthy 1/1 membership for all three schemas,
# with no membership change and matching online/offline identities.
report_mutations = [
    lambda report: report.update(healthy=False),
    lambda report: report.update(target_active=False),
    lambda report: report["databases"].pop(),
    lambda report: report_row(report, "OVN_Northbound").update(
        database="Wrong_Northbound"
    ),
    lambda report: report_row(report, "PVN_Control").update(healthy=False),
    lambda report: report_row(report, "OVN_Northbound").update(member_count=2),
    lambda report: report_row(report, "OVN_Southbound").update(connected_members=0),
    lambda report: report_row(report, "OVN_Northbound").update(membership_change=True),
    lambda report: report_row(report, "PVN_Control").update(server_id=None),
    lambda report: report_row(report, "OVN_Southbound").update(
        address="ssl:192.0.2.99:6644"
    ),
    lambda report: report_row(report, "OVN_Northbound").update(
        cluster_id="foreign-nb-cid"
    ),
    lambda report: report["offline"]["OVN_Southbound"].update(
        cluster_id="foreign-offline-sb-cid"
    ),
]
for report_mutation in report_mutations:
    staged_report_repin_rejected(report_mutation)


def expect_package_repin_rejected(pinned, live, **ledger_overrides):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        store.create(planned_ledger(pinned, **ledger_overrides))
        backend = FakeBackend(live)
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        expect_error(control.plan, "drift")


# Mixed versions, a downgrade, any non-planned progress, and topology drift
# can never be disguised as a package-only repin.
pinned = discovery(package="0.1.1")
mixed_nodes = list(discovery(package="0.1.2").nodes)
mixed_nodes[1] = Node(**{**mixed_nodes[1].__dict__, "package_version": "0.1.3"})
mixed = Discovery(**{**discovery(package="0.1.2").__dict__, "nodes": tuple(mixed_nodes)})
expect_package_repin_rejected(pinned, mixed)
expect_package_repin_rejected(discovery(package="0.1.2"), discovery(package="0.1.1"))
expect_package_repin_rejected(pinned, discovery(package="0.1.2"), phase="staged")
topology_drift = Discovery(**{
    **discovery(package="0.1.2").__dict__, "topology_sha256": "b" * 64,
})
expect_package_repin_rejected(pinned, topology_drift)

# A schema 2 topology can replace the previously pinned schema 1 digest only
# through its exact canonical projection. This composes with the existing
# package repin, but no other durable field may change.
legacy_topology_digest = "1" * 64
schema2_topology_digest = "2" * 64
schema1_pinned = Discovery(**{
    **discovery(package="0.1.1").__dict__,
    "topology_sha256": legacy_topology_digest,
    "legacy_topology_sha256": None,
})
schema2_same_package = Discovery(**{
    **schema1_pinned.__dict__,
    "topology_sha256": schema2_topology_digest,
    "legacy_topology_sha256": legacy_topology_digest,
})
schema2_new_package = Discovery(**{
    **discovery(package="0.1.2").__dict__,
    "topology_sha256": schema2_topology_digest,
    "legacy_topology_sha256": legacy_topology_digest,
})
schema1_ledger = planned_ledger(schema1_pinned)
schema_only_repin = ControlPlane._verify_ledger(
    schema2_same_package, schema1_ledger, allow_package_repin=True
)
assert schema_only_repin.topology_schema and not schema_only_repin.package
combined_repin = ControlPlane._verify_ledger(
    schema2_new_package, schema1_ledger, allow_package_repin=True
)
assert combined_repin.topology_schema and combined_repin.package

for unsafe_schema2 in (
    Discovery(**{
        **schema2_new_package.__dict__,
        "legacy_topology_sha256": None,
    }),
    Discovery(**{
        **schema2_new_package.__dict__,
        "legacy_topology_sha256": "z" * 64,
    }),
    Discovery(**{
        **schema2_new_package.__dict__,
        "guest_mtu": schema2_new_package.guest_mtu + 1,
    }),
    Discovery(**{
        **schema2_new_package.__dict__,
        "cluster_version": schema2_new_package.cluster_version + 1,
    }),
):
    expect_error(
        lambda unsafe=unsafe_schema2: ControlPlane._verify_ledger(
            unsafe, schema1_ledger, allow_package_repin=True
        ),
        "drift",
    )

with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    store.create(copy.deepcopy(schema1_ledger))
    backend = FakeBackend(schema2_same_package)
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    plan = control.plan()
    assert plan["topology_schema_repin_required"] is True
    assert plan["package_repin_required"] is False
    result = control.apply(schema2_same_package.confirmation)
    assert result["complete"] is True
    assert store.load()["snapshot"]["topology_sha256"] == schema2_topology_digest
    assert ControlPlane._verify_ledger(schema2_same_package, store.load()) == \
        module["LedgerRepin"]()

durable_snapshot = pinned.snapshot()
epoch_only = copy.deepcopy(durable_snapshot)
epoch_only["members_version"] += 100
assert ControlPlane._durable_snapshot_matches(durable_snapshot, epoch_only)

durable_mutations = (
    lambda value: value.update(cluster_version=value["cluster_version"] + 1),
    lambda value: value.update(topology_sha256="b" * 64),
    lambda value: value["nodes"][0].update(
        node_id=value["nodes"][0]["node_id"] + 10
    ),
    lambda value: value["nodes"][0].update(control_ip="192.0.2.200"),
    lambda value: value["nodes"][0].update(package_version="0.1.2"),
)
for durable_mutation in durable_mutations:
    changed = copy.deepcopy(epoch_only)
    durable_mutation(changed)
    assert not ControlPlane._durable_snapshot_matches(durable_snapshot, changed)

epoch_package_live = discovery(package="0.1.2")
epoch_package_live = Discovery(**{
    **epoch_package_live.__dict__,
    "members_version": epoch_package_live.members_version + 100,
})
changed_node_id = list(epoch_package_live.nodes)
changed_node_id[0] = Node(**{
    **changed_node_id[0].__dict__, "node_id": changed_node_id[0].node_id + 10,
})
changed_control_ip = list(epoch_package_live.nodes)
changed_control_ip[1] = Node(**{
    **changed_control_ip[1].__dict__, "control_ip": "192.0.2.200",
})
renamed_node = list(epoch_package_live.nodes)
renamed_node[2] = Node(**{**renamed_node[2].__dict__, "name": "foreign-node"})
for durable_package_drift in (
    Discovery(**{
        **epoch_package_live.__dict__,
        "cluster_version": epoch_package_live.cluster_version + 1,
    }),
    Discovery(**{**epoch_package_live.__dict__, "nodes": tuple(changed_node_id)}),
    Discovery(**{**epoch_package_live.__dict__, "nodes": tuple(changed_control_ip)}),
    Discovery(**{**epoch_package_live.__dict__, "nodes": tuple(renamed_node)}),
    Discovery(**{**epoch_package_live.__dict__, "nodes": epoch_package_live.nodes[:-1]}),
):
    expect_package_repin_rejected(pinned, durable_package_drift)

with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    malformed = planned_ledger(pinned)
    malformed["snapshot"]["members_version"] = "7"
    store.create(malformed)
    control = ControlPlane(FakeBackend(pinned), store, timeout=0.01, interval=0)
    expect_error(control.plan, "drift")


def expect_staged_snapshot_repin_rejected(pinned, live, **ledger_overrides):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        backend = FakeBackend(live)
        enter_seed_activation_crash(backend)
        store.create(staged_seed_ledger(
            pinned, backend.cids["PVN_Control"], **ledger_overrides
        ))
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        expect_error(control.plan, "drift")


# The staged exception never widens package drift: mixed versions, downgrade,
# topology drift, incomplete pins, or already-populated final DB CIDs all fail.
expect_staged_snapshot_repin_rejected(pinned, mixed)
expect_staged_snapshot_repin_rejected(
    discovery(package="0.1.2"), discovery(package="0.1.1")
)
expect_staged_snapshot_repin_rejected(pinned, topology_drift)
expect_staged_snapshot_repin_rejected(pinned, discovery(package="0.1.2"),
                                      cert_fingerprints={})
expect_staged_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), central_complete=1,
)
expect_staged_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), nodes_complete=1,
)
expect_staged_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), control_db_cluster_id="",
)
expect_staged_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"),
    db_cluster_ids={name: f"unexpected-{name}" for name in DATABASES},
)


def expect_partial_snapshot_repin_rejected(pinned, live, **ledger_overrides):
    with tempfile.TemporaryDirectory() as temporary:
        store = LedgerStore(pathlib.Path(temporary) / "private")
        backend = FakeBackend(live)
        enter_partial_join_crash(backend)
        store.create(partial_central_ledger(
            pinned, backend.cids, **ledger_overrides
        ))
        control = ControlPlane(backend, store, timeout=0.01, interval=0)
        before = copy.deepcopy(store.load())
        expect_error(control.plan, "drift")
        assert store.load() == before


# The central-N exception is package-only and requires its exact durable phase,
# zero transport progress, complete PKI pins, and the complete three-CID set.
expect_partial_snapshot_repin_rejected(pinned, mixed)
expect_partial_snapshot_repin_rejected(
    discovery(package="0.1.2"), discovery(package="0.1.1")
)
expect_partial_snapshot_repin_rejected(pinned, topology_drift)
expect_partial_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), phase="staged"
)
expect_partial_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), central_complete=2
)
expect_partial_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), nodes_complete=1
)
expect_partial_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), cert_fingerprints={}
)
expect_partial_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), db_cluster_ids={}
)
expect_partial_snapshot_repin_rejected(
    pinned, discovery(package="0.1.2"), control_db_cluster_id="foreign-control-cid"
)


# Even an otherwise eligible planned snapshot is refused if a marker/service
# or database appeared outside the zero-progress ledger.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    store.create(planned_ledger(discovery(package="0.1.1")))
    backend = FakeBackend(discovery(package="0.1.2"))
    backend.control_dbs["pve-a"] = "unexpected-cid"
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    expect_error(control.plan, "repin refused")
    backend.control_dbs.clear()
    backend.restart_pending["pve-a"] = "0.1.2"
    expect_error(control.plan, "repin refused")


# Topology/membership is re-read before every destructive or activation boundary.
# A changing pmxcfs membership-view epoch alone does not interrupt those
# boundaries because every read still proves the exact online nodelist,
# corosync config version, and durable topology hash.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    backend = FakeBackend(discovery())

    def advance_membership_epoch(current):
        current.found = Discovery(**{
            **current.found.__dict__,
            "members_version": current.found.members_version + 1,
        })

    backend.discover_hook = advance_membership_epoch
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    result = control.apply("test-cluster")
    assert result["complete"] and backend.found.members_version > 7


with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    backend = FakeBackend(discovery())
    backend.discover_hook = lambda current: (
        drift_topology(current) if current.discoveries == 3 else None
    )
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    expect_error(lambda: control.apply("test-cluster"), "activation refused")
    assert not backend.control_dbs and backend.central == [] and backend.nodes == []

with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    backend = FakeBackend(discovery())

    def drift_after_control_init(current):
        if current.control_dbs and not current.central:
            drift_topology(current)

    backend.discover_hook = drift_after_control_init
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    expect_error(lambda: control.apply("test-cluster"), "central target activation")
    assert "init:pve-a" in backend.log and backend.central == [] and backend.nodes == []

with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    backend = FakeBackend(discovery())

    def drift_before_transport(current):
        if len(current.central) == len(current.found.nodes) and not current.nodes:
            drift_topology(current)

    backend.discover_hook = drift_before_transport
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    expect_error(lambda: control.apply("test-cluster"), "transport target activation")
    assert backend.central == ["pve-a", "pve-b", "pve-c"] and backend.nodes == []


# Partial second-voter join is resumed forward. Nothing is deleted or rolled back.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    backend = FakeBackend(discovery())
    backend.block_at = 2
    control = ControlPlane(backend, store, timeout=0.001, interval=0)
    expect_error(lambda: control.apply("test-cluster"), "timed out")
    ledger = store.load()
    assert ledger["central_complete"] == 1
    assert backend.central == ["pve-a", "pve-b"]
    assert not any("delete" in entry or "leave" in entry for entry in backend.log)
    backend.block_at = None
    result = control.apply("test-cluster")
    assert result["complete"]
    assert backend.central == ["pve-a", "pve-b", "pve-c"]
    assert backend.log.count("init:pve-b") == 1


# A previously created foreign Control DB is rejected before central activation.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    backend = FakeBackend(discovery())
    backend.crash_after_init = "pve-b"
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    expect_error(lambda: control.apply("test-cluster"), "simulated crash")
    assert backend.central == ["pve-a"]
    assert store.load()["control_db_cluster_id"] == backend.cids["PVN_Control"]
    backend.control_dbs["pve-b"] = "foreign-control-cid"
    expect_error(lambda: control.apply("test-cluster"), "differs from the ledger")
    assert backend.central == ["pve-a"]


# Standalone uses the same durable workflow without Raft CIDs.
with tempfile.TemporaryDirectory() as temporary:
    store = LedgerStore(pathlib.Path(temporary) / "private")
    backend = FakeBackend(discovery(1))
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    result = control.apply("standalone-pve-a")
    assert result["complete"] and result["nodes"] == 1
    assert store.load()["db_cluster_ids"] == {}
    assert backend.log[-1] == "node:pve-a"


# An existing cluster lease blocks apply and is never removed by a non-owner.
with tempfile.TemporaryDirectory() as temporary:
    private = pathlib.Path(temporary) / "private"
    owner = {"domain": "mutation", "token": "b" * 32}
    lease_state.write_text(json.dumps(owner))
    store = LedgerStore(private)
    backend = FakeBackend(discovery())
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    expect_error(lambda: control.apply("test-cluster"), "could not acquire cluster-global")
    assert backend.log == []
    assert json.loads(lease_state.read_text()) == owner
    lease_state.unlink()

lease_temporary.cleanup()
print("pvn-control-plane mock tests passed")
PY

# The production helper may clean only its pmxcfs lock/temp files. It must not
# contain any automatic Raft leave/kick or central database deletion path.
if grep -Eq 'cluster/(leave|kick)|rm .*/var/lib/(ovn|pvn)|unlink\(.*/var/lib/(ovn|pvn)' "$SCRIPT"; then
    echo "pvn-control-plane contains an automatic database rollback" >&2
    exit 1
fi
if grep -Fq 'sys.argv[2]' "$SCRIPT" || ! grep -Fq 'payload = json.load(sys.stdin)' "$SCRIPT"; then
    echo "pvn-control-plane must not expose staged private keys in process arguments" >&2
    exit 1
fi

echo "pvn-control-plane tests passed"
