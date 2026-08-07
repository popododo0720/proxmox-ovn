#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
BOOTSTRAP=$REPO/deploy/scripts/pvn-install.sh
# Pin the fixture to the bootstrap DEFAULT_VERSION so its default asset URL is tested.
RELEASE_VERSION=0.2.19
WORK=$(mktemp -d)

cleanup() {
    rm -rf "$WORK"
}
trap cleanup 0 HUP INT TERM

BIN=$WORK/bin
ASSETS=$WORK/assets
CURL_LOG=$WORK/curl.log
INSTALLER_LOG=$WORK/installer.log
DEB_LOG=$WORK/deb-path.log
SETUP_LOG=$WORK/setup.log
PVECM_LOG=$WORK/pvecm.log
PVECM_MODE=$WORK/pvecm.mode
mkdir "$BIN" "$ASSETS"
: > "$CURL_LOG"
: > "$INSTALLER_LOG"
: > "$DEB_LOG"
: > "$SETUP_LOG"
: > "$PVECM_LOG"
printf '%s\n' standalone > "$PVECM_MODE"

cat > "$BIN/curl" <<'EOF'
#!/bin/sh
set -eu

output=
url=
original_args=$*
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output)
            shift
            output=$1
            ;;
        https://*)
            url=$1
            ;;
    esac
    shift
done
[ -n "$output" ] && [ -n "$url" ]
printf 'args=%s url=%s output=%s\n' "$original_args" "$url" "$output" >> "$PVN_TEST_CURL_LOG"
asset=${url##*/}
cp "$PVN_TEST_ASSETS/$asset" "$output"
EOF
chmod 0755 "$BIN/curl"

# Simulate the outer public-script fetch separately from the bootstrap's own
# checksum-verified asset downloads. In failure mode it emits executable
# partial output before returning curl's failure status.
cat > "$BIN/public-curl" <<'EOF'
#!/bin/sh
set -eu
if [ "${PVN_TEST_PUBLIC_CURL_FAIL:-0}" -eq 1 ]; then
    printf '%s\n' ': > "$PVN_TEST_PUBLIC_BOOTSTRAP_RAN"'
    exit 22
fi
cat "$PVN_TEST_PUBLIC_BOOTSTRAP"
EOF
chmod 0755 "$BIN/public-curl"

cat > "$ASSETS/pvn-cluster-install" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$PVN_TEST_INSTALLER_LOG"
previous=
for argument in "$@"; do
    if [ "$previous" = --deb ]; then
        [ -f "$argument" ]
        printf '%s\n' "$argument" >> "$PVN_TEST_DEB_LOG"
    fi
    previous=$argument
done
exit "${PVN_TEST_INSTALLER_STATUS:-0}"
EOF
chmod 0755 "$ASSETS/pvn-cluster-install"

cat > "$ASSETS/pvn-cluster-lease" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 0755 "$ASSETS/pvn-cluster-lease"

cat > "$BIN/pvn-topology" <<'EOF'
#!/bin/sh
set -eu
printf 'topology %s\n' "$*" >> "$PVN_TEST_SETUP_LOG"
EOF
chmod 0755 "$BIN/pvn-topology"

cat > "$BIN/pvn-control-plane" <<'EOF'
#!/bin/sh
set -eu
printf 'control %s\n' "$*" >> "$PVN_TEST_SETUP_LOG"
EOF
chmod 0755 "$BIN/pvn-control-plane"

cat > "$BIN/pvecm" <<EOF
#!/bin/sh
set -eu
[ "\$#" -eq 1 ] && [ "\$1" = status ] || exit 9
printf '%s\n' "\$*" >> "$PVECM_LOG"
case "\$(cat "$PVECM_MODE")" in
    standalone)
        printf '%s\n' \
            "Error: Corosync config '/etc/pve/corosync.conf' does not exist - is this node part of a cluster?" \
            >&2
        exit 255
        ;;
    clustered)
        printf '%s\n' 'Cluster information' '-------------------' 'Name: lab-cluster'
        ;;
    error)
        echo 'cannot contact pmxcfs' >&2
        exit 1
        ;;
    *) exit 8 ;;
esac
EOF
chmod 0755 "$BIN/pvecm"

