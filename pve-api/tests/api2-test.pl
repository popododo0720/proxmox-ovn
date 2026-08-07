#!/usr/bin/perl

use strict;
use warnings;

use FindBin qw($Bin);
use HTTP::Response;
use JSON qw(decode_json);
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

is(scalar(@PVN::APITest::methods), 12, 'registered the fixed PVN route set');

my %methods = map { $_->{name} => $_ } @PVN::APITest::methods;
is_deeply(
    [sort keys %methods],
    [sort qw(
        index health runtime_port_resolve port_provision port_attach port_detach port_deprovision
        list_resources create_resource get_resource update_resource delete_resource
    )],
    'route names are stable',
);

for my $method (@PVN::APITest::methods) {
    ok(!exists($method->{protected}), "$method->{name} runs in the unprivileged pveproxy worker");
    is_deeply($method->{permissions}, { user => 'all' }, "$method->{name} requires a PVE login");
    unlike($method->{path}, qr/projects?/, "$method->{name} has no project route");
    ok(!exists($method->{parameters}->{properties}->{project_id}), "$method->{name} has no project parameter");
}

like($methods{list_resources}->{path}, qr/operations/, 'operations are readable');
unlike($methods{create_resource}->{path}, qr/operations/, 'operations cannot be created');
unlike($methods{update_resource}->{path}, qr/operations/, 'operations cannot be updated');
unlike($methods{delete_resource}->{path}, qr/operations/, 'operations cannot be deleted');

is(
    PVN::API2::validate_payload('{"name":"blue"}'),
    '{"name":"blue"}',
    'projectless object payload is accepted',
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

done_testing();
