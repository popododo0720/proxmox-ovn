#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
TOPOLOGY=$REPO/deploy/scripts/pvn-topology
WORK=$(mktemp -d)

cleanup() {
    rm -rf "$WORK"
}
trap cleanup 0 HUP INT TERM

BIN=$WORK/bin
MEMBERS=$WORK/members.json
MEMBERS_GOOD=$WORK/members.good.json
PVE_NODES=$WORK/nodes
PVE_IDENTITY=$WORK/id_rsa
LOCK=$WORK/pvn-topology.lock
LEASE_DIR=$WORK/leases
PERL_DIR=$WORK/perl
STATE=$WORK/state.json
COROSYNC=$WORK/corosync.conf
LOG=$WORK/ssh.log
REMOTE_HELPER=$WORK/remote-helper.py
mkdir "$BIN" "$PVE_NODES" "$LEASE_DIR" "$PERL_DIR"
mkdir -p "$PERL_DIR/PVE"
: > "$LOG"

cat > "$MEMBERS" <<'EOF'
{
  "nodename": "prox1",
  "version": 9,
  "cluster": {"name": "lab-cluster", "version": 3, "nodes": 3, "quorate": 1},
  "nodelist": {
    "prox2": {"id": 1, "online": 1, "ip": "192.168.0.126"},
    "prox1": {"id": 2, "online": 1, "ip": "192.168.0.80"},
    "prox3": {"id": 3, "online": 1, "ip": "192.168.0.78"}
  }
}
EOF
cp "$MEMBERS" "$MEMBERS_GOOD"

for node in prox1 prox2 prox3; do
    mkdir "$PVE_NODES/$node"
    printf '%s ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest%s\n' "$node" "$node" \
        > "$PVE_NODES/$node/ssh_known_hosts"
    chmod 0640 "$PVE_NODES/$node/ssh_known_hosts"
done
: > "$PVE_IDENTITY"
chmod 0600 "$PVE_IDENTITY"

cat > "$PERL_DIR/PVE/Cluster.pm" <<'EOF'
package PVE::Cluster;
use strict;
use warnings;
sub cfs_update { return 1; }
sub cfs_lock_domain {
    my ($domain, $timeout, $code) = @_;
    die "unexpected topology lease domain\n" if $domain ne 'pvn-lease-mutation';
    return $code->();
}
1;
EOF

GLOBAL_LOCK=$LEASE_DIR/pvn-mutation.lease

cat > "$COROSYNC" <<'EOF'
logging {
    to_syslog: yes
}

nodelist {
    node {
        name: prox1
        nodeid: 2
        quorum_votes: 1
        ring0_addr: 192.168.100.25
    }
    node {
        name: prox2
        nodeid: 1
        quorum_votes: 1
        ring0_addr: 192.168.100.54
    }
    node {
        name: prox3
        nodeid: 3
        quorum_votes: 1
        ring0_addr: 192.168.100.163
    }
}

quorum {
    provider: corosync_votequorum
}

totem {
    cluster_name: lab-cluster
    config_version: 3
    interface {
        linknumber: 0
    }
    transport: knet
    version: 2
}
EOF

cat > "$BIN/ssh" <<'PY'
#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import sys


def stop(message):
    print(message, file=sys.stderr)
    raise SystemExit(1)


args = sys.argv[1:]
raw_request = sys.stdin.read()
try:
    destination_index = next(i for i, value in enumerate(args) if value.startswith("root@"))
    if len(args) != destination_index + 2:
        stop("remote request metadata escaped into SSH argv")
    remote_command = args[destination_index + 1]
    remote_argv = shlex.split(remote_command)
    if len(remote_argv) != 4 or remote_argv[:2] != ["python3", "-c"]:
        stop("missing quoted remote Python command")
    remote_helper, action = remote_argv[2:]
    if "lab-cluster" in remote_command:
        stop("request sentinel leaked into SSH argv")
    if len(raw_request.encode()) > 8 * 1024 * 1024:
        stop("fake SSH received an unbounded request")
    request = json.loads(raw_request)
    if request.get("cluster_name") == "lab-cluster" and "lab-cluster" not in raw_request:
        stop("request sentinel was not transported through stdin")
except Exception as exc:
    stop("bad fake SSH invocation: %s" % exc)
Path(os.environ["PVN_TEST_REMOTE_HELPER"]).write_text(remote_helper, encoding="utf-8")

host = args[destination_index][5:]
node = request.get("node")
node_members_version = {"prox1": 9, "prox2": 11, "prox3": 5}.get(node)
if node_members_version is None:
    stop("request does not identify a known PVE member")
lease_held = Path(os.environ["PVN_TEST_GLOBAL_LOCK"]).is_file()
with open(os.environ["PVN_TEST_LOG"], "a", encoding="utf-8") as stream:
    stream.write("host=%s node=%s action=%s lease=%d members_version=%d transport=stdin argv_request=0 options=%s\n" % (
        host, node, action, lease_held, node_members_version,
        " ".join(args[:destination_index]),
    ))

cleanup_actions = {
    "rollback-network", "discard-stage", "restore-ledger",
}
members_path = Path(os.environ["PVN_TOPOLOGY_MEMBERS_FILE"])
members = json.loads(members_path.read_text(encoding="utf-8"))
members["version"] = node_members_version
actual_membership = {
    "cluster_name": members["cluster"]["name"],
    "cluster_version": members["cluster"]["version"],
    "nodes": sorted((
        {"name": name, "node_id": entry["id"], "management_ip": entry["ip"]}
        for name, entry in members["nodelist"].items()
        if entry.get("online") in (1, True, "1")
    ), key=lambda item: (item["node_id"], item["name"])),
}
if "members_version" in request.get("membership_snapshot", {}):
    stop("node-local PVE members version leaked into the shared membership guard")
if action not in cleanup_actions and request.get("membership_snapshot") != actual_membership:
    stop("exact PVE membership snapshot changed during topology apply")

if os.environ.get("PVN_TEST_PROBE_FAIL_HOST") == node and action == "probe":
    stop("simulated unsafe topology probe")
if os.environ.get("PVN_TEST_FAIL_VERIFY_HOST") == node and action == "verify-network":
    stop("simulated post-ifreload verification failure")
if os.environ.get("PVN_TEST_FAIL_LEDGER_WRITE") == "yes" and action == "write-ledger":
    stop("simulated shared ledger write failure")
if os.environ.get("PVN_TEST_FAIL_LEDGER_VERIFY_HOST") == node and action == "verify-ledger":
    stop("simulated shared ledger verification failure")
if os.environ.get("PVN_TEST_FAIL_DISCARD_HOST") == node and action == "discard-stage":
    stop("simulated pending cleanup failure")
if (
    os.environ.get("PVN_TEST_FAIL_COMPLETE_PHASE_HOST") == node
    and action == "record-phase"
    and request.get("phase") == "complete"
):
    stop("simulated final journal phase failure")

mutating = {
    "prepare", "validate-corosync", "apply-corosync",
    "reload-corosync", "restart-corosync",
    "record-phase", "stage-network", "apply-network", "rollback-network",
    "discard-stage", "write-ledger", "restore-ledger",
}
if action in mutating:
    global_lock = Path(os.environ["PVN_TEST_GLOBAL_LOCK"])
    if not global_lock.is_file():
        stop("cluster-global lock is not held during mutation")
    owner = json.loads(global_lock.read_text(encoding="utf-8"))
    if owner.get("cluster") != "lab-cluster" or owner.get("node") != "prox1" or \
            owner.get("domain") != "mutation":
        stop("cluster-global lock owner record is invalid")
    if not owner.get("token") or not owner.get("transaction"):
        stop("cluster-global lock owner record is incomplete")

