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
DATABASES = module["DATABASES"]


def discovery(count=3, package="0.1.1"):
    records = (
        ("pve-a", 1, "192.0.2.11", "198.51.100.11"),
        ("pve-b", 2, "192.0.2.12", "198.51.100.12"),
        ("pve-c", 3, "192.0.2.13", "198.51.100.13"),
    )[:count]
    nodes = tuple(Node(name, node_id, control, geneve, "br-provider",
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


# Existing pmxcfs lock blocks all apply mutations and is never auto-broken.
with tempfile.TemporaryDirectory() as temporary:
    private = pathlib.Path(temporary) / "private"
    private.mkdir()
    (private / "control-plane.lock").write_text("held")
    store = LedgerStore(private)
    backend = FakeBackend(discovery())
    control = ControlPlane(backend, store, timeout=0.01, interval=0)
    expect_error(lambda: control.apply("test-cluster"), "lock already exists")
    assert backend.log == []

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
