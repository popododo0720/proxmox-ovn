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
KNOWN_HOSTS=$WORK/known_hosts
LOCK=$WORK/pvn-topology.lock
STATE=$WORK/state.json
COROSYNC=$WORK/corosync.conf
LOG=$WORK/ssh.log
mkdir "$BIN"
: > "$LOG"

cat > "$MEMBERS" <<'EOF'
{
  "nodename": "prox1",
  "version": 11,
  "cluster": {"name": "lab-cluster", "version": 11, "nodes": 3, "quorate": 1},
  "nodelist": {
    "prox2": {"id": 1, "online": 1, "ip": "192.168.0.126"},
    "prox1": {"id": 2, "online": 1, "ip": "192.168.0.80"},
    "prox3": {"id": 3, "online": 1, "ip": "192.168.0.78"}
  }
}
EOF

cat > "$KNOWN_HOSTS" <<'EOF'
prox1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestprox1
prox2 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestprox2
prox3 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestprox3
EOF
chmod 0600 "$KNOWN_HOSTS"

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
import sys


def stop(message):
    print(message, file=sys.stderr)
    raise SystemExit(1)


args = sys.argv[1:]
sys.stdin.read()
try:
    destination_index = next(i for i, value in enumerate(args) if value.startswith("root@"))
    python_index = args.index("python3", destination_index)
    if args[python_index + 1] != "-":
        stop("missing remote stdin program")
    action = args[python_index + 2]
    request = json.loads(bytes.fromhex(args[python_index + 3]).decode())
except Exception as exc:
    stop("bad fake SSH invocation: %s" % exc)

host = args[destination_index][5:]
node = request.get("node")
with open(os.environ["PVN_TEST_LOG"], "a", encoding="utf-8") as stream:
    stream.write("host=%s node=%s action=%s options=%s\n" % (
        host, node, action, " ".join(args[:destination_index]),
    ))

if os.environ.get("PVN_TEST_PROBE_FAIL_HOST") == node and action == "probe":
    stop("simulated unsafe topology probe")
if os.environ.get("PVN_TEST_FAIL_VERIFY_HOST") == node and action == "verify-network":
    stop("simulated post-ifreload verification failure")

state_path = Path(os.environ["PVN_TEST_STATE"])
if state_path.exists():
    state = json.loads(state_path.read_text(encoding="utf-8"))
else:
    state = {
        "corosync_text": Path(os.environ["PVN_TEST_COROSYNC"]).read_text(encoding="utf-8"),
        "ledger": None,
        "ledger_sha256": None,
        "nodes": {},
    }

node_state = state["nodes"].setdefault(node, {
    "network": "initial", "staged": False, "journal": False, "phase": None,
})


def save():
    state_path.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")


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


def report():
    network = node_state["network"]
    interfaces_sha = sha("interfaces-%s-%s" % (node, network))
    pending_sha = sha("interfaces-%s-desired" % node) if node_state["staged"] else None
    return {
        "node": node,
        "interfaces_sha256": interfaces_sha,
        "pending_interfaces_sha256": pending_sha,
        "corosync_sha256": sha(state["corosync_text"]),
        "corosync_text": state["corosync_text"],
        "management": {
            "interface": "vmbr0",
            "address": management[node] + "/24",
            "default_routes": [{"dst": "default", "gateway": "192.168.0.1", "dev": "vmbr0"}],
        },
        "geneve": {
            "interface": "ens4", "address": geneve[node], "mac": "fa:16:3e:00:00:04",
            "mtu": 1442, "max_mtu": 1442,
        },
        "provider": {
            "interface": "ens5", "address": provider[node], "mac": "fa:16:3e:00:00:05",
            "mtu": 1442, "max_mtu": 1442,
        },
        "network_state": network,
        "journal_phase": node_state["phase"],
        "ledger_sha256": state["ledger_sha256"],
        "activation_safe": True,
    }


def emit(**values):
    values["ok"] = True
    print(json.dumps(values, sort_keys=True, separators=(",", ":")))


if action == "probe":
    emit(report=report())
elif action == "prepare":
    node_state["journal"] = True
    node_state["phase"] = node_state["phase"] or "prepared"
    save()
    emit(journal={"schema": 1}, report=report())
elif action == "validate-corosync":
    if sha(state["corosync_text"]) != request["expected_sha256"]:
        stop("fake corosync CAS mismatch")
    if sha(request["candidate"]) != request["candidate_sha256"]:
        stop("fake candidate hash mismatch")
    emit(valid=True)
elif action == "apply-corosync":
    if sha(state["corosync_text"]) != request["expected_sha256"]:
        stop("fake corosync changed")
    state["corosync_text"] = request["candidate"]
    save()
    if os.environ.get("PVN_TEST_FAIL_COROSYNC_AFTER_WRITE") == "yes":
        stop("simulated SSH failure after shared Corosync write")
    emit(corosync_sha256=request["candidate_sha256"])
elif action == "restore-corosync":
    current = sha(state["corosync_text"])
    if current == request["failed_candidate_sha256"]:
        state["corosync_text"] = request["rollback_candidate"]
        save()
        emit(restored=True, corosync_sha256=request["rollback_sha256"])
    elif current == request["original_sha256"]:
        emit(restored=False, corosync_sha256=current)
    else:
        stop("fake rollback CAS mismatch")
elif action == "verify-cluster":
    if sha(state["corosync_text"]) not in request["allowed_corosync_sha256"]:
        stop("fake cluster has unexpected corosync hash")
    emit(verified=True, corosync_sha256=sha(state["corosync_text"]))
elif action == "record-phase":
    node_state["phase"] = request["phase"]
    save()
    emit(phase=request["phase"])
