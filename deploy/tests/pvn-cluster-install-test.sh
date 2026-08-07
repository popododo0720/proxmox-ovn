#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
INSTALLER=$REPO/deploy/scripts/pvn-cluster-install
WORK=$(mktemp -d)

cleanup() {
    rm -rf "$WORK"
}
trap cleanup 0 HUP INT TERM

BIN=$WORK/bin
LOG=$WORK/calls.log
REMOTE_SCRIPT=$WORK/remote-script
mkdir "$BIN"
: > "$LOG"

cat > "$BIN/ssh" <<'EOF'
#!/bin/sh
set -eu

host=
alias=
action=
previous=
for arg in "$@"; do
    case "$arg" in root@*) host=${arg#root@} ;; esac
    case "$arg" in probe|prepare|verify|apply|cleanup) action=$arg ;; esac
    if [ "$previous" = -o ]; then
        case "$arg" in HostKeyAlias=*) alias=${arg#HostKeyAlias=} ;; esac
    fi
    previous=$arg
done
cat > "$PVN_TEST_REMOTE_SCRIPT"
node=${alias:-$host}
printf 'ssh host=%s action=%s node=%s args=%s\n' "$host" "$action" "$node" "$*" >> "$PVN_TEST_LOG"

if [ "${PVN_TEST_FAIL_HOST:-}" = "$node" ] && [ "$action" = probe ]; then
    echo "simulated preflight failure" >&2
    exit 1
fi

case "$action" in
    probe)
        cluster=${PVN_TEST_CLUSTER:-lab-cluster}
        if [ "${PVN_TEST_OTHER_CLUSTER_HOST:-}" = "$node" ]; then
            cluster=other-cluster
        fi
        package=absent
        version=absent
        if [ -e "$PVN_TEST_STATE/$node" ] || [ "${PVN_TEST_PREINSTALLED:-no}" = yes ]; then
            package=installed
            version=${PVN_TEST_INSTALLED_VERSION:-0.1.0}
        fi
        mode=${PVN_TEST_MODE:-cluster}
        case "$node" in
            node-a) hostname=pve-a; nodeid=1 ;;
            node-b) hostname=pve-b; nodeid=2 ;;
            pve-a) hostname=pve-a; nodeid=1 ;;
            pve-b) hostname=pve-b; nodeid=2 ;;
            pve-solo) hostname=pve-solo; nodeid=0 ;;
            *) hostname=$node; nodeid=9 ;;
        esac
        if [ "$mode" = standalone ]; then
            cluster=standalone-$hostname
        fi
        if [ "${PVN_TEST_WRONG_NODE_ID:-}" = "$node" ]; then nodeid=99; fi
        printf 'PVN_PREFLIGHT mode=%s cluster=%s nodes=%s pve=9.2.2 arch=amd64 package=%s version=%s hostname=%s nodeid=%s\n' \
            "$mode" "$cluster" "${PVN_TEST_NODE_COUNT:-2}" "$package" "$version" \
            "$hostname" "$nodeid"
        ;;
    prepare)
        printf '/var/tmp/pvn-node.%s.deb\n' "$node"
        ;;
    verify)
        [ "${PVN_TEST_VERIFY_FAIL:-no}" != yes ]
        ;;
    apply)
        : > "$PVN_TEST_STATE/$node"
        ;;
    cleanup)
        ;;
    *)
        echo "fake ssh could not identify remote action" >&2
        exit 1
        ;;
esac
EOF
chmod 0755 "$BIN/ssh"

cat > "$BIN/local-sh" <<'EOF'
#!/bin/sh
set -eu

node=${PVN_NODE_NAME:?}
action=
for arg in "$@"; do
    case "$arg" in probe|prepare|verify|apply|cleanup) action=$arg ;; esac
done
cat > "$PVN_TEST_REMOTE_SCRIPT"
printf 'local host=%s action=%s node=%s args=%s\n' "$node" "$action" "$node" "$*" >> "$PVN_TEST_LOG"

if [ "${PVN_TEST_FAIL_HOST:-}" = "$node" ] && [ "$action" = probe ]; then
    echo "simulated preflight failure" >&2
    exit 1
fi

