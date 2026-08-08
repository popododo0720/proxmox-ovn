#!/usr/bin/perl

use strict;
use warnings;
no warnings 'once';

use FindBin qw($Bin);
use Digest::SHA qw(sha256_hex);
use POSIX qw(strftime);
use Test::More;
use Time::HiRes ();

use lib "$Bin/..";
require PVN::ComputeLifecycle;

my $now = strftime('%Y-%m-%dT%H:%M:%SZ', gmtime(time()));
my $healthy = {
    last_success => $now,
    report => { errors => 0 },
};
my $pvn_conf = {
    net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=br-int,firewall=1',
    net1 => 'e1000=AA:BB:CC:DD:EE:02,bridge=vmbr0',
};

sub with_defaults {
    my ($code) = @_;
    local $PVN::ComputeLifecycle::NODE_OVERRIDE = sub { 'prox1' };
    local $PVN::ComputeLifecycle::UNIT_CHECK_OVERRIDE = sub { 1 };
    local $PVN::ComputeLifecycle::AGENT_HEALTH_OVERRIDE = sub { $healthy };
    local $PVN::ComputeLifecycle::HA_MANAGED_OVERRIDE = sub { 0 };
    local $PVN::ComputeLifecycle::HA_RUNTIME_OVERRIDE = sub {
        return { origin => 'cli', user => 'root@pam' };
    };
    local $PVN::ComputeLifecycle::LOCAL_MTU_OVERRIDE = sub { 1500 };
    local $PVN::ComputeLifecycle::REMOTE_MTU_OVERRIDE = sub { 1500 };
    local $PVN::ComputeLifecycle::LIFECYCLE_ID_OVERRIDE =
        sub { return 'test-' . join('-', @_); };
    return $code->();
}

my @timeout_cases = (
    {
        name => 'start',
        payload => { nics => [map { +{} } 1 .. 32] },
        expected => 970,
    },
    {
        name => 'clone_prepare',
        payload => { nics => [{}] },
        expected => 120,
    },
    {
        name => 'snapshot_cleanup',
        payload => {},
        expected => 90,
    },
    {
        name => 'destroy_capture',
        payload => {
            nics => [{}, {}],
            snapshots => [{}, {}, {}],
        },
        expected => 240,
    },
    {
        name => 'migration_finalize',
        payload => { transaction => { ports => [{}, {}, {}, {}] } },
        expected => 210,
    },
    {
        name => 'clone_commit',
        payload => { ports => [map { +{} } 1 .. 100] },
        expected => 1800,
    },
);
my @effective_timeouts;
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload, $request_timeout) = @_;
        push @effective_timeouts, $request_timeout;
        return {};
    };
    for my $case (@timeout_cases) {
        PVN::ComputeLifecycle::_request($case->{name}, $case->{payload});
    }
});
is_deeply(
    \@effective_timeouts,
    [map { $_->{expected} } @timeout_cases],
    'request transport receives the per-action and per-item effective timeout',
);
my @lifecycle_actions = qw(
    clone_prepare clone_commit clone_abort
    migration_begin migration_finalize migration_abort
    template_prepare template_commit template_abort
    snapshot_create snapshot_prepare snapshot_commit snapshot_abort snapshot_cleanup
    destroy_capture destroy_commit destroy_abort
);
is_deeply(
    [map { PVN::ComputeLifecycle::_request_timeout($_, {}) } @lifecycle_actions],
    [(90) x scalar(@lifecycle_actions)],
    'every lifecycle mutation has an explicit long-timeout class',
);
{
    my $attempts = 0;
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        $attempts++;
        die $attempts == 1
            ? "initial transport response lost\n"
            : "terminal replay rejected\n";
    };
    my $completed = eval {
        PVN::ComputeLifecycle::_request_bounded_retry('clone_prepare', { nics => [{}] }, 3);
        1;
    };
    ok(!$completed, 'terminal replay failure remains an ambiguous lifecycle error');
    like($@, qr/initial transport response lost/, 'bounded retry preserves the first response-loss error');
}

