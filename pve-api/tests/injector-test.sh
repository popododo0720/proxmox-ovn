#!/bin/sh
set -eu

REPO=$(CDPATH= cd -P "$(dirname "$0")/../.." && pwd)
INJECTOR="$REPO/pve-api/inject.sh"
VERIFIER="$REPO/deploy/scripts/pvn-api-verify"
FIXTURE="$REPO/pve-api/tests/fixtures/API2.pm"
MODULE="$REPO/pve-api/PVN/API2.pm"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT HUP INT TERM

fail() {
    echo "pvn-api injector test failed: $*" >&2
    exit 1
}

fresh_dispatcher() {
    cp "$FIXTURE" "$TMPDIR/API2.pm"
}

fresh_dispatcher
chmod 0644 "$TMPDIR/API2.pm"
PVN_PVE_VERSION=9.2 PVN_API_SKIP_PERL_CHECK=1 \
    "$INJECTOR" install "$TMPDIR/API2.pm" "$MODULE"
PVN_PVE_VERSION=9.2 PVN_API_SKIP_PERL_CHECK=1 \
    "$VERIFIER" "$TMPDIR/API2.pm" "$MODULE"
[ "$(grep -Fc '# PVN-API-IMPORT:BEGIN' "$TMPDIR/API2.pm")" -eq 1 ] || fail "import marker count is not one"
[ "$(grep -Fc '# PVN-API-ROUTE:BEGIN' "$TMPDIR/API2.pm")" -eq 1 ] || fail "route marker count is not one"
[ "$(stat -c '%a' "$TMPDIR/API2.pm")" = 644 ] || fail "install changed dispatcher mode"
cp "$TMPDIR/API2.pm" "$TMPDIR/installed.pm"
PVN_PVE_VERSION=9.2 PVN_API_SKIP_PERL_CHECK=1 \
    "$INJECTOR" install "$TMPDIR/API2.pm" "$MODULE"
cmp -s "$TMPDIR/API2.pm" "$TMPDIR/installed.pm" || fail "install is not idempotent"

PVN_API_SKIP_PERL_CHECK=1 "$INJECTOR" remove "$TMPDIR/API2.pm" "$MODULE"
cmp -s "$TMPDIR/API2.pm" "$FIXTURE" || fail "remove did not restore the dispatcher"
[ "$(stat -c '%a' "$TMPDIR/API2.pm")" = 644 ] || fail "remove changed dispatcher mode"

fresh_dispatcher
cp "$TMPDIR/API2.pm" "$TMPDIR/unsupported.pm"
PVN_PVE_VERSION=10.0 PVN_API_SKIP_PERL_CHECK=1 \
    "$INJECTOR" install "$TMPDIR/API2.pm" "$MODULE"
cmp -s "$TMPDIR/API2.pm" "$TMPDIR/unsupported.pm" || fail "unsupported PVE version was modified"

fresh_dispatcher
sed '/use PVE::API2::Storage::Config;/d' "$TMPDIR/API2.pm" > "$TMPDIR/unknown.pm"
cp "$TMPDIR/unknown.pm" "$TMPDIR/unknown-before.pm"
if PVN_PVE_VERSION=9.2 PVN_API_SKIP_PERL_CHECK=1 \
    "$INJECTOR" install "$TMPDIR/unknown.pm" "$MODULE" >/dev/null 2>&1; then
    fail "unknown dispatcher signature was accepted"
fi
cmp -s "$TMPDIR/unknown.pm" "$TMPDIR/unknown-before.pm" || fail "unknown dispatcher was partially modified"

fresh_dispatcher
sed '/use PVE::API2::Storage::Config;/a # PVN-API-IMPORT:BEGIN' \
    "$TMPDIR/API2.pm" > "$TMPDIR/malformed.pm"
cp "$TMPDIR/malformed.pm" "$TMPDIR/malformed-before.pm"
if PVN_PVE_VERSION=9.2 PVN_API_SKIP_PERL_CHECK=1 \
    "$INJECTOR" install "$TMPDIR/malformed.pm" "$MODULE" >/dev/null 2>&1; then
    fail "malformed markers were accepted"
fi
cmp -s "$TMPDIR/malformed.pm" "$TMPDIR/malformed-before.pm" || fail "malformed dispatcher was partially modified"

fresh_dispatcher
if PVN_PVE_VERSION=9.2 PVN_API_SKIP_PERL_CHECK=1 \
    "$INJECTOR" install "$TMPDIR/API2.pm" "$TMPDIR/missing/PVN/API2.pm" >/dev/null 2>&1; then
    fail "missing PVN module was accepted"
fi
cmp -s "$TMPDIR/API2.pm" "$FIXTURE" || fail "missing module changed the dispatcher"

fresh_dispatcher
ln -s "$TMPDIR/API2.pm" "$TMPDIR/API2-link.pm"
if PVN_PVE_VERSION=9.2 PVN_API_SKIP_PERL_CHECK=1 \
    "$INJECTOR" install "$TMPDIR/API2-link.pm" "$MODULE" >/dev/null 2>&1; then
    fail "symlinked dispatcher was accepted"
fi
cmp -s "$TMPDIR/API2.pm" "$FIXTURE" || fail "symlink rejection changed its target"

echo "pvn-api injector tests passed"