case "$action" in
    probe)
        mode=${PVN_TEST_MODE:-cluster}
        cluster=${PVN_TEST_CLUSTER:-lab-cluster}
        package=absent
        version=absent
        if [ -e "$PVN_TEST_STATE/$node" ] || [ "${PVN_TEST_PREINSTALLED:-no}" = yes ]; then
            package=installed
            version=${PVN_TEST_INSTALLED_VERSION:-0.1.0}
        fi
        case "$node" in
            pve-a) nodeid=1 ;;
            pve-b) nodeid=2 ;;
            pve-solo) nodeid=0 ;;
            *) nodeid=9 ;;
        esac
        if [ "$mode" = standalone ]; then cluster=standalone-$node; fi
        if [ "${PVN_TEST_WRONG_NODE_ID:-}" = "$node" ]; then nodeid=99; fi
        printf 'PVN_PREFLIGHT mode=%s cluster=%s nodes=%s pve=9.2.2 arch=amd64 package=%s version=%s hostname=%s nodeid=%s\n' \
            "$mode" "$cluster" "${PVN_TEST_NODE_COUNT:-2}" "$package" \
            "$version" "$node" "$nodeid"
        ;;
    prepare)
        printf '/var/tmp/pvn-node.%s.deb\n' "$node"
        ;;
    verify)
        [ "${PVN_TEST_VERIFY_FAIL:-no}" != yes ]
        ;;
    apply)
        : > "$PVN_TEST_STATE/$node"
        ;;
    cleanup)
        ;;
    *)
        echo "fake local sh could not identify action" >&2
        exit 1
        ;;
esac
EOF
chmod 0755 "$BIN/local-sh"

cat > "$BIN/scp" <<'EOF'
#!/bin/sh
set -eu
printf 'scp args=%s\n' "$*" >> "$PVN_TEST_LOG"
exit 0
EOF
chmod 0755 "$BIN/scp"

cat > "$BIN/cp" <<'EOF'
#!/bin/sh
set -eu
printf 'cp args=%s\n' "$*" >> "$PVN_TEST_LOG"
exit 0
EOF
chmod 0755 "$BIN/cp"

cat > "$BIN/pvecm" <<'EOF'
#!/bin/sh
set -eu

case "${1:-}" in
    status)
        if [ "${PVN_TEST_PVECM_STANDALONE:-no}" = yes ]; then exit 1; fi
        cat <<OUT
Cluster information
-------------------
Name:             ${PVN_TEST_CLUSTER:-lab-cluster}

Quorum information
------------------
Nodes:            ${PVN_TEST_PVECM_NODE_COUNT:-2}
Node ID:          0x00000001
Quorate:          ${PVN_TEST_PVECM_QUORATE:-Yes}
OUT
        ;;
    nodes)
        count=0
        if [ -n "${PVN_TEST_PVECM_COUNTER:-}" ]; then
            count=$(cat "$PVN_TEST_PVECM_COUNTER" 2>/dev/null || printf '0')
            count=$((count + 1))
            printf '%s\n' "$count" > "$PVN_TEST_PVECM_COUNTER"
        fi
        cat <<OUT

Membership information
----------------------
    Nodeid      Votes Name
         1          1 pve-a (local)
OUT
        if [ -z "${PVN_TEST_PVECM_MUTATE_AT:-}" ] || [ "$count" -lt "$PVN_TEST_PVECM_MUTATE_AT" ]; then
            printf '         2          1 pve-b\n'
        fi
        ;;
    *) exit 2 ;;
esac
EOF
chmod 0755 "$BIN/pvecm"

cat > "$BIN/dpkg-deb" <<'EOF'
#!/bin/sh
set -eu
field=
for arg in "$@"; do field=$arg; done
case "$field" in
    Package) printf 'pvn-node\n' ;;
    Version) printf '%s\n' "${PVN_TEST_DEB_VERSION:-0.1.0}" ;;
    Architecture) printf 'amd64\n' ;;
    *) exit 1 ;;
esac
EOF
chmod 0755 "$BIN/dpkg-deb"

cat > "$BIN/sha256sum" <<'EOF'
#!/bin/sh
set -eu
printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  %s\n' "$1"
EOF
chmod 0755 "$BIN/sha256sum"

