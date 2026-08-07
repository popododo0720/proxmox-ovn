package PVN::API2;

use strict;
use warnings;

use bytes ();
use HTTP::Response;
use IO::Socket::UNIX;
use JSON::PP qw(decode_json encode_json);
use MIME::Base64 qw(encode_base64);
use Socket qw(SOCK_STREAM);
use URI::Escape qw(uri_escape_utf8);

use PVE::Exception qw(raise raise_param_exc);
use PVE::RESTHandler;
use PVE::RPCEnvironment;

use base qw(PVE::RESTHandler);

our $SOCKET_PATH = '/run/pvn-api/manager.sock';

my $MAX_REQUEST = 1 << 20;
my $MAX_RESPONSE = 8 << 20;
my $TIMEOUT = 10;
my $ID_PATTERN = '[A-Za-z0-9][A-Za-z0-9._:-]{0,127}';
my $ITEM_ID_PATTERN = $ID_PATTERN;
my @READ_COLLECTIONS = qw(
    networks subnets ports ip-allocations routers router-interfaces floating-ips
    provider-networks provider-segments security-groups security-group-rules nodes operations
);
my %WRITE_COLLECTIONS = map { $_ => 1 } grep { $_ ne 'operations' } @READ_COLLECTIONS;

my $auth_permissions = { user => 'all' };
my $empty_parameters = { additionalProperties => 0, properties => {} };
my $object_return = { type => 'object' };
my $array_return = { type => 'array', items => { type => 'object' } };
my $null_return = { type => 'null' };

my $id_property = {
    type => 'string',
    pattern => $ITEM_ID_PATTERN,
    minLength => 1,
    maxLength => 128,
};
my $payload_property = {
    type => 'string',
    minLength => 2,
    maxLength => $MAX_REQUEST,
};
my $idempotency_property = {
    type => 'string',
    pattern => '[A-Za-z0-9][A-Za-z0-9._:-]{0,199}',
    minLength => 1,
    maxLength => 200,
};
my $revision_property = {
    type => 'integer',
    minimum => 1,
};

sub pve_identity_headers {
    my $rpcenv = PVE::RPCEnvironment::get();
    my $authid = $rpcenv->get_user();
    raise("authenticated PVE identity is unavailable\n", code => 401)
        if !defined($authid) || $authid !~ m/^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}\@[A-Za-z0-9][A-Za-z0-9._-]{0,63}(?:![A-Za-z0-9][A-Za-z0-9._-]{0,63})?$/;

    my $permissions = encode_json($rpcenv->get_effective_permissions($authid));
    my $encoded = encode_base64($permissions, '');
    $encoded =~ tr{+/}{-_};
    $encoded =~ s/=+$//;
    raise("PVE permission map exceeds the PVN gateway limit\n", code => 500)
        if length($encoded) == 0 || length($encoded) > 256 << 10;
    return ($authid, $encoded);
}

sub validate_payload {
    my ($payload) = @_;
    raise_param_exc({ payload => 'payload is required' }) if !defined($payload);
    raise_param_exc({ payload => 'payload exceeds the PVN request limit' })
        if bytes::length($payload) > $MAX_REQUEST;
    my $decoded = eval { decode_json($payload) };
    raise_param_exc({ payload => 'payload must contain one JSON object' })
        if $@ || ref($decoded) ne 'HASH';
    raise_param_exc({ payload => 'project_id is not part of the PVN API' })
        if exists($decoded->{project_id});
    return $payload;
}

sub query_string {
    my ($param, @names) = @_;
    my @pairs;
    for my $name (@names) {
        next if !defined($param->{$name});
        push @pairs, uri_escape_utf8($name) . '=' . uri_escape_utf8("$param->{$name}");
    }
    return @pairs ? '?' . join('&', @pairs) : '';
}

sub request_headers {
    my ($param, $body) = @_;
    my ($authid, $permissions) = pve_identity_headers();
    my @headers = (
        'Host: pvn-manager.local',
        'Accept: application/json',
        'Connection: close',
        'X-PVN-PVE-Authid: ' . $authid,
        'X-PVN-PVE-Permissions: ' . $permissions,
    );
    if (defined($param->{revision})) {
        push @headers, 'If-Match: "' . $param->{revision} . '"';
    }
    if (defined($param->{idempotency_key})) {
        push @headers, 'Idempotency-Key: ' . $param->{idempotency_key};
    }
    if (bytes::length($body)) {
        push @headers, 'Content-Type: application/json';
    }
    push @headers, 'Content-Length: ' . bytes::length($body);
    return @headers;
}

sub write_all {
    my ($socket, $data) = @_;
    my $offset = 0;
    while ($offset < bytes::length($data)) {
        my $written = syswrite($socket, $data, bytes::length($data) - $offset, $offset);
        die "write PVN browser socket: $!\n" if !defined($written) || $written == 0;
        $offset += $written;
    }
}

