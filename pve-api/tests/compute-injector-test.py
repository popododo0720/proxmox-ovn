#!/usr/bin/python3

from __future__ import annotations

import hashlib
import os
import pathlib
import re
import shutil
import subprocess
import tempfile
import time


REPO = pathlib.Path(__file__).resolve().parents[2]
INJECTOR = REPO / "pve-api" / "compute-inject.py"
MODULE = REPO / "pve-api" / "PVN" / "ComputeLifecycle.pm"
FIXTURES = REPO / "pve-api" / "tests" / "fixtures" / "qemu-server-9.1.15"
NAMES = ("QemuServer.pm", "QemuMigrate.pm", "API2-Qemu.pm")


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def fixture_tree() -> pathlib.Path:
    root = pathlib.Path(tempfile.mkdtemp(prefix="pvn-compute-inject-test-"))
    for name in NAMES:
        shutil.copy2(FIXTURES / name, root / name)
    module_dir = root / "PVN"
    module_dir.mkdir()
    shutil.copy2(MODULE, module_dir / "ComputeLifecycle.pm")
    return root


def contents(root: pathlib.Path) -> dict[str, bytes]:
    return {name: (root / name).read_bytes() for name in NAMES}


def runtime_fixture_test(root: pathlib.Path) -> None:
    harness = root / "runtime-test.pl"
    harness.write_text(
        r'''#!/usr/bin/perl
use strict;
use warnings;
use Test::More;

BEGIN { $INC{'PVE/QemuServer/DBusVMState.pm'} = 1; }

package PVE::Storage;
sub activate_volumes { return }
sub deactivate_volumes { return }
sub vdisk_free { return }

package PVE::QemuServer::BlockJob;
sub qemu_blockjobs_cancel { return }

package PVE::QemuServer;
our $FAIL_STAGE = '';
sub destroy_vm { die "destroy_vm fault\n" if $FAIL_STAGE eq 'destroy_vm'; return }
sub template_create { return }

package PVE::AccessControl;
sub add_vm_to_pool { return }
sub remove_vm_access { die "access fault\n" if $PVE::QemuServer::FAIL_STAGE eq 'access'; return }

package PVE::Firewall;
sub remove_vmfw_conf { die "firewall fault\n" if $PVE::QemuServer::FAIL_STAGE eq 'firewall'; return }

package PVE::QemuConfig;
our ($CONF, $SOURCE, $TARGET, $EXISTS, $PROBE_FAIL, $CFS_WRITE_PATH, $SNAPSHOT_FAIL);
our @SNAPSHOT_EVENTS;
sub config_file { my ($class, $vmid, $node) = @_; return $node ? $TARGET : $SOURCE; }
sub cfs_config_path { my ($class, $vmid, $node) = @_; $node //= 'prox1'; return "nodes/$node/qemu-server/$vmid.conf"; }
sub load_config { die "config absent\n" if defined($EXISTS) && !$EXISTS; return $CONF; }
sub destroy_config {
    $EXISTS = 0;
    die "destroy_config fault\n" if $PVE::QemuServer::FAIL_STAGE eq 'destroy_config';
    return;
}
sub snapshot_create { push @SNAPSHOT_EVENTS, 'pve:create'; return }
sub snapshot_delete {
    push @SNAPSHOT_EVENTS, 'pve:delete';
    die "snapshot delete fault\n" if ($SNAPSHOT_FAIL // '') eq 'delete';
    return;
}
sub snapshot_rollback {
    push @SNAPSHOT_EVENTS, 'pve:rollback';
    die "snapshot rollback fault\n" if ($SNAPSHOT_FAIL // '') eq 'rollback';
    return;
}

package PVE::Cluster;
sub cfs_write_file { my ($path, $conf) = @_; $PVE::QemuConfig::CFS_WRITE_PATH = $path; $PVE::QemuConfig::CONF = $conf; return; }
sub cfs_read_file { die "probe fault\n" if $PVE::QemuConfig::PROBE_FAIL; return $PVE::QemuConfig::EXISTS ? $PVE::QemuConfig::CONF : undef; }
sub log_msg { return }

package PVE::API2::Qemu;
sub vm_qmp_peer { return undef }

package main;
require $ENV{PVN_API2_FIXTURE};

local $PVN::ComputeLifecycle::NODE_OVERRIDE = sub { 'prox1' };
local $PVN::ComputeLifecycle::LIFECYCLE_ID_OVERRIDE = sub { 'fixture-' . join('-', @_) };

sub reset_clone_files {
    my ($suffix) = @_;
    $PVE::QemuConfig::SOURCE = "$ENV{PVN_FIXTURE_ROOT}/source-$suffix.conf";
    $PVE::QemuConfig::TARGET = "$ENV{PVN_FIXTURE_ROOT}/target-$suffix.conf";
    unlink $PVE::QemuConfig::SOURCE;
    unlink $PVE::QemuConfig::TARGET;
    open(my $handle, '>', $PVE::QemuConfig::SOURCE) or die $!;
    print {$handle} "clone config\n";
    close($handle);
    $PVE::QemuConfig::CFS_WRITE_PATH = undef;
}

my @requests;
my $clone_response = sub {
    my ($path, $payload) = @_;
    push @requests, $path;
    if ($path =~ m{/clone/prepare$}) {
        return {
            clone_id => $payload->{clone_id}, source_vmid => $payload->{source_vmid},
            target_vmid => $payload->{target_vmid}, source_node => $payload->{source_node},
            target_node => $payload->{target_node}, source_template => $payload->{source_template},
            operation_id => 'clone-operation', payload_hash => 'clone-payload',
            ports => [{ nic => 'net0', mac_address => '02:aa:bb:cc:dd:ee',
                port_id => 'clone-port', ownership_digest => 'clone-owner' }],
        };
    }
    return { state => 'committed' } if $path =~ m{/clone/commit$};
    return { state => 'aborted' } if $path =~ m{/clone/abort$};
    die "unexpected clone endpoint $path\n";
};

reset_clone_files('success');
my $source_conf = { net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=br-int' };
my $target_conf = { net0 => 'virtio=02:00:00:00:00:01,bridge=br-int' };
$PVE::QemuConfig::CONF = $target_conf;
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = $clone_response;
    PVE::API2::Qemu::clone_fixture(
        100, 200, 'prox2', 'prox1', $source_conf, $target_conf, undef,
        $PVE::QemuConfig::SOURCE,
    );
}
ok(!-e $PVE::QemuConfig::SOURCE, 'remote clone activation does not recreate source config');
ok(-f $PVE::QemuConfig::TARGET, 'remote clone keeps the moved target config');
is($PVE::QemuConfig::CFS_WRITE_PATH, 'nodes/prox2/qemu-server/200.conf', 'remote activation writes exact target CFS path');
like($target_conf->{net0}, qr/link_down=0/, 'definite clone commit enables target NIC');

reset_clone_files('ambiguous');
$target_conf = { net0 => 'virtio=02:00:00:00:00:02,bridge=br-int' };
$PVE::QemuConfig::CONF = $target_conf;
@requests = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        my $response = $clone_response->($path, $payload);
        die "commit response lost\n" if $path =~ m{/clone/commit$};
        return $response;
    };
    my $ok = eval {
        PVE::API2::Qemu::clone_fixture(
            100, 201, 'prox2', 'prox1', $source_conf, $target_conf, undef,
            $PVE::QemuConfig::SOURCE,
        );
        1;
    };
    ok(!$ok, 'ambiguous clone commit fails its PVE task');
}
ok(!-e $PVE::QemuConfig::SOURCE && -f $PVE::QemuConfig::TARGET, 'ambiguous commit preserves completed remote clone');
like($target_conf->{net0}, qr/link_down=1/, 'ambiguous commit preserves target NIC guard');
ok(!grep(m{/clone/abort$}, @requests), 'ambiguous commit never aborts or deletes prepared clone ports');

sub pvn_snapshot_config {
    my ($bridge) = @_;
    return {
        snapshots => {
            daily => {
                snaptime => 1700000001,
                net0 => "virtio=AA:BB:CC:DD:EE:01,bridge=$bridge",
            },
        },
    };
}

sub snapshot_manager {
    my (%options) = @_;
    my %prepared;
    return sub {
        my ($path, $payload) = @_;
        push @PVE::QemuConfig::SNAPSHOT_EVENTS, "manager:$path";
        if ($path =~ m{/snapshot/prepare$}) {
            my $transaction = {
                lifecycle_id => $payload->{lifecycle_id}, action => $payload->{action},
                vmid => $payload->{vmid}, snapshot_id => $payload->{snapshot_id},
                snapshot_epoch => $payload->{snapshot_epoch},
                operation_id => "snapshot-$payload->{action}-operation",
                payload_hash => "snapshot-$payload->{action}-payload",
                ports => [{ port_id => 'snapshot-port', nic => 'net0' }],
            };
            $prepared{$payload->{action}} = $transaction;
            return $transaction;
        }
        if ($path =~ m{/snapshot/(?:commit|abort)$}) {
            is_deeply(
                $payload, $prepared{$payload->{action}},
                'snapshot finish echoes the exact prepare transaction',
            );
            die "snapshot commit response lost\n"
                if $options{commit_fail} && $path =~ m{/commit$};
            return { state => 'finished' };
        }
        if ($path =~ m{/snapshot/create$}) {
            die "snapshot create callback response lost\n" if $options{create_fail};
            return { state => 'created' };
        }
        if ($path =~ m{/snapshot/cleanup$}) {
            is_deeply(
                $payload,
                { vmid => 400, snapshot_id => 'daily', snapshot_epoch => 1700000001 },
                'post-create cleanup sends only the exact removed generation',
            );
            return { state => 'deleted' };
        }
        die "unexpected snapshot endpoint $path\n";
    };
}

$PVE::QemuConfig::CONF = pvn_snapshot_config('br-int');
$PVE::QemuConfig::SNAPSHOT_FAIL = '';
@PVE::QemuConfig::SNAPSHOT_EVENTS = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = snapshot_manager();
    PVE::API2::Qemu::snapshot_rollback_fixture(400, 'daily');
}
is_deeply(
    \@PVE::QemuConfig::SNAPSHOT_EVENTS,
    [
        'manager:/api/v1/runtime/compute/snapshot/prepare',
        'pve:rollback',
        'manager:/api/v1/runtime/compute/snapshot/commit',
    ],
    'snapshot rollback holds one durable transaction across the PVE mutation',
);

$PVE::QemuConfig::SNAPSHOT_FAIL = 'rollback';
@PVE::QemuConfig::SNAPSHOT_EVENTS = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = snapshot_manager();
    my $ok = eval { PVE::API2::Qemu::snapshot_rollback_fixture(400, 'daily'); 1 };
    ok(!$ok, 'PVE snapshot rollback failure remains a task error');
    like($@, qr/snapshot rollback fault/, 'original rollback error is preserved');
}
is_deeply(
    \@PVE::QemuConfig::SNAPSHOT_EVENTS,
    [
        'manager:/api/v1/runtime/compute/snapshot/prepare',
        'pve:rollback',
        'manager:/api/v1/runtime/compute/snapshot/abort',
    ],
    'definite rollback failure aborts its prepared transaction',
);

$PVE::QemuConfig::SNAPSHOT_FAIL = '';
@PVE::QemuConfig::SNAPSHOT_EVENTS = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = snapshot_manager(commit_fail => 1);
    my $ok = eval { PVE::API2::Qemu::snapshot_rollback_fixture(400, 'daily'); 1 };
    ok(!$ok, 'ambiguous rollback commit remains a task error');
}
is(
    scalar(grep(m{/snapshot/commit$}, @PVE::QemuConfig::SNAPSHOT_EVENTS)),
    3,
    'snapshot commit is retried with one transaction',
);
ok(
    !grep(m{/snapshot/abort$}, @PVE::QemuConfig::SNAPSHOT_EVENTS),
    'ambiguous rollback commit is never followed by abort',
);

@PVE::QemuConfig::SNAPSHOT_EVENTS = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = snapshot_manager();
    PVE::API2::Qemu::snapshot_delete_fixture(400, 'daily', { force => 0 }, 'root@pam');
}
is_deeply(
    \@PVE::QemuConfig::SNAPSHOT_EVENTS,
    [
        'manager:/api/v1/runtime/compute/snapshot/prepare',
        'pve:delete',
        'manager:/api/v1/runtime/compute/snapshot/commit',
    ],
    'snapshot delete commits only after PVE deletion succeeds',
);

$PVE::QemuConfig::SNAPSHOT_FAIL = 'delete';
@PVE::QemuConfig::SNAPSHOT_EVENTS = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = snapshot_manager();
    my $ok = eval {
        PVE::API2::Qemu::snapshot_delete_fixture(400, 'daily', { force => 0 }, 'root@pam');
        1;
    };
    ok(!$ok, 'PVE snapshot delete failure remains a task error');
}
ok(
    grep(m{/snapshot/abort$}, @PVE::QemuConfig::SNAPSHOT_EVENTS)
        && !grep(m{/snapshot/commit$}, @PVE::QemuConfig::SNAPSHOT_EVENTS),
    'definite PVE delete failure aborts and never commits',
);

$PVE::QemuConfig::CONF = pvn_snapshot_config('vmbr0');
$PVE::QemuConfig::SNAPSHOT_FAIL = '';
@PVE::QemuConfig::SNAPSHOT_EVENTS = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub { die "unexpected manager request\n" };
    PVE::API2::Qemu::snapshot_delete_fixture(401, 'daily', { force => 0 }, 'root@pam');
}
is_deeply(
    \@PVE::QemuConfig::SNAPSHOT_EVENTS,
    ['pve:delete'],
    'ordinary non-PVN snapshot deletion bypasses the manager',
);

$PVE::QemuConfig::CONF = pvn_snapshot_config('br-int');
@PVE::QemuConfig::SNAPSHOT_EVENTS = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = snapshot_manager(create_fail => 1);
    my $ok = eval {
        PVE::API2::Qemu::snapshot_create_fixture(400, 'daily', {}, undef);
        1;
    };
    ok(!$ok, 'failed post-create callback rolls back the PVE snapshot and reports failure');
}
is_deeply(
    \@PVE::QemuConfig::SNAPSHOT_EVENTS,
    [
        'pve:create',
        ('manager:/api/v1/runtime/compute/snapshot/create') x 3,
        'pve:delete',
        'manager:/api/v1/runtime/compute/snapshot/cleanup',
    ],
    'post-create callback loss cleans the manifest only after definite physical deletion',
);

$PVE::QemuConfig::SNAPSHOT_FAIL = 'delete';
@PVE::QemuConfig::SNAPSHOT_EVENTS = ();
{
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = snapshot_manager(create_fail => 1);
    my $ok = eval {
        PVE::API2::Qemu::snapshot_create_fixture(400, 'daily', {}, undef);
        1;
    };
    ok(!$ok, 'ambiguous physical rollback-delete preserves the failed create');
}
ok(
    !grep(m{/snapshot/cleanup$}, @PVE::QemuConfig::SNAPSHOT_EVENTS),
    'failed physical rollback-delete never cleans the durable manifest',
);
$PVE::QemuConfig::SNAPSHOT_FAIL = '';

sub destroy_case {
    my ($stage, $exists, $probe_fail) = @_;
    $PVE::QemuServer::FAIL_STAGE = $stage;
    $PVE::QemuConfig::EXISTS = $exists;
    $PVE::QemuConfig::PROBE_FAIL = $probe_fail;
    $PVE::QemuConfig::CONF = { net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=br-int' };
    my @paths;
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        push @paths, $path;
        return {
            lifecycle_id => ($payload->{lifecycle_id} // 'destroy-id'), vmid => 300,
            node => 'prox1', operation_id => 'destroy-operation', payload_hash => 'destroy-payload',
            ports => [],
        } if $path =~ m{/destroy/capture$};
        return { state => 'finished' };
    };
    my $ok = eval { PVE::API2::Qemu::destroy_fixture(undef, 300, undef, undef); 1 };
    return ($ok, \@paths, $@);
}

for my $stage (qw(destroy_vm access firewall)) {
    my ($ok, $paths) = destroy_case($stage, 1, 0);
    ok(!$ok, "$stage failure remains a PVE error");
    ok(grep(m{/destroy/abort$}, @$paths), "$stage failure with config present restores identity");
    ok(!grep(m{/destroy/commit$}, @$paths), "$stage failure does not commit deletion");
}
my ($removed_ok, $removed_paths) = destroy_case('destroy_config', 1, 0);
ok(!$removed_ok, 'post-removal destroy failure remains a PVE error');
ok(grep(m{/destroy/commit$}, @$removed_paths), 'absent config commits detached identity cleanup');
ok(!grep(m{/destroy/abort$}, @$removed_paths), 'absent config never reattaches identity');

my ($probe_ok, $probe_paths) = destroy_case('access', 1, 1);
ok(!$probe_ok, 'ambiguous config probe remains a PVE error');
ok(!grep(m{/destroy/(?:abort|commit)$}, @$probe_paths), 'ambiguous config probe preserves durable detached intent');

my ($success_ok, $success_paths) = destroy_case('', 1, 0);
ok($success_ok, 'successful full PVE destroy completes');
ok(grep(m{/destroy/commit$}, @$success_paths), 'successful destroy commits only after config removal');

done_testing();
''',
        encoding="utf-8",
    )
    environment = dict(os.environ)
    environment.update(
        {
            "PVN_API2_FIXTURE": str(root / "API2-Qemu.pm"),
            "PVN_FIXTURE_ROOT": str(root),
            "PERL5LIB": str(root),
        }
    )
    result = subprocess.run(
        ["perl", str(harness)], env=environment, text=True, capture_output=True
    )
    check(result.returncode == 0, f"runtime lifecycle fixture failed:\n{result.stdout}{result.stderr}")