printf 'test deb payload\n' > "$ASSETS/pvn-node_${RELEASE_VERSION}_amd64.deb"
make_manifest() {
    (
        cd "$ASSETS"
        sha256sum "pvn-node_${RELEASE_VERSION}_amd64.deb" pvn-cluster-install \
            pvn-cluster-lease > SHA256SUMS
    )
}
make_manifest

INVENTORY=$WORK/inventory
IDENTITY=$WORK/id_ed25519
MEMBERS=$WORK/members.json
printf '%s\n' 'PVN_TARGET_NODES="node-a node-b"' > "$INVENTORY"
: > "$IDENTITY"
chmod 0600 "$IDENTITY"

write_members() {
    pvn_member_count=$1
    case "$pvn_member_count" in
        1)
            cat > "$MEMBERS" <<'EOF'
{"nodename":"node-a","version":7}
EOF
            ;;
        2|3|4|5|6|7)
            pvn_member_nodes=
            pvn_member_index=1
            while [ "$pvn_member_index" -le "$pvn_member_count" ]; do
                case "$pvn_member_index" in
                    1) pvn_member_name=node-a ;;
                    2) pvn_member_name=node-b ;;
                    3) pvn_member_name=node-c ;;
                    4) pvn_member_name=node-d ;;
                    5) pvn_member_name=node-e ;;
                    6) pvn_member_name=node-f ;;
                    7) pvn_member_name=node-g ;;
                esac
                [ -z "$pvn_member_nodes" ] || pvn_member_nodes=$pvn_member_nodes,
                pvn_member_nodes=$pvn_member_nodes\"$pvn_member_name\":{\"id\":$pvn_member_index,\"online\":1,\"ip\":\"192.0.2.$((10 + pvn_member_index))\"}
                pvn_member_index=$((pvn_member_index + 1))
            done
            printf '%s\n' \
                "{\"nodename\":\"node-a\",\"version\":7,\"cluster\":{\"name\":\"lab-cluster\",\"version\":9,\"nodes\":$pvn_member_count,\"quorate\":1},\"nodelist\":{$pvn_member_nodes}}" \
                > "$MEMBERS"
            ;;
        *) fail "unsupported test membership count: $pvn_member_count" ;;
    esac
    chmod 0600 "$MEMBERS"
}

write_members 3

export PATH="$BIN:$PATH"
export PVN_TEST_ASSETS=$ASSETS
export PVN_TEST_CURL_LOG=$CURL_LOG
export PVN_TEST_INSTALLER_LOG=$INSTALLER_LOG
export PVN_TEST_DEB_LOG=$DEB_LOG
export PVN_TEST_SETUP_LOG=$SETUP_LOG
export PVN_TEST_PVECM_LOG=$PVECM_LOG
export PVN_TEST_PUBLIC_BOOTSTRAP=$BOOTSTRAP
export PVN_TEST_PUBLIC_BOOTSTRAP_RAN=$WORK/public-bootstrap-ran

fail() {
    echo "pvn-install test failed: $*" >&2
    exit 1
}

reset_logs() {
    : > "$CURL_LOG"
    : > "$INSTALLER_LOG"
    : > "$DEB_LOG"
    : > "$SETUP_LOG"
    : > "$PVECM_LOG"
}

run_bootstrap() {
    PVN_RELEASE_BASE_URL=https://releases.example.invalid/v$RELEASE_VERSION \
    PVN_INVENTORY=$INVENTORY PVN_IDENTITY=$IDENTITY "$BOOTSTRAP" "$@"
}

run_local_bootstrap() {
    pvn_test_pvecm=${PVN_INSTALL_PVECM:-$BIN/pvecm}
    PVN_RELEASE_BASE_URL=https://releases.example.invalid/v$RELEASE_VERSION \
    PVN_CP_MEMBERS=$MEMBERS \
    PVN_INSTALL_PVECM=$pvn_test_pvecm \
    "$BOOTSTRAP" "$@"
}