sub read_response {
    my ($socket) = @_;
    my $raw = '';
    while (1) {
        my $chunk = '';
        my $read = sysread($socket, $chunk, 64 << 10);
        die "read PVN browser socket: $!\n" if !defined($read);
        last if $read == 0;
        $raw .= $chunk;
        die "PVN browser response exceeds the gateway limit\n"
            if bytes::length($raw) > $MAX_RESPONSE;
    }
    return $raw;
}

sub unix_request {
    my ($method, $path, $param, $body) = @_;
    my $socket = IO::Socket::UNIX->new(
        Type => SOCK_STREAM,
        Peer => $SOCKET_PATH,
        Timeout => $TIMEOUT,
    );
    raise("PVN manager is unavailable\n", code => 503) if !$socket;

    my @headers = request_headers($param, $body);
    my $request = "$method $path HTTP/1.0\r\n" . join("\r\n", @headers) . "\r\n\r\n" . $body;
    my $raw;
    my $error;
    eval {
        local $SIG{ALRM} = sub { die "PVN browser request timed out\n" };
        alarm($TIMEOUT);
        write_all($socket, $request);
        # Do not half-close the client write side here. Go's net/http server
        # treats the resulting EOF as a disconnected client and cancels the
        # request context before PVN can query its control databases. The
        # explicit Content-Length is sufficient to delimit the request body,
        # and Connection: close makes the manager close after its response.
        $raw = read_response($socket);
        alarm(0);
    };
    $error = $@;
    alarm(0);
    close($socket);
    raise("PVN manager request failed\n", code => 503) if $error;

    my $response = HTTP::Response->parse($raw);
    raise("PVN manager returned an invalid response\n", code => 502)
        if !$response || !$response->code;
    return $response;
}

sub response_data {
    my ($response) = @_;
    my $status = $response->code;
    return undef if $status == 204;

    my $decoded = eval { decode_json($response->content) };
    raise("PVN manager returned invalid JSON\n", code => 502) if $@ || ref($decoded) ne 'HASH';
    if ($status >= 200 && $status < 300) {
        return $decoded->{data};
    }

    my $message = 'PVN manager request failed';
    if (ref($decoded->{error}) eq 'HASH' && defined($decoded->{error}->{message})) {
        $message = "$decoded->{error}->{message}";
    }
    $message =~ s/[\x00-\x1f\x7f]+/ /g;
    $message =~ s/\s+/ /g;
    $message = substr($message, 0, 500);
    $status = 502 if $status < 400 || $status > 599;
    raise("$message\n", code => $status);
}

