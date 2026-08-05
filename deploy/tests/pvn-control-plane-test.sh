#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
SCRIPT=$REPO/deploy/scripts/pvn-control-plane

"$SCRIPT" --help >/dev/null

PVN_CONTROL_PLANE_SCRIPT=$SCRIPT python3 <<'PY'
import base64
import copy
import json
import os
import pathlib
import tempfile
import sys
import types

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


def discovery(count=3, package="0.1.1"):
    records = (
        ("pve-a", 1, "192.0.2.11", "198.51.100.11"),
        ("pve-b", 2, "192.0.2.12", "198.51.100.12"),
        ("pve-c", 3, "192.0.2.13", "198.51.100.13"),
    )[:count]
    nodes = tuple(Node(name, node_id, control, geneve, "ens4", "ens5", "br-provider",
                       package, 1450, index == 0)
                  for index, (name, node_id, control, geneve) in enumerate(records))
    mode = "standalone" if count == 1 else "raft"
    confirm = "standalone-pve-a" if count == 1 else "test-cluster"
    return Discovery(mode, confirm, confirm, "pve-a", 7, 3 if count == 3 else 0,
                     "a" * 64, 1380, "provider", nodes)


class FakeBackend:
    def __init__(self, found):
        self.found = found
        self.log = []
        self.shared = None
        self.staged = {}
        self.control_dbs = set()
        self.central = []
        self.nodes = []
        self.cids = {name: f"cid-{index}" for index, name in enumerate(DATABASES, 1)}
        self.block_at = None
        self.pki_variant = "one"
        self.pristine = True

    def discover(self):
        return self.found

    def assert_pristine(self, found):
        self.log.append("pristine")
        if not self.pristine:
            raise ControlPlaneError("unledgered state")

    def ensure_shared_config(self, config):
        encoded = json.dumps(config, sort_keys=True)
        if self.shared is None:
            self.shared = encoded
            self.log.append("config")
        elif self.shared != encoded:
            raise ControlPlaneError("config drift")

    def ensure_pki(self, cluster_id, nodes):
        fingerprints = {"ca": "ca-" + self.pki_variant}
        bundles = {}
        for node in nodes:
            fingerprints[node.name] = "cert-" + self.pki_variant + "-" + node.name
            bundles[node.name] = [self.file("/etc/pvn/pki/ca.pem", "ca"),
                                  self.file("/etc/pvn/pki/node.pem", node.name),
                                  self.file("/etc/pvn/pki/node-key.pem", "key-" + node.name)]
        if not any(entry.startswith("pki:") for entry in self.log):
            self.log.append("pki:" + cluster_id)
        return fingerprints, bundles

    @staticmethod
    def file(path, value):
        return {"path": path, "content": base64.b64encode(value.encode()).decode(),
                "mode": "0640", "group": "pvn"}

    def stage(self, node, files):
        snapshot = json.dumps(files, sort_keys=True)
        if node.name not in self.staged:
            self.staged[node.name] = snapshot
            self.log.append("stage:" + node.name)
        elif self.staged[node.name] != snapshot:
            raise ControlPlaneError("stage drift")

    def init_control(self, node, mode, cluster_id, seed):
        if node.name not in self.control_dbs:
            self.control_dbs.add(node.name)
            self.log.append("init:" + node.name)

    def activate_central(self, node):
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
            self.log.append("node:" + node.name)

    def node_status(self, node):
        return {"ready": node.name in self.nodes, "marker": node.name in self.nodes}


def expect_error(action, text):
    try:
        action()
    except ControlPlaneError as error:
        assert text in str(error), (text, str(error))
    else:
        raise AssertionError("expected ControlPlaneError")


lease_temporary = tempfile.TemporaryDirectory()
lease_root = pathlib.Path(lease_temporary.name)
lease_helper = lease_root / "pvn-cluster-lease"
lease_state = lease_root / "control-plane.lease"
lease_helper.write_text(r'''#!/usr/bin/python3
import json
import os
import pathlib
import sys

state = pathlib.Path(os.environ["PVN_TEST_CP_LEASE"])
action, domain, token = sys.argv[1:]
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


def canonical_topology_fixture(root):
    records = (
        ("pve-a", 1, "192.0.2.11", "198.51.100.11"),
        ("pve-b", 2, "192.0.2.12", "198.51.100.12"),
        ("pve-c", 3, "192.0.2.13", "198.51.100.13"),
    )
    members = {
        "nodename": "pve-a",
        "version": 7,
        "cluster": {"name": "test-cluster", "version": 3, "nodes": 3, "quorate": 1},
        "nodelist": {
            name: {"id": node_id, "online": 1, "ip": management}
            for name, node_id, management, _ in records
        },
    }
    membership = {
        "cluster_name": "test-cluster",
        "nodes": [
            {"name": name, "node_id": node_id, "management_ip": management}
            for name, node_id, management, _ in records
        ],
    }
    topology = {
        "schema": 1,
        "phase": "complete",
        "cluster_name": "test-cluster",
        "membership_snapshot": membership,
        "membership_hash": module["sha256"](
            json.dumps(membership, sort_keys=True, separators=(",", ":")).encode()
        ),
        "nodes": [
            {
                "name": name,
                "node_id": node_id,
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
        }
        for name, _, management, geneve in records
    }
    backend._remote = lambda name, _action, _payload: copy.deepcopy(probes[name])
    return backend, topology_path, topology


# Control-plane discovery consumes the exact completed topology ledger contract.
with tempfile.TemporaryDirectory() as temporary:
    backend, topology_path, topology = canonical_topology_fixture(pathlib.Path(temporary))
    found = backend.discover()
    assert found.guest_mtu == 1300
    assert found.nodes[0].geneve_interface == "ens4"
    assert found.nodes[0].provider_interface == "ens5"

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


with tempfile.TemporaryDirectory() as temporary:
    root = pathlib.Path(temporary)
    store = LedgerStore(root / "private")
    backend = FakeBackend(discovery())
    control = ControlPlane(backend, store, timeout=0.01, interval=0)

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
        listener = next(item for item in decoded if item["path"].endswith("ovn-listeners.env"))
        text = base64.b64decode(listener["content"]).decode()
        expected_ip = next(node.control_ip for node in backend.found.nodes if node.name == name)
        assert f"PVN_OVN_LISTEN={expected_ip}\n" in text
    ledger = store.load()
    assert ledger["phase"] == "complete"
    assert ledger["db_cluster_ids"] == backend.cids
    assert ledger["snapshot"]["nodes"][0]["geneve_ip"] == "198.51.100.11"

    # Exact rerun verifies live state and performs no new mutation.
    before = list(backend.log)
    control.apply("test-cluster")
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
    owner = {"domain": "control-plane", "token": "b" * 32}
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
