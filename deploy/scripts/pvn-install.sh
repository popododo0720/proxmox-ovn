#!/bin/sh
# Thin, checksum-verifying bootstrap intended for:
#   bash -c "$(curl -fsSL https://PUBLIC_URL/pvn-install.sh)"
# On a PVE node the default discovers local standalone/cluster membership and
# runs read-only preflight. Inventory/key prompts exist only for an explicitly
# selected advanced deployment-host flow.
set -eu

PROGRAM=${0##*/}
DEFAULT_VERSION=0.1.1
VERSION=${PVN_VERSION:-$DEFAULT_VERSION}
ARCH=${PVN_ARCH:-amd64}
RELEASE_BASE_URL=${PVN_RELEASE_BASE_URL:-}
if [ "${PVN_PHASE+x}" = x ]; then
    PHASE=$PVN_PHASE
    PHASE_EXPLICIT=1
else
    PHASE=preflight
    PHASE_EXPLICIT=0
fi
INVENTORY=${PVN_INVENTORY:-}
IDENTITY=${PVN_IDENTITY:-}
LOCAL_PVE_EXPLICIT=0
APPLY=${PVN_APPLY:-0}
CONFIRM=${PVN_CONFIRM:-}
CURL_BIN=${PVN_CURL_BIN:-curl}
WORK=

usage() {
    cat >&2 <<EOF
usage: $PROGRAM [preflight|install] [options]

options:
  --local-pve               discover from this PVE node (default)
  --inventory FILE          advanced deployment inventory
  --identity PRIVATE_KEY    advanced deployment SSH private key
  --version VERSION         release version (default: $DEFAULT_VERSION)
  --arch ARCH               Debian architecture (default: amd64)
  --release-base-url URL    HTTPS release directory
  --apply                   permit the install phase to write remotely
  --confirm DEPLOYMENT_ID   exact discovered ID required with --apply

The default curl one-liner uses --local-pve and read-only preflight. Supplying
PVN_INVENTORY or PVN_IDENTITY explicitly selects advanced mode and prompts for
the missing counterpart. Other settings may use PVN_PHASE, PVN_APPLY,
PVN_CONFIRM, PVN_VERSION, and PVN_RELEASE_BASE_URL.
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
        preflight|install)
            PHASE=$1
            PHASE_EXPLICIT=1
            shift
            ;;
        --inventory)
            need_value "$@"
            INVENTORY=$2
            shift 2
            ;;
        --identity)
            need_value "$@"
            IDENTITY=$2
            shift 2
            ;;
        --local-pve)
            LOCAL_PVE_EXPLICIT=1
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
        --apply)
            APPLY=1
            shift
            ;;
        --confirm)
            need_value "$@"
            CONFIRM=$2
            shift 2
            ;;
        -h|--help)
            usage
            ;;
        *)
            usage
            ;;
    esac
done

case "$PHASE" in
    preflight|install) ;;
    *) fail "PVN_PHASE must be preflight or install" ;;
esac
case "$APPLY" in
    0|no|false) APPLY=0 ;;
    1|yes|true) APPLY=1 ;;
    *) fail "PVN_APPLY must be 0/1, no/yes, or false/true" ;;
esac
[ "$PHASE" = install ] || [ "$APPLY" = 0 ] ||
    fail "--apply is valid only with the install phase"
if [ "$APPLY" -eq 1 ]; then
    [ -n "$CONFIRM" ] || fail "install --apply requires --confirm DEPLOYMENT_ID"
fi

case "$VERSION" in
    ''|*[!A-Za-z0-9.+~_-]*) fail "unsafe release version: $VERSION" ;;
esac

case "$ARCH" in
    ''|*[!A-Za-z0-9_-]*) fail "unsafe Debian architecture: $ARCH" ;;
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

prompt_path() {
    pvn_prompt_name=$1
    pvn_prompt_text=$2
    if [ -r /dev/tty ]; then
        printf '%s: ' "$pvn_prompt_text" >/dev/tty
        IFS= read -r pvn_prompt_value </dev/tty ||
            fail "could not read $pvn_prompt_name"
        [ -n "$pvn_prompt_value" ] || fail "$pvn_prompt_name is required"
        printf '%s\n' "$pvn_prompt_value"
    else
        fail "$pvn_prompt_name is required in a non-interactive session"
    fi
}

ADVANCED=0
if [ -n "$INVENTORY" ] || [ -n "$IDENTITY" ]; then
    ADVANCED=1
fi
[ "$LOCAL_PVE_EXPLICIT" -eq 0 ] || [ "$ADVANCED" -eq 0 ] ||
    fail "--local-pve cannot be combined with inventory or identity settings"

