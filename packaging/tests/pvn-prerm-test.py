#!/usr/bin/python3
"""Black-box tests for guarded PVE hook removal ordering."""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
SOURCE = (REPO / "packaging/debian/pvn-node.prerm").read_text(encoding="utf-8")


def executable(path: pathlib.Path, body: str) -> None:
    path.write_text(body, encoding="utf-8")
    path.chmod(0o755)


def run(action: str, *, preflight_failure: bool = False):
    with tempfile.TemporaryDirectory(prefix="pvn-prerm-") as temporary:
        root = pathlib.Path(temporary)
        commands = root / "bin"
        commands.mkdir()
        events = root / "events"
        transformed = SOURCE.replace(
            "/etc/pvn/node-enabled", str(root / "node-enabled")
        ).replace("/etc/pvn/central/enabled", str(root / "central-enabled"))
        for name in (
            "pvn-compute-inject",
            "pvn-api-inject",
            "pvn-ui-inject",
            "pvn-pve-refresh",
        ):
            target = commands / name
            transformed = transformed.replace(f"/usr/lib/pvn/{name}", str(target))
            executable(
                target,
                r'''#!/usr/bin/python3
import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
with pathlib.Path(os.environ['EVENTS']).open('a', encoding='utf-8') as stream:
    stream.write(json.dumps([name, *sys.argv[1:]]) + '\n')
if name == 'pvn-pve-refresh' and sys.argv[1:] == ['--check'] and os.environ['PREFLIGHT_FAILURE'] == 'yes':
    raise SystemExit(73)
''',
            )
        prerm = root / "prerm"
        executable(prerm, transformed)
        executable(
            commands / "systemctl",
            r'''#!/usr/bin/python3
import json, os, pathlib, sys
args = sys.argv[1:]
with pathlib.Path(os.environ['EVENTS']).open('a', encoding='utf-8') as stream:
    stream.write(json.dumps(['systemctl', *args]) + '\n')
if args[0] == 'show': print('257'); raise SystemExit(0)
if args[0] in {'is-enabled', 'is-active'}: raise SystemExit(1 if args[0] == 'is-enabled' else 3)
raise SystemExit(0)
''',
        )
        executable(commands / "ovs-vsctl", "#!/bin/sh\nexit 0\n")
        environment = dict(os.environ)
        environment.update(
            {
                "EVENTS": str(events),
                "PREFLIGHT_FAILURE": "yes" if preflight_failure else "no",
                "PATH": f"{commands}:/usr/bin:/bin",
            }
        )
        result = subprocess.run(
            [str(prerm), action], env=environment, text=True, capture_output=True
        )
        recorded = []
        if events.exists():
            recorded = [json.loads(line) for line in events.read_text().splitlines()]
        return result, recorded


removed, removed_events = run("remove")
assert removed.returncode == 0, removed.stderr
helpers = [event for event in removed_events if event[0] != "systemctl"]
assert helpers == [
    ["pvn-pve-refresh", "--check"],
    ["pvn-compute-inject", "remove"],
    ["pvn-api-inject", "remove"],
    ["pvn-ui-inject", "remove"],
    ["pvn-pve-refresh"],
], helpers

blocked, blocked_events = run("remove", preflight_failure=True)
assert blocked.returncode != 0
blocked_helpers = [event for event in blocked_events if event[0] != "systemctl"]
assert blocked_helpers == [["pvn-pve-refresh", "--check"]]

ignored, ignored_events = run("upgrade")
assert ignored.returncode == 0 and ignored_events == []

print("pvn prerm hook-removal tests passed")
