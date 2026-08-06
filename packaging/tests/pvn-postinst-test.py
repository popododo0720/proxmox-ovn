#!/usr/bin/python3
"""Black-box lifecycle tests for the pvn-node maintainer postinst."""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
SOURCE = (REPO / "packaging/debian/pvn-node.postinst").read_text(
    encoding="utf-8"
)


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def write_executable(path: pathlib.Path, body: str) -> None:
    path.write_text(body, encoding="utf-8")
    path.chmod(0o755)


def run_scenario(
    *,
    node_active: bool,
    central_active: bool,
    central_state: str | None = None,
    old_version: str = "0.2.16",
    node_enabled: bool = True,
    node_ready_failed: bool = False,
    node_state: str = "inactive",
    unsafe_node_marker: bool = False,
    component_failure: bool = False,
    pid_drift: bool = False,
    final_failure: bool = False,
    condition_skip: bool = False,
    late_pid_drift: bool = False,
    late_marker_unsafe: bool = False,
    late_pid_query_failure: bool = False,
    consume_failure: bool = False,
    pid_base: int = 0,
    work_root: pathlib.Path | None = None,
) -> tuple[subprocess.CompletedProcess[str], list[dict[str, object]], bool, bool]:
    temporary = tempfile.TemporaryDirectory() if work_root is None else None
    root = pathlib.Path(temporary.name) if temporary is not None else work_root
    assert root is not None
    commands = root / "commands"
    commands.mkdir(exist_ok=True)
    etc_pvn = root / "etc-pvn"
    etc_pvn.mkdir(exist_ok=True)
    (etc_pvn / "ovn-host.env").write_text("test\n", encoding="ascii")
    node_marker = etc_pvn / "node-enabled"
    if node_enabled:
        node_marker.write_text("unsafe\n" if unsafe_node_marker else "", encoding="ascii")
        node_marker.chmod(0o644)
    else:
        node_marker.unlink(missing_ok=True)
    if central_active:
        central_marker = etc_pvn / "central/enabled"
        central_marker.parent.mkdir(exist_ok=True)
        central_marker.write_bytes(b"")
        central_marker.chmod(0o644)
    example = root / "ovn-host.env.example"
    example.write_text("test\n", encoding="ascii")
    event_log = root / "events.jsonl"
    event_log.unlink(missing_ok=True)
    restarted = root / "components-restarted"
    restarted.unlink(missing_ok=True)
    final_started = root / "final-started"
    final_started.unlink(missing_ok=True)
    active_state = root / "node-active"
    ready_state = root / "node-ready"
    if node_active:
        active_state.touch()
        ready_state.touch()
    else:
        active_state.unlink(missing_ok=True)
        ready_state.unlink(missing_ok=True)

    replacements = [
        ("/var/lib/pvn-node", str(root / "var-lib-pvn-node")),
        ("/run/pvn-node", str(root / "run-pvn-node")),
        ("/var/lib/pvn", str(root / "var-lib-pvn")),
        ("/etc/pvn", str(etc_pvn)),
        ("/usr/share/doc/pvn-node/examples/ovn-host.env", str(example)),
        ("/usr/lib/pvn/pvn-ui-inject", str(commands / "pvn-ui-inject")),
        ("/usr/lib/pvn/pvn-ui-verify", str(commands / "pvn-ui-verify")),
        ("/usr/lib/pvn/pvn-ovn-northd", str(commands / "pvn-ovn-northd")),
        ("/usr/sbin/pvnctl", str(commands / "pvnctl")),
    ]
    transformed = SOURCE
    for old, new in replacements:
        transformed = transformed.replace(old, new)
    postinst = root / "pvn-node.postinst"
    postinst.write_text(transformed, encoding="utf-8")
    postinst.chmod(0o755)

    write_executable(commands / "getent", "#!/bin/sh\nexit 0\n")
    write_executable(commands / "addgroup", "#!/bin/sh\nexit 99\n")
    write_executable(commands / "adduser", "#!/bin/sh\nexit 99\n")
    for name in ("pvn-ui-inject", "pvn-ui-verify", "pvnctl", "pvn-ovn-northd"):
        write_executable(commands / name, "#!/bin/sh\nexit 0\n")
    write_executable(
        commands / "dpkg-query",
        """#!/usr/bin/python3
import os, sys
if "-f=${Version}" in sys.argv:
    print(os.environ["FAKE_VERSION"])
    raise SystemExit(0)
raise SystemExit(2)
""",
    )
    write_executable(
        commands / "install",
        """#!/usr/bin/python3
import os, pathlib, shutil, sys
arguments = sys.argv[1:]
directory = "-d" in arguments
mode = None
paths = []
index = 0
while index < len(arguments):
    value = arguments[index]
    if value in {"-m", "-o", "-g"}:
        if value == "-m":
            mode = int(arguments[index + 1], 8)
        index += 2
    elif value == "-d":
        index += 1
    else:
        paths.append(value)
        index += 1
if directory:
    for value in paths:
        pathlib.Path(value).mkdir(parents=True, exist_ok=True)
        if mode is not None:
            os.chmod(value, mode)
else:
    source, destination = map(pathlib.Path, paths)
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, destination)
    if mode is not None:
        destination.chmod(mode)
""",
    )
    write_executable(
        commands / "systemctl",
        """#!/usr/bin/python3
import fcntl, json, os, pathlib, stat, sys

arguments = sys.argv[1:]
action = arguments[0] if arguments else ""
event = {"action": action, "arguments": arguments[1:]}
with pathlib.Path(os.environ["FAKE_EVENTS"]).open("a", encoding="utf-8") as stream:
    stream.write(json.dumps(event) + "\\n")

def yes(name):
    return os.environ.get(name) == "yes"

def verify_transition():
    if not yes("FAKE_CENTRAL_ACTIVE"):
        return
    marker = pathlib.Path(os.environ["FAKE_RESTART_MARKER"])
    auth = pathlib.Path(os.environ["FAKE_PACKAGE_AUTH"])
    if marker.read_text(encoding="ascii") != os.environ["FAKE_VERSION"] + "\\n":
        raise SystemExit(81)
    metadata = auth.stat()
    if not stat.S_ISREG(metadata.st_mode) or stat.S_IMODE(metadata.st_mode) != 0o600:
        raise SystemExit(82)
    if auth.read_text(encoding="ascii") != os.environ["FAKE_VERSION"] + "\\n":
        raise SystemExit(83)
    descriptor = os.open(auth, os.O_RDONLY)
    try:
        if pathlib.Path("/proc/self/fd/9").exists():
            raise SystemExit(85)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        raise SystemExit(84)
    finally:
        os.close(descriptor)

if action == "is-active":
    unit = arguments[-1]
    if unit == "pvn-node.target":
        raise SystemExit(0 if pathlib.Path(os.environ["FAKE_ACTIVE_STATE"]).exists() else 3)
    if unit == "pvn-central.target":
        raise SystemExit(0 if yes("FAKE_CENTRAL_ACTIVE") else 3)
    if unit == "pvn-node-ready.service":
        raise SystemExit(0 if pathlib.Path(os.environ["FAKE_READY_STATE"]).exists() else 3)
    if unit in {"pvn-manager.service", "pvn-agent.service", "pvn-ovn-host-config.service", "ovn-controller.service"}:
        raise SystemExit(0 if pathlib.Path(os.environ["FAKE_RESTARTED"]).exists() else 3)
    raise SystemExit(3)
if action == "is-enabled":
    raise SystemExit(0 if yes("FAKE_NODE_ENABLED") else 1)
if action == "is-failed":
    raise SystemExit(0 if yes("FAKE_NODE_READY_FAILED") else 1)
if action == "show":
    unit = arguments[-1]
    if any("ActiveState" in argument for argument in arguments):
        print(os.environ["FAKE_CENTRAL_STATE"] if unit == "pvn-central.target" else os.environ["FAKE_NODE_STATE"])
        raise SystemExit(0)
    pids = {
        "pvn-control-db.service": 101,
        "ovn-ovsdb-server-nb.service": 102,
        "ovn-ovsdb-server-sb.service": 103,
        "ovn-northd.service": 104,
    }
    if yes("FAKE_LATE_PID_QUERY_FAILURE") and pathlib.Path(os.environ["FAKE_FINAL_STARTED"]).exists():
        raise SystemExit(1)
    value = pids[unit] + int(os.environ["FAKE_PID_BASE"])
    if yes("FAKE_PID_DRIFT") and pathlib.Path(os.environ["FAKE_RESTARTED"]).exists():
        value += 1000
    if yes("FAKE_LATE_PID_DRIFT") and pathlib.Path(os.environ["FAKE_FINAL_STARTED"]).exists():
        value += 2000
    print(value)
    raise SystemExit(0)
if action == "restart" and "pvn-manager.service" in arguments:
    verify_transition()
    pathlib.Path(os.environ["FAKE_RESTARTED"]).touch()
    raise SystemExit(1 if yes("FAKE_COMPONENT_FAILURE") else 0)
if action == "stop" and "pvn-node.target" in arguments:
    pathlib.Path(os.environ["FAKE_ACTIVE_STATE"]).unlink(missing_ok=True)
    pathlib.Path(os.environ["FAKE_READY_STATE"]).unlink(missing_ok=True)
    raise SystemExit(0)
if action == "start" and arguments[1:] == ["pvn-node.target"]:
    verify_transition()
    if yes("FAKE_FINAL_FAILURE"):
        pathlib.Path(os.environ["FAKE_ACTIVE_STATE"]).touch()
        pathlib.Path(os.environ["FAKE_READY_STATE"]).touch()
        raise SystemExit(1)
    if not yes("FAKE_CONDITION_SKIP"):
        pathlib.Path(os.environ["FAKE_ACTIVE_STATE"]).touch()
        pathlib.Path(os.environ["FAKE_READY_STATE"]).touch()
    pathlib.Path(os.environ["FAKE_FINAL_STARTED"]).touch()
    if yes("FAKE_LATE_MARKER_UNSAFE"):
        pathlib.Path(os.environ["FAKE_CENTRAL_ACTIVATION_MARKER"]).chmod(0o666)
    if yes("FAKE_CONSUME_FAILURE"):
        pathlib.Path(os.environ["FAKE_NODE_RESTART_INTENT"]).chmod(0o666)
    raise SystemExit(0)
raise SystemExit(0)
""",
    )

    environment = os.environ.copy()
    environment.update(
        {
            "PATH": str(commands) + ":/usr/sbin:/usr/bin:/sbin:/bin",
            "FAKE_VERSION": "0.2.17",
            "FAKE_NODE_ACTIVE": "yes" if node_active else "no",
            "FAKE_CENTRAL_ACTIVE": "yes" if central_active else "no",
            "FAKE_CENTRAL_STATE": central_state or ("active" if central_active else "inactive"),
            "FAKE_NODE_ENABLED": "yes" if node_enabled else "no",
            "FAKE_NODE_READY_FAILED": "yes" if node_ready_failed else "no",
            "FAKE_NODE_STATE": node_state,
            "FAKE_COMPONENT_FAILURE": "yes" if component_failure else "no",
            "FAKE_PID_DRIFT": "yes" if pid_drift else "no",
            "FAKE_PID_BASE": str(pid_base),
            "FAKE_FINAL_FAILURE": "yes" if final_failure else "no",
            "FAKE_CONDITION_SKIP": "yes" if condition_skip else "no",
            "FAKE_LATE_PID_DRIFT": "yes" if late_pid_drift else "no",
            "FAKE_LATE_MARKER_UNSAFE": "yes" if late_marker_unsafe else "no",
            "FAKE_LATE_PID_QUERY_FAILURE": "yes" if late_pid_query_failure else "no",
            "FAKE_CONSUME_FAILURE": "yes" if consume_failure else "no",
            "FAKE_EVENTS": str(event_log),
            "FAKE_RESTARTED": str(restarted),
            "FAKE_ACTIVE_STATE": str(active_state),
            "FAKE_READY_STATE": str(ready_state),
            "FAKE_FINAL_STARTED": str(final_started),
            "FAKE_RESTART_MARKER": str(
                root / "var-lib-pvn-node/central-restart-pending"
            ),
            "FAKE_PACKAGE_AUTH": str(root / "run-pvn-node/package-configuring"),
            "FAKE_CENTRAL_ACTIVATION_MARKER": str(etc_pvn / "central/enabled"),
            "FAKE_NODE_RESTART_INTENT": str(
                root / "var-lib-pvn-node/node-restart-pending"
            ),
        }
    )
    arguments = [str(postinst), "configure"]
    if old_version:
        arguments.append(old_version)
    result = subprocess.run(
        arguments,
        env=environment,
        text=True,
        capture_output=True,
        check=False,
        timeout=10,
    )
    events = [
        json.loads(line)
        for line in event_log.read_text(encoding="utf-8").splitlines()
    ]
    auth_exists = (root / "run-pvn-node/package-configuring").exists()
    intent_exists = (root / "var-lib-pvn-node/node-restart-pending").exists()
    if temporary is not None:
        temporary.cleanup()
    return result, events, auth_exists, intent_exists