sub forward_request {
    my ($method, $path, $param, $query_names) = @_;
    my $body = defined($param->{payload}) ? validate_payload($param->{payload}) : '';
    $path .= query_string($param, @{ $query_names // [] });
    return response_data(unix_request($method, $path, $param, $body));
}

__PACKAGE__->register_method({
    name => 'index',
    path => '',
    method => 'GET',
    permissions => $auth_permissions,
    description => 'PVN API index.',
    parameters => $empty_parameters,
    returns => $array_return,
    code => sub {
        return [
            map { { subdir => $_ } }
            qw(health networks subnets routers ports floating-ips security-groups provider-networks nodes operations)
        ];
    },
});

__PACKAGE__->register_method({
    name => 'health',
    path => 'health',
    method => 'GET',
    permissions => $auth_permissions,
    description => 'Read PVN manager health.',
    parameters => $empty_parameters,
    returns => $object_return,
    code => sub { return forward_request('GET', '/api/v1/health', $_[0]); },
});

__PACKAGE__->register_method({
    name => 'runtime_port_resolve',
    path => 'runtime/ports/resolve',
    method => 'GET',
    permissions => $auth_permissions,
    description => 'Resolve a PVN VM port.',
    parameters => {
        additionalProperties => 0,
        properties => {
            node => { type => 'string', pattern => '[A-Za-z0-9][A-Za-z0-9._-]{0,127}' },
            vmid => { type => 'integer', minimum => 1 },
            nic => { type => 'string', pattern => 'net[0-9]+' },
        },
    },
    returns => $object_return,
    code => sub {
        return forward_request(
            'GET', '/api/v1/runtime/ports/resolve', $_[0], [qw(node vmid nic)],
        );
    },
});

__PACKAGE__->register_method({
    name => 'port_provision',
    path => "ports/{id:$ITEM_ID_PATTERN}",
    method => 'POST',
    permissions => $auth_permissions,
    description => 'Provision a PVN port.',
    parameters => {
        additionalProperties => 0,
        properties => {
            id => { type => 'string', enum => ['provision'] },
            payload => $payload_property,
            idempotency_key => $idempotency_property,
        },
    },
    returns => $object_return,
    code => sub {
        my ($param) = @_;
        raise_param_exc({ id => 'only the provision action is accepted here' })
            if $param->{id} ne 'provision';
        return forward_request('POST', '/api/v1/ports/provision', $param);
    },
});

for my $action (qw(attach detach)) {
    __PACKAGE__->register_method({
        name => "port_$action",
        path => "ports/{id:$ITEM_ID_PATTERN}/$action",
        method => 'POST',
        permissions => $auth_permissions,
        description => "Begin PVN port $action.",
        parameters => {
            additionalProperties => 0,
            properties => {
                id => $id_property,
                payload => $payload_property,
                revision => $revision_property,
                idempotency_key => $idempotency_property,
            },
        },
        returns => $object_return,
        code => sub {
            my ($param) = @_;
            return forward_request(
                'POST', '/api/v1/ports/' . uri_escape_utf8($param->{id}) . "/$action", $param,
            );
        },
    });
}

__PACKAGE__->register_method({
    name => 'port_deprovision',
    path => "ports/{id:$ITEM_ID_PATTERN}/deprovision",
    method => 'DELETE',
    permissions => $auth_permissions,
    description => 'Deprovision a PVN port.',
    parameters => {
        additionalProperties => 0,
        properties => {
            id => $id_property,
            revision => $revision_property,
            idempotency_key => $idempotency_property,
        },
    },
    returns => $null_return,
    code => sub {
        my ($param) = @_;
        return forward_request(
            'DELETE', '/api/v1/ports/' . uri_escape_utf8($param->{id}) . '/deprovision', $param,
        );
    },
});

for my $collection (@READ_COLLECTIONS) {
    (my $method_suffix = $collection) =~ tr/-/_/;

    __PACKAGE__->register_method({
        name => "list_$method_suffix",
        path => $collection,
        method => 'GET',
        permissions => $auth_permissions,
        description => "List PVN $collection.",
        parameters => {
            additionalProperties => 0,
            properties => {
                network_id => { type => 'string', pattern => $ID_PATTERN, optional => 1 },
                subnet_id => { type => 'string', pattern => $ID_PATTERN, optional => 1 },
                router_id => { type => 'string', pattern => $ID_PATTERN, optional => 1 },
                security_group_id => { type => 'string', pattern => $ID_PATTERN, optional => 1 },
                provider_network_id => { type => 'string', pattern => $ID_PATTERN, optional => 1 },
                node_id => { type => 'string', pattern => $ID_PATTERN, optional => 1 },
                vmid => { type => 'integer', minimum => 1, optional => 1 },
                nic => { type => 'string', pattern => 'net[0-9]+', optional => 1 },
                limit => { type => 'integer', minimum => 1, maximum => 500, optional => 1 },
            },
        },
        returns => $array_return,
        code => sub {
            return forward_request(
                'GET', "/api/v1/$collection", $_[0], [qw(
                    network_id subnet_id router_id security_group_id provider_network_id
                    node_id vmid nic limit
                )],
            );
        },
    });

    if ($WRITE_COLLECTIONS{$collection}) {
        __PACKAGE__->register_method({
            name => "create_$method_suffix",
            path => $collection,
            method => 'POST',
            permissions => $auth_permissions,
            description => "Create a PVN $collection resource.",
            parameters => {
                additionalProperties => 0,
                properties => {
                    payload => $payload_property,
                    idempotency_key => $idempotency_property,
                },
            },
            returns => $object_return,
            code => sub { return forward_request('POST', "/api/v1/$collection", $_[0]); },
        });
    }

    __PACKAGE__->register_method({
        name => "get_$method_suffix",
        path => "$collection/{id:$ITEM_ID_PATTERN}",
        method => 'GET',
        permissions => $auth_permissions,
        description => "Read a PVN $collection resource.",
        parameters => {
            additionalProperties => 0,
            properties => { id => $id_property },
        },
        returns => $object_return,
        code => sub {
            my ($param) = @_;
            return forward_request(
                'GET', "/api/v1/$collection/" . uri_escape_utf8($param->{id}), $param,
            );
        },
    });

    next if !$WRITE_COLLECTIONS{$collection};

    __PACKAGE__->register_method({
        name => "update_$method_suffix",
        path => "$collection/{id:$ITEM_ID_PATTERN}",
        method => 'PUT',
        permissions => $auth_permissions,
        description => "Update a PVN $collection resource.",
        parameters => {
            additionalProperties => 0,
            properties => {
                id => $id_property,
                payload => $payload_property,
                revision => { %$revision_property, optional => 1 },
                idempotency_key => $idempotency_property,
            },
        },
        returns => $object_return,
        code => sub {
            my ($param) = @_;
            return forward_request(
                'PUT', "/api/v1/$collection/" . uri_escape_utf8($param->{id}), $param,
            );
        },
    });

    __PACKAGE__->register_method({
        name => "delete_$method_suffix",
        path => "$collection/{id:$ITEM_ID_PATTERN}",
        method => 'DELETE',
        permissions => $auth_permissions,
        description => "Delete a PVN $collection resource.",
        parameters => {
            additionalProperties => 0,
            properties => {
                id => $id_property,
                revision => $revision_property,
                idempotency_key => $idempotency_property,
            },
        },
        returns => $null_return,
        code => sub {
            my ($param) = @_;
            return forward_request(
                'DELETE', "/api/v1/$collection/" . uri_escape_utf8($param->{id}), $param,
            );
        },
    });
}

1;
