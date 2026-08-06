#!/usr/bin/env python3
"""Focused pure tests for PVN's quorum-safe Corosync transition helpers."""

from importlib.machinery import SourceFileLoader
from pathlib import Path
import hashlib
import re
import types


REPO = Path(__file__).resolve().parents[2]
MODULE_PATH = REPO / "deploy" / "scripts" / "pvn-topology"
loader = SourceFileLoader("pvn_topology_test_module", str(MODULE_PATH))
module = types.ModuleType(loader.name)
loader.exec_module(module)


def expect_failure(call, message):
    try:
        call()
    except module.TopologyError:
        return
    raise AssertionError(message)


def config_for(count):
    blocks = []
    for index in range(1, count + 1):
        blocks.append(
            "    node {\n"
            f"        name: prox{index}\n"
            f"        nodeid: {index}\n"
            f"        ring0_addr: 192.168.100.{index}\n"
            "    }"
        )
    return (
        "nodelist {\n"
        + "\n".join(blocks)
        + "\n}\n\n"
        "totem {\n"
        "    cluster_name: lab\n"
        "    config_version: 1\n"
        "    interface {\n"
        "        linknumber: 0\n"
        "    }\n"
        "    transport: knet\n"
        "    version: 2\n"
        "}\n"
    )


for count in (3, 5, 7):
    module.validate_corosync_node_count(count)
for count in (0, 1, 2, 4, 6):
    expect_failure(
        lambda count=count: module.validate_corosync_node_count(count),
        f"even/non-positive count {count} unexpectedly passed",
    )

for count in (3, 5):
    nodes = [
        {"name": f"prox{index}", "id": index, "ip": f"192.168.0.{index}"}
        for index in range(1, count + 1)
    ]
    coordinator = nodes[0]
    order = module.corosync_restart_order(nodes, coordinator)
    assert len(order) == count
    assert len({node["name"] for node in order}) == count
    assert order[-1] == coordinator

    original = config_for(count)
    names = {node["name"] for node in nodes}
    management = {node["name"]: node["ip"] for node in nodes}
    dual = module.add_ring(original, 1, management, 2)
    version, rings = module.parse_corosync(dual, names, "lab")
    assert version == 2
    assert set(rings) == {0, 1}
    final = module.remove_ring(dual, 0, 3)
    version, rings = module.parse_corosync(final, names, "lab")
    assert version == 3
    assert set(rings) == {1}

assert module.unused_corosync_ring({0: {}, 2: {}}) == 1
expect_failure(
    lambda: module.unused_corosync_ring({ring: {} for ring in range(8)}),
    "eight occupied KNET links unexpectedly produced a transition ring",
)


def parsed_config(text):
    version = int(re.search(r"(?m)^\s*config_version:\s*(\d+)\s*$", text).group(1))
    rows = {}
    for match in re.finditer(r"(?ms)^\s*node\s*\{.*?^\s*\}", text):
        block = match.group(0)
        name = re.search(r"(?m)^\s*name:\s*(\S+)\s*$", block).group(1)
        node_id = int(re.search(r"(?m)^\s*nodeid:\s*(\d+)\s*$", block).group(1))
        rows[name] = {
            "node_id": node_id,
            "rings": dict(re.findall(r"(?m)^\s*ring([0-7])_addr:\s*(\S+)\s*$", block)),
        }
    return version, rows