state_path = Path(os.environ["PVN_TEST_STATE"])
if state_path.exists():
    state = json.loads(state_path.read_text(encoding="utf-8"))
else:
    initial_corosync = Path(os.environ["PVN_TEST_COROSYNC"]).read_text(encoding="utf-8")
    state = {
        "corosync_text": initial_corosync,
        "corosync_runtime": {
            name: initial_corosync for name in ("prox1", "prox2", "prox3")
        },
        "corosync_write_count": 0,
        "corosync_writes": [],
        "corosync_validation_count": 0,
        "block_probes": False,
        "restart_order": [],
        "ledger": None,
        "ledger_sha256": None,
        "nodes": {},
    }

node_state = state["nodes"].setdefault(node, {
    "network": "initial", "staged": False, "journal": False, "phase": None,
    "effective_underlay_mtu": None,
})
if action == "probe" and state.get("block_probes") and os.environ.get(
    "PVN_TEST_CRASH_AFTER_COROSYNC_WRITE_NUMBER"
):
    stop("simulated process crash boundary blocks recovery probes")
if (
    os.environ.get("PVN_TEST_FAIL_FINAL_PROBE_HOST") == node
    and action == "probe"
    and len(state["nodes"]) == 3
    and all(value["network"] == "desired" for value in state["nodes"].values())
):
    stop("simulated final exact-state probe failure")


def save():
    state_path.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")


def drift_membership():
    current = json.loads(members_path.read_text(encoding="utf-8"))
    kind = os.environ.get("PVN_TEST_MEMBERSHIP_DRIFT_KIND", "cluster-version")
    if kind == "members-version":
        current["version"] += 1
    elif kind == "cluster-version":
        current["cluster"]["version"] += 1
    elif kind == "node-name":
        current["nodelist"]["prox4"] = current["nodelist"].pop("prox3")
    elif kind == "node-id":
        current["nodelist"]["prox3"]["id"] = 4
    elif kind == "node-ip":
        current["nodelist"]["prox3"]["ip"] = "192.168.0.77"
    elif kind == "node-offline":
        current["nodelist"]["prox3"]["online"] = 0
    else:
        stop("unknown membership drift kind: %s" % kind)
    members_path.write_text(json.dumps(current, sort_keys=True), encoding="utf-8")


def sha(value):
    return hashlib.sha256(value.encode()).hexdigest()


management = {
    "prox1": "192.168.0.80",
    "prox2": "192.168.0.126",
    "prox3": "192.168.0.78",
}
geneve = {
    "prox1": "192.168.100.25/24",
    "prox2": "192.168.100.54/24",
    "prox3": "192.168.100.163/24",
}
provider = {
    "prox1": "192.168.200.44/24",
    "prox2": "192.168.200.173/24",
    "prox3": "192.168.200.178/24",
}

node_ids = {"prox1": 2, "prox2": 1, "prox3": 3}


def parse_corosync(text):
    import re
    version_match = re.findall(r"^\s*config_version:\s*(\d+)\s*$", text, re.M)
    cluster_match = re.findall(r"^\s*cluster_name:\s*(\S+)\s*$", text, re.M)
    if len(version_match) != 1 or len(cluster_match) != 1:
        stop("fake received malformed Corosync config")
    nodes = {}
    for match in re.finditer(r"(?ms)^\s*node\s*\{.*?^\s*\}", text):
        block = match.group(0)
        names = re.findall(r"^\s*name:\s*(\S+)\s*$", block, re.M)
        ids = re.findall(r"^\s*nodeid:\s*(\d+)\s*$", block, re.M)
        rings = dict(re.findall(r"^\s*ring([0-7])_addr:\s*(\S+)\s*$", block, re.M))
        if len(names) != 1 or len(ids) != 1 or not rings:
            stop("fake received malformed Corosync node")
        nodes[names[0]] = {"node_id": int(ids[0]), "rings": rings}
    return {
        "cluster_name": cluster_match[0],
        "config_version": int(version_match[0]),
        "nodes": nodes,
    }


def runtime_report(local_name):
    configs = {
        name: parse_corosync(text)
        for name, text in state["corosync_runtime"].items()
    }
    local = configs[local_name]
    links = {}
    expected_ids = sorted(node_ids.values())
    for raw_ring, local_address in local["nodes"][local_name]["rings"].items():
        connected = []
        states = {}
        for peer, peer_id in node_ids.items():
            peer_ring = configs[peer]["nodes"].get(peer, {}).get("rings", {}).get(raw_ring)
            expected_peer = local["nodes"].get(peer, {}).get("rings", {}).get(raw_ring)
            is_connected = peer_ring is not None and peer_ring == expected_peer
            if is_connected:
                connected.append(peer_id)
                states[str(peer_id)] = "localhost" if peer == local_name else "connected"
            else:
                states[str(peer_id)] = "disconnected"
        links[raw_ring] = {
            "address": local_address,
            "connected_node_ids": sorted(connected),
            "node_states": states,
        }
    members = {}
    for peer, peer_id in node_ids.items():
        peer_config = configs[peer]
        members[str(peer_id)] = {
            "status": "joined",
            "config_version": peer_config["config_version"],
            "rings": peer_config["nodes"][peer]["rings"],
        }
    return {
        "local_node_id": node_ids[local_name],
        "links": links,
        "config": {
            "cluster_name": local["cluster_name"],
            "config_version": local["config_version"],
            "local_node_id": node_ids[local_name],
            "nodes": local["nodes"],
            "members": members,
            "bind_addresses": local["nodes"][local_name]["rings"],
        },
    }


def bridge_is_full(local_name, request):
    runtime = runtime_report(local_name)
    link = runtime["links"].get(str(request["bridge_ring"]))
    return (
        link is not None
        and link["connected_node_ids"] == sorted(node_ids.values())
        and link["address"] == request["bridge_addresses"][local_name]
    )


def boundary_target_is_full(local_name, target):
    if not isinstance(target, dict):
        return False
    runtime = runtime_report(local_name)["config"]
    if (
        runtime["cluster_name"] != target.get("cluster_name")
        or runtime["config_version"] != target.get("config_version")
        or runtime["nodes"] != target.get("nodes")
        or runtime["bind_addresses"] != target.get("bind_addresses", {}).get(local_name)
    ):
        return False
    expected_ids = {str(row["node_id"]) for row in target["nodes"].values()}
    if set(runtime["members"]) != expected_ids:
        return False
    for name, target_node in target["nodes"].items():
        member = runtime["members"][str(target_node["node_id"])]
        if (
            member["status"] != "joined"
            or member["config_version"] != target["config_version"]
            or member["rings"] != target["bind_addresses"][name]
        ):
            return False
    return True


def replace_node_ring(text, node_name, ring, address):
    import re
    pattern = re.compile(r"(?ms)(^\s*node\s*\{.*?^\s*name:\s*%s\s*$.*?^\s*ring%s_addr:\s*)\S+" % (
        re.escape(node_name), ring,
    ))
    candidate, count = pattern.subn(lambda match: match.group(1) + address, text, count=1)
    if count != 1:
        stop("fake could not corrupt the requested bridge ring")
    return candidate


