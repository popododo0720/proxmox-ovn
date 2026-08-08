#!/usr/bin/python3
"""Install/remove the PVN QEMU lifecycle hooks for the exact supported PVE build."""

from __future__ import annotations

import hashlib
import fcntl
import json
import os
import pathlib
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import time


SUPPORTED_QEMU_SERVER_VERSION = "9.1.15"
SUPPORTED_SHA256 = {
    "qemu_server": "15251482cdca5fe006f6cdba53b2eb2cec8fc3fd776efbbc601a5ef46b171bf4",
    "qemu_migrate": "f53ba901cb219276952eb389b957dbf8a0cdc7d31cecfdb53d0607f37f2e6c43",
    "api2_qemu": "0b2f0afea3655a016745185dc1a924f34abcaebebd4a12b8f55057d3ec3ab8a6",
}
DEFAULT_PATHS = {
    "qemu_server": pathlib.Path("/usr/share/perl5/PVE/QemuServer.pm"),
    "qemu_migrate": pathlib.Path("/usr/share/perl5/PVE/QemuMigrate.pm"),
    "api2_qemu": pathlib.Path("/usr/share/perl5/PVE/API2/Qemu.pm"),
}
DEFAULT_MODULE = pathlib.Path("/usr/share/perl5/PVN/ComputeLifecycle.pm")
BEGIN_RE = re.compile(rb"^[ \t]*# PVN-COMPUTE:BEGIN ([a-z0-9-]+)\r?\n$")
END_RE = re.compile(rb"^[ \t]*# PVN-COMPUTE:END ([a-z0-9-]+)\r?\n$")


def fail(message: str) -> "None":
    raise RuntimeError(f"PVN compute hooks: {message}")


def block(label: str, content: bytes, indent: bytes = b"") -> bytes:
    return (
        indent
        + f"# PVN-COMPUTE:BEGIN {label}\n".encode()
        + content
        + indent
        + f"# PVN-COMPUTE:END {label}\n".encode()
    )


def strip_blocks(data: bytes) -> bytes:
    output: list[bytes] = []
    active: bytes | None = None
    seen: set[bytes] = set()
    for line in data.splitlines(keepends=True):
        begin = BEGIN_RE.match(line)
        end = END_RE.match(line)
        if begin:
            label = begin.group(1)
            if active is not None or label in seen:
                fail("malformed or duplicate lifecycle marker")
            active = label
            seen.add(label)
            continue
        if end:
            if active is None or end.group(1) != active:
                fail("orphaned lifecycle marker")
            active = None
            continue
        if active is None:
            output.append(line)
    if active is not None:
        fail("unterminated lifecycle marker")
    return b"".join(output)


def replace_once(data: bytes, anchor: bytes, replacement: bytes, label: str) -> bytes:
    count = data.count(anchor)
    if count != 1:
        fail(f"unknown PVE 9.1.15 {label} signature (found {count}, expected 1)")
    return data.replace(anchor, replacement, 1)


def inject_qemu_server(data: bytes) -> bytes:
    import_anchor = b"use PVE::QemuServer::DBusVMState;\n"
    data = replace_once(
        data,
        import_anchor,
        import_anchor
        + block("qemu-server-import", b"use PVN::ComputeLifecycle;\n"),
        "QemuServer import",
    )
    hook = b"    PVE::GuestHelpers::exec_hookscript($conf, $vmid, 'pre-start', 1);\n"
    data = replace_once(
        data,
        hook,
        block(
            "qemu-server-pre-start",
            b"    PVN::ComputeLifecycle::pre_start($vmid, $conf, $migratedfrom);\n",
            b"    ",
        )
        + hook,
        "QemuServer pre-start",
    )
    return data


