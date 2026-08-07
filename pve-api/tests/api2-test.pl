#!/usr/bin/perl

use strict;
use warnings;

use FindBin qw($Bin);
use HTTP::Response;
use JSON::PP qw(decode_json);
use MIME::Base64 qw(decode_base64);
use Test::More;

BEGIN {
    package PVE::RESTHandler;

    sub register_method {
        my ($class, $method) = @_;
        push @PVN::APITest::methods, $method;
    }

    $INC{'PVE/RESTHandler.pm'} = __FILE__;

    package PVE::Exception;

    sub import {
        my ($class, @symbols) = @_;
        my $caller = caller;
        no strict 'refs';
        for my $symbol (@symbols) {
            *{"${caller}::$symbol"} = \&{$symbol};
        }
    }

    sub raise {
        my ($message, %options) = @_;
        die "raise:$options{code}:$message";
    }

    sub raise_param_exc {
        my ($errors) = @_;
        die 'parameter:' . join(',', sort keys %$errors) . ':' . join(',', map { $errors->{$_} } sort keys %$errors);
    }

    $INC{'PVE/Exception.pm'} = __FILE__;

    package PVE::RPCEnvironment;

    sub get { return bless({}, 'PVN::APITest::RPCEnvironment'); }

    $INC{'PVE/RPCEnvironment.pm'} = __FILE__;

    package PVN::APITest::RPCEnvironment;

    sub get_user { return 'root@pam'; }
    sub get_effective_permissions {
        return {
            '/' => { 'PVN.Audit' => 1 },
            '/pool/blue' => { 'VM.Audit' => 1, 'VM.Config.Network' => 0 },
        };
    }
}

use lib "$Bin/..";
require PVN::API2;

my %methods = map { $_->{name} => $_ } @PVN::APITest::methods;
my @collections = qw(
    networks subnets ports ip-allocations routers router-interfaces floating-ips
    provider-networks provider-segments security-groups security-group-rules nodes operations
);
my @writable_collections = grep { $_ ne 'operations' } @collections;
my @expected_methods = qw(index health runtime_port_resolve port_provision port_attach port_detach port_deprovision);
for my $collection (@collections) {
    (my $suffix = $collection) =~ tr/-/_/;
    push @expected_methods, "list_$suffix", "get_$suffix";
    push @expected_methods, "create_$suffix", "update_$suffix", "delete_$suffix"
        if $collection ne 'operations';
}
is(scalar(@PVN::APITest::methods), 69, 'registered the fixed PVN route set');
is_deeply(
    [sort keys %methods],
    [sort @expected_methods],
    'route names are stable',
);

for my $method (@PVN::APITest::methods) {
    ok(!exists($method->{protected}), "$method->{name} runs in the unprivileged pveproxy worker");
    is_deeply($method->{permissions}, { user => 'all' }, "$method->{name} requires a PVE login");
    unlike($method->{path}, qr/projects?/, "$method->{name} has no project route");
    ok(!exists($method->{parameters}->{properties}->{project_id}), "$method->{name} has no project parameter");
}

