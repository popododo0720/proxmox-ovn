#!/bin/sh
set -eu

repo=$(CDPATH= cd -P "$(dirname "$0")/../.." && pwd)
cd "$repo"

for script in deploy/scripts/* packaging/debian/pvn-node.postinst packaging/debian/pvn-node.prerm packaging/debian/pvn-node.postrm pve-ui/inject.sh; do
    sh -n "$script"
done

if grep -R -n -E '(^|[[:space:]])(ovs-vsctl|ip)[[:space:]].*(add-br|add-port).*br-provider' deploy packaging; then
    echo "package must never create or attach a physical provider bridge" >&2
    exit 1
fi
if grep -n -E 'systemctl[[:space:]]+enable.*pvn-central' packaging/debian/pvn-node.postinst; then
    echo "central role must never be enabled by package installation" >&2
    exit 1
fi
if grep -n 'pvnctl node can-remove' packaging/debian/pvn-node.prerm; then
    echo "package removal must not depend on the currently unwritten node-state file" >&2
    exit 1
fi
if grep -n -E '(touch|install).*/etc/pvn/node-enabled' packaging/debian/pvn-node.postinst; then
    echo "package installation must not create the local activation marker" >&2
    exit 1
fi
if grep -n '/usr/lib/pvn/pvn-ui-inject install || true' packaging/debian/pvn-node.postinst; then
    echo "supported PVE 9 UI installation failures must fail package configuration" >&2
    exit 1
fi
grep -q '/usr/lib/pvn/pvn-ui-verify' packaging/debian/pvn-node.postinst || {
    echo "package configuration must verify the installed PVE UI hook" >&2
    exit 1
}
if grep -n 'systemctl restart.*|| true' packaging/debian/pvn-node.postinst; then
    echo "active-node upgrade restart failures must fail package configuration" >&2
    exit 1
fi
grep -q 'PVN node stack failed to restart during package upgrade' packaging/debian/pvn-node.postinst || {
    echo "active-node upgrade must report a failed restart" >&2
    exit 1
}
grep -q 'PVN_CONTROL_PORT must be 6645' deploy/scripts/pvn-control-db-run || {
    echo "PVN Control client port must be pinned to 6645" >&2
    exit 1
}
grep -q 'PVN_CONTROL_PORT must be explicitly set to 6645' deploy/scripts/pvn-central-preflight || {
    echo "central preflight must reject a nonstandard PVN Control port" >&2
    exit 1
}
manager_runtime=$(sed -n 's/^RuntimeDirectory=//p' deploy/systemd/pvn-manager.service)
control_runtime=$(sed -n 's/^RuntimeDirectory=//p' deploy/systemd/pvn-control-db.service)
[ -n "$manager_runtime" ] && [ -n "$control_runtime" ] && \
    [ "$manager_runtime" != "$control_runtime" ] || {
        echo "manager and control DB must not share a systemd-owned runtime directory" >&2
        exit 1
    }
if grep -q '/run/pvn/' deploy/scripts/pvn-control-db-run deploy/systemd/pvn-control-db.service; then
    echo "control DB must not place sockets in the manager-owned runtime directory" >&2
    exit 1
fi
if grep -n 'if \[ -e /etc/pve/pvn/config.json \]' packaging/debian/pvn-node.prerm; then
    echo "package removal safety must not depend on pmxcfs availability" >&2
    exit 1
fi
grep -q '/etc/pvn/node-enabled' packaging/debian/pvn-node.prerm || {
    echo "package removal must require deletion of the local activation marker" >&2
    exit 1
}

for unit in \
    deploy/systemd/pvn-node.target \
    deploy/systemd/pvn-manager.service \
    deploy/systemd/pvn-agent.service \
    deploy/systemd/pvn-ovn-host-config.service \
    deploy/systemd/pvn-node-ready.service \
    deploy/systemd/ovn-controller.service.d/90-pvn.conf
do
    grep -q '^ConditionPathExists=/etc/pvn/node-enabled$' "$unit" || {
        echo "$unit is not gated by the local activation marker" >&2
        exit 1
    }
done
grep -q '^LoadCredential=pvn-config:/etc/pve/pvn/config.json$' deploy/systemd/pvn-manager.service || {
    echo "the unprivileged manager must receive pmxcfs config as a credential" >&2
    exit 1
}
grep -q '^ExecStart=/usr/sbin/pvn-manager --config %d/pvn-config$' deploy/systemd/pvn-manager.service || {
    echo "the unprivileged manager must read its credential copy" >&2
    exit 1
}
grep -q '^Requires=pvn-guest-gate.service$' deploy/systemd/pve-guests.service.d/90-pvn.conf || {
    echo "PVE guest startup must use the one-shot PVN readiness gate" >&2
    exit 1
}
grep -q '/usr/bin/curl --fail' deploy/scripts/pvn-node-ready || {
    echo "PVN readiness must check the agent health endpoint" >&2
    exit 1
}
grep -q 'curl' packaging/pvn-node.control || {
    echo "the runtime package must depend on the readiness HTTP client" >&2
    exit 1
}