def positions(events: list[dict[str, object]]) -> tuple[int, int, int, int, int]:
    stop = next(
        index
        for index, event in enumerate(events)
        if event["action"] == "stop" and "pvn-node.target" in event["arguments"]
    )
    restart = next(
        index
        for index, event in enumerate(events)
        if event["action"] == "restart" and "pvn-manager.service" in event["arguments"]
    )
    pid_positions = [
        index
        for index, event in enumerate(events)
        if event["action"] == "show"
        and any("MainPID" in argument for argument in event["arguments"])
    ]
    final = next(
        index
        for index, event in enumerate(events)
        if event["action"] == "start" and event["arguments"] == ["pvn-node.target"]
    )
    post_pid = max(index for index in pid_positions if index < final)
    final_pid = min(index for index in pid_positions if index > final)
    return stop, restart, post_pid, final, final_pid


def stopped_after_final_start(events: list[dict[str, object]]) -> bool:
    final = next(
        index
        for index, event in enumerate(events)
        if event["action"] == "start" and event["arguments"] == ["pvn-node.target"]
    )
    return any(
        event["action"] == "stop" and "pvn-node.target" in event["arguments"]
        for event in events[final + 1 :]
    )


active, active_events, active_auth, active_intent = run_scenario(
    node_active=True, central_active=True
)
check(active.returncode == 0, f"active upgrade failed: {active.stderr}")
check(positions(active_events) == tuple(sorted(positions(active_events))), "active upgrade order is unsafe")
check(
    not active_auth,
    "postinst authorization survived successful package configuration",
)
check(not active_intent, "successful upgrade retained node restart intent")

