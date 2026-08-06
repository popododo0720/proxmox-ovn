#!/usr/bin/python3
"""Black-box tests for the packaged ovn-northd launcher/readiness gate."""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
HELPER = REPO / "deploy/scripts/pvn-ovn-northd"


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
        """#!/bin/sh
[ "$1 $2" = "-t ovn-northd" ] || exit 2
case "$3" in
  nb-connection-status) printf '%s\n' "${FAKE_NB_CONNECTION:-connected}" ;;
  sb-connection-status) printf '%s\n' "${FAKE_SB_CONNECTION:-connected}" ;;
  status) printf 'Status: %s\n' "${FAKE_ROLE:-active}" ;;
  *) exit 2 ;;
esac
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

    environment = os.environ.copy()
    environment.update(
        {
            "PVN_NORTHD_CONFIG": str(config_path),
            "PVN_NORTHD_OVN_CTL": str(ovn_ctl),
            "PVN_NORTHD_OVN_APPCTL": str(appctl),
            "PVN_NORTHD_OVN_NBCTL": str(nbctl),
            "PVN_NORTHD_OVN_SBCTL": str(sbctl),
            "PVN_NORTHD_SYSTEMCTL": str(systemctl),
            "PVN_NORTHD_DB_PARAMS": str(db_params),
            "FAKE_START_CALLS": str(calls),
            "FAKE_QUERY_CALLS": str(query_calls),
            "OVN_NB_DB": "unix:/poisoned-nb.sock",
            "OVN_NBCTL_OPTIONS": "--db=unix:/poisoned-nb.sock",
            "OVN_NB_DAEMON": "poisoned",
            "OVN_SB_DB": "unix:/poisoned-sb.sock",
            "OVN_SBCTL_OPTIONS": "--db=unix:/poisoned-sb.sock",
            "OVN_SB_DAEMON": "poisoned",
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

    malformed_cfg = run("status", {"FAKE_SB_NB_CFG": "[42]"})
    check(malformed_cfg.returncode != 0 and "canonical non-negative integer" in malformed_cfg.stderr, "malformed cfg value was accepted")

    inactive = run("status", {"FAKE_SYSTEMD_ACTIVE": "no"})
    check(inactive.returncode != 0 and "systemctl" in inactive.stderr, "inactive systemd northd was accepted")

    waiting = run("wait", {"FAKE_ROLE": "standby"})
    check(waiting.returncode == 0 and "role=standby" in waiting.stdout, "readiness wait rejected an immediately healthy standby")

print("pvn-ovn-northd tests passed")