verify_root=$(mktemp -d)
cleanup() {
    rm -rf "$verify_root"
}
trap cleanup EXIT HUP INT TERM

readiness_bin="$verify_root/readiness-bin"
readiness_test="$verify_root/pvn-node-ready"
install -d "$readiness_bin"
sed \
    -e 's#\[ ! -S /run/pvn/manager.sock \]#false#g' \
    -e 's#\[ -S /run/pvn/manager.sock \]#true#g' \
    -e "s#/usr/bin/systemctl#$readiness_bin/systemctl#g" \
    -e "s#/usr/bin/curl#$readiness_bin/curl#g" \
    -e "s#/usr/sbin/pvnctl#$readiness_bin/pvnctl#g" \
    -e "s#/usr/lib/pvn/pvn-ui-verify#$readiness_bin/ui-verify#g" \
    -e "s#/usr/bin/sleep#$readiness_bin/sleep#g" \
    deploy/scripts/pvn-node-ready > "$readiness_test"
chmod 0755 "$readiness_test"
for command in systemctl pvnctl ui-verify sleep; do
    printf '#!/bin/sh\nexit 0\n' > "$readiness_bin/$command"
    chmod 0755 "$readiness_bin/$command"
done
printf '#!/bin/sh\nexit 22\n' > "$readiness_bin/curl"
chmod 0755 "$readiness_bin/curl"
if "$readiness_test" >/dev/null 2>&1; then
    echo "PVN readiness must fail closed while agent health is unavailable" >&2
    exit 1
fi
printf '#!/bin/sh\nexit 0\n' > "$readiness_bin/curl"
if ! "$readiness_test" >/dev/null 2>&1; then
    echo "PVN readiness did not accept a fully healthy test stack" >&2
    exit 1
fi

ui_test_root="$verify_root/ui-test"
install -d "$ui_test_root/js"
install -m 0644 pve-ui/tests/fixtures/index.html.tpl "$ui_test_root/index.html.tpl"
install -m 0644 pve-ui/pvn-loader.js "$ui_test_root/source-loader.js"
install -m 0644 pve-ui/pvn-loader.js "$ui_test_root/js/pvn-loader.js"
if PVN_PVE_VERSION=9.0 deploy/scripts/pvn-ui-verify \
    "$ui_test_root/index.html.tpl" "$ui_test_root/js/pvn-loader.js" \
    "$ui_test_root/source-loader.js" >/dev/null 2>&1
then
    echo "PVE 9 UI verification accepted a template without the PVN marker" >&2
    exit 1
fi
PVN_PVE_VERSION=9.0 pve-ui/inject.sh install \
    "$ui_test_root/index.html.tpl" "$ui_test_root/js/pvn-loader.js"
PVN_PVE_VERSION=9.0 deploy/scripts/pvn-ui-verify \
    "$ui_test_root/index.html.tpl" "$ui_test_root/js/pvn-loader.js" \
    "$ui_test_root/source-loader.js"
sed '/PVN-LOADER:END/d' "$ui_test_root/index.html.tpl" > "$ui_test_root/malformed.tpl"
if PVN_PVE_VERSION=9.0 deploy/scripts/pvn-ui-verify \
    "$ui_test_root/malformed.tpl" "$ui_test_root/js/pvn-loader.js" \
    "$ui_test_root/source-loader.js" >/dev/null 2>&1
then
    echo "PVE 9 UI verification accepted malformed markers" >&2
    exit 1
fi
PVN_PVE_VERSION=10.0 deploy/scripts/pvn-ui-verify \
    "$ui_test_root/missing.tpl" "$ui_test_root/missing-loader.js" \
    "$ui_test_root/missing-source.js"