assert_temp_cleaned() {
    while IFS= read -r line; do
        output=${line#* output=}
        [ ! -e "$output" ] || fail "temporary download remains: $output"
    done < "$CURL_LOG"
}

sh -n "$BOOTSTRAP"

# The documented assignment-and-AND wrapper preserves curl's nonzero status
# and never executes even partial response data.
if PVN_TEST_PUBLIC_CURL_FAIL=1 sh -c \
    'pvn_bootstrap=$($1 -fsSL https://releases.example.invalid/pvn-install.sh) && bash -c "$pvn_bootstrap"' \
    sh "$BIN/public-curl"
then
    fail "public bootstrap wrapper masked curl failure"
fi
[ ! -e "$PVN_TEST_PUBLIC_BOOTSTRAP_RAN" ] ||
    fail "public bootstrap wrapper executed partial curl output"

# With no inventory/key settings the one-line bootstrap selects local PVE
# discovery and never asks for deployment-host paths.
reset_logs
PVN_PHASE=preflight run_local_bootstrap > "$WORK/local-preflight.out"
[ "$(cat "$INSTALLER_LOG")" = "preflight --local-pve" ] ||
    fail "default bootstrap did not select --local-pve"
assert_temp_cleaned

reset_logs
PVN_PHASE=install run_local_bootstrap > "$WORK/local-install-dry-run.out"
local_install_args=$(cat "$INSTALLER_LOG")
case "$local_install_args" in
    "install --local-pve --deb "*) ;;
    *) fail "local PVE install dry-run arguments were incorrect" ;;
esac
case "$local_install_args" in *' --apply '*) fail "local install default applied" ;; esac
assert_temp_cleaned

reset_logs
PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=lab-cluster \
    run_local_bootstrap > "$WORK/local-install-apply.out"
grep -q -- '^install --local-pve .*--apply --confirm lab-cluster$' \
    "$INSTALLER_LOG" || fail "local PVE apply confirmation was not forwarded"
assert_temp_cleaned

reset_logs
PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=lab-cluster PVN_FULL=1 \
    PVN_GENEVE_CIDR=192.168.100.0/24 \
    PVN_PROVIDER_CIDR=192.168.200.0/24 PVN_GUEST_MTU=1300 \
    PVN_PROVIDER_PORT_READY=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
    PVN_TOPOLOGY_BIN=$BIN/pvn-topology \
    PVN_CONTROL_PLANE_BIN=$BIN/pvn-control-plane \
    run_local_bootstrap > "$WORK/local-full-apply.out"
cat > "$WORK/expected-setup.log" <<'EOF'
topology plan --geneve-cidr 192.168.100.0/24 --provider-cidr 192.168.200.0/24 --guest-mtu 1300
topology apply --geneve-cidr 192.168.100.0/24 --provider-cidr 192.168.200.0/24 --guest-mtu 1300 --provider-port-ready OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP --confirm lab-cluster
control plan
control apply --confirm lab-cluster
EOF
cmp "$WORK/expected-setup.log" "$SETUP_LOG" ||
    fail "full install did not run the topology/control-plane sequence"
[ ! -s "$PVECM_LOG" ] ||
    fail "clustered full install unexpectedly used standalone pvecm detection"
assert_temp_cleaned

# A standalone node remains a supported full-setup placement.
write_members 1
reset_logs
PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=standalone-node-a PVN_FULL=1 \
    PVN_GENEVE_CIDR=192.168.100.0/24 \
    PVN_PROVIDER_CIDR=192.168.200.0/24 PVN_GUEST_MTU=1300 \
    PVN_PROVIDER_PORT_READY=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
    PVN_TOPOLOGY_BIN=$BIN/pvn-topology \
    PVN_CONTROL_PLANE_BIN=$BIN/pvn-control-plane \
    run_local_bootstrap > "$WORK/standalone-full-apply.out"
cat > "$WORK/expected-standalone-setup.log" <<'EOF'
topology plan --geneve-cidr 192.168.100.0/24 --provider-cidr 192.168.200.0/24 --guest-mtu 1300
topology apply --geneve-cidr 192.168.100.0/24 --provider-cidr 192.168.200.0/24 --guest-mtu 1300 --provider-port-ready OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP --confirm standalone-node-a
control plan
control apply --confirm standalone-node-a
EOF
cmp "$WORK/expected-standalone-setup.log" "$SETUP_LOG" ||
    fail "standalone full install did not preserve the setup sequence"
[ "$(cat "$PVECM_LOG")" = status ] ||
    fail "standalone full install did not run exact pvecm status detection"
assert_temp_cleaned