elif action == "stage-network":
    if node_state["network"] == "desired":
        emit(noop=True, desired_interfaces_sha256=sha("interfaces-%s-desired" % node))
    else:
        node_state["staged"] = True
        node_state["phase"] = "network-staged"
        save()
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
else:
    stop("unexpected remote action %s" % action)
PY
chmod 0755 "$BIN/ssh"

export PVN_TOPOLOGY_MEMBERS_FILE=$MEMBERS
export PVN_TOPOLOGY_KNOWN_HOSTS_FILE=$KNOWN_HOSTS
export PVN_TOPOLOGY_LOCK_FILE=$LOCK
export PVN_TOPOLOGY_SSH_BIN=$BIN/ssh
export PVN_TEST_STATE=$STATE
export PVN_TEST_COROSYNC=$COROSYNC
export PVN_TEST_LOG=$LOG

GENEVE=192.168.100.0/24
PROVIDER=192.168.200.0/24
ACK=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP

fail() {
    echo "pvn-topology test failed: $*" >&2
    exit 1
}

reset_state() {
    rm -f "$STATE" "$LOCK"
    : > "$LOG"
}

assert_no_mutation() {
    if grep -Eq 'action=(prepare|validate-corosync|apply-corosync|restore-corosync|record-phase|stage-network|apply-network|rollback-network|discard-stage|write-ledger)' "$LOG"; then
        fail "read-only/invalid invocation attempted a mutating remote action"
    fi
}

PYTHONDONTWRITEBYTECODE=1 python3 -c \
    "compile(open('$TOPOLOGY', encoding='utf-8').read(), '$TOPOLOGY', 'exec')"

reset_state
"$TOPOLOGY" --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" > "$WORK/plan.out"
[ ! -e "$LOCK" ] || fail "read-only plan created a lock/journal artifact"
[ "$(grep -c 'action=probe' "$LOG")" -eq 3 ] || fail "plan did not probe all online members"
assert_no_mutation
grep -q 'guest-mtu=1300 ceiling=1342' "$WORK/plan.out" || fail "safe default guest MTU is wrong"
grep -q 'Corosync ring0: Geneve -> management' "$WORK/plan.out" || fail "Corosync migration was not planned"
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
grep -q "UserKnownHostsFile=$KNOWN_HOSTS" "$LOG" || fail "native PVE known_hosts was not pinned"
for node in prox1 prox2 prox3; do
    grep -q "HostKeyAlias=$node" "$LOG" || fail "node-specific HostKeyAlias $node was not pinned"
done

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
"$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/apply.out"
[ "$(grep -c 'action=prepare' "$LOG")" -eq 3 ] || fail "journals/backups were not prepared on all nodes"
[ "$(grep -c 'action=validate-corosync' "$LOG")" -eq 6 ] || fail "forward/rollback Corosync candidates were not validated everywhere"
[ "$(grep -c 'action=apply-corosync' "$LOG")" -eq 1 ] || fail "shared Corosync config was not applied exactly once"
[ "$(grep -c 'action=stage-network' "$LOG")" -eq 3 ] || fail "all candidates were not staged before apply"
[ "$(grep -c 'action=apply-network' "$LOG")" -eq 3 ] || fail "network was not applied one node at a time"
[ "$(grep -c 'action=verify-network' "$LOG")" -eq 3 ] || fail "each applied node was not verified"
[ "$(grep -c 'action=write-ledger' "$LOG")" -eq 1 ] || fail "shared ledger was not written exactly once"
[ "$(grep -c 'action=verify-ledger' "$LOG")" -eq 3 ] || fail "shared ledger was not read back on every node"
grep -q 'PVN activation markers and central/node targets remain untouched' "$WORK/apply.out" ||
    fail "apply did not report the activation boundary"

python3 - "$STATE" "$ACK" <<'PY'
import json
import sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
ledger = json.loads(state["ledger"])
assert ledger["phase"] == "complete"
assert ledger["guest_mtu"] == 1300
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
PY

: > "$LOG"
"$TOPOLOGY" apply --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/rerun.out"
if grep -Eq 'action=(validate-corosync|apply-corosync|restore-corosync|stage-network|apply-network|rollback-network)' "$LOG"; then
    fail "exact-state rerun repeated a Corosync/network mutation"
fi
[ "$(grep -c 'exact network state already present; no-op' "$WORK/rerun.out")" -eq 3 ] ||
    fail "exact-state rerun did not report all no-ops"

reset_state
if PVN_TEST_FAIL_COROSYNC_AFTER_WRITE=yes "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/corosync-rollback.out" 2>&1
then
    fail "post-write Corosync failure unexpectedly succeeded"
fi
grep -q 'action=restore-corosync' "$LOG" || fail "post-write Corosync failure was not rolled back"
if grep -Eq 'action=(stage-network|apply-network|write-ledger)' "$LOG"; then
    fail "network/ledger mutation ran after failed Corosync migration"
fi

reset_state
if PVN_TEST_FAIL_VERIFY_HOST=prox1 "$TOPOLOGY" apply \
    --geneve-cidr "$GENEVE" --provider-cidr "$PROVIDER" \
    --provider-port-ready "$ACK" --confirm lab-cluster > "$WORK/rollback.out" 2>&1
then
    fail "post-ifreload verification failure unexpectedly succeeded"
fi
grep -q 'node=prox1 action=rollback-network' "$LOG" || fail "failed current node was not rolled back"
if grep -q 'node=prox3 action=apply-network' "$LOG"; then
    fail "later node applied after an earlier node failed"
fi
grep -q 'node=prox3 action=discard-stage' "$LOG" || fail "later staged candidate was not discarded"
if grep -q 'action=write-ledger' "$LOG"; then
    fail "failure path published a complete topology ledger"
fi

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