def report():
    network = node_state["network"]
    interfaces_sha = sha("interfaces-%s-%s" % (node, network))
    pending_sha = sha("interfaces-%s-desired" % node) if node_state["staged"] else None
    bad_mtu_host = os.environ.get("PVN_TEST_UNRESTORABLE_MTU_HOST")
    bad_mtu_role = os.environ.get("PVN_TEST_UNRESTORABLE_MTU_ROLE")
    geneve_configured_mtu = 1450 if node == bad_mtu_host and bad_mtu_role == "geneve" else 1442
    provider_configured_mtu = 1450 if node == bad_mtu_host and bad_mtu_role == "provider" else 1442
    return {
        "node": node,
        "interfaces_sha256": interfaces_sha,
        "pending_interfaces_sha256": pending_sha,
        "corosync_sha256": sha(state["corosync_text"]),
        "corosync_text": state["corosync_text"],
        "corosync_package_version": (
            "3.1.10-pve99"
            if (
                os.environ.get("PVN_TEST_COROSYNC_VERSION_MISMATCH_HOST") == node
                or (
                    os.environ.get("PVN_TEST_COROSYNC_VERSION_DRIFT_AFTER_WRITE_HOST") == node
                    and state.get("corosync_write_count", 0) > 0
                )
            )
            else "3.1.10-pve2"
        ),
        "corosync_runtime": runtime_report(node),
        "management": {
            "interface": "vmbr0",
            "address": management[node] + "/24",
            "default_routes": [{"dst": "default", "gateway": "192.168.0.1", "dev": "vmbr0"}],
        },
        "geneve": {
            "interface": "ens4", "address": geneve[node], "mac": "fa:16:3e:00:00:04",
            "mtu": 1442, "max_mtu": 65535, "configured_mtu": geneve_configured_mtu,
        },
        "provider": {
            "interface": "ens5", "address": provider[node], "mac": "fa:16:3e:00:00:05",
            "mtu": 1442, "max_mtu": 65535, "configured_mtu": provider_configured_mtu,
        },
        "network_state": network,
        "journal_phase": node_state["phase"],
        "ledger_text": state["ledger"],
        "ledger_sha256": state["ledger_sha256"],
        "activation_safe": True,
    }


def emit(**values):
    values["ok"] = True
    print(json.dumps(values, sort_keys=True, separators=(",", ":")))


if action == "probe":
    if node_state["network"] == "desired":
        expected_effective_mtu = request.get("effective_underlay_mtu")
        if expected_effective_mtu is None:
            expected_effective_mtu = node_state.get("effective_underlay_mtu")
        if expected_effective_mtu != 1442:
            stop("desired-state probe lacks the journaled effective underlay MTU")
        bad_bridge = os.environ.get("PVN_TEST_BAD_BRIDGE_MTU")
        if os.environ.get("PVN_TEST_BAD_BRIDGE_MTU_HOST") == node and bad_bridge in {
            "br-int", "br-provider",
        }:
            stop("desired %s MTU differs from the journaled effective MTU" % bad_bridge)
    response = report()
    if (
        os.environ.get("PVN_TEST_DRIFT_BEFORE_LEDGER_HOST") == node
        and len(state["nodes"]) == 3
        and all(value["network"] == "desired" for value in state["nodes"].values())
    ):
        drift_membership()
    emit(report=response)
elif action == "prepare":
    node_state["journal"] = True
    node_state["phase"] = node_state["phase"] or "prepared"
    node_state["effective_underlay_mtu"] = request.get("effective_underlay_mtu")
    save()
    emit(journal={"schema": 1}, report=report())
elif action == "validate-corosync":
    if sha(state["corosync_text"]) != request["expected_sha256"]:
        stop("fake corosync CAS mismatch")
    if sha(request["candidate"]) != request["candidate_sha256"]:
        stop("fake candidate hash mismatch")
    state["corosync_validation_count"] += 1
    validation_count = state["corosync_validation_count"]
    if (
        os.environ.get("PVN_TEST_DRIFT_AFTER_COROSYNC_VALIDATIONS") == "yes"
        and validation_count == 12
    ):
        drifted = request["candidate"].replace("config_version: 4", "config_version: 99", 1)
        state["corosync_text"] = drifted
    if os.environ.get("PVN_TEST_RUNTIME_DRIFT_AFTER_VALIDATION_COUNT") == str(
        validation_count
    ):
        drift_node = os.environ.get(
            "PVN_TEST_RUNTIME_DRIFT_AFTER_VALIDATION_HOST", "prox3"
        )
        state["corosync_runtime"][drift_node] = replace_node_ring(
            state["corosync_runtime"][drift_node], drift_node, 0,
            "203.0.113.254",
        )
    if os.environ.get("PVN_TEST_RUNTIME_VERSION_DRIFT_AFTER_VALIDATION_COUNT") == str(
        validation_count
    ):
        drift_node = os.environ.get(
            "PVN_TEST_RUNTIME_DRIFT_AFTER_VALIDATION_HOST", "prox3"
        )
        state["corosync_runtime"][drift_node] = state["corosync_runtime"][
            drift_node
        ].replace("config_version: 3", "config_version: 2", 1)
    save()
    emit(valid=True)
elif action == "apply-corosync":
    if sha(state["corosync_text"]) != request["expected_sha256"]:
        stop("fake corosync changed")
    if os.environ.get("PVN_TEST_REMOTE_BRIDGE_DRIFT_BEFORE_CAS") == "yes":
        drift_node = os.environ.get("PVN_TEST_REMOTE_CAS_DRIFT_HOST", "prox3")
        state["corosync_runtime"][drift_node] = replace_node_ring(
            state["corosync_runtime"][drift_node], drift_node,
            request["bridge_ring"], "203.0.113.254",
        )
        save()
    if os.environ.get("PVN_TEST_REMOTE_VERSION_DRIFT_BEFORE_CAS") == "yes":
        drift_node = os.environ.get("PVN_TEST_REMOTE_CAS_DRIFT_HOST", node)
        state["corosync_runtime"][drift_node] = re.sub(
            r"(?m)^(\s*config_version:\s*)\d+(\s*)$",
            lambda match: match.group(1) + "1" + match.group(2),
            state["corosync_runtime"][drift_node], count=1,
        )
        save()
    if not bridge_is_full(node, request):
        stop("fake remote CAS rejected a non-N/N safety ring")
    boundary_mode = request.get("runtime_boundary_mode")
    if boundary_mode not in {"exact", "stale-v0.2.13", "partial-rollback"}:
        stop("fake remote CAS received an invalid runtime boundary mode")
    if boundary_mode != "partial-rollback" and not boundary_target_is_full(
        node, request.get("runtime_boundary_target")
    ):
        stop("fake remote CAS rejected runtime boundary drift")
    next_write = state["corosync_write_count"] + 1
    if os.environ.get("PVN_TEST_CRASH_BEFORE_COROSYNC_WRITE_NUMBER") == str(next_write):
        stop("simulated crash before shared Corosync write")
    state["corosync_text"] = request["candidate"]
    state["corosync_write_count"] = next_write
    state["corosync_writes"].append(request["candidate"])
    if os.environ.get("PVN_TEST_CRASH_AFTER_COROSYNC_WRITE_NUMBER") == str(next_write):
        state["block_probes"] = True
    save()
    if state.get("block_probes"):
        stop("simulated crash after shared Corosync write")
    if os.environ.get("PVN_TEST_FAIL_COROSYNC_AFTER_WRITE") == "yes":
        stop("simulated SSH failure after shared Corosync write")
    emit(corosync_sha256=request["candidate_sha256"])
elif action == "reload-corosync":
    if sha(state["corosync_text"]) != request["expected_sha256"]:
        stop("fake reload saw an unexpected shared Corosync config")
    if not bridge_is_full(node, request):
        stop("fake reload safety ring is not fully connected")
    if os.environ.get("PVN_TEST_FAIL_RELOAD_BEFORE_APPLY") == "yes":
        stop("simulated reload failure before runtime change")
    partial = {
        value for value in os.environ.get("PVN_TEST_PARTIAL_RELOAD_HOSTS", "").split(",")
        if value
    }
    for name in state["corosync_runtime"]:
        if name not in partial:
            state["corosync_runtime"][name] = state["corosync_text"]
    save()
    if os.environ.get("PVN_TEST_FAIL_RELOAD_AFTER_APPLY") == "yes":
        stop("simulated lost response after cluster-wide runtime reload")
    emit(reloaded=True, corosync_sha256=sha(state["corosync_text"]))