legacy, legacy_events, legacy_auth, legacy_intent = run_scenario(
    node_active=False,
    central_active=True,
    old_version="0.2.15",
    node_ready_failed=True,
)
check(legacy.returncode == 0, f"legacy inactive-node configure failed: {legacy.stderr}")
check(not any(event["action"] == "start" for event in legacy_events), "legacy no-intent state auto-started")
check(not legacy_auth and not legacy_intent, "legacy inert configure created transient node state")

transport, transport_events, _, _ = run_scenario(
    node_active=False,
    central_active=False,
    node_ready_failed=True,
)
check(transport.returncode == 0, f"clean transport stop failed: {transport.stderr}")
check(not any(event["action"] == "start" for event in transport_events), "transport-only stop was activated")

for label, options in (
    ("clean stop", {}),
    ("disabled target", {"node_enabled": False, "node_ready_failed": True}),
    ("unsafe marker", {"unsafe_node_marker": True, "node_ready_failed": True}),
    ("first install", {"old_version": "", "node_ready_failed": True}),
):
    result, events, _, _ = run_scenario(
        node_active=False, central_active=False, **options
    )
    check(result.returncode == 0, f"{label} inert path failed: {result.stderr}")
    check(
        not any(event["action"] in {"restart", "start"} for event in events),
        f"{label} unexpectedly activated the node stack",
    )