my $unmanaged_checks = 0;
with_defaults(sub {
    local $PVN::ComputeLifecycle::UNIT_CHECK_OVERRIDE = sub { $unmanaged_checks++; return 1 };
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub { die 'unexpected request' };
    PVN::ComputeLifecycle::pre_start(
        100,
        { net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=vmbr0' },
        undef,
    );
});
is($unmanaged_checks, 0, 'non-br-int VM bypasses PVN readiness and manager calls');

with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub { die 'unexpected request' };
    my $transaction = PVN::ComputeLifecycle::destroy_capture(
        901,
        { template => 1, net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=vmbr0' },
    );
    ok(!defined($transaction), 'ordinary non-PVN template destroy bypasses compute lifecycle');
});

with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub { die 'unexpected request' };
    my $accepted = eval {
        PVN::ComputeLifecycle::pre_start(
            100,
            { net0 => 'virtio=AA:BB:CC:DD:EE:01,macaddr=AA:BB:CC:DD:EE:09,bridge=br-int' },
            undef,
        );
        1;
    };
    ok(!$accepted, 'conflicting effective MAC fields fail closed');
    like($@, qr/conflicting MAC address fields/, 'ambiguous MAC reason is explicit');
});
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub { die 'unexpected request' };
    my $accepted = eval {
        PVN::ComputeLifecycle::pre_start(
            100,
            { net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=br-int,bridge=br-int' },
            undef,
        );
        1;
    };
    ok(!$accepted, 'malformed br-int NIC cannot bypass lifecycle checks');
    like($@, qr/invalid PVE network configuration/, 'malformed PVN NIC reason is explicit');
});

my @requests;
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        push @requests, [$path, $payload];
        return { ready => 1 };
    };
    PVN::ComputeLifecycle::pre_start(100, $pvn_conf, undef);
});
is($requests[0]->[0], '/api/v1/runtime/compute/start', 'pre-start uses privileged start endpoint');
is_deeply(
    $requests[0]->[1]->{nics},
    [{ nic => 'net0', mac_address => 'aa:bb:cc:dd:ee:01' }],
    'pre-start sends only exact br-int NIC identities',
);
ok(!$requests[0]->[1]->{ha_managed}, 'manual start is not HA managed');
ok(!exists($requests[0]->[1]->{lifecycle_id}), 'manual start has no rebind transaction ID');
ok(!exists($requests[0]->[1]->{migration_source}), 'manual start has no migration source');

@requests = ();
with_defaults(sub {
    local $PVN::ComputeLifecycle::HA_MANAGED_OVERRIDE = sub { 1 };
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        push @requests, [@_];
        return { ready => 1 };
    };
    PVN::ComputeLifecycle::pre_start(101, $pvn_conf, undef);
});
ok($requests[0]->[1]->{ha_managed}, 'configured HA VM proves HA-managed start intent');
like($requests[0]->[1]->{lifecycle_id}, qr/^test-ha-start-101$/, 'HA start has a client lifecycle ID');
ok(!exists($requests[0]->[1]->{ha_proof}), 'manual HA-configured start cannot claim HA authority');

sub ha_runtime_fixture {
    my ($vmid, $epoch) = @_;
    return {
        origin => 'ha',
        user => 'root@pam',
        quorate => 1,
        manager_status => {
            timestamp => $epoch,
            service_status => {
                "vm:$vmid" => {
                    state => 'started',
                    node => 'prox1',
                    uid => 'HAuid1234567890+/',
                },
            },
            node_status => {
                prox1 => 'online',
                prox2 => 'unknown',
            },
        },
        lrm_status => {
            timestamp => $epoch,
            state => 'active',
            mode => 'active',
            results => {},
        },
        agent_lock => {
            mode => 0040700,
            mtime => $epoch,
        },
    };
}

@requests = ();
my $ha_epoch = time();
with_defaults(sub {
    local $PVN::ComputeLifecycle::HA_MANAGED_OVERRIDE = sub { 1 };
    local $PVN::ComputeLifecycle::HA_RUNTIME_OVERRIDE = sub {
        return ha_runtime_fixture(103, $ha_epoch);
    };
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        push @requests, [@_];
        return { ready => 1 };
    };
    PVN::ComputeLifecycle::pre_start(103, $pvn_conf, undef);
    PVN::ComputeLifecycle::pre_start(103, $pvn_conf, undef);
});
my $ha_lifecycle_id = 'pve-ha-' . sha256_hex(
    join("\0", 103, 'prox1', 'HAuid1234567890+/'),
);
is($requests[0]->[1]->{lifecycle_id}, $ha_lifecycle_id, 'HA lifecycle ID uses the canonical full SHA-256');
is($requests[1]->[1]->{lifecycle_id}, $ha_lifecycle_id, 'same HA service generation replays one lifecycle ID');
is_deeply(
    $requests[0]->[1]->{ha_proof},
    {
        origin => 'ha',
        service_id => 'vm:103',
        manager_epoch => int($ha_epoch),
        service_uid => 'HAuid1234567890+/',
        service_node => 'prox1',
        service_state => 'started',
        node_states => { prox1 => 'online', prox2 => 'unknown' },
        lrm_node => 'prox1',
        lrm_epoch => int($ha_epoch),
        lrm_state => 'active',
        lrm_mode => 'active',
        agent_lock_epoch => int($ha_epoch),
    },
    'HA start sends only the exact strict manager/LRM/agent-lock proof schema',
);