# A minimal standalone .members is accepted only when pvecm returns Proxmox's
# exact no-corosync/non-cluster condition. Clustered or unrelated failures stop
# before topology planning.
for pvn_pvecm_mode in clustered error; do
    write_members 1
    reset_logs
    printf '%s\n' "$pvn_pvecm_mode" > "$PVECM_MODE"
    if PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=standalone-node-a PVN_FULL=1 \
        PVN_GENEVE_CIDR=192.168.100.0/24 \
        PVN_PROVIDER_CIDR=192.168.200.0/24 PVN_GUEST_MTU=1300 \
        PVN_PROVIDER_PORT_READY=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
        PVN_TOPOLOGY_BIN=$BIN/pvn-topology \
        PVN_CONTROL_PLANE_BIN=$BIN/pvn-control-plane \
        run_local_bootstrap > "$WORK/standalone-$pvn_pvecm_mode.out" 2>&1
    then
        fail "standalone accepted pvecm mode $pvn_pvecm_mode"
    fi
    grep -q 'pvecm status did not report the exact non-cluster condition' \
        "$WORK/standalone-$pvn_pvecm_mode.out" ||
        fail "standalone pvecm $pvn_pvecm_mode failure was unclear"
    [ "$(cat "$PVECM_LOG")" = status ] ||
        fail "standalone pvecm $pvn_pvecm_mode check was not exact"
    [ ! -s "$SETUP_LOG" ] ||
        fail "standalone pvecm $pvn_pvecm_mode failure reached setup"
    assert_temp_cleaned
done
printf '%s\n' standalone > "$PVECM_MODE"

# A legacy synthetic one-node nodelist is ambiguous: current standalone PVE
# publishes only nodename/version, while a cluster must carry cluster+nodelist.
cat > "$MEMBERS" <<'EOF'
{"nodename":"node-a","version":7,"nodelist":{"node-a":{"id":1,"online":1,"ip":"192.0.2.11"}}}
EOF
chmod 0600 "$MEMBERS"
reset_logs
if PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=standalone-node-a PVN_FULL=1 \
    PVN_GENEVE_CIDR=192.168.100.0/24 \
    PVN_PROVIDER_CIDR=192.168.200.0/24 PVN_GUEST_MTU=1300 \
    PVN_PROVIDER_PORT_READY=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
    PVN_TOPOLOGY_BIN=$BIN/pvn-topology \
    PVN_CONTROL_PLANE_BIN=$BIN/pvn-control-plane \
    run_local_bootstrap > "$WORK/standalone-ambiguous-members.out" 2>&1
then
    fail "ambiguous standalone nodelist reached full setup"
fi
grep -q 'must contain only nodename/version' "$WORK/standalone-ambiguous-members.out" ||
    fail "ambiguous standalone membership failure was unclear"
[ ! -s "$PVECM_LOG" ] || fail "ambiguous standalone membership invoked pvecm"
[ ! -s "$SETUP_LOG" ] || fail "ambiguous standalone membership invoked setup"
assert_temp_cleaned

# The pvecm identity itself is pinned to a root-owned executable regular file.
write_members 1
ln -s "$BIN/pvecm" "$WORK/pvecm-link"
reset_logs
if PVN_INSTALL_PVECM=$WORK/pvecm-link \
    PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=standalone-node-a PVN_FULL=1 \
    PVN_GENEVE_CIDR=192.168.100.0/24 \
    PVN_PROVIDER_CIDR=192.168.200.0/24 PVN_GUEST_MTU=1300 \
    PVN_PROVIDER_PORT_READY=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
    PVN_TOPOLOGY_BIN=$BIN/pvn-topology \
    PVN_CONTROL_PLANE_BIN=$BIN/pvn-control-plane \
    run_local_bootstrap > "$WORK/standalone-symlink-pvecm.out" 2>&1
then
    fail "standalone accepted a symlinked pvecm"
fi
grep -q 'root-owned executable non-symlink' "$WORK/standalone-symlink-pvecm.out" ||
    fail "standalone unsafe pvecm failure was unclear"
[ ! -s "$PVECM_LOG" ] || fail "unsafe standalone pvecm was executed"
[ ! -s "$SETUP_LOG" ] || fail "unsafe standalone pvecm reached setup"
assert_temp_cleaned