unit_root="$verify_root/usr/lib/systemd/system"
install -d "$unit_root" "$verify_root/usr/sbin" "$verify_root/usr/lib/pvn" "$verify_root/usr/bin" "$verify_root/bin"
install -m 0644 deploy/systemd/*.service deploy/systemd/*.target "$unit_root/"
for source in deploy/systemd/*.service.d; do
    unit=$(basename "$source")
    install -d "$unit_root/$unit"
    install -m 0644 "$source"/*.conf "$unit_root/$unit/"
done

for unit in sysinit.target basic.target network.target network-online.target shutdown.target sockets.target timers.target paths.target multi-user.target pve-cluster.service openvswitch-switch.service pve-guests.service ovn-host.service ovn-central.service ovn-controller.service ovn-ovsdb-server-nb.service ovn-ovsdb-server-sb.service ovn-northd.service; do
    case "$unit" in
        *.target)
            printf '[Unit]\nDescription=package check stub\n' > "$unit_root/$unit"
            ;;
        *)
            if [ ! -e "$unit_root/$unit" ]; then
                printf '[Unit]\nDescription=package check stub\n[Service]\nType=oneshot\nExecStart=/bin/true\nRemainAfterExit=yes\n' > "$unit_root/$unit"
            fi
            ;;
    esac
done

for executable in \
    /usr/sbin/pvn-manager \
    /usr/sbin/pvn-agent \
    /usr/sbin/pvnctl \
    /usr/lib/pvn/pvn-control-db-run \
    /usr/lib/pvn/pvn-central-preflight \
    /usr/lib/pvn/pvn-ovn-host-preflight \
    /usr/lib/pvn/pvn-ovn-db-listeners \
    /usr/lib/pvn/pvn-node-ready \
    /usr/lib/pvn/pvn-guest-gate \
    /usr/lib/pvn/pvn-ui-verify \
    /usr/bin/test \
    /bin/chown \
    /bin/true
do
    install -D -m 0755 /dev/null "$verify_root$executable"
done

systemd-analyze verify --root="$verify_root" pvn-node.target pvn-central.target pve-guests.service

if [ "$#" -eq 0 ]; then
    exit 0
fi
[ "$#" -eq 1 ] || { echo "usage: $0 [pvn-node.deb]" >&2; exit 2; }
deb=$1
[ -r "$deb" ] || { echo "package is not readable: $deb" >&2; exit 1; }

package_root="$verify_root/package"
control_root="$verify_root/control"
install -d "$package_root" "$control_root"
dpkg-deb -x "$deb" "$package_root"
dpkg-deb -e "$deb" "$control_root"

for path in \
    usr/sbin/pvn-manager \
    usr/sbin/pvn-agent \
    usr/sbin/pvnctl \
    usr/lib/pvn/pvn-ui-inject \
    usr/lib/pvn/pvn-control-db-run \
    usr/lib/pvn/pvn-central-preflight \
    usr/lib/pvn/pvn-ovn-host-preflight \
    usr/lib/pvn/pvn-ovn-db-listeners \
    usr/lib/pvn/pvn-node-ready \
    usr/lib/pvn/pvn-guest-gate \
    usr/lib/pvn/pvn-ui-verify \
    usr/lib/systemd/system/pvn-node.target \
    usr/lib/systemd/system/pvn-node-ready.service \
    usr/lib/systemd/system/pvn-guest-gate.service \
    usr/lib/systemd/system/pvn-central.target \
    usr/lib/systemd/system/ovn-controller.service.d/90-pvn.conf \
    usr/lib/systemd/system/ovn-ovsdb-server-nb.service.d/90-pvn.conf \
    usr/lib/systemd/system/ovn-ovsdb-server-sb.service.d/90-pvn.conf \
    usr/lib/systemd/system/ovn-northd.service.d/90-pvn.conf \
    usr/share/pvn/web/index.html \
    usr/share/pvn/schema/PVN_Control.ovsschema \
    usr/share/doc/pvn-node/examples/config.json \
    usr/share/doc/pvn-node/inventory/pve-cluster-192.168.0.example
do
    [ -e "$package_root/$path" ] || { echo "package is missing $path" >&2; exit 1; }
done

command -v file >/dev/null 2>&1 || {
    echo "file(1) is required to verify production binaries" >&2
    exit 1
}
for binary in pvn-manager pvn-agent pvnctl; do
    file "$package_root/usr/sbin/$binary" | grep -q 'statically linked' || {
        echo "$binary is not a static production binary" >&2
        exit 1
    }
done

package_version=$(dpkg-deb -f "$deb" Version)
dpkg-deb -f "$deb" Depends | grep -Eq '(^|, )curl([ ,]|$)' || {
    echo "built package does not depend on curl for agent readiness" >&2
    exit 1
}
"$package_root/usr/sbin/pvn-manager" --version | grep -Fq "pvn-manager $package_version (" || {
    echo "pvn-manager build version does not match the package" >&2
    exit 1
}
"$package_root/usr/sbin/pvn-agent" --version | grep -Fq "pvn-agent $package_version (" || {
    echo "pvn-agent build version does not match the package" >&2
    exit 1
}
"$package_root/usr/sbin/pvnctl" version | grep -Fq "pvnctl $package_version (" || {
    echo "pvnctl build version does not match the package" >&2
    exit 1
}

for maintainer_script in postinst prerm postrm; do
    [ -x "$control_root/$maintainer_script" ] || { echo "$maintainer_script is missing or not executable" >&2; exit 1; }
    sh -n "$control_root/$maintainer_script"
done

if find "$package_root" -path '*/multi-user.target.wants/pvn-central.target' -print -quit | grep -q .; then
    echo "package unexpectedly enables pvn-central.target" >&2
    exit 1
fi

exit 0