def inject_qemu_migrate(data: bytes) -> bytes:
    import_anchor = b"use PVE::QemuServer::DBusVMState;\n"
    data = replace_once(
        data,
        import_anchor,
        import_anchor + block("qemu-migrate-import", b"use PVN::ComputeLifecycle;\n"),
        "QemuMigrate import",
    )
    phase1 = (
        b"sub phase1 {\n"
        b"    my ($self, $vmid) = @_;\n\n"
        b"    $self->log('info', \"starting migration of VM $vmid to node '$self->{node}' ($self->{nodeip})\");\n\n"
        b"    my $conf = $self->{vmconf};\n"
    )
    data = replace_once(
        data,
        phase1,
        phase1
        + block(
            "qemu-migrate-begin",
            b"    $self->{pvn_compute_lifecycle} =\n"
            b"        PVN::ComputeLifecycle::migration_begin($self, $vmid, $conf);\n",
            b"    ",
        ),
        "QemuMigrate phase1",
    )
    phase1_cleanup = (
        b"sub phase1_cleanup {\n"
        b"    my ($self, $vmid, $err) = @_;\n\n"
        b"    $self->log('info', \"aborting phase 1 - cleanup resources\");\n"
    )
    cleanup_body = (
        b"    if ($self->{pvn_compute_lifecycle}) {\n"
        b"        eval {\n"
        b"            PVN::ComputeLifecycle::migration_abort($self->{pvn_compute_lifecycle});\n"
        b"            delete $self->{pvn_compute_lifecycle};\n"
        b"        };\n"
        b"        $self->log('err', \"PVN migration abort failed: $@\") if $@;\n"
        b"    }\n"
    )
    data = replace_once(
        data,
        phase1_cleanup,
        phase1_cleanup + block("qemu-migrate-phase1-abort", cleanup_body, b"    "),
        "QemuMigrate phase1 cleanup",
    )
    phase2_cleanup = (
        b"sub phase2_cleanup {\n"
        b"    my ($self, $vmid, $err) = @_;\n\n"
        b"    return if !$self->{errors};\n"
    )
    data = replace_once(
        data,
        phase2_cleanup,
        phase2_cleanup + block("qemu-migrate-phase2-abort", cleanup_body, b"    "),
        "QemuMigrate phase2 cleanup",
    )
    finalize_anchor = (
        b"    if ($self->{opts}->{remote} && $self->{opts}->{delete}) {\n"
        b"        eval { PVE::QemuServer::destroy_vm($self->{storecfg}, $vmid, 1, undef, 0) };\n"
        b"        warn \"Failed to remove source VM - $@\\n\" if $@;\n"
        b"    }\n"
        b"}\n\n"
        b"sub final_cleanup {\n"
    )
    finalize_body = (
        b"    if ($self->{pvn_compute_lifecycle}) {\n"
        b"        eval { PVN::ComputeLifecycle::migration_finalize($self->{pvn_compute_lifecycle}); };\n"
        b"        if (my $err = $@) {\n"
        b"            $self->log('err', \"PVN migration finalize failed: $err\");\n"
        b"            $self->{errors} = 1;\n"
        b"        } else {\n"
        b"            delete $self->{pvn_compute_lifecycle};\n"
        b"        }\n"
        b"    }\n"
    )
    data = replace_once(
        data,
        finalize_anchor,
        b"    if ($self->{opts}->{remote} && $self->{opts}->{delete}) {\n"
        b"        eval { PVE::QemuServer::destroy_vm($self->{storecfg}, $vmid, 1, undef, 0) };\n"
        b"        warn \"Failed to remove source VM - $@\\n\" if $@;\n"
        b"    }\n"
        + block("qemu-migrate-finalize", finalize_body, b"    ")
        + b"}\n\nsub final_cleanup {\n",
        "QemuMigrate finalize",
    )
    return data