# Every member of every supported odd-sized cluster becomes a central voter.
for pvn_supported_count in 3 5 7; do
    write_members "$pvn_supported_count"
    reset_logs
    PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=lab-cluster PVN_FULL=1 \
        PVN_GENEVE_CIDR=192.168.100.0/24 \
        PVN_PROVIDER_CIDR=192.168.200.0/24 PVN_GUEST_MTU=1300 \
        PVN_PROVIDER_PORT_READY=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
        PVN_TOPOLOGY_BIN=$BIN/pvn-topology \
        PVN_CONTROL_PLANE_BIN=$BIN/pvn-control-plane \
        run_local_bootstrap > "$WORK/$pvn_supported_count-node-full-apply.out"
    cmp "$WORK/expected-setup.log" "$SETUP_LOG" ||
        fail "$pvn_supported_count-node full install did not run the setup sequence"
    assert_temp_cleaned
done

# The package stage may support other cluster sizes, but automated control-plane
# activation requires an odd voter count. Reject an even cluster before even the
# read-only topology plan, and especially before topology/control-plane apply.
for pvn_unsupported_count in 2 4 6; do
    write_members "$pvn_unsupported_count"
    reset_logs
    if PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=lab-cluster PVN_FULL=1 \
        PVN_GENEVE_CIDR=192.168.100.0/24 \
        PVN_PROVIDER_CIDR=192.168.200.0/24 PVN_GUEST_MTU=1300 \
        PVN_PROVIDER_PORT_READY=OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
        PVN_TOPOLOGY_BIN=$BIN/pvn-topology \
        PVN_CONTROL_PLANE_BIN=$BIN/pvn-control-plane \
        run_local_bootstrap > "$WORK/$pvn_unsupported_count-node-unsupported.out" 2>&1
    then
        fail "$pvn_unsupported_count-node cluster reached full setup"
    fi
    grep -q "odd clustered node count of at least three.*found $pvn_unsupported_count clustered node" \
        "$WORK/$pvn_unsupported_count-node-unsupported.out" ||
        fail "$pvn_unsupported_count-node cluster error was unclear"
    [ ! -s "$SETUP_LOG" ] ||
        fail "$pvn_unsupported_count-node cluster invoked setup tooling"
    assert_temp_cleaned
done
write_members 3

reset_logs
if PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=lab-cluster PVN_FULL=1 \
    PVN_GENEVE_CIDR=192.168.100.0/24 \
    PVN_PROVIDER_CIDR=192.168.200.0/24 \
    run_local_bootstrap > "$WORK/full-no-provider-ack.out" 2>&1
then
    fail "full install without provider-port acknowledgement succeeded"
fi
[ ! -s "$CURL_LOG" ] || fail "invalid full install downloaded artifacts"
[ ! -s "$SETUP_LOG" ] || fail "invalid full install ran setup tools"

reset_logs
if PVN_RELEASE_BASE_URL=https://releases.example.invalid/v$RELEASE_VERSION \
    PVN_INVENTORY=$INVENTORY PVN_IDENTITY=$IDENTITY \
    "$BOOTSTRAP" --local-pve preflight > "$WORK/mixed-mode.out" 2>&1
then
    fail "bootstrap mixed local PVE and advanced settings"
fi
[ ! -s "$CURL_LOG" ] || fail "mixed bootstrap mode downloaded artifacts"

reset_logs
PVN_PHASE=preflight run_bootstrap > "$WORK/preflight.out"
[ "$(wc -l < "$CURL_LOG")" -eq 4 ] || fail "bootstrap did not download exactly four files"
grep -q "/pvn-node_${RELEASE_VERSION}_amd64.deb " "$CURL_LOG" || fail "versioned DEB was not downloaded"
grep -q '/pvn-cluster-install ' "$CURL_LOG" || fail "cluster installer was not downloaded"
grep -q '/pvn-cluster-lease ' "$CURL_LOG" || fail "cluster lease helper was not downloaded"
grep -q '/SHA256SUMS ' "$CURL_LOG" || fail "SHA256SUMS was not downloaded"
[ "$(cat "$INSTALLER_LOG")" = "preflight --inventory $INVENTORY --identity $IDENTITY" ] ||
    fail "explicit preflight arguments were incorrect"
assert_temp_cleaned

