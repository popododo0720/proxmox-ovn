#!/usr/bin/python3
"""Focused transition and activation-marker tests for pvn-node-ready."""

from __future__ import annotations

import os
import pathlib
import subprocess
import tempfile


REPO = pathlib.Path(__file__).resolve().parents[2]
SOURCE = (REPO / "deploy/scripts/pvn-node-ready").read_text(encoding="utf-8")


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def executable(path: pathlib.Path, body: str) -> None:
    path.write_text("#!/bin/sh\n" + body, encoding="utf-8")
    path.chmod(0o755)


with tempfile.TemporaryDirectory() as temporary:
    root = pathlib.Path(temporary)
    commands = root / "commands"
    commands.mkdir()
    marker = root / "central-enabled"
    node_marker = root / "node-enabled"
    calls = root / "northd-calls"
    manager_socket = root / "manager.sock"

    transformed = SOURCE.replace("max_attempts=30", "max_attempts=1")
    transformed = transformed.replace(
        "[ ! -S /run/pvn/manager.sock ]", "false"
    ).replace("[ -S /run/pvn/manager.sock ]", "true")
    transformed = transformed.replace(
        "/etc/pvn/central/enabled", str(marker)
    ).replace("/etc/pvn/node-enabled", str(node_marker)).replace(
        "/run/pvn/manager.sock", str(manager_socket)
    )
    for original, replacement in {
        "/usr/bin/systemctl": commands / "systemctl",
        "/usr/bin/curl": commands / "curl",
        "/usr/bin/ovn-appctl": commands / "ovn-appctl",
        "/usr/sbin/pvnctl": commands / "pvnctl",
        "/usr/lib/pvn/pvn-ui-verify": commands / "ui-verify",
        "/usr/lib/pvn/pvn-ovn-northd": commands / "pvn-ovn-northd",
        "/usr/bin/sleep": commands / "sleep",
    }.items():
        transformed = transformed.replace(original, str(replacement))
    script = root / "pvn-node-ready"
    script.write_text(transformed, encoding="utf-8")
    script.chmod(0o755)

    for name in ("systemctl", "curl", "pvnctl", "ui-verify", "sleep"):
        executable(commands / name, "exit 0\n")
    executable(commands / "ovn-appctl", "printf 'connected\\n'\n")
    executable(
        commands / "pvn-ovn-northd",
        f'[ "$1" = node-ready ] || exit 2\nprintf "%s\\n" "$1" >> "{calls}"\n',
    )

    def run() -> subprocess.CompletedProcess[str]:
        calls.unlink(missing_ok=True)
        return subprocess.run(
            [str(script)], text=True, capture_output=True, check=False, timeout=5
        )

    node_marker.write_bytes(b"")
    node_marker.chmod(0o644)
    absent = run()
    check(absent.returncode == 0, f"non-central readiness failed: {absent.stderr}")
    check(not calls.exists(), "non-central readiness invoked the northd gate")

    node_marker.write_text("unsafe\n", encoding="ascii")
    unsafe_node = run()
    check(unsafe_node.returncode != 0, "non-empty node marker was accepted")
    node_marker.write_bytes(b"")
    node_marker.chmod(0o644)

    marker.write_bytes(b"")
    marker.chmod(0o644)
    healthy = run()
    check(healthy.returncode == 0, f"safe central readiness failed: {healthy.stderr}")
    check(
        calls.read_text(encoding="ascii").splitlines()
        == ["node-ready", "node-ready"],
        "central northd was not gated both before and after the stability wait",
    )

    marker.write_text("not-empty\n", encoding="ascii")
    nonempty = run()
    check(nonempty.returncode != 0, "non-empty central marker was accepted")

    marker.write_bytes(b"")
    marker.chmod(0o666)
    writable = run()
    check(writable.returncode != 0, "writable central marker was accepted")

    marker.unlink()
    target = root / "central-target"
    target.write_bytes(b"")
    target.chmod(0o644)
    marker.symlink_to(target)
    symlink = run()
    check(symlink.returncode != 0, "symlinked central marker was accepted")
    marker.unlink()

    marker.mkdir(mode=0o755)
    directory = run()
    check(directory.returncode != 0, "directory central marker was accepted")
    marker.rmdir()

    marker.write_bytes(b"")
    marker.chmod(0o644)
    hardlink = root / "central-hardlink"
    os.link(marker, hardlink)
    linked = run()
    check(linked.returncode != 0, "hardlinked central marker was accepted")

print("pvn-node-ready tests passed")