for my $invalid (
    {
        label => 'unknown RPC origin',
        mutate => sub { $_[0]->{origin} = 'unknown-worker' },
        error => qr/runtime origin is invalid/,
    },
    {
        label => 'non-root HA worker',
        mutate => sub { $_[0]->{user} = 'operator@pam' },
        error => qr/worker identity is invalid/,
    },
    {
        label => 'non-quorate HA worker',
        mutate => sub { $_[0]->{quorate} = 0 },
        error => qr/worker identity is invalid/,
    },
    {
        label => 'stale manager status',
        mutate => sub { $_[0]->{manager_status}->{timestamp} -= 31 },
        error => qr/manager timestamp is stale/,
    },
    {
        label => 'wrong CRM service target',
        mutate => sub { $_[0]->{manager_status}->{service_status}->{'vm:103'}->{node} = 'prox2' },
        error => qr/does not assign this VM to the local node/,
    },
    {
        label => 'offline local CRM node',
        mutate => sub { $_[0]->{manager_status}->{node_status}->{prox1} = 'unknown' },
        error => qr/does not mark the local node online/,
    },
    {
        label => 'inactive local LRM',
        mutate => sub { $_[0]->{lrm_status}->{state} = 'wait_for_agent_lock' },
        error => qr/LRM is not the active local start authority/,
    },
    {
        label => 'stale HA agent lock',
        mutate => sub { $_[0]->{agent_lock}->{mtime} -= 31 },
        error => qr/agent lock timestamp is stale/,
    },
    {
        label => 'non-directory HA agent lock',
        mutate => sub { $_[0]->{agent_lock}->{mode} = 0100600 },
        error => qr/agent lock metadata is invalid/,
    },
) {
    with_defaults(sub {
        my $runtime = ha_runtime_fixture(103, time());
        $invalid->{mutate}->($runtime);
        local $PVN::ComputeLifecycle::HA_MANAGED_OVERRIDE = sub { 1 };
        local $PVN::ComputeLifecycle::HA_RUNTIME_OVERRIDE = sub { return $runtime };
        my $manager_called = 0;
        local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
            $manager_called++;
            return { ready => 1 };
        };
        my $accepted = eval {
            PVN::ComputeLifecycle::pre_start(103, $pvn_conf, undef);
            1;
        };
        ok(!$accepted, "$invalid->{label} fails HA start closed");
        like($@, $invalid->{error}, "$invalid->{label} has an explicit diagnostic");
        is($manager_called, 0, "$invalid->{label} never reaches the manager");
    });
}

@requests = ();
with_defaults(sub {
    local $PVN::ComputeLifecycle::HA_MANAGED_OVERRIDE = sub { 1 };
    local $PVN::ComputeLifecycle::HA_RUNTIME_OVERRIDE = sub {
        die "incoming migration must not claim an HA start proof\n";
    };
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        push @requests, [@_];
        return { ready => 1 };
    };
    PVN::ComputeLifecycle::pre_start(102, $pvn_conf, 'prox2');
});
is($requests[0]->[1]->{migration_source}, 'prox2', 'incoming migration identifies its source');
ok(!exists($requests[0]->[1]->{lifecycle_id}), 'incoming migration resolves its existing begin intent');
ok(!$requests[0]->[1]->{ha_managed}, 'incoming migration uses its migration intent rather than HA recovery');
ok(!exists($requests[0]->[1]->{ha_proof}), 'incoming migration never supplies an unrelated HA proof');