cat > "$BIN/pvn-cluster-lease" <<'EOF'
#!/bin/sh
set -eu
action=$1
domain=$2
token=${3:-}
path=$PVN_TEST_GLOBAL_LOCK
[ "$domain" = mutation ]
printf '%s %s\n' "$action" "$domain" >> "$PVN_TEST_LEASE_LOG"
case "$action" in
    acquire)
        [ ! -e "$path" ] || {
            echo "test lease already exists" >&2
            exit 1
        }
        IFS= read -r owner
        printf '%s\n' "$owner" | grep -Fq "\"token\":\"$token\""
        printf '%s\n' "$owner" > "$path"
        chmod 0600 "$path"
        printf '%s\n' "$path"
        ;;
    release)
        [ -f "$path" ]
        grep -Fq "\"token\":\"$token\"" "$path"
        rm -f "$path"
        ;;
    show)
        cat "$path"
        ;;
    *) exit 2 ;;
esac
EOF
chmod 0755 "$BIN/pvn-cluster-lease"

export PATH="$BIN:$PATH"
export PVN_TEST_LOG=$LOG
export PVN_TEST_REMOTE_SCRIPT=$REMOTE_SCRIPT
export PVN_TEST_STATE=$WORK/state
export PVN_INSTALL_LOCK_FILE=$WORK/install.lock
export PVN_TEST_LEASE_LOG=$WORK/lease.log
: > "$PVN_TEST_LEASE_LOG"
mkdir "$PVN_TEST_STATE"

INVENTORY=$WORK/inventory
IDENTITY=$WORK/id_ed25519
DEB=$WORK/pvn-node.deb
cat > "$INVENTORY" <<'EOF'
# Test inventory must be parsed, not sourced.
PVN_TARGET_NODES="node-a node-b"
PVN_PROVIDER_UPLINK=$(exit 99)
EOF
: > "$IDENTITY"
: > "$DEB"
chmod 0600 "$IDENTITY"

PVE_FIXTURE=$WORK/pve
PVE_MEMBERS=$PVE_FIXTURE/.members
PVE_COROSYNC=$PVE_FIXTURE/corosync.conf
PVE_NODES=$PVE_FIXTURE/nodes
PVE_IDENTITY=$PVE_FIXTURE/id_rsa
PVE_GLOBAL_LOCK=$PVE_FIXTURE/pvn-install.lock
PVECM_COUNTER=$PVE_FIXTURE/pvecm-counter
mkdir -p "$PVE_NODES/pve-a" "$PVE_NODES/pve-b"
: > "$PVE_COROSYNC"
: > "$PVE_IDENTITY"
chmod 0600 "$PVE_IDENTITY"
printf '%s\n' 'pve-b ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCtest pve-b' \
    > "$PVE_NODES/pve-b/ssh_known_hosts"
chmod 0640 "$PVE_NODES/pve-b/ssh_known_hosts"

write_cluster_members() {
    pvn_online=${1:-1}
    pvn_nodes=${2:-2}
    cat > "$PVE_MEMBERS" <<EOF
{
  "nodename": "pve-a",
  "version": 9,
  "cluster": {"name":"lab-cluster","version":3,"nodes":$pvn_nodes,"quorate":1},
  "nodelist": {
    "pve-a": {"id":1,"online":1,"ip":"192.0.2.10"},
    "pve-b": {"id":2,"online":$pvn_online,"ip":"192.0.2.11"}
  }
}
EOF
    chmod 0440 "$PVE_MEMBERS"
}

write_standalone_members() {
    cat > "$PVE_MEMBERS" <<'EOF'
{"nodename":"pve-solo","version":0}
EOF
    chmod 0440 "$PVE_MEMBERS"
}

run_local_pve() {
    PVN_PVE_MEMBERS_FILE=$PVE_MEMBERS \
    PVN_PVE_COROSYNC_CONF=$PVE_COROSYNC \
    PVN_PVE_NODES_DIR=$PVE_NODES \
    PVN_PVE_IDENTITY=$PVE_IDENTITY \
    PVN_CLUSTER_LEASE_BIN=$BIN/pvn-cluster-lease \
    PVN_TEST_GLOBAL_LOCK=$PVE_GLOBAL_LOCK \
    PVN_PVECM_BIN=$BIN/pvecm \
    PVN_LOCAL_SH_BIN=$BIN/local-sh \
    PVN_CP_BIN=$BIN/cp \
    "$INSTALLER" "$@"
}

fail() {
    echo "pvn-cluster-install test failed: $*" >&2
    exit 1
}

assert_no_mutation_calls() {
    if grep -Eq 'action=(prepare|verify|apply|cleanup)|^scp ' "$LOG"; then
        fail "read-only invocation attempted a mutation"
    fi
}

