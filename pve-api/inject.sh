#!/bin/sh
set -eu

IMPORT_BEGIN='# PVN-API-IMPORT:BEGIN'
IMPORT_END='# PVN-API-IMPORT:END'
ROUTE_BEGIN='# PVN-API-ROUTE:BEGIN'
ROUTE_END='# PVN-API-ROUTE:END'
IMPORT_ANCHOR='use PVE::API2::Storage::Config;'
POOL_CLASS='    subclass => "PVE::API2::Pool",'
DEFAULT_DISPATCHER='/usr/share/perl5/PVE/API2.pm'
DEFAULT_MODULE='/usr/share/perl5/PVN/API2.pm'

usage() {
    echo "usage: $0 install|remove [PVE/API2.pm [PVN/API2.pm]]" >&2
    exit 2
}

[ "$#" -ge 1 ] && [ "$#" -le 3 ] || usage
ACTION=$1
DISPATCHER=${2:-$DEFAULT_DISPATCHER}
MODULE=${3:-$DEFAULT_MODULE}

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
            echo "PVN API: unsupported or unknown pve-manager version '${PVE_VERSION:-unknown}'; leaving the Proxmox API unchanged" >&2
            exit 0
            ;;
    esac
fi

[ -f "$DISPATCHER" ] || {
    echo "PVN API: PVE dispatcher is missing; leaving it unchanged" >&2
    [ "$ACTION" != install ] || exit 1
    exit 0
}
[ ! -L "$DISPATCHER" ] || {
    echo "PVN API: refusing a symlinked PVE dispatcher; leaving it unchanged" >&2
    [ "$ACTION" != install ] || exit 1
    exit 0
}

if [ "$ACTION" = install ]; then
    [ -f "$MODULE" ] && [ ! -L "$MODULE" ] || {
        echo "PVN API: packaged API module is missing or symlinked; leaving the Proxmox API unchanged" >&2
        exit 1
    }
fi

TMP=$(mktemp "${DISPATCHER}.pvn.XXXXXX")
CLEAN=$(mktemp "${DISPATCHER}.pvn-clean.XXXXXX")
cleanup() {
    [ -z "${TMP:-}" ] || rm -f "$TMP"
    [ -z "${CLEAN:-}" ] || rm -f "$CLEAN"
}
trap cleanup EXIT HUP INT TERM
cp --preserve=all "$DISPATCHER" "$TMP"

awk -v import_begin="$IMPORT_BEGIN" -v import_end="$IMPORT_END" \
    -v route_begin="$ROUTE_BEGIN" -v route_end="$ROUTE_END" '
    $0 == import_begin {
        if (state != 0 || import_blocks != 0) exit 41
        state=1
        import_blocks++
        next
    }
    $0 == import_end {
        if (state != 1) exit 42
        state=0
        next
    }
    $0 == route_begin {
        if (state != 0 || route_blocks != 0) exit 43
        state=2
        route_blocks++
        next
    }
    $0 == route_end {
        if (state != 2) exit 44
        state=0
        next
    }
    state == 0 { print }
    END { if (state != 0) exit 45 }
' "$DISPATCHER" > "$CLEAN" || {
    echo "PVN API: malformed PVN markers; leaving the Proxmox API unchanged" >&2
    exit 1
}

case "$ACTION" in
    install)
        awk -v import_anchor="$IMPORT_ANCHOR" -v pool_class="$POOL_CLASS" \
            -v import_begin="$IMPORT_BEGIN" -v import_end="$IMPORT_END" \
            -v route_begin="$ROUTE_BEGIN" -v route_end="$ROUTE_END" '
            {
                print
                if ($0 == import_anchor) {
                    import_anchors++
                    print import_begin
                    print "use PVN::API2;"
                    print import_end
                }
                if ($0 == pool_class) {
                    if (pool_open) exit 51
                    pool_open=1
                    pool_classes++
                } else if (pool_open && $0 == "});") {
                    print route_begin
                    print "__PACKAGE__->register_method({"
                    print "    subclass => \"PVN::API2\","
                    print "    path => '\''pvn'\'',"
                    print "});"
                    print route_end
                    route_anchors++
                    pool_open=0
                }
            }
            END {
                if (pool_open || import_anchors != 1 || pool_classes != 1 || route_anchors != 1) exit 52
            }
        ' "$CLEAN" > "$TMP" || {
            echo "PVN API: unknown PVE 9 dispatcher signature; leaving the Proxmox API unchanged" >&2
            exit 1
        }
        ;;
    remove)
        cp "$CLEAN" "$TMP"
        ;;
esac

if [ "${PVN_API_SKIP_PERL_CHECK:-0}" != 1 ]; then
    command -v perl >/dev/null 2>&1 || {
        echo "PVN API: perl is unavailable; leaving the Proxmox API unchanged" >&2
        exit 1
    }
    MODULE_ROOT=$(dirname "$(dirname "$MODULE")")
    if [ "$ACTION" = install ]; then
        perl -T -I "$MODULE_ROOT" -c "$MODULE" >/dev/null || {
            echo "PVN API: packaged API module failed its Perl check; leaving the Proxmox API unchanged" >&2
            exit 1
        }
    fi
    perl -T -I "$MODULE_ROOT" -c "$TMP" >/dev/null || {
        echo "PVN API: staged PVE dispatcher failed its Perl check; leaving the Proxmox API unchanged" >&2
        exit 1
    }
fi

if ! cmp -s "$DISPATCHER" "$TMP"; then
    mv "$TMP" "$DISPATCHER"
    TMP=''
fi

exit 0