with_defaults(sub {
    local $PVN::ComputeLifecycle::AGENT_HEALTH_OVERRIDE = sub {
        return { last_success => $now, report => { errors => 1 } };
    };
    my $accepted = eval { PVN::ComputeLifecycle::assert_node_ready(); 1 };
    ok(!$accepted, 'agent scan errors fail readiness closed');
    like($@, qr/binding scan contains errors/, 'agent error reason is explicit');
});
with_defaults(sub {
    local $PVN::ComputeLifecycle::AGENT_HEALTH_OVERRIDE = sub {
        return { last_success => $now, report => { errors => 'not-a-count' } };
    };
    my $accepted = eval { PVN::ComputeLifecycle::assert_node_ready(); 1 };
    ok(!$accepted, 'non-numeric agent error count fails readiness closed');
    like($@, qr/binding scan contains errors/, 'malformed error count is diagnosed');
});
with_defaults(sub {
    local $PVN::ComputeLifecycle::AGENT_HEALTH_OVERRIDE = sub {
        return { last_success => '2999-01-01T00:00:00Z', report => { errors => 0 } };
    };
    my $accepted = eval { PVN::ComputeLifecycle::assert_node_ready(); 1 };
    ok(!$accepted, 'impossible future health timestamp fails closed');
});

my $clone_transaction;
my $clone_prepare_attempts = 0;
my $target_conf = {
    net0 => 'virtio=02:00:00:00:00:01,macaddr=02:00:00:00:00:01,bridge=br-int,firewall=1',
    net1 => $pvn_conf->{net1},
};
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        is($path, '/api/v1/runtime/compute/clone/prepare', 'clone prepare endpoint is fixed');
        is($payload->{source_vmid}, 100, 'clone carries source VMID');
        is($payload->{source_node}, 'prox1', 'clone pins its local source node');
        ok(!$payload->{source_template}, 'clone proves the source is not a template');
        is($payload->{target_vmid}, 200, 'clone carries target VMID');
        $clone_prepare_attempts++;
        die "prepare response lost\n" if $clone_prepare_attempts < 3;
        return {
            clone_id => $payload->{clone_id},
            source_vmid => 100,
            target_vmid => 200,
            source_node => 'prox1',
            target_node => 'prox2',
            operation_id => 'clone-operation',
            payload_hash => 'clone-payload',
            ports => [{
                nic => 'net0',
                mac_address => '02:aa:bb:cc:dd:ee',
                port_id => 'clone-port',
                ownership_digest => 'digest',
            }],
        };
    };
    $clone_transaction = PVN::ComputeLifecycle::clone_prepare(
        100, 200, 'prox2', $pvn_conf, $target_conf, undef,
    );
});
is($clone_prepare_attempts, 3, 'clone prepare replays one client-generated lifecycle ID');
is(
    $target_conf->{net0},
    'virtio=02:AA:BB:CC:DD:EE,macaddr=02:AA:BB:CC:DD:EE,bridge=br-int,firewall=1,link_down=1',
    'clone response rewrites every effective target MAC and forces link_down',
);
is($target_conf->{net1}, $pvn_conf->{net1}, 'clone leaves non-PVN NIC untouched');

my $commit_attempts = 0;
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        is_deeply($payload, $clone_transaction, 'clone commit echoes exact prepare transaction');
        $commit_attempts++;
        die "ambiguous response\n" if $commit_attempts < 3;
        return { state => 'committed' };
    };
    PVN::ComputeLifecycle::clone_commit($clone_transaction);
    PVN::ComputeLifecycle::clone_activate_config($clone_transaction, $target_conf);
});
is($commit_attempts, 3, 'clone commit has bounded idempotent retries');
is(
    $target_conf->{net0},
    'virtio=02:AA:BB:CC:DD:EE,macaddr=02:AA:BB:CC:DD:EE,bridge=br-int,firewall=1,link_down=0',
    'definite clone commit activates the cloned NIC',
);
my $ambiguous_conf = {
    net0 => 'virtio=02:AA:BB:CC:DD:EE,bridge=br-int,firewall=1,link_down=1',
};
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub { die "commit response unavailable\n" };
    my $committed = eval { PVN::ComputeLifecycle::clone_commit($clone_transaction); 1 };
    PVN::ComputeLifecycle::clone_activate_config($clone_transaction, $ambiguous_conf)
        if $committed;
    ok(!$committed, 'ambiguous clone commit remains an error after bounded retries');
});
like($ambiguous_conf->{net0}, qr/link_down=1/, 'ambiguous clone commit keeps NIC disconnected');

