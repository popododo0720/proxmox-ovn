#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
BOOTSTRAP=$REPO/deploy/scripts/pvn-install.sh
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
mkdir "$BIN" "$ASSETS"
: > "$CURL_LOG"
: > "$INSTALLER_LOG"
: > "$DEB_LOG"

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

printf 'test deb payload\n' > "$ASSETS/pvn-node_0.1.1_amd64.deb"
make_manifest() {
    (
        cd "$ASSETS"
        sha256sum pvn-node_0.1.1_amd64.deb pvn-cluster-install > SHA256SUMS
    )
}
make_manifest

INVENTORY=$WORK/inventory
IDENTITY=$WORK/id_ed25519
printf '%s\n' 'PVN_TARGET_NODES="node-a node-b"' > "$INVENTORY"
: > "$IDENTITY"
chmod 0600 "$IDENTITY"

export PATH="$BIN:$PATH"
export PVN_TEST_ASSETS=$ASSETS
export PVN_TEST_CURL_LOG=$CURL_LOG
export PVN_TEST_INSTALLER_LOG=$INSTALLER_LOG
export PVN_TEST_DEB_LOG=$DEB_LOG

fail() {
    echo "pvn-install test failed: $*" >&2
    exit 1
}

reset_logs() {
    : > "$CURL_LOG"
    : > "$INSTALLER_LOG"
    : > "$DEB_LOG"
}

run_bootstrap() {
    PVN_RELEASE_BASE_URL=https://releases.example.invalid/v0.1.1 \
    PVN_INVENTORY=$INVENTORY PVN_IDENTITY=$IDENTITY "$BOOTSTRAP" "$@"
}

assert_temp_cleaned() {
    while IFS= read -r line; do
        output=${line#* output=}
        [ ! -e "$output" ] || fail "temporary download remains: $output"
    done < "$CURL_LOG"
}

sh -n "$BOOTSTRAP"

reset_logs
PVN_PHASE=preflight run_bootstrap > "$WORK/preflight.out"
[ "$(wc -l < "$CURL_LOG")" -eq 3 ] || fail "bootstrap did not download exactly three files"
grep -q '/pvn-node_0.1.1_amd64.deb ' "$CURL_LOG" || fail "versioned DEB was not downloaded"
grep -q '/pvn-cluster-install ' "$CURL_LOG" || fail "cluster installer was not downloaded"
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
if PVN_RELEASE_BASE_URL=http://releases.example.invalid/v0.1.1 \
    PVN_INVENTORY=$INVENTORY PVN_IDENTITY=$IDENTITY PVN_PHASE=preflight \
    "$BOOTSTRAP" > "$WORK/http.out" 2>&1
then
    fail "non-HTTPS release base unexpectedly succeeded"
fi
[ ! -s "$CURL_LOG" ] || fail "non-HTTPS base reached curl"

cp "$ASSETS/pvn-node_0.1.1_amd64.deb" "$WORK/deb.good"
printf 'tampered payload\n' > "$ASSETS/pvn-node_0.1.1_amd64.deb"
reset_logs
if PVN_PHASE=preflight run_bootstrap > "$WORK/bad-checksum.out" 2>&1; then
    fail "a bad DEB checksum unexpectedly succeeded"
fi
[ ! -s "$INSTALLER_LOG" ] || fail "installer ran after checksum failure"
assert_temp_cleaned
cp "$WORK/deb.good" "$ASSETS/pvn-node_0.1.1_amd64.deb"
make_manifest

reset_logs
PVN_PHASE=preflight PVN_TEST_INSTALLER_STATUS=17 run_bootstrap \
    > "$WORK/installer-failure.out" 2>&1 &&
    fail "bootstrap did not propagate installer failure"
assert_temp_cleaned

# This exercises the exact command-substitution shape. A blank terminal reply
# performs preflight and safely stops without invoking install.
reset_logs
printf '\n' | script -qefc \
    "PVN_RELEASE_BASE_URL=https://releases.example.invalid/v0.1.1 PVN_INVENTORY='$INVENTORY' PVN_IDENTITY='$IDENTITY' bash -c \"\$(cat '$BOOTSTRAP')\"" \
    /dev/null > "$WORK/interactive-stop.out"
[ "$(wc -l < "$INSTALLER_LOG")" -eq 1 ] ||
    fail "implicit interactive blank confirmation ran more than preflight"
grep -q '^preflight ' "$INSTALLER_LOG" || fail "implicit interactive flow did not preflight first"
grep -q 'installation was not requested' "$WORK/interactive-stop.out" ||
    fail "interactive blank confirmation did not stop safely"

reset_logs
printf 'lab-cluster\n' | script -qefc \
    "PVN_RELEASE_BASE_URL=https://releases.example.invalid/v0.1.1 PVN_INVENTORY='$INVENTORY' PVN_IDENTITY='$IDENTITY' bash -c \"\$(cat '$BOOTSTRAP')\"" \
    /dev/null > "$WORK/interactive-apply.out"
[ "$(wc -l < "$INSTALLER_LOG")" -eq 2 ] ||
    fail "implicit interactive apply did not run exactly preflight then install"
sed -n '1p' "$INSTALLER_LOG" | grep -q '^preflight ' ||
    fail "interactive apply did not begin with preflight"
sed -n '2p' "$INSTALLER_LOG" | grep -q -- '^install .*--apply --confirm lab-cluster$' ||
    fail "interactive exact cluster confirmation was not applied"
assert_temp_cleaned

echo "pvn-install tests passed"