elif action == "restart-corosync":
    if sha(state["corosync_text"]) != request["expected_sha256"]:
        stop("fake restart saw an unexpected shared Corosync config")
    if not bridge_is_full(node, request):
        stop("fake restart safety ring is not fully connected")
    if os.environ.get("PVN_TEST_FAIL_RESTART_BEFORE_HOST") == node:
        stop("simulated restart failure before runtime change")
    state["corosync_runtime"][node] = state["corosync_text"]
    state["restart_order"].append(node)
    if os.environ.get("PVN_TEST_BREAK_BRIDGE_AFTER_RESTART_HOST") == node:
        broken_node = os.environ.get("PVN_TEST_BREAK_BRIDGE_NODE", "prox3")
        state["corosync_runtime"][broken_node] = replace_node_ring(
            state["corosync_runtime"][broken_node], broken_node,
            request["bridge_ring"], "203.0.113.254",
        )
    save()
    if not bridge_is_full(node, request):
        stop("fake restarted node did not rejoin the safety ring")
    if os.environ.get("PVN_TEST_FAIL_RESTART_AFTER_HOST") == node:
        stop("simulated lost response after guarded restart")
    emit(restarted=True, corosync_sha256=sha(state["corosync_text"]))
elif action == "verify-cluster":
    if sha(state["corosync_text"]) not in request["allowed_corosync_sha256"]:
        stop("fake cluster has unexpected corosync hash")
    emit(verified=True, corosync_sha256=sha(state["corosync_text"]))
elif action == "record-phase":
    node_state["phase"] = request["phase"]
    if (
        request["phase"] == "corosync-migrated"
        and os.environ.get("PVN_TEST_DRIFT_AFTER_MIGRATED_PHASE_HOST") == node
    ):
        state["corosync_runtime"][node] = Path(
            os.environ["PVN_TEST_COROSYNC"]
        ).read_text(encoding="utf-8")
    save()
    emit(phase=request["phase"])
elif action == "stage-network":
    if request.get("effective_underlay_mtu") != 1442:
        stop("hardware max_mtu was used instead of the safe live MTU")
    if request["geneve"].get("mtu") != 1442 or request["provider"].get("mtu") != 1442:
        stop("stage request did not preserve live NIC MTUs")
    if node_state["network"] == "desired":
        emit(noop=True, desired_interfaces_sha256=sha("interfaces-%s-desired" % node))
    else:
        node_state["staged"] = True
        node_state["phase"] = "network-staged"
        save()
        if os.environ.get("PVN_TEST_DRIFT_AFTER_STAGE_HOST") == node:
            drift_membership()
        if os.environ.get("PVN_TEST_FAIL_STAGE_RESPONSE_HOST") == node:
            stop("simulated lost response after remote network staging")
        emit(noop=False, desired_interfaces_sha256=sha("interfaces-%s-desired" % node))
elif action == "apply-network":
    if not node_state["staged"]:
        stop("fake network was not staged")
    node_state["staged"] = False
    node_state["network"] = "desired"
    node_state["phase"] = "network-applied-unverified"
    save()
    emit(noop=False, interfaces_sha256=sha("interfaces-%s-desired" % node))
elif action == "verify-network":
    if node_state["network"] != "desired":
        stop("fake network is not desired")
    emit(verified=True, report=report())
elif action == "rollback-network":
    node_state["staged"] = False
    node_state["network"] = "initial"
    node_state["phase"] = "network-rolled-back"
    save()
    emit(noop=False, interfaces_sha256=sha("interfaces-%s-initial" % node))
elif action == "discard-stage":
    was_staged = node_state["staged"]
    node_state["staged"] = False
    save()
    emit(noop=not was_staged)
elif action == "write-ledger":
    if state["ledger_sha256"] != request["expected_ledger_sha256"]:
        stop("fake ledger CAS mismatch")
    if sha(request["ledger"]) != request["ledger_sha256"]:
        stop("fake ledger hash mismatch")
    state["ledger"] = request["ledger"]
    state["ledger_sha256"] = request["ledger_sha256"]
    save()
    emit(noop=False, ledger_sha256=state["ledger_sha256"])
elif action == "verify-ledger":
    if state["ledger_sha256"] != request["ledger_sha256"]:
        stop("fake ledger differs")
    emit(ledger_sha256=state["ledger_sha256"])
elif action == "restore-ledger":
    current = state["ledger_sha256"]
    original = request.get("original_ledger")
    original_sha = request.get("original_ledger_sha256")
    if current == original_sha:
        emit(noop=True, ledger_sha256=current)
    elif current != request.get("failed_ledger_sha256"):
        stop("fake ledger rollback CAS mismatch")
    else:
        if original is not None and sha(original) != original_sha:
            stop("fake original ledger hash mismatch")
        state["ledger"] = original
        state["ledger_sha256"] = original_sha
        save()
        emit(noop=False, ledger_sha256=original_sha)
else:
    stop("unexpected remote action %s" % action)
PY
chmod 0755 "$BIN/ssh"

export PVN_TOPOLOGY_MEMBERS_FILE=$MEMBERS
export PVN_TOPOLOGY_NODES_DIR=$PVE_NODES
export PVN_TOPOLOGY_IDENTITY_FILE=$PVE_IDENTITY
export PVN_TOPOLOGY_LOCK_FILE=$LOCK
export PVN_TOPOLOGY_LEASE_BIN=$REPO/deploy/scripts/pvn-cluster-lease
export PVN_TOPOLOGY_SSH_BIN=$BIN/ssh
export PVN_TOPOLOGY_TEST_FAST=1
export PVN_TEST_STATE=$STATE
export PVN_TEST_COROSYNC=$COROSYNC
export PVN_TEST_LOG=$LOG
export PVN_TEST_GLOBAL_LOCK=$GLOBAL_LOCK
export PVN_TEST_REMOTE_HELPER=$REMOTE_HELPER
export PVN_CLUSTER_LEASE_DIR=$LEASE_DIR
export PERL5LIB=$PERL_DIR

GENEVE=192.168.100.0/24
PROVIDER=192.168.200.0/24
ACK=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP

fail() {
    echo "pvn-topology test failed: $*" >&2
    exit 1
}

reset_state() {
    rm -f "$STATE" "$LOCK" "$GLOBAL_LOCK"
    cp "$MEMBERS_GOOD" "$MEMBERS"
    : > "$LOG"
}

assert_no_mutation() {
    if grep -Eq 'action=(prepare|validate-corosync|apply-corosync|reload-corosync|restart-corosync|record-phase|stage-network|apply-network|rollback-network|discard-stage|write-ledger|restore-ledger)' "$LOG"; then
        fail "read-only/invalid invocation attempted a mutating remote action"
    fi
}

PYTHONDONTWRITEBYTECODE=1 python3 -c \
    "compile(open('$TOPOLOGY', encoding='utf-8').read(), '$TOPOLOGY', 'exec')"

reset_state
"$TOPOLOGY" --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" > "$WORK/plan.out"
[ ! -e "$LOCK" ] || fail "read-only plan created a lock/journal artifact"
[ ! -e "$GLOBAL_LOCK" ] || fail "read-only plan created a cluster-global lock"
[ "$(grep -c 'action=probe' "$LOG")" -eq 3 ] || fail "plan did not probe all online members"
[ "$(grep -c 'transport=stdin argv_request=0' "$LOG")" -eq 3 ] ||
    fail "remote requests were not carried exclusively through stdin"