transitional, transitional_events, _, _ = run_scenario(
    node_active=False,
    central_active=False,
    node_state="deactivating",
)
check(
    transitional.returncode != 0 and "safely settled" in transitional.stderr,
    "transitional node target state was accepted",
)
check(
    not any(event["action"] in {"stop", "restart", "start"} for event in transitional_events),
    "transitional node target state was mutated",
)

active_without_central, active_without_central_events, _, _ = run_scenario(
    node_active=True,
    central_active=False,
)
check(
    active_without_central.returncode != 0
    and "requires an active central" in active_without_central.stderr,
    "active node was accepted without its required central target",
)
check(
    not any(event["action"] in {"stop", "restart", "start"} for event in active_without_central_events),
    "active node without central mutated services",
)

for unsafe_central_state in ("failed", "activating", "deactivating"):
    unsafe_central, unsafe_central_events, _, _ = run_scenario(
        node_active=True,
        central_active=False,
        central_state=unsafe_central_state,
    )
    check(
        unsafe_central.returncode != 0 and "safely inactive" in unsafe_central.stderr,
        f"central state {unsafe_central_state} was accepted",
    )
    check(
        not any(event["action"] in {"stop", "restart", "start"} for event in unsafe_central_events),
        f"central state {unsafe_central_state} mutated node services",
    )