grep -q -- "--proto =https" "$CURL_LOG" || fail "curl did not restrict the initial protocol"
grep -q -- "--proto-redir =https" "$CURL_LOG" || fail "curl did not restrict redirect protocols"
grep -q -- "--tlsv1.2" "$CURL_LOG" || fail "curl did not require modern TLS"
if grep -Eq '(^|[[:space:]])(-k|--insecure)([[:space:]]|$)' "$BOOTSTRAP"; then
    fail "bootstrap permits insecure curl TLS"
fi

reset_logs
PVN_PHASE=install run_bootstrap > "$WORK/install-dry-run.out"
install_args=$(cat "$INSTALLER_LOG")
case "$install_args" in
    "install --inventory $INVENTORY --identity $IDENTITY --deb "*) ;;
    *) fail "explicit install dry-run arguments were incorrect" ;;
esac
case "$install_args" in *' --apply '*) fail "install default unexpectedly applied" ;; esac
deb_path=$(cat "$DEB_LOG")
[ -n "$deb_path" ] && [ ! -e "$deb_path" ] || fail "downloaded DEB was not cleaned up"

reset_logs
PVN_PHASE=install PVN_APPLY=1 PVN_CONFIRM=lab-cluster run_bootstrap \
    > "$WORK/install-apply.out"
grep -q -- '--apply --confirm lab-cluster$' "$INSTALLER_LOG" ||
    fail "explicit install confirmation was not forwarded"
assert_temp_cleaned

reset_logs
if PVN_PHASE=install PVN_APPLY=1 run_bootstrap > "$WORK/no-confirm.out" 2>&1; then
    fail "apply without a confirmation unexpectedly succeeded"
fi
[ ! -s "$CURL_LOG" ] || fail "missing confirmation downloaded artifacts"

reset_logs
if PVN_RELEASE_BASE_URL=http://releases.example.invalid/v$RELEASE_VERSION \
    PVN_INVENTORY=$INVENTORY PVN_IDENTITY=$IDENTITY PVN_PHASE=preflight \
    "$BOOTSTRAP" > "$WORK/http.out" 2>&1
then
    fail "non-HTTPS release base unexpectedly succeeded"
fi
[ ! -s "$CURL_LOG" ] || fail "non-HTTPS base reached curl"

cp "$ASSETS/pvn-node_${RELEASE_VERSION}_amd64.deb" "$WORK/deb.good"
printf 'tampered payload\n' > "$ASSETS/pvn-node_${RELEASE_VERSION}_amd64.deb"
reset_logs
if PVN_PHASE=preflight run_bootstrap > "$WORK/bad-checksum.out" 2>&1; then
    fail "a bad DEB checksum unexpectedly succeeded"
fi
[ ! -s "$INSTALLER_LOG" ] || fail "installer ran after checksum failure"
assert_temp_cleaned
cp "$WORK/deb.good" "$ASSETS/pvn-node_${RELEASE_VERSION}_amd64.deb"
make_manifest

reset_logs
PVN_PHASE=preflight PVN_TEST_INSTALLER_STATUS=17 run_bootstrap \
    > "$WORK/installer-failure.out" 2>&1 &&
    fail "bootstrap did not propagate installer failure"
assert_temp_cleaned

# The exact no-argument curl shape performs local-PVE preflight without asking
# for an inventory or identity. Empty confirmation stops safely.
reset_logs
printf '\n' | script -qefc \
    "pvn_bootstrap=\$($BIN/public-curl -fsSL https://releases.example.invalid/pvn-install.sh) && PVN_RELEASE_BASE_URL=https://releases.example.invalid/v$RELEASE_VERSION bash -c \"\$pvn_bootstrap\"" \
    /dev/null > "$WORK/local-interactive-stop.out"
[ "$(wc -l < "$INSTALLER_LOG")" -eq 1 ] ||
    fail "default local interactive flow ran more than preflight"
grep -q '^preflight --local-pve$' "$INSTALLER_LOG" ||
    fail "default interactive flow did not use local PVE discovery"
if grep -q 'PVN inventory path\|SSH private-key path' "$WORK/local-interactive-stop.out"; then
    fail "default local interactive flow prompted for advanced paths"
