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
action=
for arg in "$@"; do
    case "$arg" in root@*) host=${arg#root@} ;; esac
    case "$arg" in probe|prepare|verify|apply|cleanup) action=$arg ;; esac
done
cat > "$PVN_TEST_REMOTE_SCRIPT"
printf 'ssh host=%s action=%s args=%s\n' "$host" "$action" "$*" >> "$PVN_TEST_LOG"

if [ "${PVN_TEST_FAIL_HOST:-}" = "$host" ] && [ "$action" = probe ]; then
    echo "simulated preflight failure" >&2
    exit 1
fi

case "$action" in
    probe)
        cluster=${PVN_TEST_CLUSTER:-lab-cluster}
        if [ "${PVN_TEST_OTHER_CLUSTER_HOST:-}" = "$host" ]; then
            cluster=other-cluster
        fi
        package=absent
        version=absent
        if [ -e "$PVN_TEST_STATE/$host" ] || [ "${PVN_TEST_PREINSTALLED:-no}" = yes ]; then
            package=installed
            version=${PVN_TEST_INSTALLED_VERSION:-0.1.0}
        fi
        case "$host" in
            node-a) hostname=pve-a; nodeid=0x00000001 ;;
            node-b) hostname=pve-b; nodeid=0x00000002 ;;
            *) hostname=$host; nodeid=0x00000009 ;;
        esac
        printf 'PVN_PREFLIGHT cluster=%s nodes=%s pve=9.2.2 arch=amd64 package=%s version=%s hostname=%s nodeid=%s\n' \
            "$cluster" "${PVN_TEST_NODE_COUNT:-2}" "$package" "$version" \
            "$hostname" "$nodeid"
        ;;
    prepare)
        printf '/var/tmp/pvn-node.%s.deb\n' "$host"
        ;;
    verify)
        [ "${PVN_TEST_VERIFY_FAIL:-no}" != yes ]
        ;;
    apply)
        : > "$PVN_TEST_STATE/$host"
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

cat > "$BIN/scp" <<'EOF'
#!/bin/sh
set -eu
printf 'scp args=%s\n' "$*" >> "$PVN_TEST_LOG"
exit 0
EOF
chmod 0755 "$BIN/scp"

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

export PATH="$BIN:$PATH"
export PVN_TEST_LOG=$LOG
export PVN_TEST_REMOTE_SCRIPT=$REMOTE_SCRIPT
export PVN_TEST_STATE=$WORK/state
export PVN_INSTALL_LOCK_FILE=$WORK/install.lock
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
    rm -f "$PVN_TEST_STATE/node-a" "$PVN_TEST_STATE/node-b"
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

echo "pvn-cluster-install tests passed"