ok(exists($methods{list_operations}) && exists($methods{get_operations}), 'operations are readable');
ok(!exists($methods{create_operations}), 'operations cannot be created');
ok(!exists($methods{update_operations}), 'operations cannot be updated');
ok(!exists($methods{delete_operations}), 'operations cannot be deleted');
for my $method (@PVN::APITest::methods) {
    unlike($method->{path}, qr/^\{collection:/, "$method->{name} uses a PVE-compatible fixed collection path");
}

sub parameter_is_required {
    my ($method, $parameter) = @_;
    my $schema = $methods{$method}->{parameters}->{properties}->{$parameter};
    return defined($schema) && !$schema->{optional};
}

sub parameter_is_optional {
    my ($method, $parameter) = @_;
    my $schema = $methods{$method}->{parameters}->{properties}->{$parameter};
    return defined($schema) && $schema->{optional};
}

for my $method (qw(port_provision port_attach port_detach)) {
    ok(parameter_is_required($method, 'payload'), "$method requires a JSON payload under PVE schema rules");
    ok(parameter_is_required($method, 'idempotency_key'), "$method requires an idempotency key under PVE schema rules");
}
for my $collection (@writable_collections) {
    (my $suffix = $collection) =~ tr/-/_/;
    ok(parameter_is_required("create_$suffix", 'payload'), "create $collection requires a JSON payload");
    ok(parameter_is_required("create_$suffix", 'idempotency_key'), "create $collection requires an idempotency key");
    ok(parameter_is_required("update_$suffix", 'payload'), "update $collection requires a JSON payload");
    ok(parameter_is_optional("update_$suffix", 'revision'), "update $collection may take revision from its JSON payload");
    ok(parameter_is_required("update_$suffix", 'idempotency_key'), "update $collection requires an idempotency key");
    ok(parameter_is_required("delete_$suffix", 'revision'), "delete $collection requires a revision");
    ok(parameter_is_required("delete_$suffix", 'idempotency_key'), "delete $collection requires an idempotency key");
}
ok(parameter_is_required('port_deprovision', 'revision'), 'port deprovision requires a revision');
ok(parameter_is_required('port_deprovision', 'idempotency_key'), 'port deprovision requires an idempotency key');
for my $parameter (qw(
    network_id subnet_id router_id security_group_id provider_network_id node_id vmid nic limit
)) {
    ok(parameter_is_optional('list_networks', $parameter), "list filter $parameter is optional");
}

is(
    PVN::API2::validate_payload('{"name":"blue"}'),
    '{"name":"blue"}',
    'projectless object payload is accepted',
);
my $network_options_payload = '{"dns_nameservers":["1.1.1.1"],"dns_domain":"guest.example","dns_search_domains":["guest.example"],"static_routes":[{"destination":"10.60.0.0/16","next_hop":"10.42.0.2"}]}';
is(
    PVN::API2::validate_payload($network_options_payload),
    $network_options_payload,
    'nested DNS and static-route payload is forwarded byte-for-byte',
);
for my $payload (undef, '[]', '{broken', '{"project_id":"legacy"}') {
    my $accepted = eval { PVN::API2::validate_payload($payload); 1 };
    ok(!$accepted, 'invalid or project-bearing payload is rejected');
}

is(
    PVN::API2::query_string({ node => 'pve 1', vmid => 100, ignored => 'x' }, qw(node vmid nic)),
    '?node=pve%201&vmid=100',
    'query construction is allowlisted and encoded',
);

my ($authid, $encoded_permissions) = PVN::API2::pve_identity_headers();
is($authid, 'root@pam', 'PVE auth identity is forwarded');
$encoded_permissions =~ tr{-_}{+/};
$encoded_permissions .= '=' while length($encoded_permissions) % 4;
my $permissions = decode_json(decode_base64($encoded_permissions));
is_deeply(
    $permissions,
    {
        '/' => { 'PVN.Audit' => 1 },
        '/pool/blue' => { 'VM.Audit' => 1, 'VM.Config.Network' => 0 },
    },
    'effective PVE permissions are forwarded intact',
);

my $success = HTTP::Response->new(200, 'OK');
$success->content('{"data":{"status":"ready"}}');
is_deeply(PVN::API2::response_data($success), { status => 'ready' }, 'successful manager envelope is unwrapped');

my $invalid = HTTP::Response->new(200, 'OK');
$invalid->content('not-json');
my $valid_response = eval { PVN::API2::response_data($invalid); 1 };
ok(!$valid_response, 'invalid manager response is rejected');
like($@, qr/raise:502:/, 'invalid manager response maps to a bad gateway error');

open my $module_source_handle, '<', "$Bin/../PVN/API2.pm"
    or die "open PVN API module source: $!";
local $/;
my $module_source = <$module_source_handle>;
close $module_source_handle;
unlike(
    $module_source,
    qr/\bshutdown\s*\(\s*\$socket\s*,\s*1\s*\)/,
    'gateway does not half-close the request and cancel the manager context',
);

done_testing();
