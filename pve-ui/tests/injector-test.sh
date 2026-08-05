#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
PATCHER="$TEST_DIR/../inject.sh"
FIXTURE="$TEST_DIR/fixtures/index.html.tpl"
WORK=$(mktemp -d)
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT HUP INT TERM

cp "$FIXTURE" "$WORK/index.html.tpl"
mkdir "$WORK/js"

PVN_PVE_VERSION=9.0.1 "$PATCHER" install "$WORK/index.html.tpl" "$WORK/js/pvn-loader.js"
[ "$(grep -c 'PVN-LOADER:BEGIN' "$WORK/index.html.tpl")" -eq 1 ]
[ "$(grep -c 'PVN-LOADER:END' "$WORK/index.html.tpl")" -eq 1 ]
[ "$(grep -c '/pve2/js/pvn-loader.js' "$WORK/index.html.tpl")" -eq 1 ]
[ -f "$WORK/js/pvn-loader.js" ]
ANCHOR_LINE=$(grep -n 'pvemanagerlib.js?ver=' "$WORK/index.html.tpl" | cut -d: -f1)
MARKER_LINE=$(grep -n 'PVN-LOADER:BEGIN' "$WORK/index.html.tpl" | cut -d: -f1)
[ "$MARKER_LINE" -eq $((ANCHOR_LINE + 1)) ]

cp "$WORK/index.html.tpl" "$WORK/once.tpl"
PVN_PVE_VERSION=9.0.1 "$PATCHER" install "$WORK/index.html.tpl" "$WORK/js/pvn-loader.js"
cmp "$WORK/once.tpl" "$WORK/index.html.tpl"

"$PATCHER" remove "$WORK/index.html.tpl" "$WORK/js/pvn-loader.js"
cmp "$FIXTURE" "$WORK/index.html.tpl"
[ ! -e "$WORK/js/pvn-loader.js" ]

"$PATCHER" remove "$WORK/index.html.tpl" "$WORK/js/pvn-loader.js"
cmp "$FIXTURE" "$WORK/index.html.tpl"

printf '%s\n' '<html><body>' '<!-- PVN-LOADER:BEGIN -->' '</body></html>' > "$WORK/broken.tpl"
cp "$WORK/broken.tpl" "$WORK/broken-before.tpl"
PVN_PVE_VERSION=9.0.1 "$PATCHER" install "$WORK/broken.tpl" "$WORK/js/pvn-loader.js" 2>/dev/null
cmp "$WORK/broken-before.tpl" "$WORK/broken.tpl"

cp "$FIXTURE" "$WORK/unsupported.tpl"
PVN_PVE_VERSION=10.0 "$PATCHER" install "$WORK/unsupported.tpl" "$WORK/js/unsupported-loader.js" 2>/dev/null
cmp "$FIXTURE" "$WORK/unsupported.tpl"
[ ! -e "$WORK/js/unsupported-loader.js" ]

printf '%s\n' '<!doctype html>' '<html><head></head><body></body></html>' > "$WORK/unknown.tpl"
cp "$WORK/unknown.tpl" "$WORK/unknown-before.tpl"
PVN_PVE_VERSION=9.0.1 "$PATCHER" install "$WORK/unknown.tpl" "$WORK/js/unknown-loader.js" 2>/dev/null
cmp "$WORK/unknown-before.tpl" "$WORK/unknown.tpl"
[ ! -e "$WORK/js/unknown-loader.js" ]

mkdir -p "$WORK/packaged/js"
cp "$FIXTURE" "$WORK/packaged/index.html.tpl"
cp "$PATCHER" "$WORK/packaged/inject.sh"
cp "$TEST_DIR/../pvn-loader.js" "$WORK/packaged/js/pvn-loader.js"
PVN_PVE_VERSION=9.0.1 "$WORK/packaged/inject.sh" install "$WORK/packaged/index.html.tpl" "$WORK/packaged/js/pvn-loader.js"
[ "$(grep -c 'PVN-LOADER:BEGIN' "$WORK/packaged/index.html.tpl")" -eq 1 ]
"$WORK/packaged/inject.sh" remove "$WORK/packaged/index.html.tpl" "$WORK/packaged/js/pvn-loader.js"
[ ! -e "$WORK/packaged/js/pvn-loader.js" ]
"$WORK/packaged/inject.sh" remove "$WORK/packaged/index.html.tpl" "$WORK/packaged/js/pvn-loader.js"

echo 'injector tests passed'
