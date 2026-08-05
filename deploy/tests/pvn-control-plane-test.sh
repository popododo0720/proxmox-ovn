#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
SCRIPT=$REPO/deploy/scripts/pvn-control-plane

"$SCRIPT" --help >/dev/null

PVN_CONTROL_PLANE_SCRIPT=$SCRIPT python3 <<'PY'
import base64
import copy
import hashlib
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
        self.control_dbs = {}
        self.central = []
        self.nodes = []
        self.cids = {name: f"cid-{index}" for index, name in enumerate(DATABASES, 1)}
        self.block_at = None
        self.pki_variant = "one"
        self.pki_installed = set()
        self.pki_pin_check = None
        self.pristine = True
        self.crash_after_init = None

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
    helper_sources = {}
    for node in discovery().nodes:
        node_root = root / "nodes" / node.name
        node_root.mkdir(mode=0o700, parents=True)
        pki_dir = node_root / "pki"
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
        }
        for old, new in replacements.items():
            assert old in source
            source = source.replace(old, new, 1)
        node_roots[node.name] = node_root
        ca_dirs[node.name] = ca_dir
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
