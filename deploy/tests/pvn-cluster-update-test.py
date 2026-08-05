#!/usr/bin/python3
"""Focused orchestration tests for the rolling cluster updater."""

from __future__ import annotations

import contextlib
import importlib.machinery
import importlib.util
import io
import pathlib
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


def probe_line(snapshot, node, version, *, central="inactive") -> str:
    pids = "101,102,103,104" if central == "active" else "none"
    return (
        "PVN_UPDATE "
        f"mode={snapshot.mode} cluster={snapshot.deployment} "
        f"config={snapshot.config_version} fingerprint={snapshot.fingerprint} "
        f"nodes={len(snapshot.nodes)} pve=9.2.2 arch=amd64 version={version} "
        f"hostname={node.name} nodeid={node.node_id} node=active "
        f"central={central} centralpids={pids}\n"
    )


nodes = (
    module.Node(1, "pve-a", "192.0.2.10", True),
    module.Node(2, "pve-b", "192.0.2.11", False),
)
fingerprint = module.membership_fingerprint("cluster", "lab-cluster", 7, nodes)
snapshot = module.Snapshot("cluster", "lab-cluster", 7, "pve-a", nodes, fingerprint)


parsed = module.parse_probe(nodes[0], probe_line(snapshot, nodes[0], "0.1.0"), snapshot)
check(parsed.version == "0.1.0" and parsed.node_state == "active", "valid probe was not parsed")
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
            return subprocess.CompletedProcess([], 0, probe_line(self.snapshot, node, versions[node.name]), "")
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
            return subprocess.CompletedProcess([], 0, f"PVN_UPDATED version={arguments[2]} central-restart=none\n", "")
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
        with contextlib.redirect_stdout(io.StringIO()):
            module.update_cluster(snapshot, deb, "0.2.1", "amd64", "a" * 64)
    check(versions == {"pve-a": "0.2.1", "pve-b": "0.2.1"}, "not every node was updated")
    check(events[0] == "lease-acquire" and events[-1] == "lease-release", "mutation lease did not bracket rollout")
    first_apply = min(index for index, event in enumerate(events) if event.startswith("apply:"))
    for expected in ("verify:pve-a", "verify:pve-b"):
        check(events.index(expected) < first_apply, "every pending DEB was not verified before the first apply")
    check(events.index("apply:pve-a") < events.index("apply:pve-b"), "nodes were not updated sequentially")
    for expected in ("cleanup:pve-a", "cleanup:pve-b"):
        check(events.count(expected) == 1, "successful update left a staged DEB behind")
    check(events.count("membership-revalidate") >= 4, "membership was not repeatedly revalidated")

    events.clear()
    versions.update({"pve-a": "0.1.0", "pve-b": "0.1.0"})
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
    "central service restarted unexpectedly; stop the rollout",
    "persistent /etc/pvn or shared PVN configuration changed during update",
    '"domain": "mutation"',
):
    check(required in source, f"missing fail-closed updater behavior: {required}")

print("pvn-cluster-update tests passed")
