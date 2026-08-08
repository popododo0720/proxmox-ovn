#!/bin/sh
set -eu

repo=$(CDPATH= cd -P "$(dirname "$0")/../.." && pwd)
cd "$repo"

case "$#" in
    0) package_check_group=all ;;
    1) package_check_group=$1 ;;
    *) echo "usage: $0 [fast|topology|control-plane|backup|all]" >&2; exit 2 ;;
esac
case "$package_check_group" in
    fast|topology|control-plane|backup|all) ;;
    *) echo "usage: $0 [fast|topology|control-plane|backup|all]" >&2; exit 2 ;;
esac

group_selected() {
    [ "$package_check_group" = all ] || [ "$package_check_group" = "$1" ]
}

now_ns() {
    date +%s%N
}

report_elapsed() {
    elapsed_ms=$(($2 / 1000000))
    printf 'package-check: group=%s elapsed=%d.%03ds\n' \
        "$1" "$((elapsed_ms / 1000))" "$((elapsed_ms % 1000))"
}

fast_elapsed_ns=0

if group_selected fast; then
fast_started_ns=$(now_ns)

for script in deploy/scripts/pvn-*; do
    [ -f "$script" ] && [ ! -L "$script" ] || continue
    case "$(sed -n '1p' "$script")" in
        *python3*)
            python3 - "$script" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
compile(path.read_bytes(), str(path), "exec")
PY
            ;;
        *perl*) perl -c "$script" >/dev/null ;;
        *) sh -n "$script" ;;
    esac
done
for script in packaging/debian/pvn-node.postinst packaging/debian/pvn-node.prerm \
    packaging/debian/pvn-node.postrm pve-ui/inject.sh pve-api/inject.sh
do
    sh -n "$script"
done
python3 -m py_compile pve-api/compute-inject.py pve-api/tests/compute-injector-test.py \
    pve-api/tests/compute-contract-test.py

perl pve-api/tests/api2-test.pl >/dev/null
perl pve-api/tests/compute-lifecycle-test.pl >/dev/null
pve-api/tests/injector-test.sh
pve-api/tests/compute-injector-test.py
pve-api/tests/compute-contract-test.py

deploy/tests/pvn-cluster-install-test.sh
deploy/tests/pvn-install-test.sh
python3 deploy/tests/pvn-cluster-update-test.py
deploy/tests/pvn-update-test.sh
deploy/tests/pvn-cluster-lease-test.sh
deploy/tests/pvn-ovn-db-listeners-test.sh
python3 -B deploy/tests/pvn-ovn-northd-test.py
python3 -B deploy/tests/pvn-node-ready-test.py
python3 -B deploy/tests/pvn-pve-refresh-test.py
python3 -B packaging/tests/pvn-postinst-test.py
python3 -B packaging/tests/pvn-prerm-test.py
python3 -B packaging/tests/pvn-postrm-test.py
fast_finished_ns=$(now_ns)
fast_elapsed_ns=$((fast_elapsed_ns + fast_finished_ns - fast_started_ns))
fi

if group_selected topology; then
group_started_ns=$(now_ns)
deploy/tests/pvn-topology-test.sh
python3 -B deploy/tests/pvn-topology-standalone-test.py
python3 -B deploy/tests/pvn-topology-corosync-test.py
group_finished_ns=$(now_ns)
report_elapsed topology "$((group_finished_ns - group_started_ns))"
fi

if group_selected control-plane; then
group_started_ns=$(now_ns)
deploy/tests/pvn-control-plane-test.sh
group_finished_ns=$(now_ns)
report_elapsed control-plane "$((group_finished_ns - group_started_ns))"
fi

if group_selected backup; then
group_started_ns=$(now_ns)
python3 deploy/tests/pvn-db-backup-test.py
group_finished_ns=$(now_ns)
report_elapsed backup "$((group_finished_ns - group_started_ns))"
fi

if group_selected fast; then
fast_started_ns=$(now_ns)

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
grep -q '/usr/lib/pvn/pvn-api-verify' packaging/debian/pvn-node.postinst || {
    echo "package configuration must verify the installed PVE API hook" >&2
    exit 1
}
grep -q '/usr/lib/pvn/pvn-compute-verify' packaging/debian/pvn-node.postinst || {
    echo "package configuration must verify the installed PVE compute hooks" >&2
    exit 1
}
python3 - <<'PY'
from pathlib import Path

