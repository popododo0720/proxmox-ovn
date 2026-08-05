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
FULL=${PVN_FULL:-0}
GENEVE_CIDR=${PVN_GENEVE_CIDR:-}
PROVIDER_CIDR=${PVN_PROVIDER_CIDR:-}
GUEST_MTU=${PVN_GUEST_MTU:-}
PROVIDER_PORT_READY=${PVN_PROVIDER_PORT_READY:-}
CURL_BIN=${PVN_CURL_BIN:-curl}
TOPOLOGY_BIN=${PVN_TOPOLOGY_BIN:-/usr/lib/pvn/pvn-topology}
CONTROL_PLANE_BIN=${PVN_CONTROL_PLANE_BIN:-/usr/lib/pvn/pvn-control-plane}
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
  --full                    configure topology and activate OVN after install
  --geneve-cidr CIDR        dedicated Geneve IPv4 network for --full
  --provider-cidr CIDR      provider-uplink IPv4 network for --full
  --guest-mtu MTU           optional guest MTU for --full
  --provider-port-ready P   required provider-port acknowledgement for --full

The default curl one-liner uses --local-pve and read-only preflight. Supplying
PVN_INVENTORY or PVN_IDENTITY explicitly selects advanced mode and prompts for
the missing counterpart. Other settings may use PVN_PHASE, PVN_APPLY,
PVN_CONFIRM, PVN_VERSION, PVN_RELEASE_BASE_URL, PVN_FULL,
PVN_GENEVE_CIDR, PVN_PROVIDER_CIDR, PVN_GUEST_MTU, and
PVN_PROVIDER_PORT_READY.
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
        --full)
            FULL=1
            shift
            ;;
        --geneve-cidr)
            need_value "$@"
            GENEVE_CIDR=$2
            shift 2
            ;;
        --provider-cidr)
            need_value "$@"
            PROVIDER_CIDR=$2
            shift 2
            ;;
        --guest-mtu)
            need_value "$@"
            GUEST_MTU=$2
            shift 2
            ;;
        --provider-port-ready)
            need_value "$@"
            PROVIDER_PORT_READY=$2
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
case "$FULL" in
    0|no|false) FULL=0 ;;
    1|yes|true) FULL=1 ;;
    *) fail "PVN_FULL must be 0/1, no/yes, or false/true" ;;
esac
[ "$PHASE" = install ] || [ "$APPLY" = 0 ] ||
    fail "--apply is valid only with the install phase"
if [ "$APPLY" -eq 1 ]; then
    [ -n "$CONFIRM" ] || fail "install --apply requires --confirm DEPLOYMENT_ID"
fi
if [ "$FULL" -eq 1 ]; then
    [ "$PHASE" = install ] && [ "$APPLY" -eq 1 ] ||
        fail "--full requires install --apply"
    [ -n "$GENEVE_CIDR" ] || fail "--full requires --geneve-cidr"
    [ -n "$PROVIDER_CIDR" ] || fail "--full requires --provider-cidr"
    [ "$PROVIDER_PORT_READY" = OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP ] ||
        fail "--full requires the exact provider-port acknowledgement"
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
[ "$FULL" -eq 0 ] || [ "$ADVANCED" -eq 0 ] ||
    fail "--full must run directly on a PVE node"

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

run_topology() {
    pvn_topology_phase=$1
    pvn_topology_confirm=$2
    pvn_topology_ack=$3
    [ -x "$TOPOLOGY_BIN" ] || fail "installed topology tool is unavailable: $TOPOLOGY_BIN"
    set -- "$pvn_topology_phase" \
        --geneve-cidr "$GENEVE_CIDR" \
        --provider-cidr "$PROVIDER_CIDR"
    if [ -n "$GUEST_MTU" ]; then
        set -- "$@" --guest-mtu "$GUEST_MTU"
    fi
    if [ -n "$pvn_topology_ack" ]; then
        set -- "$@" --provider-port-ready "$pvn_topology_ack"
    fi
    if [ -n "$pvn_topology_confirm" ]; then
        set -- "$@" --confirm "$pvn_topology_confirm"
    fi
    "$TOPOLOGY_BIN" "$@"
}