reset_state() {
    rm -f "$PVN_TEST_STATE/node-a" "$PVN_TEST_STATE/node-b" \
        "$PVN_TEST_STATE/pve-a" "$PVN_TEST_STATE/pve-b" \
        "$PVN_TEST_STATE/pve-solo" "$PVE_GLOBAL_LOCK" "$PVECM_COUNTER"
}

sh -n "$INSTALLER"

: > "$LOG"
reset_state
"$INSTALLER" preflight --inventory "$INVENTORY" --identity "$IDENTITY" \
    > "$WORK/preflight.out"
grep -q 'Preflight passed for all 2 nodes in cluster lab-cluster; no changes made.' \
    "$WORK/preflight.out" || fail "preflight did not report the verified cluster"
[ "$(grep -c 'action=probe' "$LOG")" -eq 2 ] ||
    fail "preflight did not inspect every inventory node"
assert_no_mutation_calls
for option in \
    BatchMode=yes PasswordAuthentication=no KbdInteractiveAuthentication=no \
    PubkeyAuthentication=yes PreferredAuthentications=publickey \
    IdentitiesOnly=yes NumberOfPasswordPrompts=0 StrictHostKeyChecking=yes
do
    grep -q "$option" "$LOG" || fail "SSH did not enforce $option"
done

: > "$LOG"
reset_state
"$INSTALLER" install --inventory "$INVENTORY" --identity "$IDENTITY" \
    --deb "$DEB" > "$WORK/dry-run.out"
grep -q '^Dry run:' "$WORK/dry-run.out" || fail "install did not default to dry run"
grep -q -- '--apply --confirm lab-cluster' "$WORK/dry-run.out" ||
    fail "dry run did not print the exact confirmation value"
assert_no_mutation_calls

: > "$LOG"
reset_state
if "$INSTALLER" install --inventory "$INVENTORY" --identity "$IDENTITY" \
    --deb "$DEB" --apply > "$WORK/missing-confirm.out" 2>&1
then
    fail "--apply without --confirm unexpectedly succeeded"
fi
[ ! -s "$LOG" ] || fail "missing confirmation contacted a node"

: > "$LOG"
reset_state
if "$INSTALLER" install --inventory "$INVENTORY" --identity "$IDENTITY" \
    --deb "$DEB" --apply --confirm wrong-cluster \
    > "$WORK/wrong-confirm.out" 2>&1
then
    fail "a mismatched cluster confirmation unexpectedly succeeded"
fi
assert_no_mutation_calls

: > "$LOG"
reset_state
PVN_TEST_FAIL_HOST=node-b "$INSTALLER" install \
    --inventory "$INVENTORY" --identity "$IDENTITY" --deb "$DEB" \
    > "$WORK/node-failure.out" 2>&1 &&
    fail "install dry run accepted a failed node preflight"
assert_no_mutation_calls

: > "$LOG"
reset_state
PVN_TEST_NODE_COUNT=3 "$INSTALLER" preflight \
    --inventory "$INVENTORY" --identity "$IDENTITY" \
    > "$WORK/count-mismatch.out" 2>&1 &&
    fail "preflight accepted an inventory that omits a cluster node"
assert_no_mutation_calls

: > "$LOG"
reset_state
PVN_TEST_OTHER_CLUSTER_HOST=node-b "$INSTALLER" preflight \
    --inventory "$INVENTORY" --identity "$IDENTITY" \
    > "$WORK/cluster-mismatch.out" 2>&1 &&
    fail "preflight accepted nodes from different clusters"
assert_no_mutation_calls

: > "$LOG"
reset_state
PVN_TEST_VERIFY_FAIL=yes "$INSTALLER" install \
    --inventory "$INVENTORY" --identity "$IDENTITY" --deb "$DEB" \
    --apply --confirm lab-cluster > "$WORK/hash-failure.out" 2>&1 &&
    fail "install continued after remote DEB verification failed"
grep -q 'host=node-a action=cleanup' "$LOG" ||
    fail "failed staging did not clean up the remote DEB"
if grep -q 'action=apply' "$LOG"; then
    fail "apt phase ran after remote DEB verification failed"
fi

: > "$LOG"
reset_state
"$INSTALLER" install --inventory "$INVENTORY" --identity "$IDENTITY" \
    --deb "$DEB" --apply --confirm lab-cluster > "$WORK/apply.out"