if [ "$ADVANCED" -eq 1 ]; then
    if [ -z "$INVENTORY" ]; then
        INVENTORY=$(prompt_path PVN_INVENTORY "PVN inventory path")
    fi
    if [ -z "$IDENTITY" ]; then
        IDENTITY=$(prompt_path PVN_IDENTITY "SSH private-key path")
    fi
    [ -r "$INVENTORY" ] && [ -f "$INVENTORY" ] ||
        fail "inventory is not a readable regular file: $INVENTORY"
    [ -r "$IDENTITY" ] && [ -f "$IDENTITY" ] ||
        fail "SSH identity is not a readable regular file: $IDENTITY"
fi

for command_name in "$CURL_BIN" sha256sum awk mktemp chmod rm; do
    command -v "$command_name" >/dev/null 2>&1 ||
        fail "required command is unavailable: $command_name"
done

WORK=$(mktemp -d /tmp/pvn-install.XXXXXXXX) ||
    fail "could not create a private temporary directory"
case "$WORK" in
    /tmp/pvn-install.*) ;;
    *) fail "mktemp returned an unsafe directory" ;;
esac

cleanup() {
    if [ -n "$WORK" ]; then
        case "$WORK" in
            /tmp/pvn-install.*) rm -rf -- "$WORK" ;;
        esac
        WORK=
    fi
}
trap cleanup 0
trap 'exit 130' HUP INT TERM

DEB_NAME=pvn-node_${VERSION}_${ARCH}.deb
INSTALLER_NAME=pvn-cluster-install
DEB_PATH=$WORK/$DEB_NAME
INSTALLER_PATH=$WORK/$INSTALLER_NAME

download() {
    pvn_download_url=$1
    pvn_download_path=$2
    "$CURL_BIN" \
        --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --output "$pvn_download_path" \
        "$pvn_download_url"
}

verify_checksum() {
    pvn_artifact=$1
    pvn_artifact_name=${pvn_artifact##*/}
    pvn_checksum_file=$2
    pvn_expected=$(awk -v name="$pvn_artifact_name" '
        $2 == name || $2 == ("*" name) { value=$1; count++ }
        END { if (count != 1) exit 1; print value }
    ' "$pvn_checksum_file") || fail "invalid SHA256SUMS entry for $pvn_artifact_name"
    case "$pvn_expected" in
        ''|*[!0-9a-f]*) fail "invalid SHA-256 for ${pvn_artifact##*/}" ;;
    esac
    [ "${#pvn_expected}" -eq 64 ] ||
        fail "invalid SHA-256 length for ${pvn_artifact##*/}"
    pvn_actual=$(sha256sum -- "$pvn_artifact" | awk '{ print $1 }')
    [ "$pvn_actual" = "$pvn_expected" ] ||
        fail "SHA-256 mismatch for ${pvn_artifact##*/}"
}

echo "Downloading PVN $VERSION ($ARCH) from $RELEASE_BASE_URL"
download "$RELEASE_BASE_URL/$DEB_NAME" "$DEB_PATH"
download "$RELEASE_BASE_URL/$INSTALLER_NAME" "$INSTALLER_PATH"
download "$RELEASE_BASE_URL/SHA256SUMS" "$WORK/SHA256SUMS"

verify_checksum "$DEB_PATH" "$WORK/SHA256SUMS"
verify_checksum "$INSTALLER_PATH" "$WORK/SHA256SUMS"
chmod 0700 "$INSTALLER_PATH"

run_cluster_installer() {
    pvn_run_phase=$1
    pvn_run_apply=$2
    pvn_run_confirm=$3
    set -- "$pvn_run_phase"
    if [ "$ADVANCED" -eq 1 ]; then
        set -- "$@" --inventory "$INVENTORY" --identity "$IDENTITY"
    else
        set -- "$@" --local-pve
    fi
    if [ "$pvn_run_phase" = install ]; then
        set -- "$@" --deb "$DEB_PATH"
    fi
    if [ "$pvn_run_apply" -eq 1 ]; then
        set -- "$@" --apply --confirm "$pvn_run_confirm"
    fi
    "$INSTALLER_PATH" "$@"
}

echo "Verified downloads; running PVN $PHASE."
if [ "$PHASE_EXPLICIT" -eq 0 ] && [ -t 0 ] && [ -r /dev/tty ]; then
    if run_cluster_installer preflight 0 ''; then
        printf '%s' \
            'Type the exact deployment ID shown above to install pvn-node, or press Enter to stop: ' \
            >/dev/tty
        if IFS= read -r pvn_typed_cluster </dev/tty; then
            if [ -z "$pvn_typed_cluster" ]; then
                echo "Preflight complete; installation was not requested."
                pvn_status=0
            elif run_cluster_installer install 1 "$pvn_typed_cluster"; then
                pvn_status=0
            else
                pvn_status=$?
            fi
        else
            echo "No confirmation read; installation was not requested." >&2
            pvn_status=0
        fi
    else
        pvn_status=$?
    fi
elif run_cluster_installer "$PHASE" "$APPLY" "$CONFIRM"; then
    pvn_status=0
else
    pvn_status=$?
fi
cleanup
trap - 0 HUP INT TERM
exit "$pvn_status"