fi
grep -q 'installation was not requested' "$WORK/local-interactive-stop.out" ||
    fail "default local interactive blank confirmation did not stop safely"
assert_temp_cleaned

# Supplying one advanced setting explicitly prompts only for its missing pair.
reset_logs
printf '%s\n' "$IDENTITY" | script -qefc \
    "pvn_bootstrap=\$($BIN/public-curl -fsSL https://releases.example.invalid/pvn-install.sh) && PVN_RELEASE_BASE_URL=https://releases.example.invalid/v$RELEASE_VERSION PVN_INVENTORY='$INVENTORY' PVN_PHASE=preflight bash -c \"\$pvn_bootstrap\"" \
    /dev/null > "$WORK/advanced-prompt.out"
grep -q 'SSH private-key path' "$WORK/advanced-prompt.out" ||
    fail "explicit advanced flow did not prompt for the missing identity"
[ "$(cat "$INSTALLER_LOG")" = "preflight --inventory $INVENTORY --identity $IDENTITY" ] ||
    fail "advanced prompt did not forward both paths"
assert_temp_cleaned

# This exercises the documented fail-propagating wrapper. A blank terminal
# reply proves that its inner bash still owns /dev/tty and stops safely.
reset_logs
printf '\n' | script -qefc \
    "pvn_bootstrap=\$($BIN/public-curl -fsSL https://releases.example.invalid/pvn-install.sh) && PVN_RELEASE_BASE_URL=https://releases.example.invalid/v$RELEASE_VERSION PVN_INVENTORY='$INVENTORY' PVN_IDENTITY='$IDENTITY' bash -c \"\$pvn_bootstrap\"" \
    /dev/null > "$WORK/interactive-stop.out"
[ "$(wc -l < "$INSTALLER_LOG")" -eq 1 ] ||
    fail "implicit interactive blank confirmation ran more than preflight"
grep -q '^preflight ' "$INSTALLER_LOG" || fail "implicit interactive flow did not preflight first"
grep -q 'installation was not requested' "$WORK/interactive-stop.out" ||
    fail "interactive blank confirmation did not stop safely"

reset_logs
printf 'lab-cluster\n' | script -qefc \
    "pvn_bootstrap=\$($BIN/public-curl -fsSL https://releases.example.invalid/pvn-install.sh) && PVN_RELEASE_BASE_URL=https://releases.example.invalid/v$RELEASE_VERSION PVN_INVENTORY='$INVENTORY' PVN_IDENTITY='$IDENTITY' bash -c \"\$pvn_bootstrap\"" \
    /dev/null > "$WORK/interactive-apply.out"
[ "$(wc -l < "$INSTALLER_LOG")" -eq 2 ] ||
    fail "implicit interactive apply did not run exactly preflight then install"
sed -n '1p' "$INSTALLER_LOG" | grep -q '^preflight ' ||
    fail "interactive apply did not begin with preflight"
sed -n '2p' "$INSTALLER_LOG" | grep -q -- '^install .*--apply --confirm lab-cluster$' ||
    fail "interactive exact cluster confirmation was not applied"
assert_temp_cleaned

# The optional interactive full-setup prompt uses the same pre-mutation
# compatibility gate as --full.
write_members 2
reset_logs
if printf 'lab-cluster\ny\n' | script -qefc \
    "pvn_bootstrap=\$($BIN/public-curl -fsSL https://releases.example.invalid/pvn-install.sh) && PVN_RELEASE_BASE_URL=https://releases.example.invalid/v$RELEASE_VERSION PVN_CP_MEMBERS='$MEMBERS' PVN_TOPOLOGY_BIN='$BIN/pvn-topology' PVN_CONTROL_PLANE_BIN='$BIN/pvn-control-plane' bash -c \"\$pvn_bootstrap\"" \
    /dev/null > "$WORK/interactive-unsupported-full.out" 2>&1
then
    fail "interactive unsupported cluster reached full setup"
fi
grep -q 'odd clustered node count of at least three.*found 2 clustered node' \
    "$WORK/interactive-unsupported-full.out" ||
    fail "interactive unsupported cluster error was unclear"
[ ! -s "$SETUP_LOG" ] ||
    fail "interactive unsupported cluster invoked topology/control-plane tooling"
assert_temp_cleaned
write_members 3

echo "pvn-install tests passed"