def invoke(
    root: pathlib.Path,
    action: str,
    *,
    version: str = "9.1.15",
    test_mode: bool = True,
    crash_after: int = 0,
) -> subprocess.CompletedProcess[str]:
    environment = dict(os.environ)
    environment.update(
        {
            "PVN_QEMU_SERVER_VERSION": version,
            "PVN_COMPUTE_SKIP_PERL_CHECK": "1",
        }
    )
    if test_mode:
        environment.update(
            {
                "PVN_COMPUTE_TEST_MODE": "1",
                "PVN_COMPUTE_TEST_ROOT": str(root),
            }
        )
    else:
        environment.pop("PVN_COMPUTE_TEST_MODE", None)
        environment.pop("PVN_COMPUTE_TEST_ROOT", None)
    if crash_after:
        environment["PVN_COMPUTE_TEST_CRASH_AFTER"] = str(crash_after)
    return subprocess.run(
        [
            str(INJECTOR),
            action,
            str(root / "QemuServer.pm"),
            str(root / "QemuMigrate.pm"),
            str(root / "API2-Qemu.pm"),
            str(root / "PVN" / "ComputeLifecycle.pm"),
        ],
        env=environment,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


roots: list[pathlib.Path] = []
try:
    root = fixture_tree()
    roots.append(root)
    pristine = contents(root)
    result = invoke(root, "install")
    check(result.returncode == 0, f"fixture install failed: {result.stderr}")
    installed = contents(root)
    check(installed != pristine, "install did not change fixtures")

    qemu_server = installed["QemuServer.pm"].decode()
    qemu_migrate = installed["QemuMigrate.pm"].decode()
    api2 = installed["API2-Qemu.pm"].decode()
    check(
        qemu_server.index("PVN::ComputeLifecycle::pre_start")
        < qemu_server.index("PVE::GuestHelpers::exec_hookscript"),
        "PVN pre-start is not before the user hookscript",
    )
    check(
        qemu_migrate.index("PVN::ComputeLifecycle::migration_begin")
        < qemu_migrate.index("# set migrate lock"),
        "migration intent is not established before PVE migration mutation",
    )
    check(
        qemu_migrate.index("# clear migrate lock")
        < qemu_migrate.index("PVN::ComputeLifecycle::migration_finalize"),
        "migration finalization runs before source cleanup/unlock",
    )
    check(
        qemu_migrate.index("Failed to remove source VM")
        < qemu_migrate.index("PVN::ComputeLifecycle::migration_finalize"),
        "migration finalization runs before optional source deletion",
    )
    finalize = qemu_migrate[qemu_migrate.index("PVN migration finalize failed") :]
    check(
        "$self->{errors} = 1" in finalize and "delete $self->{pvn_compute_lifecycle}" in finalize,
        "migration finalization does not preserve failed durable state",
    )
    check(
        api2.index("PVN::ComputeLifecycle::clone_abort")
        < api2.index("qemu_blockjobs_cancel"),
        "PVE clone failure does not abort its exact PVN transaction first",
    )
    check(
        api2.index('die "clone failed: $err"')
        < api2.index("PVN::ComputeLifecycle::clone_commit"),
        "clone commit remains inside the destructive PVE cleanup eval",
    )
    check(
        api2.index("PVN::ComputeLifecycle::clone_commit")
        < api2.index("PVN::ComputeLifecycle::clone_activate_config")
        < api2.index("PVE::Cluster::cfs_write_file", api2.index("PVN::ComputeLifecycle::clone_activate_config")),
        "clone NIC is not enabled strictly after a definite manager commit",
    )
    clone_commit = api2[api2.index("PVN::ComputeLifecycle::clone_commit") : api2.index("sub snapshot_create_fixture")]
    check(
        "PVE::QemuConfig->load_config($newid, $pvn_clone_target_node)" in clone_commit
        and "PVE::QemuConfig->cfs_config_path($newid, $pvn_clone_target_node)" in clone_commit
        and "remote clone left a duplicate source-node config" in clone_commit
        and "PVE::QemuConfig->write_config($newid, $newconf)" not in clone_commit,
        "remote clone activation can recreate a duplicate source-node config",
    )
    check(
        api2.index("PVE::QemuConfig->snapshot_create")
        < api2.index("PVN::ComputeLifecycle::snapshot_create"),
        "snapshot manifest is persisted before PVE snapshot success",
    )
    check(
        "rollback-delete of snapshot" in api2,
        "snapshot manifest failure lacks exact PVE snapshot rollback-delete",
    )
    create_failure = api2[api2.index("if (my $pvn_err = $@)") : api2.index("sub snapshot_rollback_fixture")]
    check(
        create_failure.index("if $delete_error")
        < create_failure.index("PVN::ComputeLifecycle::snapshot_cleanup"),
        "snapshot manifest is deleted even when PVE rollback-delete is ambiguous",
    )
    snapshot_rollback = api2[
        api2.index("sub snapshot_rollback_fixture") : api2.index("sub snapshot_delete_fixture")
    ]
    check(
        snapshot_rollback.index("PVN::ComputeLifecycle::snapshot_prepare")
        < snapshot_rollback.index("PVE::QemuConfig->snapshot_rollback")
        < snapshot_rollback.index("PVN::ComputeLifecycle::snapshot_abort")
        < snapshot_rollback.index("PVN::ComputeLifecycle::snapshot_commit"),
        "snapshot rollback does not hold and finish one exact manager transaction",
    )
    snapshot_delete = api2[
        api2.index("sub snapshot_delete_fixture") : api2.index("sub template_fixture")
    ]
    check(
        snapshot_delete.index("PVN::ComputeLifecycle::snapshot_prepare")
        < snapshot_delete.index("PVE::QemuConfig->snapshot_delete")
        < snapshot_delete.index("PVN::ComputeLifecycle::snapshot_abort")
        < snapshot_delete.index("PVN::ComputeLifecycle::snapshot_commit"),
        "snapshot delete does not hold and finish one exact manager transaction",
    )
    check(
        "link_down" not in api2,
        "injector hard-codes cloned NIC data instead of using the manager response",
    )
    template_failure = api2[api2.index("PVN template abort failed") - 500 : api2.index("sub destroy_fixture")]
    check(
        "!$pvn_after->{template}" in template_failure
        and "PVN::ComputeLifecycle::template_commit" in template_failure,
        "partial template conversion can incorrectly restore active port identity",
    )
    check(
        api2.index("PVE::QemuConfig->destroy_config")
        < api2.index("PVN::ComputeLifecycle::destroy_commit"),
        "destroy commit runs before PVE worker cleanup completes",
    )
    destroy_scope = api2[api2.index("PVN::ComputeLifecycle::destroy_capture") :]
    check(
        destroy_scope.index("PVE::QemuServer::destroy_vm")
        < destroy_scope.index("PVE::AccessControl::remove_vm_access")
        < destroy_scope.index("PVE::Firewall::remove_vmfw_conf")
        < destroy_scope.index("PVE::QemuConfig->destroy_config")
        < destroy_scope.index("if (my $pvn_destroy_error = $@)"),
        "destroy transaction does not cover the full PVE cleanup window",
    )
    check(
        "PVE::Cluster::cfs_read_file($pvn_destroy_conf_path)" in destroy_scope
        and "if ($pvn_destroy_config_exists)" in destroy_scope
        and "destroy_abort($pvn_destroy_lifecycle)" in destroy_scope
        and "preserving detached intent" in destroy_scope,
        "destroy failure does not branch safely on exact config existence",
    )
    runtime_fixture_test(root)

    second = invoke(root, "install")
    check(second.returncode == 0, f"idempotent install failed: {second.stderr}")
    check(contents(root) == installed, "second install was not byte-idempotent")
    verified = invoke(root, "verify")
    check(verified.returncode == 0, f"installed hook verification failed: {verified.stderr}")
    removed = invoke(root, "remove")
    check(removed.returncode == 0, f"remove failed: {removed.stderr}")
    check(contents(root) == pristine, "remove did not restore pristine files byte-for-byte")

    missing_module = fixture_tree()
    roots.append(missing_module)
    missing_module_before = contents(missing_module)
    installed_without_module = invoke(missing_module, "install")
    check(
        installed_without_module.returncode == 0,
        f"missing-module setup install failed: {installed_without_module.stderr}",
    )
    (missing_module / "PVN" / "ComputeLifecycle.pm").unlink()
    removed_without_module = invoke(missing_module, "remove")
    check(
        removed_without_module.returncode == 0,
        f"remove depended on an already-unpacked lifecycle module: {removed_without_module.stderr}",
    )
    check(
        contents(missing_module) == missing_module_before,
        "remove without lifecycle module did not restore pristine PVE files",
    )

    interrupted = fixture_tree()
    roots.append(interrupted)
    interrupted_before = contents(interrupted)
    result = invoke(interrupted, "install", crash_after=1)
    check(result.returncode == 99, "simulated injector crash did not stop after one replacement")
    check(contents(interrupted) != interrupted_before, "simulated crash did not leave a partial write to recover")
    check((interrupted / ".pvn-compute-transaction").is_dir(), "partial transaction has no durable journal")
    interrupted_partial = contents(interrupted)
    unsupported_recovery = invoke(interrupted, "install", version="9.1.16")
    check(unsupported_recovery.returncode != 0, "unsupported upgrade recovered a pending journal")
    check(contents(interrupted) == interrupted_partial, "unsupported version mutated files during journal preflight")
    check((interrupted / ".pvn-compute-transaction").is_dir(), "unsupported preflight erased recovery evidence")
    recovered = invoke(interrupted, "remove")
    check(recovered.returncode == 0, f"journal recovery failed: {recovered.stderr}")
    check(contents(interrupted) == interrupted_before, "journal recovery did not restore all three originals")
    check(not (interrupted / ".pvn-compute-transaction").exists(), "completed recovery left an active journal")

    concurrent = fixture_tree()
    roots.append(concurrent)
    concurrent_env = dict(os.environ)
    concurrent_env.update(
        {
            "PVN_QEMU_SERVER_VERSION": "9.1.15",
            "PVN_COMPUTE_SKIP_PERL_CHECK": "1",
            "PVN_COMPUTE_TEST_MODE": "1",
            "PVN_COMPUTE_TEST_ROOT": str(concurrent),
            "PVN_COMPUTE_TEST_HOLD_LOCK_MS": "500",
        }
    )
    concurrent_command = [
        str(INJECTOR),
        "install",
        str(concurrent / "QemuServer.pm"),
        str(concurrent / "QemuMigrate.pm"),
        str(concurrent / "API2-Qemu.pm"),
        str(concurrent / "PVN" / "ComputeLifecycle.pm"),
    ]
    first = subprocess.Popen(
        concurrent_command,
        env=concurrent_env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    time.sleep(0.1)
    second_env = dict(concurrent_env)
    second_env.pop("PVN_COMPUTE_TEST_HOLD_LOCK_MS")
    second = subprocess.run(
        concurrent_command,
        env=second_env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    first_stdout, first_stderr = first.communicate(timeout=10)
    check(first.returncode == 0, f"first concurrent injector failed: {first_stdout}{first_stderr}")
    check(second.returncode == 0, f"second concurrent injector failed: {second.stderr}")
    check(not (concurrent / ".pvn-compute-transaction").exists(), "concurrent install left a journal")
    check(invoke(concurrent, "verify").returncode == 0, "concurrent install corrupted hook set")

    unsupported = fixture_tree()
    roots.append(unsupported)
    unsupported_before = contents(unsupported)
    result = invoke(unsupported, "install", version="9.1.16")
    check(result.returncode != 0, "unsupported qemu-server version was accepted")
    check(contents(unsupported) == unsupported_before, "version failure partially modified PVE files")
    check("no PVE file was changed" in result.stderr, "version failure does not state atomic refusal")

    unknown_signature = fixture_tree()
    roots.append(unknown_signature)
    signature_before = contents(unknown_signature)
    result = invoke(unknown_signature, "install", test_mode=False)
    check(result.returncode != 0, "unknown production signature was accepted")
    check(contents(unknown_signature) == signature_before, "signature failure partially modified files")
    check("unknown qemu_server signature" in result.stderr, "signature failure is not explicit")

    unknown_anchor = fixture_tree()
    roots.append(unknown_anchor)
    path = unknown_anchor / "QemuServer.pm"
    path.write_text(path.read_text().replace("    PVE::GuestHelpers::exec_hookscript($conf, $vmid, 'pre-start', 1);\n", ""))
    anchor_before = contents(unknown_anchor)
    result = invoke(unknown_anchor, "install")
    check(result.returncode != 0, "unknown hook anchor was accepted")
    check(contents(unknown_anchor) == anchor_before, "anchor failure partially modified files")

    malformed = fixture_tree()
    roots.append(malformed)
    path = malformed / "QemuMigrate.pm"
    path.write_bytes(b"# PVN-COMPUTE:BEGIN broken\n" + path.read_bytes())
    malformed_before = contents(malformed)
    result = invoke(malformed, "install")
    check(result.returncode != 0, "malformed lifecycle marker was accepted")
    check(contents(malformed) == malformed_before, "marker failure partially modified files")

    source = INJECTOR.read_text()
    check(
        'SUPPORTED_QEMU_SERVER_VERSION = "9.1.15"' in source,
        "injector is not pinned to exact qemu-server 9.1.15",
    )
    expected_hashes = {
        "15251482cdca5fe006f6cdba53b2eb2cec8fc3fd776efbbc601a5ef46b171bf4",
        "f53ba901cb219276952eb389b957dbf8a0cdc7d31cecfdb53d0607f37f2e6c43",
        "0b2f0afea3655a016745185dc1a924f34abcaebebd4a12b8f55057d3ec3ab8a6",
    }
    check(expected_hashes <= set(re.findall(r'[0-9a-f]{64}', source)), "exact live PVE hashes changed")

    print("pvn compute injector tests passed")
finally:
    for root in roots:
        shutil.rmtree(root, ignore_errors=True)