def inject_api2_qemu(data: bytes) -> bytes:
    import_anchor = b"use PVE::QemuServer::DBusVMState;\n"
    data = replace_once(
        data,
        import_anchor,
        import_anchor + block("api2-qemu-import", b"use PVN::ComputeLifecycle;\n"),
        "API2 Qemu import",
    )

    jobs_anchor = b"            my $jobs = {};\n"
    data = replace_once(
        data,
        jobs_anchor,
        jobs_anchor
        + block(
            "api2-clone-state",
            b"            my $pvn_clone_lifecycle;\n"
            b"            my $pvn_clone_target_config;\n"
            b"            my $pvn_clone_target_node = $target // $localnode;\n",
            b"            ",
        ),
        "clone state",
    )
    activate_anchor = b"                PVE::Storage::activate_volumes($storecfg, $vollist, $snapname);\n"
    clone_prepare = (
        b"                $pvn_clone_lifecycle = PVN::ComputeLifecycle::clone_prepare(\n"
        b"                    $vmid, $newid, $pvn_clone_target_node, $oldconf, $newconf, $snapname,\n"
        b"                );\n"
    )
    data = replace_once(
        data,
        activate_anchor,
        block("api2-clone-prepare", clone_prepare, b"                ") + activate_anchor,
        "clone prepare",
    )
    remote_config_anchor = (
        b"                    my $newconffile = PVE::QemuConfig->config_file($newid, $target);\n"
    )
    data = replace_once(
        data,
        remote_config_anchor,
        remote_config_anchor
        + block(
            "api2-clone-remote-config",
            b"                    $pvn_clone_target_config = $newconffile;\n",
            b"                    ",
        ),
        "clone target config",
    )
    clone_error_anchor = b"            if (my $err = $@) {\n"
    clone_error = (
        b"                if ($pvn_clone_lifecycle) {\n"
        b"                    eval { PVN::ComputeLifecycle::clone_abort($pvn_clone_lifecycle); };\n"
        b"                    warn \"PVN clone abort failed: $@\" if $@;\n"
        b"                }\n"
    )
    # The clone worker is the only error block immediately followed by block-job cleanup.
    clone_error_context = (
        clone_error_anchor
        +
        b"                eval {\n"
        b"                    PVE::QemuServer::BlockJob::qemu_blockjobs_cancel(vm_qmp_peer($vmid), $jobs);\n"
    )
    data = replace_once(
        data,
        clone_error_context,
        clone_error_anchor
        + block("api2-clone-abort", clone_error, b"                ")
        + b"                eval {\n"
        + b"                    PVE::QemuServer::BlockJob::qemu_blockjobs_cancel(vm_qmp_peer($vmid), $jobs);\n",
        "clone abort",
    )
    clone_unlink_anchor = b"                unlink $conffile; # avoid races -> last thing before die\n"
    data = replace_once(
        data,
        clone_unlink_anchor,
        block(
            "api2-clone-target-cleanup",
            b"                unlink $pvn_clone_target_config\n"
            b"                    if defined($pvn_clone_target_config) && -e $pvn_clone_target_config;\n",
            b"                ",
        )
        + clone_unlink_anchor,
        "clone target cleanup",
    )
    clone_return_anchor = (
        b"                die \"clone failed: $err\";\n"
        b"            }\n\n"
        b"            return;\n"
    )
    data = replace_once(
        data,
        clone_return_anchor,
        b"                die \"clone failed: $err\";\n"
        b"            }\n"
        + block(
            "api2-clone-commit",
            b"            # Commit is outside the PVE work/cleanup eval. An ambiguous manager\n"
            b"            # response must preserve the completed clone and its durable intent.\n"
            b"            PVN::ComputeLifecycle::clone_commit($pvn_clone_lifecycle);\n"
            b"            my $pvn_clone_committed_file =\n"
            b"                PVE::QemuConfig->config_file($newid, $pvn_clone_target_node);\n"
            b"            die \"committed clone config is missing from target node '$pvn_clone_target_node'\\n\"\n"
            b"                if !-f $pvn_clone_committed_file;\n"
            b"            die \"remote clone left a duplicate source-node config\\n\"\n"
            b"                if $target && -f PVE::QemuConfig->config_file($newid);\n"
            b"            my $pvn_clone_committed_conf =\n"
            b"                PVE::QemuConfig->load_config($newid, $pvn_clone_target_node);\n"
            b"            PVN::ComputeLifecycle::clone_activate_config(\n"
            b"                $pvn_clone_lifecycle, $pvn_clone_committed_conf,\n"
            b"            );\n"
            b"            my $pvn_clone_committed_path =\n"
            b"                PVE::QemuConfig->cfs_config_path($newid, $pvn_clone_target_node);\n"
            b"            PVE::Cluster::cfs_write_file(\n"
            b"                $pvn_clone_committed_path, $pvn_clone_committed_conf,\n"
            b"            );\n",
            b"            ",
        )
        + b"\n            return;\n",
        "clone post-work commit",
    )

    snapshot_create_anchor = (
        b"            PVE::QemuConfig->snapshot_create(\n"
        b"                $vmid, $snapname, $param->{vmstate}, $param->{description},\n"
        b"            );\n"
    )
    snapshot_create_after = (
        b"            my $pvn_snapshot_conf = PVE::QemuConfig->load_config($vmid);\n"
        b"            my $pvn_snapshot_epoch = PVN::ComputeLifecycle::snapshot_epoch(\n"
        b"                $pvn_snapshot_conf->{snapshots}->{$snapname},\n"
        b"            );\n"
        b"            eval {\n"
        b"                PVN::ComputeLifecycle::snapshot_create(\n"
        b"                    $vmid, $snapname, $pvn_snapshot_conf->{snapshots}->{$snapname},\n"
        b"                );\n"
        b"            };\n"
        b"            if (my $pvn_err = $@) {\n"
        b"                my $delete_error;\n"
        b"                eval { PVE::QemuConfig->snapshot_delete($vmid, $snapname, 1); };\n"
        b"                $delete_error = $@;\n"
        b"                die \"$pvn_err; rollback-delete of snapshot '$snapname' failed: $delete_error\"\n"
        b"                    if $delete_error;\n"
        b"                eval {\n"
        b"                    PVN::ComputeLifecycle::snapshot_cleanup(\n"
        b"                        $vmid, $snapname, $pvn_snapshot_epoch,\n"
        b"                    );\n"
        b"                };\n"
        b"                my $manifest_delete_error = $@;\n"
        b"                die \"$pvn_err; PVN snapshot manifest cleanup failed: $manifest_delete_error\"\n"
        b"                    if $manifest_delete_error;\n"
        b"                die $pvn_err;\n"
        b"            }\n"
    )
    data = replace_once(
        data,
        snapshot_create_anchor,
        snapshot_create_anchor
        + block("api2-snapshot-create-manifest", snapshot_create_after, b"            "),
        "snapshot create",
    )
    snapshot_rollback_anchor = b"            PVE::QemuConfig->snapshot_rollback($vmid, $snapname);\n"
    snapshot_rollback_open = (
        b"            my $pvn_snapshot_conf = PVE::QemuConfig->load_config($vmid);\n"
        b"            my $pvn_snapshot_rollback = PVN::ComputeLifecycle::snapshot_prepare(\n"
        b"                $vmid, $snapname, $pvn_snapshot_conf->{snapshots}->{$snapname},\n"
        b"                'rollback',\n"
        b"            );\n"
        b"            eval {\n"
    )
    snapshot_rollback_close = (
        b"            };\n"
        b"            if (my $pvn_snapshot_rollback_error = $@) {\n"
        b"                eval { PVN::ComputeLifecycle::snapshot_abort($pvn_snapshot_rollback); };\n"
        b"                my $pvn_snapshot_abort_error = $@;\n"
        b"                die \"$pvn_snapshot_rollback_error; PVN snapshot rollback abort failed: $pvn_snapshot_abort_error\"\n"
        b"                    if $pvn_snapshot_abort_error;\n"
        b"                die $pvn_snapshot_rollback_error;\n"
        b"            }\n"
        b"            # Commit is outside the PVE mutation eval. An ambiguous manager\n"
        b"            # response must preserve the durable prepared transaction.\n"
        b"            PVN::ComputeLifecycle::snapshot_commit($pvn_snapshot_rollback);\n"
    )
    data = replace_once(
        data,
        snapshot_rollback_anchor,
        block("api2-snapshot-rollback-open", snapshot_rollback_open, b"            ")
        + snapshot_rollback_anchor
        + block("api2-snapshot-rollback-close", snapshot_rollback_close, b"            "),
        "snapshot rollback",
    )
    snapshot_delete_anchor = (
        b"            PVE::Cluster::log_msg('info', $authuser, \"delete snapshot VM $vmid: $snapname\");\n"
        b"            PVE::QemuConfig->snapshot_delete($vmid, $snapname, $param->{force});\n"
    )
    snapshot_delete_open = (
        b"            my $pvn_snapshot_delete_conf = PVE::QemuConfig->load_config($vmid);\n"
        b"            my $pvn_snapshot_delete =\n"
        b"                PVN::ComputeLifecycle::snapshot_prepare(\n"
        b"                $vmid, $snapname,\n"
        b"                $pvn_snapshot_delete_conf->{snapshots}->{$snapname}, 'delete',\n"
        b"            );\n"
        b"            eval {\n"
    )
    snapshot_delete_close = (
        b"            };\n"
        b"            if (my $pvn_snapshot_delete_error = $@) {\n"
        b"                eval { PVN::ComputeLifecycle::snapshot_abort($pvn_snapshot_delete); };\n"
        b"                my $pvn_snapshot_abort_error = $@;\n"
        b"                die \"$pvn_snapshot_delete_error; PVN snapshot delete abort failed: $pvn_snapshot_abort_error\"\n"
        b"                    if $pvn_snapshot_abort_error;\n"
        b"                die $pvn_snapshot_delete_error;\n"
        b"            }\n"
        b"            # Commit is outside the PVE mutation eval. Never abort after\n"
        b"            # an ambiguous commit response.\n"
        b"            PVN::ComputeLifecycle::snapshot_commit($pvn_snapshot_delete);\n"
    )
    snapshot_delete_replacement = (
        b"            PVE::Cluster::log_msg('info', $authuser, \"delete snapshot VM $vmid: $snapname\");\n"
        + block("api2-snapshot-delete-open", snapshot_delete_open, b"            ")
        + b"            PVE::QemuConfig->snapshot_delete($vmid, $snapname, $param->{force});\n"
        + block("api2-snapshot-delete-close", snapshot_delete_close, b"            ")
    )
    data = replace_once(
        data,
        snapshot_delete_anchor,
        snapshot_delete_replacement,
        "snapshot delete",
    )

    template_open_anchor = b"                    $conf->{template} = 1;\n"
    template_open = (
        b"                    my $pvn_template_lifecycle =\n"
        b"                        PVN::ComputeLifecycle::template_prepare($vmid, $conf);\n"
        b"                    eval {\n"
    )
    data = replace_once(
        data,
        template_open_anchor,
        block("api2-template-open", template_open, b"                    ") + template_open_anchor,
        "template prepare",
    )
    template_close_anchor = b"                    PVE::QemuServer::template_create($vmid, $conf, $disk);\n"
    template_close = (
        b"                    };\n"
        b"                    if (my $err = $@) {\n"
        b"                        my ($pvn_after, $pvn_load_error);\n"
        b"                        eval { $pvn_after = PVE::QemuConfig->load_config($vmid); };\n"
        b"                        $pvn_load_error = $@;\n"
        b"                        if (!$pvn_load_error && !$pvn_after->{template}) {\n"
        b"                            eval { PVN::ComputeLifecycle::template_abort($pvn_template_lifecycle); };\n"
        b"                            warn \"PVN template abort failed: $@\" if $@;\n"
        b"                        } else {\n"
        b"                            # A persisted template flag or ambiguous reload means disk\n"
        b"                            # conversion may be partial. Preserve/commit the blueprint.\n"
        b"                            eval { PVN::ComputeLifecycle::template_commit($pvn_template_lifecycle); };\n"
        b"                            warn \"PVN template blueprint commit failed: $@\" if $@;\n"
        b"                        }\n"
        b"                        die $err;\n"
        b"                    }\n"
        b"                    PVN::ComputeLifecycle::template_commit($pvn_template_lifecycle);\n"
    )
    data = replace_once(
        data,
        template_close_anchor,
        template_close_anchor + block("api2-template-close", template_close, b"                    "),
        "template commit",
    )

    destroy_open_anchor = (
        b"                    PVE::QemuServer::destroy_vm(\n"
        b"                        $storecfg,\n"
        b"                        $vmid,\n"
        b"                        $skiplock,\n"
        b"                        { lock => 'destroyed' },\n"
        b"                        $purge_unreferenced,\n"
        b"                    );\n"
    )
    destroy_before = (
        b"                    my $pvn_destroy_conf = PVE::QemuConfig->load_config($vmid);\n"
        b"                    my $pvn_destroy_conf_path =\n"
        b"                        PVE::QemuConfig->cfs_config_path($vmid);\n"
        b"                    my $pvn_destroy_lifecycle =\n"
        b"                        PVN::ComputeLifecycle::destroy_capture($vmid, $pvn_destroy_conf);\n"
        b"                    eval {\n"
    )
    data = replace_once(
        data,
        destroy_open_anchor,
        block("api2-destroy-open", destroy_before, b"                    ") + destroy_open_anchor,
        "destroy lifecycle",
    )
    destroy_config_anchor = b"                    PVE::QemuConfig->destroy_config($vmid);\n"
    destroy_close = (
        b"                    };\n"
        b"                    if (my $pvn_destroy_error = $@) {\n"
        b"                        my ($pvn_destroy_config_exists, $pvn_destroy_probe_error);\n"
        b"                        eval {\n"
        b"                            my $pvn_destroy_after =\n"
        b"                                PVE::Cluster::cfs_read_file($pvn_destroy_conf_path);\n"
        b"                            $pvn_destroy_config_exists =\n"
        b"                                defined($pvn_destroy_after) ? 1 : 0;\n"
        b"                        };\n"
        b"                        $pvn_destroy_probe_error = $@;\n"
        b"                        if ($pvn_destroy_probe_error) {\n"
        b"                            warn \"PVN destroy state probe failed; preserving detached intent: \"\n"
        b"                                . $pvn_destroy_probe_error;\n"
        b"                            die $pvn_destroy_error;\n"
        b"                        }\n"
        b"                        if ($pvn_destroy_config_exists) {\n"
        b"                            eval {\n"
        b"                                PVN::ComputeLifecycle::destroy_abort($pvn_destroy_lifecycle);\n"
        b"                            };\n"
        b"                            my $pvn_abort_error = $@;\n"
        b"                            die \"$pvn_destroy_error; PVN destroy abort failed: $pvn_abort_error\"\n"
        b"                                if $pvn_abort_error;\n"
        b"                            die $pvn_destroy_error;\n"
        b"                        }\n"
        b"                        eval {\n"
        b"                            PVN::ComputeLifecycle::destroy_commit($pvn_destroy_lifecycle);\n"
        b"                        };\n"
        b"                        my $pvn_commit_error = $@;\n"
        b"                        die \"$pvn_destroy_error; PVN destroy commit failed: $pvn_commit_error\"\n"
        b"                            if $pvn_commit_error;\n"
        b"                        die $pvn_destroy_error;\n"
        b"                    }\n"
        b"                    # All PVE ACL/firewall/replication/HA/config cleanup is complete.\n"
        b"                    PVN::ComputeLifecycle::destroy_commit($pvn_destroy_lifecycle);\n"
    )
    data = replace_once(
        data,
        destroy_config_anchor,
        destroy_config_anchor
        + block("api2-destroy-close", destroy_close, b"                    "),
        "destroy post-cleanup commit",
    )
    return data