for my $race (
    {
        label => 'conflicting clone MAC fields',
        conf => { net0 => 'virtio=02:00:00:00:00:09,macaddr=02:AA:BB:CC:DD:EE,bridge=br-int,link_down=1' },
        error => qr/conflicting MAC address fields/,
    },
    {
        label => 'extra cloned PVN NIC',
        conf => {
            net0 => 'virtio=02:AA:BB:CC:DD:EE,bridge=br-int,link_down=1',
            net2 => 'virtio=02:AA:BB:CC:DD:EF,bridge=br-int,link_down=1',
        },
        error => qr/PVN NIC set changed/,
    },
) {
    my $activated = eval {
        PVN::ComputeLifecycle::clone_activate_config($clone_transaction, $race->{conf});
        1;
    };
    ok(!$activated, "$race->{label} blocks clone activation");
    like($@, $race->{error}, "$race->{label} is diagnosed");
}

my $snapshot_clone_conf = {
    net0 => 'virtio=02:00:00:00:00:09,bridge=br-int',
};
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        is($payload->{snapshot_epoch}, 1700000001, 'snapshot clone pins immutable snaptime');
        is_deeply(
            $payload->{nics},
            [{ nic => 'net0', mac_address => 'aa:bb:cc:dd:ee:01' }],
            'snapshot clone proves the immutable selected snapshot NIC identity',
        );
        return {
            clone_id => $payload->{clone_id}, source_vmid => 100, target_vmid => 209,
            source_node => 'prox1', target_node => 'prox2', snapshot_id => 'older',
            snapshot_epoch => 1700000001,
            ports => [{
                nic => 'net0', mac_address => '02:aa:bb:cc:dd:09',
                port_id => 'snapshot-clone-port', ownership_digest => 'snapshot-clone-owner',
            }],
        };
    };
    PVN::ComputeLifecycle::clone_prepare(
        100, 209, 'prox2',
        { net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=br-int', snaptime => 1700000001 },
        $snapshot_clone_conf, 'older',
    );
});
like($snapshot_clone_conf->{net0}, qr/02:AA:BB:CC:DD:09/, 'snapshot clone applies manager target MAC');

my @clone_rewrite_failure_paths;
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        push @clone_rewrite_failure_paths, $path;
        if ($path =~ m{/clone/prepare$}) {
            return {
                clone_id => $payload->{clone_id}, source_vmid => 100, target_vmid => 210,
                source_node => 'prox1', target_node => 'prox2',
                operation_id => 'rewrite-failure', payload_hash => 'rewrite-failure-payload',
                ports => [{
                    nic => 'net0', mac_address => '02:aa:bb:cc:dd:10',
                    port_id => 'rewrite-failure-port', ownership_digest => 'rewrite-failure-owner',
                }],
            };
        }
        return { state => 'aborted' } if $path =~ m{/clone/abort$};
        die "unexpected endpoint $path\n";
    };
    my $prepared = eval {
        PVN::ComputeLifecycle::clone_prepare(
            100, 210, 'prox2', $pvn_conf,
            {
                net0 => 'virtio=02:00:00:00:00:10,bridge=br-int',
                net2 => 'virtio=02:00:00:00:00:11,bridge=br-int',
            },
            undef,
        );
        1;
    };
    ok(!$prepared, 'invalid PVE target NIC set fails clone preparation');
});
is_deeply(
    \@clone_rewrite_failure_paths,
    ['/api/v1/runtime/compute/clone/prepare', '/api/v1/runtime/compute/clone/abort'],
    'post-prepare rewrite failure aborts the exact durable clone transaction internally',
);

