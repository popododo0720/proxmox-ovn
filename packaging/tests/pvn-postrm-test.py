#!/usr/bin/python3
"""Black-box cleanup tests for failed and completed pvn-node removal."""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
SOURCE = (REPO / "packaging/debian/pvn-node.postrm").read_text(encoding="utf-8")


def executable(path: pathlib.Path, body: str) -> None:
    path.write_text(body, encoding="utf-8")
    path.chmod(0o755)


def run(action: str, *, helpers: bool = True, fail: str = ""):
    with tempfile.TemporaryDirectory(prefix="pvn-postrm-") as temporary:
        root = pathlib.Path(temporary)
        commands = root / "bin"
        commands.mkdir()
        events = root / "events"
        replacements = {}
        for name in (
            "pvn-compute-inject",
            "pvn-api-inject",
            "pvn-ui-inject",
            "pvn-pve-refresh",
        ):
            target = commands / name
            replacements[f"/usr/lib/pvn/{name}"] = str(target)
            if helpers:
                executable(
                    target,
                    """#!/usr/bin/python3
import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
with pathlib.Path(os.environ['EVENTS']).open('a', encoding='utf-8') as stream:
    stream.write(json.dumps([name, *sys.argv[1:]]) + '\\n')
raise SystemExit(71 if os.environ.get('FAIL') == name else 0)
""",
                )
        transformed = SOURCE
        for source, target in replacements.items():
            transformed = transformed.replace(source, target)
        postrm = root / "postrm"
        executable(postrm, transformed)
        executable(
            commands / "systemctl",
            """#!/usr/bin/python3
import json, os, pathlib, sys
with pathlib.Path(os.environ['EVENTS']).open('a', encoding='utf-8') as stream:
    stream.write(json.dumps(['systemctl', *sys.argv[1:]]) + '\\n')
""",
        )
        environment = dict(os.environ)
        environment.update(
            {
                "EVENTS": str(events),
                "FAIL": fail,
                "PATH": f"{commands}:/usr/bin:/bin",
            }
        )
        result = subprocess.run(
            [str(postrm), action], env=environment, text=True, capture_output=True
        )
        recorded = []
        if events.exists():
            recorded = [json.loads(line) for line in events.read_text().splitlines()]
        return result, recorded


success, success_events = run("abort-install")
assert success.returncode == 0, success.stderr
assert success_events[:4] == [
    ["pvn-compute-inject", "remove"],
    ["pvn-api-inject", "remove"],
    ["pvn-ui-inject", "remove"],
    ["pvn-pve-refresh"],
], success_events
assert success_events[4:] == [
    ["systemctl", "unmask", "ovn-host.service", "ovn-central.service"],
    ["systemctl", "daemon-reload"],
], success_events

failed_recovery, failed_recovery_events = run(
    "failed-upgrade", fail="pvn-compute-inject"
)
assert failed_recovery.returncode != 0
assert failed_recovery_events == [["pvn-compute-inject", "remove"]]

failed_refresh, failed_refresh_events = run("abort-upgrade", fail="pvn-pve-refresh")
assert failed_refresh.returncode != 0
assert failed_refresh_events[:4] == success_events[:4]
assert not any(event[0] == "systemctl" for event in failed_refresh_events)

missing, missing_events = run("purge", helpers=False)
assert missing.returncode == 0, missing.stderr
assert missing_events == [
    ["systemctl", "unmask", "ovn-host.service", "ovn-central.service"],
    ["systemctl", "daemon-reload"],
]

ignored, ignored_events = run("upgrade")
assert ignored.returncode == 0 and ignored_events == []

print("pvn postrm cleanup tests passed")
