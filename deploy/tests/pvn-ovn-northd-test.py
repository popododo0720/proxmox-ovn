#!/usr/bin/python3
"""Black-box tests for the packaged ovn-northd launcher/readiness gate."""

from __future__ import annotations

import fcntl
import json
import os
import pathlib
import subprocess
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
HELPER = REPO / "deploy/scripts/pvn-ovn-northd"
HELPER_SOURCE = HELPER.read_text(encoding="utf-8")


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def write_executable(path: pathlib.Path, text: str) -> None:
    path.write_text(text, encoding="utf-8")
    path.chmod(0o755)


with tempfile.TemporaryDirectory() as temporary:
    root = pathlib.Path(temporary)
    config_path = root / "config.json"
    ca = root / "ca.pem"
    cert = root / "node.pem"
    key = root / "node-key.pem"
    for path in (ca, cert, key):
        path.write_text("test\n", encoding="ascii")
    ca.chmod(0o644)
    cert.chmod(0o644)
    key.chmod(0o640)

    calls = root / "calls.json"
    query_calls = root / "query-calls.jsonl"
    ovn_ctl = root / "ovn-ctl"
    systemctl = root / "systemctl"
    appctl = root / "ovn-appctl"
    nbctl = root / "ovn-nbctl"
    sbctl = root / "ovn-sbctl"
    dpkg_query = root / "dpkg-query"
    role_counter = root / "role-counter"
    restart_directory = root / "pvn-node"
    restart_directory.mkdir(mode=0o700)
    restart_marker = restart_directory / "central-restart-pending"
    package_transition_directory = root / "run-pvn-node"
    package_transition_directory.mkdir(mode=0o700)
    package_transition_auth = package_transition_directory / "package-configuring"
    db_params = root / "ovn-northd-db-params.conf"

    write_executable(
        ovn_ctl,
        """#!/usr/bin/python3
import json, os, pathlib, sys
pathlib.Path(os.environ["FAKE_START_CALLS"]).write_text(json.dumps(sys.argv[1:]), encoding="utf-8")
""",
    )
    write_executable(
        systemctl,
        """#!/bin/sh
[ "$1 $2 $3" = "is-active --quiet ovn-northd.service" ] || exit 2
[ "${FAKE_SYSTEMD_ACTIVE:-yes}" = yes ]
""",
    )
    write_executable(
        appctl,
        """#!/usr/bin/python3
import os, pathlib, sys
if sys.argv[1:3] != ["-t", "ovn-northd"] or len(sys.argv) != 4:
    raise SystemExit(2)
command = sys.argv[3]
if command == "nb-connection-status":
    print(os.environ.get("FAKE_NB_CONNECTION", "connected"))
elif command == "sb-connection-status":
    print(os.environ.get("FAKE_SB_CONNECTION", "connected"))
elif command == "status":
    sequence = os.environ.get("FAKE_ROLE_SEQUENCE", "")
    if sequence:
        roles = sequence.split(",")
        counter = pathlib.Path(os.environ["FAKE_ROLE_COUNTER"])
        index = int(counter.read_text(encoding="ascii")) if counter.exists() else 0
        role = roles[min(index, len(roles) - 1)]
        counter.write_text(str(index + 1), encoding="ascii")
    else:
        role = os.environ.get("FAKE_ROLE", "active")
    print(f"Status: {role}")
else:
    raise SystemExit(2)
""",
    )
    query_program = """#!/usr/bin/python3
import json, os, pathlib, sys
for name in ("OVN_NB_DB", "OVN_NBCTL_OPTIONS", "OVN_NB_DAEMON", "OVN_SB_DB", "OVN_SBCTL_OPTIONS", "OVN_SB_DAEMON"):
    if name in os.environ:
        raise SystemExit(9)
with pathlib.Path(os.environ["FAKE_QUERY_CALLS"]).open("a", encoding="utf-8") as stream:
    stream.write(json.dumps([pathlib.Path(sys.argv[0]).name, *sys.argv[1:]]) + "\\n")
column = sys.argv[-1]
if pathlib.Path(sys.argv[0]).name == "ovn-sbctl":
    value = os.environ.get("FAKE_SB_NB_CFG", "42")
elif column == "nb_cfg":
    value = os.environ.get("FAKE_NB_CFG", "42")
elif column == "sb_cfg":
    value = os.environ.get("FAKE_NB_SB_CFG", "42")
else:
    raise SystemExit(2)
print(value)
"""
    write_executable(nbctl, query_program)
    write_executable(sbctl, query_program)
    write_executable(
        dpkg_query,
        """#!/usr/bin/python3
import os, sys
if sys.argv[1:] != ["-W", r"-f=${Status}\\t${Version}\\n", "pvn-node"]:
    raise SystemExit(2)
for name in ("DPKG_ROOT", "DPKG_ADMINDIR"):
    if name in os.environ:
        raise SystemExit(9)
if os.environ.get("FAKE_DPKG_FAILURE") == "yes":
    raise SystemExit(3)
version = os.environ.get("FAKE_INSTALLED_VERSION", "0.2.16")
status = os.environ.get("FAKE_PACKAGE_STATUS", "installed")
print(f"install ok {status}\\t{version}")
""",
    )

    environment = os.environ.copy()
    environment.update(
        {
            "PVN_NORTHD_CONFIG": str(config_path),
            "PVN_NORTHD_OVN_CTL": str(ovn_ctl),
            "PVN_NORTHD_OVN_APPCTL": str(appctl),
            "PVN_NORTHD_OVN_NBCTL": str(nbctl),
            "PVN_NORTHD_OVN_SBCTL": str(sbctl),
            "PVN_NORTHD_SYSTEMCTL": str(systemctl),
            "PVN_NORTHD_DPKG_QUERY": str(dpkg_query),
            "PVN_NORTHD_RESTART_STATE_DIR": str(restart_directory),
            "PVN_NORTHD_PACKAGE_TRANSITION_DIR": str(
                package_transition_directory
            ),
            "PVN_NORTHD_DB_PARAMS": str(db_params),
            "FAKE_START_CALLS": str(calls),
            "FAKE_QUERY_CALLS": str(query_calls),
            "FAKE_ROLE_COUNTER": str(role_counter),
            "OVN_NB_DB": "unix:/poisoned-nb.sock",
            "OVN_NBCTL_OPTIONS": "--db=unix:/poisoned-nb.sock",
            "OVN_NB_DAEMON": "poisoned",
            "OVN_SB_DB": "unix:/poisoned-sb.sock",
            "OVN_SBCTL_OPTIONS": "--db=unix:/poisoned-sb.sock",
            "OVN_SB_DAEMON": "poisoned",
            "DPKG_ROOT": "/poisoned-root",
            "DPKG_ADMINDIR": "/poisoned-admindir",
        }
    )

    def config(hosts: list[str]) -> dict[str, object]:
        return {
            "ovn": {
                "control_db": [f"ssl:{host}:6645" for host in hosts],
                "northbound": [f"ssl:{host}:6641" for host in hosts],
                "southbound": [f"ssl:{host}:6642" for host in hosts],
                "tls_ca": str(ca),
                "tls_cert": str(cert),
                "tls_key": str(key),
            }
        }

    def write_config(value: dict[str, object]) -> None:
        config_path.write_text(json.dumps(value) + "\n", encoding="utf-8")
        config_path.chmod(0o640)

    def run(action: str, extra: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
        current = environment.copy()
        if extra:
            current.update(extra)
        return subprocess.run(
            [str(HELPER), action],
            env=current,
            text=True,
            capture_output=True,
            check=False,
            timeout=5,
        )

    def wait_stays_blocked(extra: dict[str, str]) -> None:
        current = environment.copy()
        current.update(extra)
        try:
            subprocess.run(
                [str(HELPER), "wait"],
                env=current,
                text=True,
                capture_output=True,
                check=False,
                timeout=0.35,
            )
        except subprocess.TimeoutExpired:
            return
        raise AssertionError("readiness wait unexpectedly completed")

    def write_marker(version: str = "0.2.16") -> None:
        restart_marker.unlink(missing_ok=True)
        restart_marker.write_text(version + "\n", encoding="ascii")
        restart_marker.chmod(0o600)

    def remove_marker() -> None:
        restart_marker.unlink(missing_ok=True)
        role_counter.unlink(missing_ok=True)

    def write_package_auth(version: str = "0.2.16", *, locked: bool) -> int:
        package_transition_auth.unlink(missing_ok=True)
        descriptor = os.open(
            package_transition_auth,
            os.O_RDWR | os.O_CREAT | os.O_EXCL,
            0o600,
        )
        os.write(descriptor, (version + "\n").encode("ascii"))
        if locked:
            fcntl.flock(descriptor, fcntl.LOCK_EX)
        return descriptor

    standalone_hosts = ["192.0.2.10"]
    write_config(config(standalone_hosts))
    standalone = run("start")
    check(standalone.returncode == 0, f"standalone start failed: {standalone.stderr}")
    standalone_arguments = json.loads(calls.read_text(encoding="utf-8"))
    check(
        "--ovn-northd-nb-db=ssl:192.0.2.10:6641" in standalone_arguments
        and "--ovn-northd-sb-db=ssl:192.0.2.10:6642" in standalone_arguments,
        "standalone start did not pin its one TLS NB/SB endpoint",
    )
    check(
        f"--ovn-northd-ssl-key={key}" in standalone_arguments
        and f"--ovn-northd-ssl-cert={cert}" in standalone_arguments
        and f"--ovn-northd-ssl-ca-cert={ca}" in standalone_arguments,
        "standalone start omitted configured TLS credentials",
    )

    raft_hosts = ["192.0.2.11", "192.0.2.12", "192.0.2.13"]
    write_config(config(raft_hosts))
    raft = run("start")
    check(raft.returncode == 0, f"Raft start failed: {raft.stderr}")
    raft_arguments = json.loads(calls.read_text(encoding="utf-8"))
    expected_nb = "--ovn-northd-nb-db=" + ",".join(
        f"ssl:{host}:6641" for host in raft_hosts
    )
    expected_sb = "--ovn-northd-sb-db=" + ",".join(
        f"ssl:{host}:6642" for host in raft_hosts
    )
    check(
        expected_nb in raft_arguments and expected_sb in raft_arguments,
        "Raft start did not preserve every ordered TLS endpoint",
    )

    five_hosts = [f"198.51.100.{index}" for index in range(11, 16)]
    write_config(config(five_hosts))
    five_voters = run("start")
    check(five_voters.returncode == 0, "five-voter odd Raft endpoint set was rejected")

    write_config(config(["192.0.2.11", "192.0.2.12"]))
    even = run("start")
    check(even.returncode != 0 and "odd Raft endpoints" in even.stderr, "even Raft endpoint set was accepted")

    mismatched = config(raft_hosts)
    mismatched_ovn = mismatched["ovn"]
    assert isinstance(mismatched_ovn, dict)
    mismatched_ovn["southbound"] = [
        "ssl:192.0.2.12:6642",
        "ssl:192.0.2.11:6642",
        "ssl:192.0.2.13:6642",
    ]
    write_config(mismatched)
    mismatch = run("start")
    check(mismatch.returncode != 0 and "same ordered voter set" in mismatch.stderr, "mismatched NB/SB voters were accepted")

    control_mismatched = config(raft_hosts)
    control_mismatched_ovn = control_mismatched["ovn"]
    assert isinstance(control_mismatched_ovn, dict)
    control_mismatched_ovn["control_db"] = [
        "ssl:192.0.2.11:6645",
        "ssl:192.0.2.13:6645",
        "ssl:192.0.2.12:6645",
    ]
    write_config(control_mismatched)
    control_mismatch = run("start")
    check(
        control_mismatch.returncode != 0
        and "same ordered voter set" in control_mismatch.stderr,
        "control/NB/SB voter mismatch was accepted",
    )

    insecure = config(standalone_hosts)
    insecure_ovn = insecure["ovn"]
    assert isinstance(insecure_ovn, dict)
    insecure_ovn["northbound"] = ["tcp:192.0.2.10:6641"]
    write_config(insecure)
    insecure_result = run("start")
    check(insecure_result.returncode != 0 and "must use ssl" in insecure_result.stderr, "non-TLS northbound endpoint was accepted")

    write_config(config(raft_hosts))
    config_path.chmod(0o660)
    unsafe_mode = run("start")
    check(unsafe_mode.returncode != 0 and "not group/world writable" in unsafe_mode.stderr, "group-writable configuration was accepted")

    write_config(config(raft_hosts))
    db_params.write_text("--ovnnb-db=unix:/wrong\n", encoding="ascii")
    override = run("start")
    check(override.returncode != 0 and "parameter override exists" in override.stderr, "vendor northd DB override bypass was accepted")
    db_params.unlink()

    query_calls.write_text("", encoding="ascii")
    healthy = run("status")
    check(healthy.returncode == 0, f"healthy status failed: {healthy.stderr}")
    check(
        healthy.stdout.strip() == "PVN_NORTHD role=active nb=connected sb=connected cfg=42",
        "healthy status record is malformed",
    )
    recorded_queries = [
        json.loads(line) for line in query_calls.read_text(encoding="utf-8").splitlines()
    ]
    check(len(recorded_queries) == 3, "status did not perform exactly three cfg reads")
    for invocation in recorded_queries:
        expected_db = expected_sb.removeprefix("--ovn-northd-sb-db=") if invocation[0] == "ovn-sbctl" else expected_nb.removeprefix("--ovn-northd-nb-db=")
        check(f"--db={expected_db}" in invocation, "status query omitted the full clustered endpoint set")
        check(
            f"--private-key={key}" in invocation
            and f"--certificate={cert}" in invocation
            and f"--ca-cert={ca}" in invocation,
            "status query omitted TLS credentials",
        )

    standby = run("status", {"FAKE_ROLE": "standby"})
    check(standby.returncode == 0 and "role=standby" in standby.stdout, "healthy standby northd was rejected")

    disconnected = run("status", {"FAKE_NB_CONNECTION": "not connected"})
    check(disconnected.returncode != 0 and "connection is incomplete" in disconnected.stderr, "disconnected NB IDL was accepted")

    role_drift = run("status", {"FAKE_ROLE": "paused"})
    check(role_drift.returncode != 0 and "invalid active/standby" in role_drift.stderr, "invalid northd role was accepted")

    cfg_drift = run("status", {"FAKE_NB_CFG": "43"})
    check(cfg_drift.returncode != 0 and "synchronization is incomplete" in cfg_drift.stderr, "NB/SB cfg drift was accepted")

    node_ready_without_marker = run("node-ready", {"FAKE_NB_CFG": "43"})
    check(
        node_ready_without_marker.returncode != 0
        and "synchronization is incomplete" in node_ready_without_marker.stderr,
        "normal node readiness accepted cfg drift without a restart marker",
    )

    malformed_cfg = run("status", {"FAKE_SB_NB_CFG": "[42]"})
    check(malformed_cfg.returncode != 0 and "canonical non-negative integer" in malformed_cfg.stderr, "malformed cfg value was accepted")

    inactive = run("status", {"FAKE_SYSTEMD_ACTIVE": "no"})
    check(inactive.returncode != 0 and "systemctl" in inactive.stderr, "inactive systemd northd was accepted")

    waiting = run("wait", {"FAKE_ROLE": "standby"})
    check(waiting.returncode == 0 and "role=standby" in waiting.stdout, "readiness wait rejected an immediately healthy standby")

    write_config(config(raft_hosts))
    write_marker()
    transition_node_ready = run(
        "node-ready",
        {"FAKE_ROLE": "active", "FAKE_NB_CFG": "43"},
    )
    check(
        transition_node_ready.returncode == 0
        and transition_node_ready.stdout.strip()
        == "PVN_NORTHD role=active nb=unchecked sb=unchecked cfg=pending transition=central-restart-pending",
        f"package transition node readiness rejected the preserved active northd: {transition_node_ready.stderr}",
    )
    disconnected_node_ready = run(
        "node-ready",
        {
            "FAKE_ROLE": "active",
            "FAKE_NB_CFG": "43",
            "FAKE_NB_CONNECTION": "not connected",
        },
    )
    check(
        disconnected_node_ready.returncode == 0
        and "transition=central-restart-pending" in disconnected_node_ready.stdout,
        "transition node readiness rejected the preserved disconnected old northd",
    )
    check(
        "connected" not in disconnected_node_ready.stdout,
        "relaxed package transition falsely reported disconnected IDLs as connected",
    )

    half_configured_without_auth = run(
        "node-ready",
        {"FAKE_ROLE": "active", "FAKE_PACKAGE_STATUS": "half-configured"},
    )
    check(
        half_configured_without_auth.returncode != 0
        and "package transition" in half_configured_without_auth.stderr,
        "half-configured package was accepted without live postinst authorization",
    )
    stale_auth_descriptor = write_package_auth(locked=False)
    os.close(stale_auth_descriptor)
    half_configured_stale_auth = run(
        "node-ready",
        {"FAKE_ROLE": "active", "FAKE_PACKAGE_STATUS": "half-configured"},
    )
    check(
        half_configured_stale_auth.returncode != 0
        and "stale and unlocked" in half_configured_stale_auth.stderr,
        "half-configured package accepted a stale postinst authorization",
    )
    live_auth_descriptor = write_package_auth(locked=True)
    half_configured_live_auth = run(
        "node-ready",
        {"FAKE_ROLE": "active", "FAKE_PACKAGE_STATUS": "half-configured"},
    )
    check(
        half_configured_live_auth.returncode == 0
        and "transition=central-restart-pending" in half_configured_live_auth.stdout,
        f"live postinst authorization was rejected: {half_configured_live_auth.stderr}",
    )
    os.close(live_auth_descriptor)
    package_transition_auth.unlink()

    wrong_auth_descriptor = write_package_auth("0.2.15", locked=True)
    half_configured_wrong_auth = run(
        "node-ready",
        {"FAKE_ROLE": "active", "FAKE_PACKAGE_STATUS": "half-configured"},
    )
    check(
        half_configured_wrong_auth.returncode != 0
        and "does not match installed" in half_configured_wrong_auth.stderr,
        "half-configured package accepted a locked authorization for another version",
    )
    os.close(wrong_auth_descriptor)
    package_transition_auth.unlink()

    unsafe_auth_descriptor = write_package_auth(locked=True)
    package_transition_auth.chmod(0o666)
    half_configured_unsafe_auth = run(
        "node-ready",
        {"FAKE_ROLE": "active", "FAKE_PACKAGE_STATUS": "half-configured"},
    )
    check(
        half_configured_unsafe_auth.returncode != 0
        and "mode 0600" in half_configured_unsafe_auth.stderr,
        "half-configured package accepted a writable authorization",
    )
    os.close(unsafe_auth_descriptor)
    package_transition_auth.unlink()

    linked_auth_descriptor = write_package_auth(locked=True)
    auth_hardlink = root / "package-auth-hardlink"
    os.link(package_transition_auth, auth_hardlink)
    half_configured_linked_auth = run(
        "node-ready",
        {"FAKE_ROLE": "active", "FAKE_PACKAGE_STATUS": "half-configured"},
    )
    check(
        half_configured_linked_auth.returncode != 0
        and "exactly one hard link" in half_configured_linked_auth.stderr,
        "half-configured package accepted a hardlinked authorization",
    )
    os.close(linked_auth_descriptor)
    auth_hardlink.unlink()
    package_transition_auth.unlink()
    transition_wait = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        transition_wait.returncode == 0
        and transition_wait.stdout.strip()
        == "PVN_NORTHD role=standby nb=connected sb=connected cfg=pending transition=central-restart-pending",
        f"matching restart marker did not admit a connected clustered standby: {transition_wait.stderr}",
    )
    strict_during_transition = run(
        "status",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        strict_during_transition.returncode != 0
        and "synchronization is incomplete" in strict_during_transition.stderr,
        "strict status accepted cfg drift merely because a restart marker existed",
    )

    already_synchronized = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_DPKG_FAILURE": "yes"},
    )
    check(
        already_synchronized.returncode == 0
        and already_synchronized.stdout.strip()
        == "PVN_NORTHD role=standby nb=connected sb=connected cfg=42",
        "synchronized wait unnecessarily depended on transition marker validation",
    )

    wait_stays_blocked({"FAKE_ROLE": "active", "FAKE_NB_CFG": "43"})
    wait_stays_blocked(
        {
            "FAKE_ROLE": "standby",
            "FAKE_NB_CFG": "43",
            "FAKE_NB_CONNECTION": "not connected",
        }
    )
    wait_stays_blocked(
        {
            "FAKE_ROLE": "standby",
            "FAKE_SB_NB_CFG": "[42]",
        }
    )

    role_counter.unlink(missing_ok=True)
    wait_stays_blocked(
        {
            "FAKE_ROLE_SEQUENCE": "standby,standby,active",
            "FAKE_NB_CFG": "43",
        }
    )

    write_config(config(standalone_hosts))
    wait_stays_blocked({"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"})
    write_config(config(raft_hosts))

    remove_marker()
    wait_stays_blocked({"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"})

    write_marker("0.2.15")
    wrong_version = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        wrong_version.returncode != 0
        and "does not match installed" in wrong_version.stderr,
        "wait accepted a restart marker for another installed version",
    )
    wrong_version_node_ready = run(
        "node-ready",
        {"FAKE_ROLE": "active", "FAKE_NB_CFG": "43"},
    )
    check(
        wrong_version_node_ready.returncode != 0
        and "does not match installed" in wrong_version_node_ready.stderr,
        "node readiness accepted a restart marker for another installed version",
    )

    write_marker()
    dpkg_failure = run(
        "wait",
        {
            "FAKE_ROLE": "standby",
            "FAKE_NB_CFG": "43",
            "FAKE_DPKG_FAILURE": "yes",
        },
    )
    check(
        dpkg_failure.returncode != 0
        and "cannot verify the installed" in dpkg_failure.stderr,
        "wait relaxed readiness without a valid installed package record",
    )

    write_marker()
    restart_marker.chmod(0o640)
    wrong_mode = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        wrong_mode.returncode != 0 and "mode 0600" in wrong_mode.stderr,
        "wait accepted an incorrectly protected restart marker",
    )

    write_marker()
    restart_marker.write_text("0.2.16\nsecond-line\n", encoding="ascii")
    multiline = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        multiline.returncode != 0 and "exactly one version line" in multiline.stderr,
        "wait accepted a multiline restart marker",
    )

    write_marker()
    restart_marker.write_bytes(b"1" * 257)
    oversized = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        oversized.returncode != 0 and "exceeds 256 bytes" in oversized.stderr,
        "wait accepted an oversized restart marker",
    )

    write_marker()
    os.chown(restart_marker, 1, 1)
    wrong_owner = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        wrong_owner.returncode != 0 and "root:root" in wrong_owner.stderr,
        "wait accepted a non-root restart marker",
    )

    restart_marker.unlink()
    restart_marker.mkdir(mode=0o700)
    nonregular = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        nonregular.returncode != 0 and "regular non-symlink" in nonregular.stderr,
        "wait accepted a non-regular restart marker",
    )
    restart_marker.rmdir()

    write_marker()
    hardlink = root / "marker-hardlink"
    os.link(restart_marker, hardlink)
    hardlinked = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        hardlinked.returncode != 0 and "exactly one hard link" in hardlinked.stderr,
        "wait accepted a hardlinked restart marker",
    )
    hardlink.unlink()

    target = root / "marker-target"
    target.write_text("0.2.16\n", encoding="ascii")
    target.chmod(0o600)
    restart_marker.unlink()
    restart_marker.symlink_to(target)
    symlinked = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        symlinked.returncode != 0 and "non-symlink" in symlinked.stderr,
        "wait accepted a symlinked restart marker",
    )
    restart_marker.unlink()

    write_marker()
    restart_directory.chmod(0o750)
    unsafe_directory = run(
        "wait",
        {"FAKE_ROLE": "standby", "FAKE_NB_CFG": "43"},
    )
    check(
        unsafe_directory.returncode != 0 and "mode 0700" in unsafe_directory.stderr,
        "wait accepted an unsafe restart state directory",
    )
    restart_directory.chmod(0o700)
    remove_marker()

for required in (
    'hasattr(os, "O_NOFOLLOW")',
    "os.fstat(marker_descriptor)",
    "marker_after = os.stat(",
    "path_after = RESTART_MARKER.lstat()",
    "marker_identity",
):
    check(required in HELPER_SOURCE, f"restart marker TOCTOU gate omits {required}")

print("pvn-ovn-northd tests passed")
