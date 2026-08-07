#!/bin/sh
# Checksum-verifying bootstrap for a fail-closed, sequential PVN package update.
# Intended for:
#   pvn_bootstrap=$(curl -fsSL https://PUBLIC_URL/pvn-update.sh) && bash -c "$pvn_bootstrap"
set -eu

PROGRAM=${0##*/}
DEFAULT_VERSION=0.2.19
VERSION=${PVN_VERSION:-$DEFAULT_VERSION}
ARCH=${PVN_ARCH:-amd64}
RELEASE_BASE_URL=${PVN_RELEASE_BASE_URL:-}
PHASE=${PVN_PHASE:-}
CONFIRM=${PVN_CONFIRM:-}
CURL_BIN=${PVN_CURL_BIN:-curl}
WORK=

usage() {
    cat >&2 <<EOF
usage: $PROGRAM [plan|apply] [options]

options:
  --version VERSION         target release (default: $DEFAULT_VERSION)
  --arch ARCH               Debian architecture (default: amd64)
  --release-base-url URL    HTTPS release directory
  --confirm DEPLOYMENT_ID   exact PVE cluster/standalone ID required by apply

With no phase, a terminal session runs a plan and prompts for the exact
deployment ID before applying. A non-interactive session defaults to plan.
PVN_VERSION, PVN_ARCH, PVN_RELEASE_BASE_URL, PVN_PHASE, and PVN_CONFIRM are
also accepted. The release package and updater helper are SHA-256 verified.
EOF
    exit 2
}

fail() {
    echo "$PROGRAM: $*" >&2
    exit 1
}

need_value() {
    [ "$#" -ge 2 ] && [ -n "$2" ] || usage
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        plan|apply)
            [ -z "$PHASE" ] || usage
            PHASE=$1
            shift
            ;;
        --version)
            need_value "$@"
            VERSION=$2
            shift 2
            ;;
        --arch)
            need_value "$@"
            ARCH=$2
            shift 2
            ;;
        --release-base-url)
            need_value "$@"
            RELEASE_BASE_URL=$2
            shift 2
            ;;
        --confirm)
            need_value "$@"
            CONFIRM=$2
            shift 2
            ;;
        -h|--help) usage ;;
        *) usage ;;
    esac
done

case "$VERSION" in
    ''|*[!A-Za-z0-9.+~_-]*) fail "unsafe release version: $VERSION" ;;
esac
case "$ARCH" in
    ''|*[!A-Za-z0-9_-]*) fail "unsafe Debian architecture: $ARCH" ;;
esac
case "$PHASE" in
    ''|plan|apply) ;;
    *) fail "PVN_PHASE must be plan or apply" ;;
esac
if [ "$PHASE" = apply ]; then
    [ -n "$CONFIRM" ] || fail "apply requires --confirm DEPLOYMENT_ID"
fi
case "$CONFIRM" in
    ''|*[!A-Za-z0-9._-]*) [ -z "$CONFIRM" ] || fail "unsafe deployment confirmation" ;;
esac

if [ -z "$RELEASE_BASE_URL" ]; then
    RELEASE_BASE_URL="https://github.com/popododo0720/proxmox-ovn/releases/download/v$VERSION"
fi
while [ "${RELEASE_BASE_URL%/}" != "$RELEASE_BASE_URL" ]; do
    RELEASE_BASE_URL=${RELEASE_BASE_URL%/}
done
case "$RELEASE_BASE_URL" in
    https://*) ;;
    *) fail "PVN_RELEASE_BASE_URL must use https://" ;;
esac
case "$RELEASE_BASE_URL" in
    *[[:space:]]*) fail "PVN_RELEASE_BASE_URL must not contain whitespace" ;;
esac

for pvn_command in "$CURL_BIN" sha256sum awk mktemp chmod rm; do
    command -v "$pvn_command" >/dev/null 2>&1 ||
        fail "required command is unavailable: $pvn_command"
done
[ "$(id -u)" -eq 0 ] || fail "must run as root on a Proxmox VE node"

