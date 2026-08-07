#!/bin/sh
set -eu

MARKER_BEGIN='<!-- PVN-LOADER:BEGIN -->'
MARKER_END='<!-- PVN-LOADER:END -->'
PVE9_ANCHOR='    <script type="text/javascript" src="/pve2/js/pvemanagerlib.js?ver=[% version %]"></script>'
DEFAULT_TEMPLATE='/usr/share/pve-manager/index.html.tpl'
DEFAULT_LOADER='/usr/share/pve-manager/js/pvn-loader.js'

SCRIPT_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
SOURCE_LOADER="$SCRIPT_DIR/pvn-loader.js"

usage() {
    echo "usage: $0 install|remove [index.html.tpl] [loader-target]" >&2
    exit 2
}

[ "$#" -ge 1 ] && [ "$#" -le 3 ] || usage
ACTION=$1
TEMPLATE=${2:-$DEFAULT_TEMPLATE}
LOADER_TARGET=${3:-$DEFAULT_LOADER}

case "$ACTION" in install|remove) ;; *) usage ;; esac

if [ "$ACTION" = install ]; then
    PVE_VERSION=${PVN_PVE_VERSION:-}
    if [ -z "$PVE_VERSION" ] && command -v dpkg-query >/dev/null 2>&1; then
        PVE_VERSION=$(dpkg-query -W -f='${Version}' pve-manager 2>/dev/null || true)
    fi
    PVE_VERSION=${PVE_VERSION#*:}
    case "$PVE_VERSION" in
        9|9.*) ;;
        *)
            echo "PVN UI: unsupported or unknown pve-manager version '${PVE_VERSION:-unknown}'; leaving the Proxmox UI unchanged" >&2
            exit 0
            ;;
    esac
fi

[ -f "$TEMPLATE" ] || {
    echo "PVN UI: PVE template is missing; leaving the Proxmox UI unchanged" >&2
    [ "$ACTION" != install ] || exit 1
    exit 0
}
[ ! -L "$TEMPLATE" ] || {
    echo "PVN UI: refusing a symlinked PVE template; leaving it unchanged" >&2
    [ "$ACTION" != install ] || exit 1
    exit 0
}

[ -f "$SOURCE_LOADER" ] || SOURCE_LOADER="$LOADER_TARGET"
[ "$ACTION" != install ] || [ -f "$SOURCE_LOADER" ] || {
    echo "PVN UI: loader source is missing; leaving the Proxmox UI unchanged" >&2
    exit 1
}
if [ "$ACTION" = install ]; then
    [ ! -L "$LOADER_TARGET" ] || {
        echo "PVN UI: refusing a symlinked loader target; leaving the Proxmox UI unchanged" >&2
        exit 1
    }
    [ -d "$(dirname "$LOADER_TARGET")" ] || {
        echo "PVN UI: loader target directory is missing; leaving the Proxmox UI unchanged" >&2
        exit 1
    }
    command -v sha256sum >/dev/null 2>&1 || {
        echo "PVN UI: sha256sum is required; leaving the Proxmox UI unchanged" >&2
        exit 1
    }
    LOADER_SHA256_OUTPUT=$(sha256sum "$SOURCE_LOADER") || {
        echo "PVN UI: could not hash the loader source; leaving the Proxmox UI unchanged" >&2
        exit 1
    }
    LOADER_SHA256=${LOADER_SHA256_OUTPUT%% *}
    case "$LOADER_SHA256" in
        ''|*[!0-9a-f]*)
            echo "PVN UI: invalid loader SHA-256; leaving the Proxmox UI unchanged" >&2
            exit 1
            ;;
    esac
    [ "${#LOADER_SHA256}" -eq 64 ] || {
        echo "PVN UI: invalid loader SHA-256; leaving the Proxmox UI unchanged" >&2
        exit 1
    }
    SCRIPT_TAG="    <script type=\"text/javascript\" src=\"/pve2/js/pvn-loader.js?v=$LOADER_SHA256\"></script>"
fi

TMP=$(mktemp "${TEMPLATE}.pvn.XXXXXX")
LOADER_TMP=
cleanup() {
    [ -z "${TMP:-}" ] || rm -f "$TMP"
    [ -z "${LOADER_TMP:-}" ] || rm -f "$LOADER_TMP"
}
trap cleanup EXIT HUP INT TERM
cp -p "$TEMPLATE" "$TMP"

case "$ACTION" in
    install)
        awk -v begin="$MARKER_BEGIN" -v end="$MARKER_END" -v tag="$SCRIPT_TAG" -v anchor="$PVE9_ANCHOR" '
            index($0, begin) { if (skip) exit 41; skip=1; next }
            index($0, end) { if (!skip) exit 42; skip=0; next }
            !skip {
                print
                if ($0 == anchor) {
                    anchors++
                    print begin
                    print tag
                    print end
                }
            }
            END {
                if (skip) exit 43
                if (anchors != 1) exit 44
            }
        ' "$TEMPLATE" > "$TMP" || {
            echo "PVN UI: unknown or malformed PVE 9 template signature; leaving the Proxmox UI unchanged" >&2
            exit 1
        }
        if [ "$SOURCE_LOADER" != "$LOADER_TARGET" ]; then
            LOADER_TMP=$(mktemp "${LOADER_TARGET}.pvn.XXXXXX")
            install -m 0644 "$SOURCE_LOADER" "$LOADER_TMP"
            cmp -s "$SOURCE_LOADER" "$LOADER_TMP" || {
                echo "PVN UI: staged loader differs from its source; leaving the template unchanged" >&2
                exit 1
            }
            mv "$LOADER_TMP" "$LOADER_TARGET"
            LOADER_TMP=''
        fi
        if ! cmp -s "$TEMPLATE" "$TMP"; then
            mv "$TMP" "$TEMPLATE"
            TMP=''
        fi
        ;;
    remove)
        awk -v begin="$MARKER_BEGIN" -v end="$MARKER_END" '
            index($0, begin) { if (skip) exit 41; skip=1; next }
            index($0, end) { if (!skip) exit 42; skip=0; next }
            !skip { print }
            END { if (skip) exit 43 }
        ' "$TEMPLATE" > "$TMP" || {
            echo "PVN UI: malformed loader markers; leaving the Proxmox UI unchanged" >&2
            exit 0
        }
        if ! cmp -s "$TEMPLATE" "$TMP"; then
            mv "$TMP" "$TEMPLATE"
            TMP=''
        fi
        if [ -f "$LOADER_TARGET" ]; then
            if cmp -s "$SOURCE_LOADER" "$LOADER_TARGET"; then
                rm -f "$LOADER_TARGET"
            else
                echo "leaving modified loader in place: $LOADER_TARGET" >&2
            fi
        fi
        ;;
esac