@requests = ();
my ($snapshot_rollback_transaction, $snapshot_delete_transaction);
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        push @requests, [$path, $payload];
        if ($path =~ m{/snapshot/prepare$}) {
            return {
                lifecycle_id => $payload->{lifecycle_id},
                action => $payload->{action},
                vmid => $payload->{vmid},
                snapshot_id => $payload->{snapshot_id},
                snapshot_epoch => $payload->{snapshot_epoch},
                operation_id => "snapshot-$payload->{action}-operation",
                payload_hash => "snapshot-$payload->{action}-payload",
                ports => [{ port_id => 'snapshot-port', nic => 'net0' }],
            };
        }
        return { state => 'ok' };
    };
    my $template = PVN::ComputeLifecycle::template_prepare(100, $pvn_conf);
    PVN::ComputeLifecycle::template_commit($template);
    my $snapshot_conf = { %$pvn_conf, snaptime => 1700000002 };
    PVN::ComputeLifecycle::snapshot_create(100, 'before-upgrade', $snapshot_conf);
    $snapshot_rollback_transaction = PVN::ComputeLifecycle::snapshot_prepare(
        100, 'before-upgrade', $snapshot_conf, 'rollback',
    );
    PVN::ComputeLifecycle::snapshot_commit($snapshot_rollback_transaction);
    $snapshot_delete_transaction = PVN::ComputeLifecycle::snapshot_prepare(
        100, 'before-upgrade', $snapshot_conf, 'delete',
    );
    PVN::ComputeLifecycle::snapshot_abort($snapshot_delete_transaction);
    PVN::ComputeLifecycle::snapshot_cleanup(100, 'before-upgrade', 1700000002);
    my $destroy = PVN::ComputeLifecycle::destroy_capture(100, $pvn_conf);
    PVN::ComputeLifecycle::destroy_commit($destroy);
});
is_deeply(
    [map { $_->[0] } @requests],
    [qw(
        /api/v1/runtime/compute/template/prepare
        /api/v1/runtime/compute/template/commit
        /api/v1/runtime/compute/snapshot/create
        /api/v1/runtime/compute/snapshot/prepare
        /api/v1/runtime/compute/snapshot/commit
        /api/v1/runtime/compute/snapshot/prepare
        /api/v1/runtime/compute/snapshot/abort
        /api/v1/runtime/compute/snapshot/cleanup
        /api/v1/runtime/compute/destroy/capture
        /api/v1/runtime/compute/destroy/commit
    )],
    'template, snapshot, and destroy transitions use fixed lifecycle endpoints',
);
is_deeply(
    $requests[3]->[1],
    {
        lifecycle_id => 'test-snapshot-rollback-100-before-upgrade',
        action => 'rollback',
        vmid => 100,
        snapshot_id => 'before-upgrade',
        snapshot_epoch => 1700000002,
        nics => [{ nic => 'net0', mac_address => 'aa:bb:cc:dd:ee:01' }],
    },
    'snapshot rollback prepare pins exact snapshot NIC identity and generation',
);
is_deeply(
    $requests[4]->[1],
    $snapshot_rollback_transaction,
    'snapshot rollback commit echoes the exact prepare transaction',
);
is_deeply(
    $requests[5]->[1],
    {
        lifecycle_id => 'test-snapshot-delete-100-before-upgrade',
        action => 'delete',
        vmid => 100,
        snapshot_id => 'before-upgrade',
        snapshot_epoch => 1700000002,
    },
    'snapshot delete prepare is identity-only and omits live NIC state',
);
is_deeply(
    $requests[6]->[1],
    $snapshot_delete_transaction,
    'failed snapshot delete abort echoes the exact prepare transaction',
);
is_deeply(
    $requests[7]->[1],
    { vmid => 100, snapshot_id => 'before-upgrade', snapshot_epoch => 1700000002 },
    'post-create cleanup pins only the deleted physical snapshot generation',
);

with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub { die "manager unavailable\n" };
    my $result = PVN::ComputeLifecycle::snapshot_prepare(
        100, 'ordinary',
        { snaptime => 1700000005, net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=vmbr0' },
        'delete',
    );
    ok(!defined($result), 'non-PVN snapshot delete bypasses unavailable PVN manager');
});

with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        is_deeply(
            $payload->{snapshots},
            [{ snapshot_id => 'pvn-old', snapshot_epoch => 1700000003 }],
            'destroy captures only durable PVN snapshot generations',
        );
        is_deeply($payload->{nics}, [], 'snapshot-only destroy has no fabricated live NIC');
        return { lifecycle_id => 'destroy-snapshot-only' };
    };
    my $transaction = PVN::ComputeLifecycle::destroy_capture(902, {
        net0 => 'virtio=AA:BB:CC:DD:EE:01,bridge=vmbr0',
        snapshots => {
            non_pvn => { snaptime => 1700000004, net0 => 'virtio=AA:BB:CC:DD:EE:02,bridge=vmbr0' },
            'pvn-old' => { snaptime => 1700000003, net0 => 'virtio=AA:BB:CC:DD:EE:03,bridge=br-int' },
        },
    });
    ok($transaction, 'snapshot-only PVN manifest participates in destroy lifecycle');
});