WORK=$(mktemp -d /tmp/pvn-update.XXXXXXXX) ||
    fail "could not create a private temporary directory"
case "$WORK" in
    /tmp/pvn-update.*) ;;
    *) fail "mktemp returned an unsafe directory" ;;
esac

cleanup() {
    if [ -n "$WORK" ]; then
        case "$WORK" in /tmp/pvn-update.*) rm -rf -- "$WORK" ;; esac
        WORK=
    fi
}
trap cleanup 0
trap 'exit 130' HUP INT TERM

DEB_NAME=pvn-node_${VERSION}_${ARCH}.deb
UPDATER_NAME=pvn-cluster-update
LEASE_NAME=pvn-cluster-lease
DEB_PATH=$WORK/$DEB_NAME
UPDATER_PATH=$WORK/$UPDATER_NAME
LEASE_PATH=$WORK/$LEASE_NAME

download() {
    pvn_url=$1
    pvn_path=$2
    "$CURL_BIN" \
        --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --output "$pvn_path" "$pvn_url"
}

verify_checksum() {
    pvn_artifact=$1
    pvn_name=${pvn_artifact##*/}
    pvn_manifest=$2
    pvn_expected=$(awk -v name="$pvn_name" '
        $2 == name || $2 == ("*" name) { value=$1; count++ }
        END { if (count != 1) exit 1; print value }
    ' "$pvn_manifest") || fail "invalid SHA256SUMS entry for $pvn_name"
    case "$pvn_expected" in ''|*[!0-9a-f]*) fail "invalid SHA-256 for $pvn_name" ;; esac
    [ "${#pvn_expected}" -eq 64 ] || fail "invalid SHA-256 length for $pvn_name"
    pvn_actual=$(sha256sum -- "$pvn_artifact" | awk '{print $1}')
    [ "$pvn_actual" = "$pvn_expected" ] || fail "SHA-256 mismatch for $pvn_name"
}

echo "Downloading the PVN $VERSION rolling updater ($ARCH) from $RELEASE_BASE_URL"
download "$RELEASE_BASE_URL/$DEB_NAME" "$DEB_PATH"
download "$RELEASE_BASE_URL/$UPDATER_NAME" "$UPDATER_PATH"
download "$RELEASE_BASE_URL/$LEASE_NAME" "$LEASE_PATH"
download "$RELEASE_BASE_URL/SHA256SUMS" "$WORK/SHA256SUMS"
verify_checksum "$DEB_PATH" "$WORK/SHA256SUMS"
verify_checksum "$UPDATER_PATH" "$WORK/SHA256SUMS"
verify_checksum "$LEASE_PATH" "$WORK/SHA256SUMS"
chmod 0700 "$UPDATER_PATH" "$LEASE_PATH"

run_updater() {
    pvn_phase=$1
    pvn_confirm=$2
    set -- "$pvn_phase" --deb "$DEB_PATH"
    if [ -n "$pvn_confirm" ]; then
        set -- "$@" --confirm "$pvn_confirm"
    fi
    PVN_CLUSTER_LEASE_BIN=$LEASE_PATH "$UPDATER_PATH" "$@"
}

if [ -n "$PHASE" ]; then
    run_updater "$PHASE" "$CONFIRM"
    pvn_status=$?
elif [ -t 0 ] && [ -r /dev/tty ]; then
    run_updater plan ''
    printf '%s' \
        'Type the exact deployment ID shown above to start the rolling update, or press Enter to stop: ' \
        >/dev/tty
    if IFS= read -r pvn_typed </dev/tty && [ -n "$pvn_typed" ]; then
        case "$pvn_typed" in *[!A-Za-z0-9._-]*) fail "unsafe deployment confirmation" ;; esac
        run_updater apply "$pvn_typed"
        pvn_status=$?
    else
        echo "Update plan complete; package changes were not requested."
        pvn_status=0
    fi
else
    run_updater plan ''
    pvn_status=$?
fi

cleanup
trap - 0 HUP INT TERM
exit "$pvn_status"