INJECTORS = {
    "qemu_server": inject_qemu_server,
    "qemu_migrate": inject_qemu_migrate,
    "api2_qemu": inject_api2_qemu,
}


def regular_file(path: pathlib.Path, label: str) -> None:
    if path.is_symlink() or not path.is_file():
        fail(f"{label} is missing, not regular, or symlinked: {path}")


def qemu_server_version() -> str:
    override = os.environ.get("PVN_QEMU_SERVER_VERSION")
    if override is not None:
        return override
    try:
        return subprocess.check_output(
            ["dpkg-query", "-W", "-f=${Version}", "qemu-server"],
            text=True,
            stderr=subprocess.DEVNULL,
        ).strip()
    except (OSError, subprocess.CalledProcessError):
        return ""


def test_mode_allows(paths: dict[str, pathlib.Path], module: pathlib.Path) -> bool:
    if os.environ.get("PVN_COMPUTE_TEST_MODE") != "1":
        return False
    root_value = os.environ.get("PVN_COMPUTE_TEST_ROOT", "")
    if not root_value:
        fail("test mode requires PVN_COMPUTE_TEST_ROOT")
    root = pathlib.Path(root_value).resolve(strict=True)
    for path in [*paths.values(), module]:
        resolved = path.resolve(strict=True)
        if root != resolved and root not in resolved.parents:
            fail("test mode target escapes PVN_COMPUTE_TEST_ROOT")
        if path in DEFAULT_PATHS.values() or path == DEFAULT_MODULE:
            fail("test mode cannot target installed PVE files")
    return True