for node_version in prox1:9 prox2:11 prox3:5; do
    node=${node_version%:*}
    version=${node_version#*:}
    grep -q "node=$node action=probe .*members_version=$version" "$LOG" ||
        fail "plan did not tolerate node-local members version $version on $node"
done
assert_no_mutation
grep -q 'guest-mtu=1300 ceiling=1342' "$WORK/plan.out" || fail "safe default guest MTU is wrong"
grep -q 'live-mtu=1442 hardware-max-mtu=65535' "$WORK/plan.out" ||
    fail "plan did not distinguish safe live MTU from hardware max_mtu"
grep -Fq "PVE::Tools::lock_file(\$network_lock, 10" "$REMOTE_HELPER" ||
    fail "network candidate changes do not use the native PVE interfaces lock"
grep -Fq "PVE::INotify::write_file('interfaces', \$config)" "$REMOTE_HELPER" ||
    fail "network candidate is not written once through PVE::INotify"
grep -Fq 'rename($pending, $interfaces)' "$REMOTE_HELPER" ||
    fail "network apply is not an atomic owned-candidate rename"
grep -Fq "cfs_lock_file('corosync.conf', 10" "$REMOTE_HELPER" ||
    fail "Corosync CAS does not use its native pmxcfs file lock"
grep -Fq 'sys.stdin.buffer.read(REMOTE_REQUEST_LIMIT + 1)' "$REMOTE_HELPER" ||
    fail "remote request stdin is not bounded"
if grep -Fq 'sys.argv[2]' "$REMOTE_HELPER" || grep -Fq 'bytes.fromhex' "$REMOTE_HELPER"; then
    fail "remote request metadata is still decoded from process argv"
fi
if grep -Fq '"members_version":' "$REMOTE_HELPER"; then
    fail "node-local PVE members version is still part of the remote equality guard"
fi
if grep -Fq 'pvesh("delete", "/nodes/%s/network"' "$REMOTE_HELPER"; then
    fail "remote helper still has broad pending-network deletion"
fi
if grep -Fq 'str(request["geneve"]["max_mtu"])' "$TOPOLOGY" ||
    grep -Fq 'str(request["provider"]["max_mtu"])' "$TOPOLOGY"
then
    fail "topology staging can raise a NIC to its hardware max_mtu"
fi
grep -q 'Corosync: dual -> final' "$WORK/plan.out" || fail "dual-ring Corosync migration was not planned"
grep -q 'cannot live-test arbitrary inner MAC/IP' "$WORK/plan.out" || fail "provider readiness warning is missing"
grep -q -- "--provider-port-ready $ACK --confirm lab-cluster" "$WORK/plan.out" ||
    fail "exact provider and cluster confirmation was not printed"

for option in \
    BatchMode=yes PasswordAuthentication=no KbdInteractiveAuthentication=no \
    IdentitiesOnly=yes StrictHostKeyChecking=yes CheckHostIP=no \
    VerifyHostKeyDNS=no UpdateHostKeys=no GlobalKnownHostsFile=/dev/null
do
    grep -q "$option" "$LOG" || fail "strict SSH option $option is missing"
done
for node in prox1 prox2 prox3; do
    grep -q "HostKeyAlias=$node" "$LOG" || fail "node-specific HostKeyAlias $node was not pinned"
    grep -q "UserKnownHostsFile=$PVE_NODES/$node/ssh_known_hosts" "$LOG" ||
        fail "node-specific PVE known_hosts for $node was not pinned"
done
grep -q -- "-F /dev/null -e none -i $PVE_IDENTITY" "$LOG" ||
    fail "PVE native identity or isolated SSH config was not enforced"

cp "$PVE_NODES/prox3/ssh_known_hosts" "$WORK/prox3-known-hosts.good"
printf '%s\n' 'wrong-alias ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestwrong' \
    > "$PVE_NODES/prox3/ssh_known_hosts"
: > "$LOG"
if "$TOPOLOGY" --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    > "$WORK/wrong-host-key-alias.out" 2>&1
then
    fail "mismatched PVE HostKeyAlias pin unexpectedly passed"
fi
[ ! -s "$LOG" ] || fail "invalid PVE host-key pin contacted a node"
cp "$WORK/prox3-known-hosts.good" "$PVE_NODES/prox3/ssh_known_hosts"

: > "$LOG"
if "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready wrong --confirm lab-cluster > "$WORK/wrong-provider.out" 2>&1
then
    fail "wrong provider acknowledgement unexpectedly applied"
fi
[ ! -s "$LOG" ] || fail "wrong provider acknowledgement contacted a node"

: > "$LOG"
if "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm wrong-cluster > "$WORK/wrong-cluster.out" 2>&1
then
    fail "wrong cluster confirmation unexpectedly applied"
fi
[ ! -s "$LOG" ] || fail "wrong cluster confirmation contacted a node"

: > "$LOG"
if "$TOPOLOGY" --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --guest-mtu 1400 > "$WORK/high-mtu.out" 2>&1
then
    fail "guest MTU above the derived ceiling unexpectedly passed"
fi
assert_no_mutation

reset_state
printf '%s\n' '{"cluster":"other","domain":"mutation","node":"prox3","pid":123,"started":1,"transaction":"other","token":"abcdef0123456789abcdef0123456789abcdef0123456789"}' \
    > "$GLOBAL_LOCK"
chmod 0600 "$GLOBAL_LOCK"
stale_hash=$(sha256sum "$GLOBAL_LOCK" | awk '{print $1}')
if "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/stale-lock.out" 2>&1
then
    fail "existing cluster-global topology lock unexpectedly applied"
fi
[ "$(sha256sum "$GLOBAL_LOCK" | awk '{print $1}')" = "$stale_hash" ] ||
    fail "existing cluster-global topology lock was modified or removed"
[ ! -s "$LOG" ] || fail "existing cluster-global lock contacted a node"
grep -q 'release it only if stale' "$WORK/stale-lock.out" ||
    fail "stale cluster-global lock did not print recovery guidance"

reset_state
cp "$COROSYNC" "$WORK/corosync.geneve"
sed -e 's/192\.168\.100\.25/192.168.200.44/' \
    -e 's/192\.168\.100\.54/192.168.200.173/' \
    -e 's/192\.168\.100\.163/192.168.200.178/' \
    "$WORK/corosync.geneve" > "$COROSYNC"
if "$TOPOLOGY" --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    > "$WORK/provider-corosync.out" 2>&1
then
    fail "provider NIC used by Corosync unexpectedly passed"
fi
assert_no_mutation
grep -q 'uses a provider NIC' "$WORK/provider-corosync.out" ||
    fail "provider/Corosync rejection reason is missing"
cp "$WORK/corosync.geneve" "$COROSYNC"

reset_state
if PVN_TEST_COROSYNC_VERSION_MISMATCH_HOST=prox3 "$TOPOLOGY" \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    > "$WORK/corosync-version-mismatch.out" 2>&1
then
    fail "mixed Corosync package versions unexpectedly passed planning"
fi
assert_no_mutation
grep -q 'do not run one exact Corosync package version' \
    "$WORK/corosync-version-mismatch.out" ||
    fail "mixed Corosync package version rejection reason is missing"

reset_state
if PVN_TEST_COROSYNC_VERSION_DRIFT_AFTER_WRITE_HOST=prox3 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster \
    > "$WORK/corosync-version-drift.out" 2>&1
then
    fail "Corosync package version drift after CAS unexpectedly applied"
fi
[ "$(grep -c 'action=apply-corosync' "$LOG")" -eq 1 ] ||
    fail "Corosync package drift did not stop at the first shared write boundary"
if grep -Eq 'action=(reload-corosync|restart-corosync|stage-network|apply-network|write-ledger)' "$LOG"; then
    fail "runtime/network mutation ran after Corosync package version drift"
fi

for mtu_role in geneve provider; do
    reset_state
    if PVN_TEST_UNRESTORABLE_MTU_HOST=prox2 PVN_TEST_UNRESTORABLE_MTU_ROLE=$mtu_role \
        "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
        --provider-port-ready "$ACK" --confirm lab-cluster \
        > "$WORK/unrestorable-$mtu_role-mtu.out" 2>&1
    then
        fail "$mtu_role configured MTU above the live-restorable ceiling unexpectedly applied"
    fi
    assert_no_mutation
    grep -q "configured MTU 1450 on prox2 $mtu_role NIC" \
        "$WORK/unrestorable-$mtu_role-mtu.out" ||
        fail "$mtu_role unrestorable MTU rejection reason is missing"
    grep -q 'exceeds its live-restorable MTU 1442' \
        "$WORK/unrestorable-$mtu_role-mtu.out" ||
        fail "$mtu_role unrestorable MTU ceiling is missing"
    [ ! -e "$GLOBAL_LOCK" ] ||
        fail "$mtu_role unrestorable MTU preflight left the cluster-global lock behind"
done

reset_state
PVN_TEST_MEMBERSHIP_DRIFT_KIND=members-version PVN_TEST_DRIFT_AFTER_STAGE_HOST=prox2 \
    "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/apply.out"
[ ! -e "$GLOBAL_LOCK" ] || fail "successful apply left the cluster-global lock behind"
if grep -q 'lease=0' "$LOG"; then
    fail "cluster-global lease was not held for the entire apply"
fi
[ "$(grep -c 'action=prepare' "$LOG")" -eq 3 ] || fail "journals/backups were not prepared on all nodes"
[ "$(grep -c 'action=validate-corosync' "$LOG")" -eq 12 ] || fail "all Corosync transition/rollback candidates were not validated everywhere"
[ "$(grep -c 'action=apply-corosync' "$LOG")" -eq 2 ] || fail "dual and final shared Corosync configs were not each applied once"
[ "$(grep -c 'action=reload-corosync' "$LOG")" -eq 2 ] || fail "dual and final Corosync runtimes were not reloaded"
[ "$(grep -c 'action=stage-network' "$LOG")" -eq 3 ] || fail "all candidates were not staged before apply"
[ "$(grep -c 'action=apply-network' "$LOG")" -eq 3 ] || fail "network was not applied one node at a time"
[ "$(grep -c 'action=verify-network' "$LOG")" -eq 3 ] || fail "each applied node was not verified"
[ "$(grep -c 'action=write-ledger' "$LOG")" -eq 1 ] || fail "shared ledger was not written exactly once"
[ "$(grep -c 'action=verify-ledger' "$LOG")" -eq 3 ] || fail "shared ledger was not read back on every node"
for node_version in prox1:9 prox2:11 prox3:5; do
    node=${node_version%:*}
    version=${node_version#*:}
    grep -q "node=$node .*members_version=$version" "$LOG" ||
        fail "apply did not tolerate node-local members version $version on $node"
done
grep -q 'PVN activation markers and central/node targets remain untouched' "$WORK/apply.out" ||
    fail "apply did not report the activation boundary"

python3 - "$STATE" "$ACK" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
ledger = json.loads(state["ledger"])
assert ledger["schema"] == 2
assert ledger["phase"] == "complete"
assert ledger["guest_mtu"] == 1300
assert ledger["effective_underlay_mtu"] == 1442
assert ledger["provider_bridge"] == "br-provider"
assert ledger["physnet"] == "provider"
assert ledger["provider_readiness"] == {
    "operator_ack": True,
    "ack_phrase": sys.argv[2],
    "live_arbitrary_mac_l2_verified": False,
    "basis": "operator-ack-only",
}
assert len(ledger["nodes"]) == 3
assert all(row["control_ip"] == row["management_ip"] for row in ledger["nodes"])
assert len(ledger["membership_hash"]) == 64
assert len(ledger["corosync"]["sha256"]) == 64
assert ledger["corosync"]["config_version"] == 5
assert set(ledger["corosync"]["rings"]) == {"1"}
PY

: > "$LOG"
"$TOPOLOGY" --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    > "$WORK/completed-plan.out"
[ "$(grep -c 'action=probe' "$LOG")" -eq 3 ] ||
    fail "completed topology plan did not probe every desired node"
assert_no_mutation

for bridge in br-int br-provider; do
    : > "$LOG"
    if PVN_TEST_BAD_BRIDGE_MTU_HOST=prox2 PVN_TEST_BAD_BRIDGE_MTU=$bridge \
        "$TOPOLOGY" --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
        > "$WORK/completed-$bridge-mtu.out" 2>&1
    then
        fail "completed topology plan accepted a mismatched $bridge MTU"
    fi
    assert_no_mutation
    grep -q "desired $bridge MTU differs from the journaled effective MTU" \
        "$WORK/completed-$bridge-mtu.out" ||
        fail "completed topology $bridge MTU mismatch was not reported"
done

: > "$LOG"
"$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/rerun.out"
if grep -Eq 'action=(validate-corosync|apply-corosync|reload-corosync|restart-corosync|stage-network|apply-network|rollback-network)' "$LOG"; then
    fail "exact-state rerun repeated a Corosync/network mutation"
fi
[ "$(grep -c 'exact network state already present; no-op' "$WORK/rerun.out")" -eq 3 ] ||
    fail "exact-state rerun did not report all no-ops"

reset_state
PVN_TEST_PARTIAL_RELOAD_HOSTS=prox1,prox2,prox3 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/partial-reload.out"
python3 - "$STATE" "$LOG" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["restart_order"] == ["prox1", "prox3", "prox2"] * 2
lines = open(sys.argv[2], encoding="utf-8").read().splitlines()
last_restart = max(i for i, line in enumerate(lines) if "action=restart-corosync" in line)
first_network = min(i for i, line in enumerate(lines) if "action=stage-network" in line)
assert last_restart < first_network
assert state["ledger"] is not None
PY

reset_state
PVN_TEST_PARTIAL_RELOAD_HOSTS=prox2 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/coordinator-reload.out"
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["restart_order"] == ["prox2", "prox2"]
PY
if grep -Eq 'node=(prox1|prox3) action=restart-corosync' "$LOG"; then
    fail "healthy non-coordinator restarted when only coordinator was stale"
fi

reset_state
PVN_TEST_PARTIAL_RELOAD_HOSTS=prox3 PVN_TEST_FAIL_RESTART_AFTER_HOST=prox3 \
    "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/restart-response-loss.out"
[ "$(grep -c 'node=prox3 action=restart-corosync' "$LOG")" -eq 2 ] ||
    fail "lost restart responses repeated or skipped the proven node restart"
grep -q 'action=write-ledger' "$LOG" ||
    fail "restart response-loss recovery did not reach the exact final ledger"

reset_state
if PVN_TEST_PARTIAL_RELOAD_HOSTS=prox1,prox2,prox3 \
    PVN_TEST_BREAK_BRIDGE_AFTER_RESTART_HOST=prox1 \
    "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/bridge-loss.out" 2>&1
then
    fail "Corosync safety-ring loss unexpectedly reached network mutation"
fi
[ "$(grep -c 'action=restart-corosync' "$LOG")" -eq 1 ] ||
    fail "another Corosync node restarted after the safety ring lost N/N"
if grep -Eq 'action=(stage-network|apply-network|write-ledger)' "$LOG"; then
    fail "network/ledger mutation ran after Corosync safety-ring loss"
fi

reset_state
if PVN_TEST_CRASH_BEFORE_COROSYNC_WRITE_NUMBER=2 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/dual-crash.out" 2>&1
then
    fail "simulated crash before final Corosync write unexpectedly completed"
fi
python3 - "$STATE" <<'PY'
import json
import re
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["corosync_write_count"] == 1
assert re.search(r"ring0_addr:\s+192\.168\.100\.", state["corosync_text"])
assert re.search(r"ring1_addr:\s+192\.168\.0\.", state["corosync_text"])
assert all(value["network"] == "initial" for value in state["nodes"].values())
PY
: > "$LOG"
"$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/dual-resume.out"
grep -q 'action=write-ledger' "$LOG" || fail "dual-boundary rerun did not resume to completion"

reset_state
if PVN_TEST_CRASH_AFTER_COROSYNC_WRITE_NUMBER=2 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/final-crash.out" 2>&1
then
    fail "simulated crash after final Corosync write unexpectedly completed"
fi
python3 - "$STATE" <<'PY'
import json
import re
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["corosync_write_count"] == 2
assert not re.search(r"ring0_addr:\s+192\.168\.100\.", state["corosync_text"])
assert re.search(r"ring1_addr:\s+192\.168\.0\.", state["corosync_text"])
assert all(value["network"] == "initial" for value in state["nodes"].values())
PY
: > "$LOG"
"$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/final-resume.out"
grep -q 'action=write-ledger' "$LOG" || fail "final-boundary rerun did not resume to completion"

reset_state
python3 - "$COROSYNC" "$STATE" <<'PY'
import json
import sys
from pathlib import Path
original = Path(sys.argv[1]).read_text(encoding="utf-8")
persisted = original.replace("config_version: 3", "config_version: 4")
for old, new in (
    ("192.168.100.25", "192.168.0.80"),
    ("192.168.100.54", "192.168.0.126"),
    ("192.168.100.163", "192.168.0.78"),
):
    persisted = persisted.replace(old, new)
state = {
    "corosync_text": persisted,
    "corosync_runtime": {name: original for name in ("prox1", "prox2", "prox3")},
    "corosync_write_count": 0,
    "corosync_writes": [],
    "corosync_validation_count": 0,
    "block_probes": False,
    "restart_order": [],
    "ledger": None,
    "ledger_sha256": None,
    "nodes": {},
}
Path(sys.argv[2]).write_text(json.dumps(state, sort_keys=True), encoding="utf-8")
PY
cp "$STATE" "$WORK/stale-state.initial"
"$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/stale-final-recovery.out"
python3 - "$STATE" <<'PY'
import json
import re
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["corosync_write_count"] == 2
dual = state["corosync_writes"][0]
assert "config_version: 5" in dual
assert re.search(r"ring0_addr:\s+192\.168\.100\.", dual)
assert re.search(r"ring1_addr:\s+192\.168\.0\.", dual)
assert "config_version: 6" in state["corosync_text"]
assert not re.search(r"ring0_addr:\s+192\.168\.100\.", state["corosync_text"])
assert state["ledger"] is not None
PY

reset_state
cp "$WORK/stale-state.initial" "$STATE"
if PVN_TEST_RUNTIME_VERSION_DRIFT_AFTER_VALIDATION_COUNT=9 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster \
    > "$WORK/stale-pre-cas-version-drift.out" 2>&1
then
    fail "stale v0.2.13 runtime version drift unexpectedly applied"
fi
if grep -q 'action=apply-corosync' "$LOG"; then
    fail "stale recovery CAS ran after pinned runtime version drift"
fi
grep -q 'stale v0.2.13 Corosync recovery boundary changed' \
    "$WORK/stale-pre-cas-version-drift.out" ||
    fail "stale recovery pre-CAS version gate did not report drift"

reset_state
cp "$WORK/stale-state.initial" "$STATE"
if PVN_TEST_REMOTE_VERSION_DRIFT_BEFORE_CAS=yes "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster \
    > "$WORK/stale-remote-cas-version-drift.out" 2>&1
then
    fail "remote stale-version TOCTOU unexpectedly wrote the dual candidate"
fi
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["corosync_write_count"] == 0
assert state["ledger"] is None
PY
grep -q 'fake remote CAS rejected runtime boundary drift' \
    "$WORK/stale-remote-cas-version-drift.out" ||
    fail "remote stale-version CAS gate did not report the TOCTOU"

reset_state
if PVN_TEST_DRIFT_AFTER_COROSYNC_VALIDATIONS=yes "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/corosync-cas-drift.out" 2>&1
then
    fail "unplanned shared Corosync drift was overwritten"
fi
if grep -q 'action=apply-corosync' "$LOG"; then
    fail "pinned Corosync CAS wrote over external drift"
fi
if grep -Eq 'action=(stage-network|apply-network|write-ledger)' "$LOG"; then
    fail "network/ledger mutation ran after external Corosync drift"
fi

reset_state
if PVN_TEST_RUNTIME_DRIFT_AFTER_VALIDATION_COUNT=12 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster \
    > "$WORK/pre-cas-bridge-drift.out" 2>&1
then
    fail "safety-ring drift immediately before Corosync CAS unexpectedly applied"
fi
if grep -q 'action=apply-corosync' "$LOG"; then
    fail "Corosync CAS ran after its pre-write safety ring lost N/N"
fi
grep -q 'not N/N immediately before config CAS' "$WORK/pre-cas-bridge-drift.out" ||
    fail "pre-CAS safety-ring rejection reason is missing"

reset_state
if PVN_TEST_REMOTE_BRIDGE_DRIFT_BEFORE_CAS=yes "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster \
    > "$WORK/remote-cas-bridge-drift.out" 2>&1
then
    fail "remote safety-ring TOCTOU unexpectedly wrote a Corosync candidate"
fi
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["corosync_write_count"] == 0
assert state["ledger"] is None
PY
grep -q 'fake remote CAS rejected a non-N/N safety ring' \
    "$WORK/remote-cas-bridge-drift.out" ||
    fail "remote bridge CAS gate did not report the TOCTOU"

reset_state
if PVN_TEST_DRIFT_AFTER_MIGRATED_PHASE_HOST=prox3 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/runtime-toctou.out" 2>&1
then
    fail "runtime drift after final phase unexpectedly reached network mutation"
fi
grep -q 'persisted/runtime state is not exact before network mutation' "$WORK/runtime-toctou.out" ||
    fail "pre-network exact Corosync gate did not report runtime drift"
if grep -Eq 'action=(stage-network|apply-network|write-ledger)' "$LOG"; then
    fail "network/ledger mutation ran after pre-network Corosync runtime drift"
fi

reset_state
PVN_TEST_FAIL_COROSYNC_AFTER_WRITE=yes "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/corosync-write-loss.out" 2>&1
[ ! -e "$GLOBAL_LOCK" ] || fail "recovered Corosync response loss left the cluster-global lock behind"
[ "$(grep -c 'action=apply-corosync' "$LOG")" -eq 2 ] ||
    fail "lost shared-write responses repeated a committed Corosync CAS"
grep -q 'action=write-ledger' "$LOG" ||
    fail "exact-state recovery after Corosync write response loss did not complete"

if [ "${PVN_TOPOLOGY_CLUSTERED_ONLY:-0}" = 1 ]; then
    echo "pvn-topology clustered Corosync tests passed"
    exit 0
fi

reset_state
if PVN_TEST_FAIL_STAGE_RESPONSE_HOST=prox2 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/stage-response-loss.out" 2>&1
then
    fail "lost stage response unexpectedly succeeded"
fi
grep -q 'node=prox2 action=stage-network' "$LOG" ||
    fail "stage response-loss injection did not run"
for node in prox1 prox2 prox3; do
    grep -q "node=$node action=discard-stage" "$LOG" ||
        fail "prepared node $node was not included in pending cleanup sweep"
done
if grep -q 'action=apply-network' "$LOG"; then
    fail "network apply ran after the lost stage response"
fi
grep -q 'all transaction-owned network changes were rolled back' "$WORK/stage-response-loss.out" ||
    fail "successful response-loss cleanup was not reported"
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert all(not value["staged"] for value in state["nodes"].values())
assert all(value["network"] == "initial" for value in state["nodes"].values())
assert state["ledger"] is None
PY

reset_state
if PVN_TEST_FAIL_STAGE_RESPONSE_HOST=prox2 PVN_TEST_FAIL_DISCARD_HOST=prox2 \
    "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/stage-cleanup-fail.out" 2>&1
then
    fail "lost stage response with failed cleanup unexpectedly succeeded"
fi
grep -q 'transaction rollback was incomplete' "$WORK/stage-cleanup-fail.out" ||
    fail "failed pending cleanup was falsely reported as a complete rollback"
if grep -q 'all transaction-owned network changes were rolled back' "$WORK/stage-cleanup-fail.out"; then
    fail "incomplete pending cleanup printed the complete rollback message"
fi
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert state["nodes"]["prox2"]["staged"] is True
assert all(value["network"] == "initial" for value in state["nodes"].values())
PY

for drift_kind in cluster-version node-name node-id node-ip node-offline; do
    reset_state
    if PVN_TEST_MEMBERSHIP_DRIFT_KIND=$drift_kind PVN_TEST_DRIFT_AFTER_STAGE_HOST=prox2 \
        "$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
        --provider-port-ready "$ACK" --confirm lab-cluster \
        > "$WORK/membership-stage-drift-$drift_kind.out" 2>&1
    then
        fail "$drift_kind membership drift after network staging unexpectedly succeeded"
    fi
    grep -Eq 'PVE membership changed before network staging|every cluster member must be online' \
        "$WORK/membership-stage-drift-$drift_kind.out" ||
        fail "$drift_kind membership drift did not fail closed at the next mutation boundary"
    [ "$(grep -c 'action=stage-network' "$LOG")" -eq 1 ] ||
        fail "another network candidate was staged after $drift_kind membership drift"
    for node in prox1 prox2 prox3; do
        grep -q "node=$node action=discard-stage" "$LOG" ||
            fail "$drift_kind membership drift did not sweep prepared node $node"
    done
    python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert all(not value["staged"] for value in state["nodes"].values())
assert all(value["network"] == "initial" for value in state["nodes"].values())
assert state["ledger"] is None
PY
done

reset_state
if PVN_TEST_MEMBERSHIP_DRIFT_KIND=node-ip PVN_TEST_DRIFT_BEFORE_LEDGER_HOST=prox3 \
    "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/membership-ledger-drift.out" 2>&1
then
    fail "membership drift before ledger publication unexpectedly succeeded"
fi
grep -q 'PVE membership changed before final ledger publication' \
    "$WORK/membership-ledger-drift.out" ||
    fail "final membership drift did not stop ledger publication"
if grep -q 'action=write-ledger' "$LOG"; then
    fail "topology ledger was published after final membership drift"
fi
[ "$(grep -c 'action=rollback-network' "$LOG")" -eq 3 ] ||
    fail "final membership drift did not roll back every applied network"
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert all(value["network"] == "initial" for value in state["nodes"].values())
assert state["ledger"] is None
PY

reset_state
if PVN_TEST_FAIL_VERIFY_HOST=prox1 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/rollback.out" 2>&1
then
    fail "post-ifreload verification failure unexpectedly succeeded"
fi
grep -q 'node=prox1 action=rollback-network' "$LOG" || fail "failed current node was not rolled back"
grep -q 'node=prox2 action=rollback-network' "$LOG" || fail "previously applied node was not rolled back"
[ ! -e "$GLOBAL_LOCK" ] || fail "failed network apply left the cluster-global lock behind"
if grep -q 'node=prox3 action=apply-network' "$LOG"; then
    fail "later node applied after an earlier node failed"
fi
grep -q 'node=prox3 action=discard-stage' "$LOG" || fail "later staged candidate was not discarded"
if grep -q 'action=write-ledger' "$LOG"; then
    fail "failure path published a complete topology ledger"
fi
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert all(value["network"] == "initial" for value in state["nodes"].values())
assert state["ledger"] is None
PY

reset_state
if PVN_TEST_FAIL_FINAL_PROBE_HOST=prox2 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/final-sweep-rollback.out" 2>&1
then
    fail "final exact-state sweep failure unexpectedly succeeded"
fi
[ "$(grep -c 'action=rollback-network' "$LOG")" -eq 3 ] ||
    fail "final sweep failure did not roll back every applied node"
if grep -q 'action=write-ledger' "$LOG"; then
    fail "final sweep failure published the topology ledger"
fi
[ ! -e "$GLOBAL_LOCK" ] || fail "final sweep failure left the cluster-global lock behind"

reset_state
if PVN_TEST_FAIL_LEDGER_VERIFY_HOST=prox1 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/ledger-rollback.out" 2>&1
then
    fail "shared ledger verification failure unexpectedly succeeded"
fi
grep -q 'action=restore-ledger' "$LOG" || fail "failed shared ledger was not restored"
[ "$(grep -c 'action=rollback-network' "$LOG")" -eq 3 ] ||
    fail "ledger failure did not roll back every applied node"
[ ! -e "$GLOBAL_LOCK" ] || fail "ledger failure left the cluster-global lock behind"
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert all(value["network"] == "initial" for value in state["nodes"].values())
assert state["ledger"] is None
assert state["ledger_sha256"] is None
PY

reset_state
if PVN_TEST_FAIL_COMPLETE_PHASE_HOST=prox1 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/phase-rollback.out" 2>&1
then
    fail "final journal phase failure unexpectedly succeeded"
fi
grep -q 'action=restore-ledger' "$LOG" || fail "phase failure did not restore the shared ledger"
[ "$(grep -c 'action=rollback-network' "$LOG")" -eq 3 ] ||
    fail "phase failure did not roll back every applied node"
python3 - "$STATE" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
assert all(value["network"] == "initial" for value in state["nodes"].values())
assert state["ledger"] is None
PY

reset_state
if PVN_TEST_PROBE_FAIL_HOST=prox1 "$TOPOLOGY" \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" > "$WORK/probe-fail.out" 2>&1
then
    fail "unsafe node probe unexpectedly passed"
fi
assert_no_mutation

cp "$MEMBERS" "$WORK/offline-members.json"
sed -i 's/"prox3": {"id": 3, "online": 1/"prox3": {"id": 3, "online": 0/' "$WORK/offline-members.json"
: > "$LOG"
if PVN_TOPOLOGY_MEMBERS_FILE=$WORK/offline-members.json "$TOPOLOGY" \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" > "$WORK/offline.out" 2>&1
then
    fail "offline cluster member unexpectedly passed discovery"
fi
[ ! -s "$LOG" ] || fail "offline membership contacted a node"

if grep -Eq 'systemctl[^\n]*(start|enable)[^\n]*(pvn-node|pvn-central)' "$TOPOLOGY"; then
    fail "topology installer can activate a PVN target"
fi
grep -q 'STATE_FILE.*state.json' "$TOPOLOGY" || fail "persistent topology journal is missing"
grep -q '0o600' "$TOPOLOGY" || fail "root-private journal/backup mode is missing"
grep -q 'expected_sha256' "$TOPOLOGY" || fail "Corosync compare-and-swap guard is missing"
grep -q 'before_interfaces_sha256' "$TOPOLOGY" || fail "network compare-and-swap guard is missing"

echo "pvn-topology tests passed"