for label, options, expected in (
    ("component failure", {"component_failure": True}, "stack failed"),
    ("central PID drift", {"pid_drift": True}, "process identity changed"),
    ("final readiness failure", {"final_failure": True}, "target/readiness failed"),
):
    result, events, auth_exists, intent_exists = run_scenario(
        node_active=True, central_active=True, **options
    )
    check(result.returncode != 0 and expected in result.stderr, f"{label} was not rejected")
    final_calls = [event for event in events if event["action"] == "start"]
    if label != "final readiness failure":
        check(not final_calls, f"{label} invoked final readiness")
    check(
        not auth_exists,
        f"{label} leaked postinst authorization",
    )
    check(intent_exists, f"{label} lost durable node restart intent")
    if label == "final readiness failure":
        check(stopped_after_final_start(events), "failed final start left readiness active")

condition_skip_result, condition_skip_events, condition_skip_auth, condition_skip_intent = run_scenario(
    node_active=True, central_active=True, condition_skip=True
)
check(
    condition_skip_result.returncode != 0
    and "is not active after package upgrade" in condition_skip_result.stderr,
    "condition-skip success was accepted as a ready node target",
)
check(not condition_skip_auth, "condition-skip failure leaked postinst authorization")
check(condition_skip_intent, "condition-skip failure lost durable restart intent")
check(stopped_after_final_start(condition_skip_events), "condition-skip failure left readiness active")

late_drift, late_drift_events, late_drift_auth, late_drift_intent = run_scenario(
    node_active=True, central_active=True, late_pid_drift=True
)
check(
    late_drift.returncode != 0
    and "final node readiness" in late_drift.stderr,
    "central PID drift during final readiness was accepted",
)
check(not late_drift_auth and late_drift_intent, "late PID drift lost recovery evidence")
check(stopped_after_final_start(late_drift_events), "late PID drift left the node target ready")

for label, options, expected in (
    ("late marker damage", {"late_marker_unsafe": True}, "activation marker is unsafe"),
    ("late PID query failure", {"late_pid_query_failure": True}, "lost final process identities"),
    ("restart intent consume failure", {"consume_failure": True}, "cannot consume"),
):
    result, events, auth_exists, intent_exists = run_scenario(
        node_active=True, central_active=True, **options
    )
    check(result.returncode != 0 and expected in result.stderr, f"{label} was accepted")
    check(not auth_exists and intent_exists, f"{label} lost transition evidence")
    check(stopped_after_final_start(events), f"{label} left node readiness active")