def compile_perl(
    paths: list[pathlib.Path], module: pathlib.Path, *, require_module: bool
) -> None:
    if os.environ.get("PVN_COMPUTE_SKIP_PERL_CHECK") == "1":
        return
    module_root = module.parent.parent
    compile_targets = [module, *paths] if require_module else paths
    for path in compile_targets:
        result = subprocess.run(
            ["perl", "-T", "-I", str(module_root), "-c", str(path)],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
        )
        if result.returncode != 0:
            detail = result.stderr.strip().splitlines()[-1] if result.stderr.strip() else "unknown error"
            fail(f"Perl compile failed for {path}: {detail}")


def atomic_replace(path: pathlib.Path, data: bytes) -> None:
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.pvn-", dir=path.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        shutil.copystat(path, temporary, follow_symlinks=False)
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def journal_path(paths: dict[str, pathlib.Path]) -> pathlib.Path:
    if os.environ.get("PVN_COMPUTE_TEST_MODE") == "1":
        test_root = os.environ.get("PVN_COMPUTE_TEST_ROOT", "")
        if test_root:
            return pathlib.Path(test_root) / ".pvn-compute-transaction"
    if paths == DEFAULT_PATHS:
        return pathlib.Path("/var/lib/pvn-node/compute-inject-transaction")
    return paths["qemu_server"].parent / ".pvn-compute-transaction"