[ "$(grep -c '^scp ' "$LOG")" -eq 2 ] || fail "DEB was not copied to every node"
[ "$(grep -c 'action=verify' "$LOG")" -eq 2 ] || fail "DEB was not verified on every node"
[ "$(grep -c 'action=apply' "$LOG")" -eq 2 ] || fail "package was not installed on every node"
[ "$(grep -c 'action=cleanup' "$LOG")" -eq 2 ] || fail "staged DEBs were not cleaned up"
grep -q 'Installed pvn-node 0.1.0 on all 2 nodes in cluster lab-cluster.' \
    "$WORK/apply.out" || fail "successful install summary is missing"

mask_line=$(grep -n 'systemctl mask ovn-host.service ovn-central.service' \
    "$REMOTE_SCRIPT" | cut -d: -f1)
apt_line=$(grep -n 'apt-get install -y' "$REMOTE_SCRIPT" | cut -d: -f1)
[ -n "$mask_line" ] && [ -n "$apt_line" ] && [ "$mask_line" -lt "$apt_line" ] ||
    fail "remote install does not mask OVN aggregate units before apt"

transition_line=$(grep -n 'check_pve_extensions "$pvn_package_state" transition' \
    "$REMOTE_SCRIPT" | cut -d: -f1)
strict_line=$(grep -n 'check_pve_extensions installed strict' \
    "$REMOTE_SCRIPT" | cut -d: -f1)
[ -n "$transition_line" ] && [ -n "$strict_line" ] && \
    [ "$transition_line" -lt "$apt_line" ] && [ "$apt_line" -lt "$strict_line" ] ||
    fail "API hook verification does not straddle the package transition"
grep -q '/usr/lib/pvn/pvn-api-verify' "$REMOTE_SCRIPT" ||
    fail "remote install does not verify the installed PVE API hook"
grep -q '^check_clean_api_dispatcher() {' "$REMOTE_SCRIPT" ||
    fail "remote install does not validate a clean PVE API dispatcher"

if grep -Eq 'ovs-vsctl[^#]*(add-br|add-port|set|create|destroy)|br-provider|/etc/network/interfaces|ip (link|address|route)' \
    "$INSTALLER"
then
    fail "cluster installer contains a network or provider mutation"
fi
if grep -Eq '(touch|install|mkdir|printf|cat)[^#\n]*(node-enabled|central/enabled)' \
    "$INSTALLER"
then
    fail "cluster installer creates an activation marker"
fi
grep -q "dpkg-query -W -f='\${db:Status-Status}' pvn-node" "$INSTALLER" ||
    fail "remote package state does not use the non-truncated dpkg status field"

: > "$LOG"
reset_state
PVN_TEST_PREINSTALLED=yes "$INSTALLER" install \
    --inventory "$INVENTORY" --identity "$IDENTITY" --deb "$DEB" \
    > "$WORK/inert-retry.out"
grep -q 'package=installed:0.1.0 inactive' "$WORK/inert-retry.out" ||
    fail "an installed but inert exact-version retry was not recognized"
assert_no_mutation_calls

: > "$LOG"
reset_state
PVN_TEST_PREINSTALLED=yes PVN_TEST_INSTALLED_VERSION=0.1.0 \
    PVN_TEST_DEB_VERSION=0.1.1 "$INSTALLER" install \
    --inventory "$INVENTORY" --identity "$IDENTITY" --deb "$DEB" \
    > "$WORK/upgrade.out"
grep -q '^Dry run: pvn-node 0.1.1' "$WORK/upgrade.out" ||
    fail "a strict Debian-version upgrade was not accepted"
assert_no_mutation_calls

: > "$LOG"
reset_state
if PVN_TEST_PREINSTALLED=yes PVN_TEST_INSTALLED_VERSION=0.2.0 \
    PVN_TEST_DEB_VERSION=0.1.1 "$INSTALLER" install \
    --inventory "$INVENTORY" --identity "$IDENTITY" --deb "$DEB" \
    > "$WORK/downgrade.out" 2>&1
then
    fail "a package downgrade unexpectedly succeeded"
fi
assert_no_mutation_calls

DASH_DEB=$WORK/-pvn-node.deb
: > "$DASH_DEB"
: > "$LOG"
reset_state
"$INSTALLER" install --inventory "$INVENTORY" --identity "$IDENTITY" \
    --deb "$DASH_DEB" > "$WORK/dash-deb.out"