with tempfile.TemporaryDirectory() as persistent_temporary:
    persistent_root = pathlib.Path(persistent_temporary)
    first, first_events, first_auth, first_intent = run_scenario(
        node_active=True,
        central_active=True,
        final_failure=True,
        work_root=persistent_root,
    )
    check(first.returncode != 0 and first_intent and not first_auth, "interrupted upgrade did not retain only durable intent")
    resumed, resumed_events, resumed_auth, resumed_intent = run_scenario(
        node_active=False,
        central_active=True,
        node_state="failed",
        work_root=persistent_root,
    )
    check(resumed.returncode == 0, f"durable-intent retry failed: {resumed.stderr}")
    check(any(event["action"] == "start" for event in resumed_events), "durable-intent retry did not start target")
    check(not resumed_auth and not resumed_intent, "successful retry did not consume transient/durable intent")

with tempfile.TemporaryDirectory() as drift_temporary:
    drift_root = pathlib.Path(drift_temporary)
    drift, _, drift_auth, drift_intent = run_scenario(
        node_active=True,
        central_active=True,
        pid_drift=True,
        work_root=drift_root,
    )
    check(drift.returncode != 0 and drift_intent and not drift_auth, "central PID drift did not retain durable evidence")
    drift_retry, drift_retry_events, _, drift_retry_intent = run_scenario(
        node_active=False,
        central_active=True,
        node_state="inactive",
        pid_base=1000,
        work_root=drift_root,
    )
    check(
        drift_retry.returncode != 0
        and "central process identity changed" in drift_retry.stderr,
        "retry adopted drifted central process identities",
    )
    check(not any(event["action"] in {"stop", "restart", "start"} for event in drift_retry_events), "PID-drift retry mutated node services")
    check(drift_retry_intent, "PID-drift retry erased original evidence")

with tempfile.TemporaryDirectory() as cross_version_temporary:
    cross_root = pathlib.Path(cross_version_temporary)
    restart_state = cross_root / "var-lib-pvn-node"
    restart_state.mkdir(mode=0o700)
    intent = restart_state / "node-restart-pending"
    intent.write_text("0.2.16|active|101,102,103,104\n", encoding="ascii")
    intent.chmod(0o600)
    central_pending = restart_state / "central-restart-pending"
    central_pending.write_text("0.2.16\n", encoding="ascii")
    central_pending.chmod(0o600)
    cross, cross_events, _, cross_intent = run_scenario(
        node_active=False,
        central_active=True,
        work_root=cross_root,
    )
    check(cross.returncode != 0 and cross_intent, "cross-version intent was accepted")
    check(central_pending.read_text(encoding="ascii") == "0.2.16\n", "cross-version failure overwrote central marker")
    check(not any(event["action"] in {"stop", "restart", "start"} for event in cross_events), "cross-version failure mutated node services")

for unsafe_kind in ("symlink", "hardlink"):
    with tempfile.TemporaryDirectory() as unsafe_temporary:
        unsafe_root = pathlib.Path(unsafe_temporary)
        restart_state = unsafe_root / "var-lib-pvn-node"
        restart_state.mkdir(mode=0o700)
        target = restart_state / "intent-target"
        target.write_text("0.2.17|active|101,102,103,104\n", encoding="ascii")
        target.chmod(0o600)
        intent = restart_state / "node-restart-pending"
        if unsafe_kind == "symlink":
            intent.symlink_to(target)
        else:
            os.link(target, intent)
        unsafe, unsafe_events, _, _ = run_scenario(
            node_active=False,
            central_active=True,
            work_root=unsafe_root,
        )
        check(unsafe.returncode != 0, f"{unsafe_kind} restart intent was accepted")
        check(not any(event["action"] in {"stop", "restart", "start"} for event in unsafe_events), f"{unsafe_kind} intent failure mutated node services")

print("pvn postinst tests passed")
