#!/bin/sh
set -eu

[ "$#" -eq 1 ] || { echo "usage: $0 pvn-node.deb" >&2; exit 2; }

deb_input=$1
case "$deb_input" in
    /*) deb=$deb_input ;;
    *)
        deb_directory=$(CDPATH= cd -P "$(dirname "$deb_input")" && pwd) || {
            echo "package directory is not accessible: $deb_input" >&2
            exit 1
        }
        deb=$deb_directory/$(basename "$deb_input")
        ;;
esac
[ -f "$deb" ] && [ ! -L "$deb" ] && [ -r "$deb" ] || {
    echo "package is not a readable regular file: $deb" >&2
    exit 1
}

repo=$(CDPATH= cd -P "$(dirname "$0")/../.." && pwd)
cd "$repo"

verify_root=$(mktemp -d)
cleanup() {
    rm -rf "$verify_root"
}
trap cleanup EXIT HUP INT TERM

package_root=$verify_root/package
control_root=$verify_root/control
install -d "$package_root" "$control_root"
dpkg-deb -x "$deb" "$package_root"
dpkg-deb -e "$deb" "$control_root"

for path in \
    usr/sbin/pvn-manager \
    usr/sbin/pvn-agent \
    usr/sbin/pvnctl \
    usr/sbin/pvn-db-backup \
    usr/lib/pvn/pvn-api-inject \
    usr/lib/pvn/pvn-api-verify \
    usr/lib/pvn/pvn-compute-inject \
    usr/lib/pvn/pvn-compute-verify \
    usr/lib/pvn/pvn-pve-refresh \
    usr/lib/pvn/pvn-ui-inject \
    usr/lib/pvn/pvn-loader.js \
    usr/lib/pvn/pvn-control-db-run \
    usr/lib/pvn/pvn-central-preflight \
    usr/lib/pvn/pvn-ovn-host-preflight \
    usr/lib/pvn/pvn-ovn-db-listeners \
    usr/lib/pvn/pvn-ovn-northd \
    usr/lib/pvn/pvn-node-ready \
    usr/lib/pvn/pvn-guest-gate \
    usr/lib/pvn/pvn-ui-verify \
    usr/lib/pvn/pvn-cluster-lease \
    usr/lib/pvn/pvn-cluster-update \
    usr/lib/pvn/pvn-update.sh \
    usr/lib/pvn/pvn-topology \
    usr/lib/pvn/pvn-control-plane \
    usr/share/perl5/PVN/API2.pm \
    usr/share/perl5/PVN/ComputeLifecycle.pm \
    usr/lib/systemd/system/pvn-node.target \
    usr/lib/systemd/system/pvn-node-ready.service \
    usr/lib/systemd/system/pvn-guest-gate.service \
    usr/lib/systemd/system/pve-ha-lrm.service.d/90-pvn.conf \
    usr/lib/systemd/system/pvn-central.target \
    usr/lib/systemd/system/pvn-ovn-northd-ready.service \
    usr/lib/systemd/system/ovn-controller.service.d/90-pvn.conf \
    usr/lib/systemd/system/ovn-ovsdb-server-nb.service.d/90-pvn.conf \
    usr/lib/systemd/system/ovn-ovsdb-server-sb.service.d/90-pvn.conf \
    usr/lib/systemd/system/ovn-northd.service.d/90-pvn.conf \
    usr/share/pvn/schema/PVN_Control.ovsschema \
    usr/share/doc/pvn-node/examples/config.json \
    usr/share/doc/pvn-node/inventory/pve-cluster.example
do
    [ -e "$package_root/$path" ] || {
        echo "package is missing $path" >&2
        exit 1
    }
done

check_packaged_payload() {
    source=$1
    target=$2
    mode=$3
    [ -f "$target" ] && [ ! -L "$target" ] || {
        echo "packaged payload is not a regular file: $target" >&2
        exit 1
    }
    [ "$(stat -c '%a' "$target")" = "$mode" ] || {
        echo "packaged payload has the wrong mode: $target" >&2
        exit 1
    }
    cmp "$source" "$target" >/dev/null || {
        echo "packaged payload differs from its source: $target" >&2
        exit 1
    }
}

check_packaged_payload pve-api/inject.sh \
    "$package_root/usr/lib/pvn/pvn-api-inject" 755
check_packaged_payload pve-api/PVN/API2.pm \
    "$package_root/usr/share/perl5/PVN/API2.pm" 644
check_packaged_payload pve-api/compute-inject.py \
    "$package_root/usr/lib/pvn/pvn-compute-inject" 755
check_packaged_payload pve-api/PVN/ComputeLifecycle.pm \
    "$package_root/usr/share/perl5/PVN/ComputeLifecycle.pm" 644
check_packaged_payload deploy/scripts/pvn-compute-verify \
    "$package_root/usr/lib/pvn/pvn-compute-verify" 755
check_packaged_payload deploy/scripts/pvn-pve-refresh \
    "$package_root/usr/lib/pvn/pvn-pve-refresh" 755
check_packaged_payload deploy/systemd/pve-ha-lrm.service.d/90-pvn.conf \
    "$package_root/usr/lib/systemd/system/pve-ha-lrm.service.d/90-pvn.conf" 644
check_packaged_payload pve-ui/inject.sh \
    "$package_root/usr/lib/pvn/pvn-ui-inject" 755
check_packaged_payload pve-ui/pvn-loader.js \
    "$package_root/usr/lib/pvn/pvn-loader.js" 644
check_packaged_payload packaging/debian/triggers "$control_root/triggers" 644

if [ "$(grep -Fxc 'PIDFile=/run/ovn/ovnsb_db.pid' \
        "$package_root/usr/lib/systemd/system/ovn-ovsdb-server-sb.service.d/90-pvn.conf")" -ne 1 ]; then
    echo "built package lacks the exact OVN SB PIDFile override" >&2
    exit 1
fi

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
dpkg-deb -f "$deb" Depends | grep -Eq '(^|, )python3([ ,]|$)' || {
    echo "built package does not depend on Python for cluster orchestration" >&2
    exit 1
}
for dependency in liburi-perl libwww-perl; do
    dpkg-deb -f "$deb" Depends | grep -Eq "(^|, )$dependency([ ,]|$)" || {
        echo "built package does not depend on $dependency for the PVE API gateway" >&2
        exit 1
    }
done
dpkg-deb -f "$deb" Depends | grep -Eq '(^|, )qemu-server \(= 9\.1\.15\)(,|$)' || {
    echo "built package does not pin the exact supported qemu-server" >&2
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
    [ -x "$control_root/$maintainer_script" ] || {
        echo "$maintainer_script is missing or not executable" >&2
        exit 1
    }
    sh -n "$control_root/$maintainer_script"
done

if find "$package_root" -path '*/multi-user.target.wants/pvn-central.target' \
        -print -quit | grep -q .; then
    echo "package unexpectedly enables pvn-central.target" >&2
    exit 1
fi
