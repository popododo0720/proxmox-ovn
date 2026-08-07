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
LOADER_SHA256=$(sha256sum "$TEST_DIR/../pvn-loader.js" | awk '{print $1}')
[ "$(grep -c 'PVN-LOADER:BEGIN' "$WORK/index.html.tpl")" -eq 1 ]
[ "$(grep -c 'PVN-LOADER:END' "$WORK/index.html.tpl")" -eq 1 ]
[ "$(grep -c '/pve2/js/pvn-loader.js' "$WORK/index.html.tpl")" -eq 1 ]
grep -Fq "/pve2/js/pvn-loader.js?v=$LOADER_SHA256" "$WORK/index.html.tpl"
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
if PVN_PVE_VERSION=9.0.1 "$PATCHER" install "$WORK/broken.tpl" "$WORK/js/pvn-loader.js" 2>/dev/null; then
    echo "malformed supported PVE 9 template unexpectedly succeeded" >&2
    exit 1
fi
cmp "$WORK/broken-before.tpl" "$WORK/broken.tpl"

cp "$FIXTURE" "$WORK/unsupported.tpl"
PVN_PVE_VERSION=10.0 "$PATCHER" install "$WORK/unsupported.tpl" "$WORK/js/unsupported-loader.js" 2>/dev/null
cmp "$FIXTURE" "$WORK/unsupported.tpl"
[ ! -e "$WORK/js/unsupported-loader.js" ]

printf '%s\n' '<!doctype html>' '<html><head></head><body></body></html>' > "$WORK/unknown.tpl"
cp "$WORK/unknown.tpl" "$WORK/unknown-before.tpl"
if PVN_PVE_VERSION=9.0.1 "$PATCHER" install "$WORK/unknown.tpl" "$WORK/js/unknown-loader.js" 2>/dev/null; then
    echo "unknown supported PVE 9 template unexpectedly succeeded" >&2
    exit 1
fi
cmp "$WORK/unknown-before.tpl" "$WORK/unknown.tpl"
[ ! -e "$WORK/js/unknown-loader.js" ]

if PVN_PVE_VERSION=9.0.1 "$PATCHER" install "$WORK/missing.tpl" "$WORK/js/missing-loader.js" 2>/dev/null; then
    echo "missing supported PVE 9 template unexpectedly succeeded" >&2
    exit 1
fi

ln -s "$FIXTURE" "$WORK/symlink.tpl"
if PVN_PVE_VERSION=9.0.1 "$PATCHER" install "$WORK/symlink.tpl" "$WORK/js/symlink-loader.js" 2>/dev/null; then
    echo "symlinked supported PVE 9 template unexpectedly succeeded" >&2
    exit 1
fi

mkdir -p "$WORK/packaged/js"
cp "$FIXTURE" "$WORK/packaged/index.html.tpl"
cp "$PATCHER" "$WORK/packaged/inject.sh"
cp "$TEST_DIR/../pvn-loader.js" "$WORK/packaged/js/pvn-loader.js"
PVN_PVE_VERSION=9.0.1 "$WORK/packaged/inject.sh" install "$WORK/packaged/index.html.tpl" "$WORK/packaged/js/pvn-loader.js"
[ "$(grep -c 'PVN-LOADER:BEGIN' "$WORK/packaged/index.html.tpl")" -eq 1 ]
"$WORK/packaged/inject.sh" remove "$WORK/packaged/index.html.tpl" "$WORK/packaged/js/pvn-loader.js"
[ ! -e "$WORK/packaged/js/pvn-loader.js" ]
"$WORK/packaged/inject.sh" remove "$WORK/packaged/index.html.tpl" "$WORK/packaged/js/pvn-loader.js"

mkdir -p "$WORK/update/js"
cp "$FIXTURE" "$WORK/update/index.html.tpl"
cp "$PATCHER" "$WORK/update/inject.sh"
cp "$TEST_DIR/../pvn-loader.js" "$WORK/update/pvn-loader.js"
PVN_PVE_VERSION=9.0.1 "$WORK/update/inject.sh" install "$WORK/update/index.html.tpl" "$WORK/update/js/pvn-loader.js"
FIRST_SHA256=$(sha256sum "$WORK/update/pvn-loader.js" | awk '{print $1}')
grep -Fq "/pve2/js/pvn-loader.js?v=$FIRST_SHA256" "$WORK/update/index.html.tpl"
cmp "$WORK/update/pvn-loader.js" "$WORK/update/js/pvn-loader.js"

printf '%s\n' '// cache-buster update test' >> "$WORK/update/pvn-loader.js"
SECOND_SHA256=$(sha256sum "$WORK/update/pvn-loader.js" | awk '{print $1}')
[ "$FIRST_SHA256" != "$SECOND_SHA256" ]
PVN_PVE_VERSION=9.0.1 "$WORK/update/inject.sh" install "$WORK/update/index.html.tpl" "$WORK/update/js/pvn-loader.js"
[ "$(grep -c 'PVN-LOADER:BEGIN' "$WORK/update/index.html.tpl")" -eq 1 ]
grep -Fq "/pve2/js/pvn-loader.js?v=$SECOND_SHA256" "$WORK/update/index.html.tpl"
if grep -Fq "/pve2/js/pvn-loader.js?v=$FIRST_SHA256" "$WORK/update/index.html.tpl"; then
    echo "stale loader cache-buster remained after loader update" >&2
    exit 1
fi
cmp "$WORK/update/pvn-loader.js" "$WORK/update/js/pvn-loader.js"

cp "$WORK/update/index.html.tpl" "$WORK/update/once.tpl"
PVN_PVE_VERSION=9.0.1 "$WORK/update/inject.sh" install "$WORK/update/index.html.tpl" "$WORK/update/js/pvn-loader.js"
cmp "$WORK/update/once.tpl" "$WORK/update/index.html.tpl"
"$WORK/update/inject.sh" remove "$WORK/update/index.html.tpl" "$WORK/update/js/pvn-loader.js"
cmp "$FIXTURE" "$WORK/update/index.html.tpl"
[ ! -e "$WORK/update/js/pvn-loader.js" ]

echo 'injector tests passed'