def lock_path(paths: dict[str, pathlib.Path]) -> pathlib.Path:
    if os.environ.get("PVN_COMPUTE_TEST_MODE") == "1":
        test_root = os.environ.get("PVN_COMPUTE_TEST_ROOT", "")
        if test_root:
            return pathlib.Path(test_root) / ".pvn-compute.lock"
    if paths == DEFAULT_PATHS:
        return pathlib.Path("/run/lock/pvn-compute-inject.lock")
    return paths["qemu_server"].parent / ".pvn-compute.lock"


def require_root_private_directory(path: pathlib.Path, label: str) -> None:
    try:
        metadata = path.lstat()
    except OSError as error:
        fail(f"cannot inspect {label} {path}: {error}")
    if (
        not stat.S_ISDIR(metadata.st_mode)
        or metadata.st_uid != 0
        or metadata.st_gid != 0
        or stat.S_IMODE(metadata.st_mode) != 0o700
    ):
        fail(f"{label} must be a root-owned non-symlink directory with mode 0700: {path}")


def acquire_lock(paths: dict[str, pathlib.Path]):
    if paths == DEFAULT_PATHS and os.geteuid() != 0:
        fail("installed PVE compute hooks may only be managed by root")
    path = lock_path(paths)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
    flags = os.O_CREAT | os.O_RDWR
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(path, flags, 0o600)
    handle = os.fdopen(descriptor, "r+")
    metadata = os.fstat(handle.fileno())
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1:
        handle.close()
        fail(f"unsafe compute hook lock file: {path}")
    if paths == DEFAULT_PATHS and (metadata.st_uid != 0 or stat.S_IMODE(metadata.st_mode) != 0o600):
        handle.close()
        fail(f"compute hook lock must be root-owned mode 0600: {path}")
    deadline = time.monotonic() + 10
    while True:
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            break
        except BlockingIOError:
            if time.monotonic() >= deadline:
                handle.close()
                fail("timed out waiting for another compute hook transaction")
            time.sleep(0.05)
    if os.environ.get("PVN_COMPUTE_TEST_MODE") == "1":
        hold_ms = int(os.environ.get("PVN_COMPUTE_TEST_HOLD_LOCK_MS", "0"))
        if hold_ms > 0:
            time.sleep(hold_ms / 1000)
    return handle