grep -q '^Dry run:' "$WORK/dash-deb.out" ||
    fail "an option-like DEB filename was not safely canonicalized"
assert_no_mutation_calls

DUPLICATE_INVENTORY=$WORK/duplicate-inventory
printf '%s\n' 'PVN_TARGET_NODES="node-a node-a"' > "$DUPLICATE_INVENTORY"
: > "$LOG"
if "$INSTALLER" preflight --inventory "$DUPLICATE_INVENTORY" \
    --identity "$IDENTITY" > "$WORK/duplicate.out" 2>&1
then
    fail "duplicate inventory node unexpectedly succeeded"
fi
[ ! -s "$LOG" ] || fail "invalid inventory contacted a node"

# Local-PVE cluster mode discovers the PVE management IPs, executes the local
# member without SSH, and pins each peer to its PVE-owned host-key alias.
: > "$LOG"
reset_state
write_cluster_members
run_local_pve preflight --local-pve > "$WORK/local-preflight.out"
grep -q 'Preflight passed for all 2 nodes in cluster lab-cluster' \
    "$WORK/local-preflight.out" || fail "local PVE preflight summary is missing"
[ "$(grep -c '^local .*action=probe' "$LOG")" -eq 1 ] ||
    fail "local PVE preflight did not execute the local node directly"
[ "$(grep -c '^ssh .*action=probe' "$LOG")" -eq 1 ] ||
    fail "local PVE preflight did not execute exactly one peer over SSH"
grep -q 'host=192.0.2.11 action=probe node=pve-b' "$LOG" ||
    fail "local PVE peer did not use its discovered management IP"
for option in \
    HostKeyAlias=pve-b \
    "UserKnownHostsFile=$PVE_NODES/pve-b/ssh_known_hosts" \
    GlobalKnownHostsFile=none StrictHostKeyChecking=yes UpdateHostKeys=no \
    CheckHostIP=no IdentitiesOnly=yes
do
    grep -q "$option" "$LOG" || fail "local PVE SSH did not enforce $option"
done
grep -q -- "-i $PVE_IDENTITY" "$LOG" ||
    fail "local PVE SSH did not use the PVE root cluster identity"
assert_no_mutation_calls

: > "$LOG"
reset_state
write_cluster_members
run_local_pve install --local-pve --deb "$DEB" \
    --apply --confirm lab-cluster > "$WORK/local-apply.out"
[ "$(grep -c 'action=apply' "$LOG")" -eq 2 ] ||
    fail "local PVE apply did not install every discovered node"
[ "$(grep -c '^cp ' "$LOG")" -eq 1 ] ||
    fail "local PVE apply did not copy the local DEB directly"
[ "$(grep -c '^scp ' "$LOG")" -eq 1 ] ||
    fail "local PVE apply did not copy the peer DEB over pinned SCP"
grep -q 'HostKeyAlias=pve-b' "$LOG" ||
    fail "local PVE SCP did not pin the peer alias"
[ ! -e "$PVE_GLOBAL_LOCK" ] ||
    fail "successful cluster apply left the cluster-global lock behind"
grep -q 'Installed pvn-node 0.1.0 on all 2 nodes in cluster lab-cluster.' \
    "$WORK/local-apply.out" || fail "local PVE install summary is missing"

# A remote probe must bind back to the exact name/id discovered in .members.
: > "$LOG"
reset_state
write_cluster_members
PVN_TEST_WRONG_NODE_ID=pve-b run_local_pve preflight --local-pve \
    > "$WORK/local-wrong-id.out" 2>&1 &&
    fail "local PVE preflight accepted the wrong peer node ID"
assert_no_mutation_calls

# JSON/quorum/membership failures stop before any node transport is attempted.
: > "$LOG"
reset_state
write_cluster_members 0 2
run_local_pve preflight --local-pve > "$WORK/local-offline.out" 2>&1 &&
    fail "local PVE discovery accepted an offline member"
[ ! -s "$LOG" ] || fail "offline discovery contacted a node"

: > "$LOG"
reset_state
write_cluster_members 1 3
run_local_pve preflight --local-pve > "$WORK/local-count.out" 2>&1 &&
    fail "local PVE discovery accepted a cluster node-count mismatch"
[ ! -s "$LOG" ] || fail "count-mismatch discovery contacted a node"