run_full_setup() {
    pvn_setup_cluster=$1
    [ -x "$CONTROL_PLANE_BIN" ] ||
        fail "installed control-plane tool is unavailable: $CONTROL_PLANE_BIN"

    echo "Planning the three-NIC PVN topology; no changes are made by this step."
    run_topology plan '' ''
    echo "Applying the approved topology on every online PVE node."
    run_topology apply "$pvn_setup_cluster" "$PROVIDER_PORT_READY"
    echo "Planning and activating the OVN/PVN control plane."
    "$CONTROL_PLANE_BIN" plan
    "$CONTROL_PLANE_BIN" apply --confirm "$pvn_setup_cluster"
    echo "PVN topology and control-plane activation completed for $pvn_setup_cluster."
}

prompt_full_setup() {
    pvn_setup_cluster=$1
    printf '%s' \
        'Configure the three-NIC topology and activate OVN now? [y/N]: ' \
        >/dev/tty
    if ! IFS= read -r pvn_setup_answer </dev/tty; then
        echo "No topology confirmation read; package installation is complete." >&2
        return 0
    fi
    case "$pvn_setup_answer" in
        y|Y|yes|YES) ;;
        *)
            echo "Package installation complete; topology was not changed."
            return 0
            ;;
    esac

    [ -x "$TOPOLOGY_BIN" ] || fail "installed topology tool is unavailable: $TOPOLOGY_BIN"
    [ -x "$CONTROL_PLANE_BIN" ] ||
        fail "installed control-plane tool is unavailable: $CONTROL_PLANE_BIN"
    GENEVE_CIDR=$(prompt_path PVN_GENEVE_CIDR "Dedicated Geneve IPv4 CIDR")
    PROVIDER_CIDR=$(prompt_path PVN_PROVIDER_CIDR "Provider-uplink IPv4 CIDR")
    run_topology plan '' ''
    cat >/dev/tty <<'EOF'
Before continuing, every outer OpenStack provider port for these PVE VMs must
allow arbitrary guest/router MAC and IP addresses. The provider NIC will lose
its host IP and become an OVS provider port.
EOF
    PROVIDER_PORT_READY=$(prompt_path PVN_PROVIDER_PORT_READY \
        "Type OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP")
    [ "$PROVIDER_PORT_READY" = OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP ] ||
        fail "provider-port acknowledgement did not match; topology was not changed"
    run_topology apply "$pvn_setup_cluster" "$PROVIDER_PORT_READY"
    "$CONTROL_PLANE_BIN" plan
    "$CONTROL_PLANE_BIN" apply --confirm "$pvn_setup_cluster"
    echo "PVN topology and control-plane activation completed for $pvn_setup_cluster."
}

echo "Verified downloads; running PVN $PHASE."
PVN_PACKAGE_INSTALLED=0
PVN_SETUP_CLUSTER=$CONFIRM
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
                PVN_PACKAGE_INSTALLED=1
                PVN_SETUP_CLUSTER=$pvn_typed_cluster
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
    if [ "$PHASE" = install ] && [ "$APPLY" -eq 1 ]; then
        PVN_PACKAGE_INSTALLED=1
    fi
else
    pvn_status=$?
fi
if [ "$pvn_status" -eq 0 ] && [ "$PVN_PACKAGE_INSTALLED" -eq 1 ]; then
    if [ "$FULL" -eq 1 ]; then
        run_full_setup "$PVN_SETUP_CLUSTER"
    elif [ "$PHASE_EXPLICIT" -eq 0 ] && [ -r /dev/tty ]; then
        prompt_full_setup "$PVN_SETUP_CLUSTER"
    fi
fi
cleanup
trap - 0 HUP INT TERM
exit "$pvn_status"
