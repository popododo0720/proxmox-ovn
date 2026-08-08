#!/usr/bin/python3
"""Black-box tests for all-or-preflighted PVE daemon hook refresh."""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = REPO / "deploy" / "scripts" / "pvn-pve-refresh"


def executable(path: pathlib.Path, body: str) -> None:
    path.write_text(body, encoding="utf-8")
    path.chmod(0o755)


def run(
    *,
    lrm_state: str = "active",
    quorate: bool = True,
    inactive_unit: str = "",
    stale_unit: str = "",
    partial_unit: str = "",
    fail_unit: str = "",
    check_only: bool = False,
):
    with tempfile.TemporaryDirectory(prefix="pvn-pve-refresh-") as temporary:
        root = pathlib.Path(temporary)
        commands = root / "bin"
        commands.mkdir()
        initial_state = {
            "pvedaemon.service": {"pid": 101, "generation": 1001},
            "pveproxy.service": {"pid": 201, "generation": 2001},
            "pve-ha-lrm.service": {"pid": 301, "generation": 3001},
        }
        state = root / "state"
        state.mkdir()
        for unit, values in initial_state.items():
            for field, value in values.items():
                (state / f"{unit}.{field}").write_text(f"{value}\n", encoding="ascii")
        proc_root = root / "proc"

        def write_process(pid: int, ppid: int, starttime: int) -> None:
            process = proc_root / str(pid)
            process.mkdir(parents=True, exist_ok=True)
            fields = [
                str(pid),
                "(worker)",
                "S",
                str(ppid),
                *(["0"] * 17),
                str(starttime),
            ]
            (process / "stat").write_text(" ".join(fields) + "\n", encoding="ascii")

        worker_sets = {
            "pvedaemon.service": ([1101, 1102, 1103], [1201, 1202, 1203]),
            "pveproxy.service": ([2101, 2102, 2103], [2201, 2202, 2203]),
        }
        for unit, (old_workers, new_workers) in worker_sets.items():
            master_pid = initial_state[unit]["pid"]
            task = proc_root / str(master_pid) / "task" / str(master_pid)
            task.mkdir(parents=True)
            (task / "children").write_text(
                " ".join(str(pid) for pid in old_workers) + "\n", encoding="ascii"
            )
            for index, pid in enumerate((*old_workers, *new_workers), start=1):
                write_process(pid, master_pid, 10_000 + index)
            (state / f"{unit}.new_workers").write_text(
                " ".join(str(pid) for pid in new_workers) + "\n", encoding="ascii"
            )
        events = root / "events"
        executable(
            commands / "systemctl",
            r'''#!/bin/sh
set -eu

{
    printf '['
    separator=
    for argument in "$@"; do
        case "$argument" in
            *[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:=+,-]*)
                echo "unsafe fake-systemctl argument: $argument" >&2
                exit 70
                ;;
        esac
        printf '%s"%s"' "$separator" "$argument"
        separator=,
    done
    printf ']\n'
} >>"$EVENTS"

[ "$#" -gt 0 ] || exit 2
action=$1
unit=
property=
for argument in "$@"; do
    unit=$argument
    case "$argument" in
        --property=*) property=${argument#--property=} ;;
    esac
done

read_state() {
    field=$1
    IFS= read -r value <"$STATE/$unit.$field"
    printf '%s\n' "$value"
}

case "$action" in
    show)
        case "$property" in
            ActiveState)
                if [ "$unit" = "${INACTIVE_UNIT:-}" ]; then
                    printf 'inactive\n'
                elif [ "$unit" = pve-ha-lrm.service ]; then
                    printf '%s\n' "$LRM_STATE"
                else
                    printf 'active\n'
                fi
                ;;
            MainPID) read_state pid ;;
            ExecMainStartTimestampMonotonic) read_state generation ;;
            *) exit 2 ;;
        esac
        ;;
    reload|restart)
        [ "$unit" != "${FAIL_UNIT:-}" ] || exit 1
        if [ "$unit" != "${STALE_UNIT:-}" ]; then
            pid=$(read_state pid)
            generation=$(read_state generation)
            if [ "$action" = restart ]; then
                printf '%s\n' "$((pid + 1))" >"$STATE/$unit.pid"
                printf '%s\n' "$((generation + 1))" >"$STATE/$unit.generation"
            else
                old_workers=$(cat "$PROC_ROOT/$pid/task/$pid/children")
                new_workers=$(cat "$STATE/$unit.new_workers")
                if [ "$unit" = "${PARTIAL_UNIT:-}" ]; then
                    set -- $new_workers
                    new_workers="$1 $2"
                fi
                printf '%s %s\n' "$old_workers" "$new_workers" \
                    >"$PROC_ROOT/$pid/task/$pid/children"
            fi
        fi
        ;;
    *) exit 2 ;;
esac
''',
        )
        executable(
            commands / "pvecm",
            "#!/bin/sh\n[ \"$1\" = status ] || exit 2\nprintf 'Quorate:          %s\\n' \"$QUORATE\"\n",
        )
        executable(commands / "sleep", "#!/bin/sh\nexit 0\n")
        environment = dict(os.environ)
        environment.update(
            {
                "PATH": f"{commands}:/usr/bin:/bin",
                "STATE": str(state),
                "EVENTS": str(events),
                "LRM_STATE": lrm_state,
                "QUORATE": "Yes" if quorate else "No",
                "INACTIVE_UNIT": inactive_unit,
                "STALE_UNIT": stale_unit,
                "PARTIAL_UNIT": partial_unit,
                "FAIL_UNIT": fail_unit,
                "PVN_PVE_REFRESH_PROC_ROOT": str(proc_root),
                "PROC_ROOT": str(proc_root),
            }
        )
        command = [str(SCRIPT)]
        if check_only:
            command.append("--check")
        result = subprocess.run(command, env=environment, text=True, capture_output=True)
        calls = [json.loads(line) for line in events.read_text().splitlines()]
        final_state = {
            unit: {
                field: int((state / f"{unit}.{field}").read_text(encoding="ascii"))
                for field in values
            }
            for unit, values in initial_state.items()
        }
        return result, calls, final_state