source = Path("packaging/debian/pvn-node.postinst").read_text()
body = source[source.index("install_pve_extensions() {") : source.index("\n}\n", source.index("install_pve_extensions() {"))]
refresh = "/usr/lib/pvn/pvn-pve-refresh"
refresh_positions = [index for index in range(len(body)) if body.startswith(refresh, index)]
if len(refresh_positions) != 3:
    raise SystemExit("PVE refresh helper must be checked, preflighted, and executed exactly once")
order = [refresh_positions[1], body.index("install_compute"), body.index("install_api"), body.index("install_ui"), refresh_positions[2]]
if order != sorted(order):
    raise SystemExit("daemon preflight and unsupported qemu-server must fail before API/UI PVE mutations")
PY
grep -Eq 'abort-install\|abort-upgrade\|failed-upgrade' packaging/debian/pvn-node.postrm || {
    echo "failed package configuration must remove or recover compute hooks" >&2
    exit 1
}
grep -q '/usr/lib/pvn/pvn-compute-inject remove' packaging/debian/pvn-node.postrm || {
    echo "postrm must invoke the journal-aware compute hook remover" >&2
    exit 1
}
grep -q '/usr/lib/pvn/pvn-pve-refresh --check' packaging/debian/pvn-node.postinst || {
    echo "postinst must preflight daemon generations before PVE file mutation" >&2
    exit 1
}
grep -q '/usr/lib/pvn/pvn-pve-refresh --check' packaging/debian/pvn-node.prerm || {
    echo "prerm must preflight daemon generations before removing PVE hooks" >&2
    exit 1
}
grep -q '^Wants=pvn-node-ready.service$' deploy/systemd/pve-ha-lrm.service.d/90-pvn.conf || {
    echo "pve-ha-lrm must pull in initial PVN node readiness" >&2
    exit 1
}
grep -q '^After=pvn-node-ready.service$' deploy/systemd/pve-ha-lrm.service.d/90-pvn.conf || {
    echo "pve-ha-lrm must start after initial PVN node readiness" >&2
    exit 1
}
grep -q '^interest-noawait /usr/share/perl5/PVE/API2.pm$' packaging/debian/triggers || {
    echo "the package must reapply the PVN API hook after PVE dispatcher updates" >&2
    exit 1
}
for pvn_compute_trigger in \
    /usr/share/perl5/PVE/QemuServer.pm \
    /usr/share/perl5/PVE/QemuMigrate.pm \
    /usr/share/perl5/PVE/API2/Qemu.pm
do
    grep -q "^interest-noawait $pvn_compute_trigger$" packaging/debian/triggers || {
        echo "the package must reapply compute hooks after $pvn_compute_trigger changes" >&2
        exit 1
    }