def remove_journal(journal: pathlib.Path) -> None:
    shutil.rmtree(journal)
    directory_fd = os.open(journal.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def recover_journal(paths: dict[str, pathlib.Path]) -> None:
    journal = journal_path(paths)
    if not journal.exists() and not journal.is_symlink():
        return
    if journal.is_symlink() or not journal.is_dir():
        fail(f"unsafe compute hook recovery journal: {journal}")
    if paths == DEFAULT_PATHS:
        require_root_private_directory(journal.parent, "compute hook journal parent")
        require_root_private_directory(journal, "compute hook recovery journal")
    manifest_path = journal / "manifest.json"
    regular_file(manifest_path, "compute hook recovery manifest")
    try:
        manifest = json.loads(manifest_path.read_text())
    except (OSError, ValueError) as error:
        fail(f"invalid compute hook recovery manifest: {error}")
    records = manifest.get("files") if isinstance(manifest, dict) else None
    if not isinstance(records, list) or len(records) != len(paths):
        fail("compute hook recovery manifest has an invalid file set")
    expected = {str(path.absolute()): name for name, path in paths.items()}
    restored: set[str] = set()
    for record in records:
        if not isinstance(record, dict):
            fail("compute hook recovery manifest contains an invalid record")
        target = record.get("target")
        name = expected.get(target)
        if name is None or name in restored:
            fail("compute hook recovery journal targets do not match this invocation")
        backup = journal / f"{name}.original"
        regular_file(backup, f"compute hook backup {name}")
        data = backup.read_bytes()
        if hashlib.sha256(data).hexdigest() != record.get("sha256"):
            fail(f"compute hook backup checksum mismatch for {name}")
        target_path = paths[name]
        regular_file(target_path, name)
        atomic_replace(target_path, data)
        restored.add(name)
    if restored != set(paths):
        fail("compute hook recovery journal is incomplete")
    remove_journal(journal)


def create_journal(paths: dict[str, pathlib.Path], originals: dict[str, bytes]) -> pathlib.Path:
    journal = journal_path(paths)
    parent = journal.parent
    if parent.exists():
        if parent.is_symlink() or not parent.is_dir():
            fail(f"unsafe compute hook journal parent: {parent}")
    else:
        parent.mkdir(parents=True, mode=0o700)
    if paths == DEFAULT_PATHS:
        require_root_private_directory(parent, "compute hook journal parent")
    if journal.exists() or journal.is_symlink():
        fail("unrecovered compute hook transaction already exists")
    temporary = pathlib.Path(tempfile.mkdtemp(prefix=f".{journal.name}.", dir=parent))
    try:
        records = []
        for name, path in paths.items():
            backup = temporary / f"{name}.original"
            backup.write_bytes(originals[name])
            shutil.copystat(path, backup, follow_symlinks=False)
            with backup.open("rb") as handle:
                os.fsync(handle.fileno())
            records.append(
                {
                    "name": name,
                    "target": str(path.absolute()),
                    "sha256": hashlib.sha256(originals[name]).hexdigest(),
                }
            )
        manifest = temporary / "manifest.json"
        manifest.write_text(json.dumps({"version": 1, "files": records}, sort_keys=True) + "\n")
        with manifest.open("rb") as handle:
            os.fsync(handle.fileno())
        directory_fd = os.open(temporary, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        os.replace(temporary, journal)
        if paths == DEFAULT_PATHS:
            require_root_private_directory(journal, "compute hook recovery journal")
        directory_fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)
    return journal


def parse_arguments() -> tuple[str, dict[str, pathlib.Path], pathlib.Path]:
    if len(sys.argv) not in (2, 6) or sys.argv[1] not in ("install", "remove", "verify"):
        print(
            f"usage: {sys.argv[0]} install|remove|verify [QemuServer.pm QemuMigrate.pm API2/Qemu.pm ComputeLifecycle.pm]",
            file=sys.stderr,
        )
        raise SystemExit(2)
    paths = dict(DEFAULT_PATHS)
    module = DEFAULT_MODULE
    if len(sys.argv) == 6:
        paths = {
            "qemu_server": pathlib.Path(sys.argv[2]),
            "qemu_migrate": pathlib.Path(sys.argv[3]),
            "api2_qemu": pathlib.Path(sys.argv[4]),
        }
        module = pathlib.Path(sys.argv[5])
    return sys.argv[1], paths, module


def main() -> int:
    action, paths, module = parse_arguments()
    transaction_lock = acquire_lock(paths)
    try:
        return main_locked(action, paths, module)
    finally:
        transaction_lock.close()


def main_locked(action: str, paths: dict[str, pathlib.Path], module: pathlib.Path) -> int:
    # Refuse an unsupported package/signature before journal recovery or any
    # other write. A pending supported transaction can still be recovered by
    # an explicit remove when qemu-server itself has moved on.
    for name, path in paths.items():
        regular_file(path, name)
    if action in ("install", "verify"):
        regular_file(module, "PVN compute lifecycle module")
        preflight_originals = {name: path.read_bytes() for name, path in paths.items()}
        preflight_clean = {
            name: strip_blocks(data) for name, data in preflight_originals.items()
        }
        test_mode = test_mode_allows(paths, module)
        version = qemu_server_version()
        if version != SUPPORTED_QEMU_SERVER_VERSION:
            fail(
                f"unsupported qemu-server version {version or 'unknown'}; "
                f"expected {SUPPORTED_QEMU_SERVER_VERSION}; no PVE file was changed"
            )
        if not test_mode:
            for name, data in preflight_clean.items():
                digest = hashlib.sha256(data).hexdigest()
                if digest != SUPPORTED_SHA256[name]:
                    fail(f"unknown {name} signature {digest}; no PVE file was changed")

    recover_journal(paths)
    for name, path in paths.items():
        regular_file(path, name)

    originals = {name: path.read_bytes() for name, path in paths.items()}
    clean = {name: strip_blocks(data) for name, data in originals.items()}
    test_mode = test_mode_allows(paths, module) if action in ("install", "verify") else False
    if action in ("install", "verify"):
        if not test_mode:
            for name, data in clean.items():
                digest = hashlib.sha256(data).hexdigest()
                if digest != SUPPORTED_SHA256[name]:
                    fail(f"unknown {name} signature {digest}; no PVE file was changed")

    staged = {
        name: INJECTORS[name](data) if action in ("install", "verify") else data
        for name, data in clean.items()
    }
    with tempfile.TemporaryDirectory(prefix="pvn-compute-compile-") as temporary_directory:
        temporary_root = pathlib.Path(temporary_directory)
        compile_paths: list[pathlib.Path] = []
        for name, data in staged.items():
            path = temporary_root / f"{name}.pm"
            path.write_bytes(data)
            compile_paths.append(path)
        compile_perl(
            compile_paths,
            module,
            require_module=action in ("install", "verify"),
        )

    if action == "verify":
        for name in paths:
            if originals[name] != staged[name]:
                fail(f"{name} does not contain the exact supported PVN lifecycle hooks")
        return 0

    if all(staged[name] == originals[name] for name in paths):
        return 0

    # Every version, signature, marker, anchor, and Perl check completed before
    # the first installed file is replaced.
    journal = create_journal(paths, originals)
    replaced = 0
    try:
        for name, path in paths.items():
            if staged[name] != originals[name]:
                atomic_replace(path, staged[name])
                replaced += 1
                crash_after = int(os.environ.get("PVN_COMPUTE_TEST_CRASH_AFTER", "0"))
                if crash_after and replaced == crash_after:
                    os._exit(99)
        for name, path in paths.items():
            if path.read_bytes() != staged[name]:
                fail(f"post-replace verification failed for {name}")
    except BaseException:
        recover_journal(paths)
        raise
    remove_journal(journal)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except RuntimeError as error:
        print(error, file=sys.stderr)
        raise SystemExit(1)