def mutations(calls):
    return [call for call in calls if call[0] in {"reload", "restart"}]


checked, checked_calls, _ = run(check_only=True)
assert checked.returncode == 0, checked.stderr
assert mutations(checked_calls) == []

inactive_lrm, inactive_lrm_calls, inactive_lrm_state = run(lrm_state="inactive")
assert inactive_lrm.returncode == 0, inactive_lrm.stderr
assert mutations(inactive_lrm_calls) == [
    ["reload", "pvedaemon.service"],
    ["reload", "pveproxy.service"],
]
assert inactive_lrm_state["pvedaemon.service"] == {"pid": 101, "generation": 1001}
assert inactive_lrm_state["pveproxy.service"] == {"pid": 201, "generation": 2001}

healthy, healthy_calls, healthy_state = run()
assert healthy.returncode == 0, healthy.stderr
assert mutations(healthy_calls) == [
    ["restart", "pve-ha-lrm.service"],
    ["reload", "pvedaemon.service"],
    ["reload", "pveproxy.service"],
]
assert healthy_state["pve-ha-lrm.service"]["generation"] == 3002
assert healthy_state["pvedaemon.service"] == {"pid": 101, "generation": 1001}
assert healthy_state["pveproxy.service"] == {"pid": 201, "generation": 2001}

for label, options, expected in (
    ("no quorum", {"quorate": False}, "without quorum"),
    ("failed LRM", {"lrm_state": "failed"}, "safely restartable"),
    ("transitional LRM", {"lrm_state": "activating"}, "safely restartable"),
    ("inactive pvedaemon", {"inactive_unit": "pvedaemon.service"}, "pvedaemon is not active"),
    ("inactive pveproxy", {"inactive_unit": "pveproxy.service"}, "pveproxy is not active"),
):
    result, calls, _ = run(**options)
    assert result.returncode != 0 and expected in result.stderr, (label, result.stderr)
    assert mutations(calls) == [], f"{label} changed a daemon generation"

for unit, expected_prior_mutations in (
    (
        "pvedaemon.service",
        [["restart", "pve-ha-lrm.service"], ["reload", "pvedaemon.service"]],
    ),
    (
        "pveproxy.service",
        [
            ["restart", "pve-ha-lrm.service"],
            ["reload", "pvedaemon.service"],
            ["reload", "pveproxy.service"],
        ],
    ),
    (
        "pve-ha-lrm.service",
        [["restart", "pve-ha-lrm.service"]],
    ),
):
    stale, stale_calls, _ = run(stale_unit=unit)
    expected_error = (
        "new active process generation"
        if unit == "pve-ha-lrm.service"
        else "complete new active worker generation"
    )
    assert stale.returncode != 0 and expected_error in stale.stderr
    assert mutations(stale_calls) == expected_prior_mutations

partial, partial_calls, _ = run(partial_unit="pvedaemon.service")
assert partial.returncode != 0
assert "complete new active worker generation" in partial.stderr
assert mutations(partial_calls) == [
    ["restart", "pve-ha-lrm.service"],
    ["reload", "pvedaemon.service"],
]

reload_failure, reload_failure_calls, _ = run(fail_unit="pvedaemon.service")
assert reload_failure.returncode != 0
assert mutations(reload_failure_calls) == [
    ["restart", "pve-ha-lrm.service"],
    ["reload", "pvedaemon.service"],
]

print("pvn PVE daemon refresh tests passed")
