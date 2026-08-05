#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
BOOTSTRAP=$REPO/deploy/scripts/pvn-update.sh
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' 0 HUP INT TERM

BIN=$WORK/bin
ASSETS=$WORK/assets
CURL_LOG=$WORK/curl.log
UPDATE_LOG=$WORK/update.log
mkdir "$BIN" "$ASSETS"
: > "$CURL_LOG"
: > "$UPDATE_LOG"

cat > "$BIN/curl" <<'EOF'
#!/bin/sh
set -eu
output=
url=
original=$*
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) shift; output=$1 ;;
        https://*) url=$1 ;;
    esac
    shift
done
[ -n "$output" ] && [ -n "$url" ]
printf 'args=%s url=%s output=%s\n' "$original" "$url" "$output" >> "$PVN_TEST_CURL_LOG"
cp "$PVN_TEST_ASSETS/${url##*/}" "$output"
EOF
chmod 0755 "$BIN/curl"

cat > "$ASSETS/pvn-cluster-update" <<'EOF'
#!/bin/sh
set -eu
[ -x "$PVN_CLUSTER_LEASE_BIN" ]
previous=
for argument in "$@"; do
    if [ "$previous" = --deb ]; then [ -f "$argument" ]; fi
    previous=$argument
done
printf '%s\n' "$*" >> "$PVN_TEST_UPDATE_LOG"
EOF
chmod 0755 "$ASSETS/pvn-cluster-update"

cat > "$ASSETS/pvn-cluster-lease" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 0755 "$ASSETS/pvn-cluster-lease"
printf 'PVN DEB\n' > "$ASSETS/pvn-node_0.2.10_amd64.deb"

make_manifest() {
    (
        cd "$ASSETS"
        sha256sum pvn-node_0.2.10_amd64.deb pvn-cluster-update \
            pvn-cluster-lease > SHA256SUMS
    )
}
make_manifest

export PATH="$BIN:$PATH"
export PVN_TEST_ASSETS=$ASSETS
export PVN_TEST_CURL_LOG=$CURL_LOG
export PVN_TEST_UPDATE_LOG=$UPDATE_LOG

fail() {
    echo "pvn-update test failed: $*" >&2
    exit 1
}

reset_logs() {
    : > "$CURL_LOG"
    : > "$UPDATE_LOG"
}

run_bootstrap() {
    PVN_RELEASE_BASE_URL=https://releases.example.invalid/v0.2.10 \
        "$BOOTSTRAP" "$@"
}

sh -n "$BOOTSTRAP"

reset_logs
run_bootstrap plan > "$WORK/plan.out"
[ "$(wc -l < "$CURL_LOG")" -eq 4 ] || fail "plan did not fetch exactly four assets"
grep -q '/pvn-node_0.2.10_amd64.deb ' "$CURL_LOG" || fail "versioned DEB was not downloaded"
grep -q '/pvn-cluster-update ' "$CURL_LOG" || fail "cluster updater was not downloaded"
grep -q '/pvn-cluster-lease ' "$CURL_LOG" || fail "lease helper was not downloaded"
grep -q '/SHA256SUMS ' "$CURL_LOG" || fail "checksum manifest was not downloaded"
case "$(cat "$UPDATE_LOG")" in "plan --deb "*) ;; *) fail "plan arguments were incorrect" ;; esac
grep -q -- '--proto =https' "$CURL_LOG" || fail "curl protocol was not restricted"
grep -q -- '--proto-redir =https' "$CURL_LOG" || fail "curl redirect protocol was not restricted"
grep -q -- '--tlsv1.2' "$CURL_LOG" || fail "curl did not require TLS 1.2"

reset_logs
run_bootstrap apply --confirm lab-cluster > "$WORK/apply.out"
grep -q -- '^apply --deb .* --confirm lab-cluster$' "$UPDATE_LOG" ||
    fail "apply confirmation was not forwarded"

reset_logs
if run_bootstrap apply > "$WORK/no-confirm.out" 2>&1; then
    fail "apply without exact confirmation succeeded"
fi
[ ! -s "$CURL_LOG" ] || fail "invalid apply downloaded release artifacts"

reset_logs
if PVN_RELEASE_BASE_URL=http://releases.example.invalid/v0.2.10 \
    "$BOOTSTRAP" plan > "$WORK/http.out" 2>&1
then
    fail "non-HTTPS release URL succeeded"
fi
[ ! -s "$CURL_LOG" ] || fail "non-HTTPS URL reached curl"

cp "$ASSETS/pvn-node_0.2.10_amd64.deb" "$WORK/good.deb"
printf 'tampered\n' > "$ASSETS/pvn-node_0.2.10_amd64.deb"
reset_logs
if run_bootstrap plan > "$WORK/checksum.out" 2>&1; then
    fail "tampered DEB passed checksum verification"
fi
[ ! -s "$UPDATE_LOG" ] || fail "updater ran after checksum failure"
cp "$WORK/good.deb" "$ASSETS/pvn-node_0.2.10_amd64.deb"
make_manifest

reset_logs
PVN_PHASE=plan run_bootstrap > "$WORK/env-plan.out"
case "$(cat "$UPDATE_LOG")" in "plan --deb "*) ;; *) fail "PVN_PHASE plan was not honored" ;; esac

echo "pvn-update tests passed"