my $migration_transaction;
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        is($path, '/api/v1/runtime/compute/migration/begin', 'migration begin endpoint is fixed');
        is($payload->{source_node}, 'prox1', 'migration identifies source node');
        is($payload->{target_node}, 'prox3', 'migration identifies target node');
        is($payload->{source_mtu}, 1500, 'migration proves source br-int MTU');
        is($payload->{target_mtu}, 1500, 'migration proves target br-int MTU');
        return {
            lifecycle_id => $payload->{lifecycle_id}, vmid => 100,
            source_node => 'prox1', target_node => 'prox3', online => JSON::PP::true(),
            transaction => {
                operation_id => 'migration-operation', payload_hash => 'migration-payload',
                ports => [{
                    port_id => 'port-1', nic => 'net0',
                    mac_address => 'aa:bb:cc:dd:ee:01', source_revision => 1,
                    prepared_revision => 2, source_generation => 1, generation => 2,
                }],
            },
        };
    };
    $migration_transaction = PVN::ComputeLifecycle::migration_begin(
        { node => 'prox3', running => 1, rem_ssh => ['ssh', 'prox3'] },
        100,
        $pvn_conf,
    );
});
ok($migration_transaction->{online}, 'running VM uses online migration intent');
ok(!$migration_transaction->{source_stopped}, 'online migration does not claim source stopped');
for my $field (qw(lifecycle_id vmid source_node target_node online transaction)) {
    ok(exists($migration_transaction->{$field}), "migration transaction retains finish field $field");
}

my $finalize_attempts = 0;
with_defaults(sub {
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        my ($path, $payload) = @_;
        is_deeply($payload, $migration_transaction, 'migration finalize echoes exact durable transaction');
        $finalize_attempts++;
        die "manager unavailable\n" if $finalize_attempts < 3;
        return { state => 'finalized' };
    };
    PVN::ComputeLifecycle::migration_finalize($migration_transaction);
});
is($finalize_attempts, 3, 'migration finalize has bounded idempotent retries');

{
    local $SIG{ALRM} = sub { die "outer timeout\n" };
    alarm(20);
    is(
        PVN::ComputeLifecycle::_with_timeout(1, 'inner timeout', sub { return 'done' }),
        'done',
        'nested timeout returns its result',
    );
    my $remaining = alarm(0);
    cmp_ok($remaining, '>=', 19, 'nested timeout preserves the PVE worker alarm');
}
{
    local $SIG{ALRM} = sub { die "outer timeout\n" };
    alarm(20);
    my $ok = eval {
        PVN::ComputeLifecycle::_with_timeout(1, 'inner timeout', sub { die "request failed\n" });
        1;
    };
    ok(!$ok, 'nested timeout propagates request failures');
    like($@, qr/request failed/, 'request failure text is preserved');
    my $remaining = alarm(0);
    cmp_ok($remaining, '>=', 19, 'failing nested timeout also preserves the outer alarm');
}
{
    local $SIG{ALRM} = sub { die "outer timeout\n" };
    alarm(20);
    is(
        PVN::ComputeLifecycle::_with_timeout(1800, 'lifecycle timeout', sub { return 'done' }),
        'done',
        'a long lifecycle timeout remains bounded by the PVE worker alarm',
    );
    my $remaining = alarm(0);
    cmp_ok($remaining, '>=', 19, 'long lifecycle timeout restores the outer PVE worker alarm');
}
{
    my $attempts = 0;
    local $SIG{ALRM} = sub { die "outer timeout\n" };
    local $PVN::ComputeLifecycle::REQUEST_OVERRIDE = sub {
        $attempts++;
        return PVN::ComputeLifecycle::_with_timeout(300, 'request timeout', sub {
            select(undef, undef, undef, 0.2);
            return {};
        });
    };
    Time::HiRes::alarm(0.03);
    my $completed = eval {
        PVN::ComputeLifecycle::_request_bounded_retry('clone_prepare', { nics => [{}] }, 3);
        1;
    };
    my $error = $@;
    Time::HiRes::alarm(0);
    ok(!$completed, 'an expired outer PVE alarm terminates a lifecycle request');
    like($error, qr/outer timeout/, 'outer PVE alarm failure is preserved');
    is($attempts, 1, 'bounded lifecycle retry does not swallow an outer PVE deadline');
}

done_testing();