def exercise_five_node_convergence(initial_runtime, candidate, expected_order):
    nodes = [
        {"name": f"prox{index}", "id": index, "ip": f"192.168.0.{index}"}
        for index in range(1, 6)
    ]
    candidate_hash = hashlib.sha256(candidate.encode()).hexdigest()
    runtime = dict(initial_runtime)
    restarts = []

    def reports():
        configs = {name: parsed_config(text) for name, text in runtime.items()}
        result = []
        for local in nodes:
            name = local["name"]
            version, local_rows = configs[name]
            links = {}
            for ring, address in local_rows[name]["rings"].items():
                connected = []
                states = {}
                for peer in nodes:
                    peer_version, peer_rows = configs[peer["name"]]
                    del peer_version
                    expected = local_rows.get(peer["name"], {}).get("rings", {}).get(ring)
                    actual = peer_rows.get(peer["name"], {}).get("rings", {}).get(ring)
                    if expected is not None and actual == expected:
                        connected.append(peer["id"])
                        states[str(peer["id"])] = (
                            "localhost" if peer == local else "connected"
                        )
                    else:
                        states[str(peer["id"])] = "disconnected"
                links[ring] = {
                    "address": address,
                    "connected_node_ids": sorted(connected),
                    "node_states": states,
                }
            members = {}
            for peer in nodes:
                peer_version, peer_rows = configs[peer["name"]]
                members[str(peer["id"])] = {
                    "status": "joined",
                    "config_version": peer_version,
                    "rings": peer_rows[peer["name"]]["rings"],
                }
            result.append({
                "node": name,
                "corosync_sha256": candidate_hash,
                "corosync_package_version": "3.1.10-pve2",
                "corosync_runtime": {
                    "local_node_id": local["id"],
                    "links": links,
                    "config": {
                        "cluster_name": "lab",
                        "config_version": version,
                        "local_node_id": local["id"],
                        "nodes": local_rows,
                        "members": members,
                        "bind_addresses": local_rows[name]["rings"],
                    },
                },
            })
        return result

    def fake_probe_all(*_args, **_kwargs):
        return reports()

    def fake_ssh_request(node, action, _request, timeout=120):
        del timeout
        if action == "reload-corosync":
            return {"ok": True}
        assert action == "restart-corosync"
        current = reports()
        bridge = {
            item["name"]: f"192.168.100.{item['id']}" for item in nodes
        }
        assert module.runtime_ring_is_full(nodes, current, 0, bridge)
        runtime[node["name"]] = candidate
        restarts.append(node["name"])
        return {"ok": True}

    original_probe = module.probe_all
    original_ssh = module.ssh_request
    original_fast = module.TEST_FAST
    original_settle = module.COROSYNC_SETTLE_ATTEMPTS
    original_final = module.COROSYNC_FINAL_ATTEMPTS
    original_poll = module.COROSYNC_POLL_SECONDS
    module.probe_all = fake_probe_all
    module.ssh_request = fake_ssh_request
    module.TEST_FAST = True
    module.COROSYNC_SETTLE_ATTEMPTS = 1
    module.COROSYNC_FINAL_ATTEMPTS = 1
    module.COROSYNC_POLL_SECONDS = 0
    try:
        base = {node["name"]: {} for node in nodes}
        module.converge_corosync_runtime(
            "lab", nodes, {}, "tx", {}, base, "3.1.10-pve2",
            candidate, candidate_hash, 0,
            {node["name"]: f"192.168.100.{node['id']}" for node in nodes},
        )
    finally:
        module.probe_all = original_probe
        module.ssh_request = original_ssh
        module.TEST_FAST = original_fast
        module.COROSYNC_SETTLE_ATTEMPTS = original_settle
        module.COROSYNC_FINAL_ATTEMPTS = original_final
        module.COROSYNC_POLL_SECONDS = original_poll
    assert restarts == expected_order


five_nodes = [
    {"name": f"prox{index}", "id": index, "ip": f"192.168.0.{index}"}
    for index in range(1, 6)
]
five_original = config_for(5)
five_management = {node["name"]: node["ip"] for node in five_nodes}
five_dual = module.add_ring(five_original, 1, five_management, 2)
exercise_five_node_convergence(
    {node["name"]: five_original for node in five_nodes},
    five_dual,
    ["prox2", "prox3", "prox4", "prox5", "prox1"],
)
exercise_five_node_convergence(
    {
        node["name"]: (five_original if node["name"] == "prox1" else five_dual)
        for node in five_nodes
    },
    five_dual,
    ["prox1"],
)

print("pvn-topology Corosync helper tests passed")