done
grep -Eq 'qemu-server \(= 9\.1\.15\)' packaging/debian/control packaging/pvn-node.control || {
    echo "runtime metadata must pin the exact supported qemu-server build" >&2
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
grep -q 'central-restart-pending' packaging/debian/pvn-node.postinst || {
    echo "active-central upgrades must leave a durable rolling-restart marker" >&2
    exit 1
}
grep -q 'root:root:700' packaging/debian/pvn-node.postinst || {
    echo "central rolling-restart state must be protected from service users" >&2
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
grep -q '^LoadCredential=pvn-pve-members:/etc/pve/.members$' deploy/systemd/pvn-manager.service || {
    echo "the unprivileged manager must receive PVE membership as a credential" >&2
    exit 1
}
grep -q '^RuntimeDirectory=pvn pvn-api pvn-compute$' deploy/systemd/pvn-manager.service || {
    echo "the manager must own separate runtime, browser, and compute socket directories" >&2
    exit 1
}
grep -q '^SupplementaryGroups=www-data$' deploy/systemd/pvn-manager.service || {
    echo "the manager must share only its browser socket with pveproxy" >&2
    exit 1
}
if grep -Eq 'pvn-(pve-ca|tls-cert|tls-key)|PVN_(PVE_CA_FILE|TLS_CERT|TLS_KEY)' deploy/systemd/pvn-manager.service; then
    echo "the Unix-only manager must not receive browser TLS credentials" >&2
    exit 1
fi
grep -q '^Environment=PVN_PVE_MEMBERS_FILE=%d/pvn-pve-members$' deploy/systemd/pvn-manager.service || {
    echo "the manager must derive its human deployment name from the membership credential" >&2
    exit 1
}
grep -q '^ExecStartPre=/usr/bin/test -r %d/pvn-pve-members$' deploy/systemd/pvn-manager.service || {
    echo "the manager must fail closed when its membership credential is unreadable" >&2
    exit 1
}
if [ "$(grep -Fxc 'PIDFile=/run/ovn/ovnsb_db.pid' \
        deploy/systemd/ovn-ovsdb-server-sb.service.d/90-pvn.conf)" -ne 1 ]; then
    echo "the OVN SB drop-in must override the vendor PIDFile with /run/ovn/ovnsb_db.pid" >&2
    exit 1
fi
grep -q '^ExecStart=/usr/sbin/pvn-manager --config %d/pvn-config --pve-members-file %d/pvn-pve-members$' deploy/systemd/pvn-manager.service || {
    echo "the unprivileged manager must pin its config and membership credential copies" >&2
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
grep -q '^ExecStart=/usr/lib/pvn/pvn-ovn-northd start$' deploy/systemd/ovn-northd.service.d/90-pvn.conf || {
    echo "OVN northd must start through the clustered PVN endpoint launcher" >&2
    exit 1
}
grep -q '^ExecStart=/usr/lib/pvn/pvn-ovn-northd wait$' deploy/systemd/pvn-ovn-northd-ready.service || {
    echo "PVN central readiness must use the bounded northd wait gate" >&2
    exit 1
}
grep -q 'transition=central-restart-pending' deploy/scripts/pvn-ovn-northd || {
    echo "PVN northd wait must identify its marker-scoped standby transition" >&2
    exit 1
}
grep -q 'def package_status_and_version' deploy/scripts/pvn-ovn-northd || {
    echo "PVN northd transition must verify the installed package version" >&2
    exit 1
}
grep -q 'class FatalNorthdError' deploy/scripts/pvn-ovn-northd || {
    echo "PVN northd wait must not retry unsafe restart authorization" >&2
    exit 1
}
grep -q '^transition_roles() {$' docs/operations.md || {
    echo "rolling central runbook must parse the complete northd role set" >&2
    exit 1
}
grep -q '^transition_selection_gate() {$' docs/operations.md || {
    echo "rolling central runbook must gate dynamic standby-first selection" >&2
    exit 1
}
grep -q 'unfinished_standbys =' docs/operations.md &&
    grep -q 'eligible = unfinished_standbys or unfinished_active' docs/operations.md || {
    echo "rolling central runbook must select any unfinished standby before an active" >&2
    exit 1
}
grep -q 'pvn-ovn-northd-ready.service' deploy/systemd/pvn-central.target || {
    echo "PVN central target must require the northd readiness gate" >&2
    exit 1
}
grep -q '/usr/lib/pvn/pvn-ovn-northd status' deploy/scripts/pvn-cluster-update || {
    echo "rolling-update health must verify clustered northd state" >&2
    exit 1
}
grep -q '\[ -x /usr/lib/pvn/pvn-ovn-northd \]' packaging/debian/pvn-node.postinst || {
    echo "package configuration must fail if the northd launcher is missing" >&2
    exit 1
}
grep -q 'pvn-ovn-northd-ready.service' packaging/debian/pvn-node.prerm || {
    echo "package removal must account for the northd readiness gate" >&2
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
    -e 's#\[ ! -S /run/pvn-api/manager.sock \]#false#g' \
    -e 's#\[ -S /run/pvn-api/manager.sock \]#true#g' \
    -e 's#safe_activation_marker /etc/pvn/node-enabled node || exit 2#true#' \
    -e "s#/usr/bin/systemctl#$readiness_bin/systemctl#g" \
    -e "s#/usr/bin/curl#$readiness_bin/curl#g" \
    -e "s#/usr/bin/ovn-appctl#$readiness_bin/ovn-appctl#g" \
    -e "s#/usr/sbin/pvnctl#$readiness_bin/pvnctl#g" \
    -e "s#/usr/lib/pvn/pvn-api-verify#$readiness_bin/api-verify#g" \
    -e "s#/usr/lib/pvn/pvn-ui-verify#$readiness_bin/ui-verify#g" \
    -e "s#/usr/lib/pvn/pvn-ovn-northd#$readiness_bin/pvn-ovn-northd#g" \
    -e "s#/usr/bin/sleep#$readiness_bin/sleep#g" \
    deploy/scripts/pvn-node-ready > "$readiness_test"
chmod 0755 "$readiness_test"
for command in systemctl pvnctl api-verify ui-verify pvn-ovn-northd sleep; do
    printf '#!/bin/sh\nexit 0\n' > "$readiness_bin/$command"
    chmod 0755 "$readiness_bin/$command"
done
printf '#!/bin/sh\nprintf "connected\\n"\n' > "$readiness_bin/ovn-appctl"
chmod 0755 "$readiness_bin/ovn-appctl"
printf '#!/bin/sh\nexit 22\n' > "$readiness_bin/curl"
chmod 0755 "$readiness_bin/curl"
if "$readiness_test" >/dev/null 2>&1; then
    echo "PVN readiness must fail closed while agent health is unavailable" >&2
    exit 1
fi
printf '#!/bin/sh\nexit 0\n' > "$readiness_bin/curl"
printf '#!/bin/sh\nprintf "disconnected\\n"\n' > "$readiness_bin/ovn-appctl"
if "$readiness_test" >/dev/null 2>&1; then
    echo "PVN readiness must fail closed while OVN Southbound is disconnected" >&2
    exit 1
fi
printf '#!/bin/sh\nprintf "connected\\n"\n' > "$readiness_bin/ovn-appctl"
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
ui_loader_sha256=$(sha256sum "$ui_test_root/source-loader.js" | awk '{print $1}')
stale_ui_loader_sha256=$(printf '%064d' 0)
sed "s/$ui_loader_sha256/$stale_ui_loader_sha256/" \
    "$ui_test_root/index.html.tpl" > "$ui_test_root/stale-digest.tpl"
if PVN_PVE_VERSION=9.0 deploy/scripts/pvn-ui-verify \
    "$ui_test_root/stale-digest.tpl" "$ui_test_root/js/pvn-loader.js" \
    "$ui_test_root/source-loader.js" >/dev/null 2>&1
then
    echo "PVE 9 UI verification accepted a stale loader digest" >&2
    exit 1
fi
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

for unit in sysinit.target basic.target network.target network-online.target shutdown.target sockets.target timers.target paths.target multi-user.target pve-cluster.service openvswitch-switch.service pve-guests.service pve-ha-lrm.service ovn-host.service ovn-central.service ovn-controller.service ovn-ovsdb-server-nb.service ovn-ovsdb-server-sb.service ovn-northd.service; do
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
    /usr/lib/pvn/pvn-ovn-northd \
    /usr/lib/pvn/pvn-node-ready \
    /usr/lib/pvn/pvn-guest-gate \
    /usr/lib/pvn/pvn-api-verify \
    /usr/lib/pvn/pvn-compute-verify \
    /usr/lib/pvn/pvn-compute-inject \
    /usr/lib/pvn/pvn-pve-refresh \
    /usr/lib/pvn/pvn-ui-verify \
    /usr/bin/ovs-appctl \
    /usr/bin/test \
    /bin/chown \
    /bin/true
do
    install -D -m 0755 /dev/null "$verify_root$executable"
done

systemd-analyze verify --root="$verify_root" pvn-node.target pvn-central.target pve-guests.service pve-ha-lrm.service
fast_finished_ns=$(now_ns)
fast_elapsed_ns=$((fast_elapsed_ns + fast_finished_ns - fast_started_ns))
report_elapsed fast "$fast_elapsed_ns"
fi