: > "$LOG"
reset_state
printf '%s\n' '{malformed' > "$PVE_MEMBERS"
chmod 0440 "$PVE_MEMBERS"
run_local_pve preflight --local-pve > "$WORK/local-json.out" 2>&1 &&
    fail "local PVE discovery accepted malformed JSON"
[ ! -s "$LOG" ] || fail "malformed discovery contacted a node"

# A pre-existing pmxcfs owner record is never removed or ignored as stale.
: > "$LOG"
reset_state
write_cluster_members
printf '%s\n' '{"node":"other","token":"stale"}' > "$PVE_GLOBAL_LOCK"
chmod 0600 "$PVE_GLOBAL_LOCK"
run_local_pve install --local-pve --deb "$DEB" \
    --apply --confirm lab-cluster > "$WORK/local-locked.out" 2>&1 &&
    fail "local PVE apply ignored a cluster-global owner lock"
[ -e "$PVE_GLOBAL_LOCK" ] || fail "stale cluster-global lock was auto-deleted"
[ ! -s "$LOG" ] || fail "locked local PVE apply contacted a node"
rm -f "$PVE_GLOBAL_LOCK"

# Membership is re-read after staging and before the first apt mutation.
: > "$LOG"
reset_state
write_cluster_members
: > "$PVECM_COUNTER"
PVN_TEST_PVECM_COUNTER=$PVECM_COUNTER PVN_TEST_PVECM_MUTATE_AT=3 \
    run_local_pve install --local-pve --deb "$DEB" \
    --apply --confirm lab-cluster > "$WORK/local-membership-change.out" 2>&1 &&
    fail "local PVE apply ignored a changed membership snapshot"
if grep -q 'action=apply' "$LOG"; then
    fail "apt mutation ran after membership changed"
fi
[ "$(grep -c 'action=cleanup' "$LOG")" -eq 2 ] ||
    fail "membership-change abort did not clean every staged DEB"
[ ! -e "$PVE_GLOBAL_LOCK" ] ||
    fail "membership-change abort left the owned global lock behind"

# Standalone mode has one explicit confirmation token and never uses SSH/SCP.
: > "$LOG"
: > "$PVN_TEST_LEASE_LOG"
reset_state
write_standalone_members
rm -f "$PVE_COROSYNC"
PVN_TEST_PVECM_STANDALONE=yes PVN_TEST_MODE=standalone \
    PVN_TEST_NODE_COUNT=1 run_local_pve install --local-pve --deb "$DEB" \
    --apply --confirm standalone-pve-solo > "$WORK/local-standalone.out"
[ "$(grep -c '^local .*action=apply' "$LOG")" -eq 1 ] ||
    fail "standalone apply did not execute exactly one local install"
if grep -Eq '^ssh |^scp ' "$LOG"; then
    fail "standalone local PVE mode attempted cluster SSH"
fi
[ ! -e "$PVE_GLOBAL_LOCK" ] ||
    fail "standalone apply created a cluster-global lock"
grep -Fxq 'acquire mutation' "$PVN_TEST_LEASE_LOG" ||
    fail "standalone apply did not acquire the shared mutation lease"
grep -Fxq 'release mutation' "$PVN_TEST_LEASE_LOG" ||
    fail "standalone apply did not release the shared mutation lease"
grep -q 'standalone-pve-solo' "$WORK/local-standalone.out" ||
    fail "standalone confirmation ID is missing from output"

# A standalone-shaped .members file is not accepted on a configured cluster.
: > "$LOG"
reset_state
: > "$PVE_COROSYNC"
PVN_TEST_PVECM_STANDALONE=yes PVN_TEST_MODE=standalone \
    run_local_pve preflight --local-pve > "$WORK/local-false-standalone.out" 2>&1 &&
    fail "corosync cluster state was misclassified as standalone"
[ ! -s "$LOG" ] || fail "false standalone discovery contacted a node"

# Target modes are deliberately mutually exclusive.
: > "$LOG"
write_cluster_members
run_local_pve preflight --local-pve --inventory "$INVENTORY" \
    --identity "$IDENTITY" > "$WORK/local-mixed-mode.out" 2>&1 &&
    fail "--local-pve accepted advanced inventory/key arguments"
[ ! -s "$LOG" ] || fail "mixed target modes contacted a node"

echo "pvn-cluster-install tests passed"
