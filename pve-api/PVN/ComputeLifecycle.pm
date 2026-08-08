package PVN::ComputeLifecycle;

use strict;
use warnings;

use bytes ();
use Digest::SHA qw(sha256_hex);
use Fcntl qw(S_ISDIR);
use HTTP::Response;
use IO::Socket::INET;
use IO::Socket::UNIX;
use JSON::PP qw(decode_json encode_json);
use Socket qw(SOCK_STREAM);
use Time::HiRes qw(alarm time);
use Time::Local qw(timegm);

our $COMPUTE_SOCKET = '/run/pvn-compute/manager.sock';
our $AGENT_HEALTH_HOST = '127.0.0.1';
our $AGENT_HEALTH_PORT = 9476;
our $REQUEST_OVERRIDE;
our $UNIT_CHECK_OVERRIDE;
our $AGENT_HEALTH_OVERRIDE;
our $NODE_OVERRIDE;
our $LOCAL_MTU_OVERRIDE;
our $REMOTE_MTU_OVERRIDE;
our $LIFECYCLE_ID_OVERRIDE;
our $HA_MANAGED_OVERRIDE;
our $HA_RUNTIME_OVERRIDE;

my $MAX_RESPONSE = 8 << 20;
my $REQUEST_TIMEOUT = 10;
my $HA_STATE_FRESHNESS = 30;
my $HA_FUTURE_TOLERANCE = 5;
my @REQUIRED_UNITS = qw(
    pvn-node.target
    pvn-node-ready.service
    pvn-manager.service
    pvn-agent.service
    pvn-ovn-host-config.service
    ovn-controller.service
);

my %PATH = (
    start => '/api/v1/runtime/compute/start',
    clone_prepare => '/api/v1/runtime/compute/clone/prepare',
    clone_commit => '/api/v1/runtime/compute/clone/commit',
    clone_abort => '/api/v1/runtime/compute/clone/abort',
    migration_begin => '/api/v1/runtime/compute/migration/begin',
    migration_finalize => '/api/v1/runtime/compute/migration/finalize',
    migration_abort => '/api/v1/runtime/compute/migration/abort',
    template_prepare => '/api/v1/runtime/compute/template/prepare',
    template_commit => '/api/v1/runtime/compute/template/commit',
    template_abort => '/api/v1/runtime/compute/template/abort',
    snapshot_create => '/api/v1/runtime/compute/snapshot/create',
    snapshot_prepare => '/api/v1/runtime/compute/snapshot/prepare',
    snapshot_commit => '/api/v1/runtime/compute/snapshot/commit',
    snapshot_abort => '/api/v1/runtime/compute/snapshot/abort',
    snapshot_cleanup => '/api/v1/runtime/compute/snapshot/cleanup',
    destroy_capture => '/api/v1/runtime/compute/destroy/capture',
    destroy_commit => '/api/v1/runtime/compute/destroy/commit',
    destroy_abort => '/api/v1/runtime/compute/destroy/abort',
);

sub _write_all {
    my ($socket, $data) = @_;
    my $offset = 0;
    while ($offset < bytes::length($data)) {
        my $written = syswrite($socket, $data, bytes::length($data) - $offset, $offset);
        die "write PVN compute socket: $!\n" if !defined($written) || $written == 0;
        $offset += $written;
    }
}

sub _read_all {
    my ($socket) = @_;
    my $raw = '';
    while (1) {
        my $chunk = '';
        my $read = sysread($socket, $chunk, 64 << 10);
        die "read PVN compute socket: $!\n" if !defined($read);
        last if $read == 0;
        $raw .= $chunk;
        die "PVN compute response exceeds the package limit\n"
            if bytes::length($raw) > $MAX_RESPONSE;
    }
    return $raw;
}

sub _with_timeout {
    my ($seconds, $message, $code) = @_;
    my $outer_remaining = alarm(0);
    my $outer_handler = $SIG{ALRM};
    my $started = time();
    my $effective = $outer_remaining && $outer_remaining < $seconds
        ? $outer_remaining
        : $seconds;
    my $outer_wins = $outer_remaining && $outer_remaining <= $seconds;
    my ($result, $error, $outer_expired);
    {
        local $SIG{ALRM} = sub {
            if ($outer_wins) {
                $outer_expired = 1;
                if (ref($outer_handler) eq 'CODE') {
                    return $outer_handler->();
                }
                return if defined($outer_handler) && $outer_handler eq 'IGNORE';
                die "PVE worker deadline expired\n";
            }
            die "$message\n";
        };
        alarm($effective);
        eval { $result = $code->(); };
        $error = $@;
        alarm(0);
    }
    if ($outer_remaining && !$outer_expired) {
        my $remaining = $outer_remaining - (time() - $started);
        if ($remaining > 0) {
            alarm($remaining);
        } elsif (ref($outer_handler) eq 'CODE') {
            $outer_handler->();
        } elsif (!defined($outer_handler) || $outer_handler ne 'IGNORE') {
            die "PVE worker deadline expired\n";
        }
    }
    die $error if $error;
    return $result;
}

sub _unix_post {
    my ($path, $payload) = @_;
    die "PVN compute lifecycle calls require root\n" if $> != 0;
    my $body = encode_json($payload);
    my $socket = IO::Socket::UNIX->new(
        Type => SOCK_STREAM,
        Peer => $COMPUTE_SOCKET,
        Timeout => $REQUEST_TIMEOUT,
    );
    die "PVN privileged compute manager is unavailable\n" if !$socket;

    my $request = "POST $path HTTP/1.0\r\n"
        . "Host: pvn-compute.local\r\n"
        . "Accept: application/json\r\n"
        . "Content-Type: application/json\r\n"
        . "Content-Length: " . bytes::length($body) . "\r\n"
        . "Connection: close\r\n\r\n$body";
    my ($raw, $error);
    eval {
        $raw = _with_timeout($REQUEST_TIMEOUT, 'PVN compute request timed out', sub {
            _write_all($socket, $request);
            return _read_all($socket);
        });
    };
    $error = $@;
    close($socket);
    die $error if $error;

    my $response = HTTP::Response->parse($raw);
    die "PVN compute manager returned an invalid HTTP response\n"
        if !$response || !$response->code;
    my $decoded = eval { decode_json($response->content) };
    die "PVN compute manager returned invalid JSON\n"
        if $@ || ref($decoded) ne 'HASH';
    if ($response->is_success) {
        die "PVN compute manager returned no transaction data\n"
            if ref($decoded->{data}) ne 'HASH';
        return $decoded->{data};
    }
    my $message = 'PVN compute lifecycle request failed';
    if (ref($decoded->{error}) eq 'HASH' && defined($decoded->{error}->{message})) {
        $message = "$decoded->{error}->{message}";
    }
    $message =~ s/[\x00-\x1f\x7f]+/ /g;
    $message =~ s/\s+/ /g;
    die substr($message, 0, 500) . "\n";
}

sub _request {
    my ($name, $payload) = @_;
    die "unknown PVN lifecycle request '$name'\n" if !defined($PATH{$name});
    die "PVN lifecycle payload must be an object\n" if ref($payload) ne 'HASH';
    my $result = $REQUEST_OVERRIDE
        ? $REQUEST_OVERRIDE->($PATH{$name}, $payload)
        : _unix_post($PATH{$name}, $payload);
    die "PVN lifecycle response must be an object\n" if ref($result) ne 'HASH';
    return $result;
}

sub _request_bounded_retry {
    my ($name, $payload, $attempts) = @_;
    my $last_error = '';
    for my $attempt (1 .. $attempts) {
        my $result = eval { _request($name, $payload) };
        return $result if !$@;
        $last_error = $@;
        select(undef, undef, undef, 0.1 * $attempt) if $attempt < $attempts;
    }
    die $last_error;
}

sub _net_parts {
    my ($value) = @_;
    return if !defined($value) || ref($value);
    my @parts = split(/,/, $value, -1);
    return if !@parts;
    my %options;
    for my $part (@parts) {
        my ($key, $item) = split(/=/, $part, 2);
        return if !defined($item) || $key eq '' || exists($options{$key});
        $options{$key} = $item;
    }
    return (\@parts, \%options);
}

sub _pvn_nics {
    my ($conf) = @_;
    die "PVE VM configuration is unavailable\n" if ref($conf) ne 'HASH';
    my @nics;
    for my $nic (sort {
        my ($a_index) = $a =~ /^net(\d+)$/;
        my ($b_index) = $b =~ /^net(\d+)$/;
        $a_index <=> $b_index;
    } grep { /^net\d+$/ } keys %$conf) {
        my ($parts, $options) = _net_parts($conf->{$nic});
        if (!$parts) {
            die "PVN NIC $nic has an invalid PVE network configuration\n"
                if defined($conf->{$nic}) && !ref($conf->{$nic})
                && $conf->{$nic} =~ /(?:\A|,)bridge=br-int(?:,|\z)/;
            next;
        }
        next if ($options->{bridge} // '') ne 'br-int';
        my $first_key = (split(/=/, $parts->[0], 2))[0];
        die "PVN NIC $nic has conflicting MAC address fields\n"
            if defined($options->{macaddr}) && defined($options->{$first_key})
            && lc($options->{macaddr}) ne lc($options->{$first_key});
        my $mac = $options->{macaddr} // $options->{$first_key};
        die "PVN NIC $nic has no valid MAC address\n"
            if !defined($mac) || $mac !~ /\A[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}\z/;
        push @nics, { nic => $nic, mac_address => lc($mac) };
    }
    return \@nics;
}

sub _rewrite_clone_nics {
    my ($target_conf, $source_nics, $ports) = @_;
    die "PVN clone response has no port list\n" if ref($ports) ne 'ARRAY';
    my %expected = map { $_->{nic} => 1 } @$source_nics;
    my $target_nics = _pvn_nics($target_conf);
    die "cloned PVE target NIC set differs from its PVN source manifest\n"
        if @$target_nics != scalar(keys %expected)
        || grep { !$expected{$_->{nic}} } @$target_nics;
    my %seen;
    for my $port (@$ports) {
        die "PVN clone response contains an invalid port\n" if ref($port) ne 'HASH';
        my $nic = $port->{nic};
        my $mac = $port->{mac_address};
        die "PVN clone response contains an unexpected NIC\n"
            if !defined($nic) || !$expected{$nic} || $seen{$nic}++;
        die "PVN clone response contains an invalid MAC address\n"
            if !defined($mac) || $mac !~ /\A[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}\z/;
        die "PVN clone response contains no exact port ownership proof\n"
            if !defined($port->{port_id}) || $port->{port_id} eq ''
            || !defined($port->{ownership_digest}) || $port->{ownership_digest} eq '';

        my ($parts, $options) = _net_parts($target_conf->{$nic});
        die "cloned PVN NIC $nic is missing from the PVE configuration\n"
            if !$parts || ($options->{bridge} // '') ne 'br-int';
        my ($model) = split(/=/, $parts->[0], 2);
        die "cloned PVN NIC $nic has an invalid model field\n" if !defined($model) || $model eq '';
        $parts->[0] = "$model=" . uc($mac);

        my $found_link_down = 0;
        my $found_macaddr = 0;
        for my $index (1 .. $#$parts) {
            if ($parts->[$index] =~ /^macaddr=/) {
                die "cloned PVN NIC $nic has duplicate macaddr fields\n" if $found_macaddr++;
                $parts->[$index] = 'macaddr=' . uc($mac);
            }
            if ($parts->[$index] =~ /^link_down=/) {
                die "cloned PVN NIC $nic has duplicate link_down fields\n" if $found_link_down++;
                $parts->[$index] = 'link_down=1';
            }
        }
        push @$parts, 'link_down=1' if !$found_link_down;
        $target_conf->{$nic} = join(',', @$parts);
    }
    die "PVN clone response omitted one or more NICs\n"
        if scalar(keys %seen) != scalar(keys %expected);
}

sub clone_activate_config {
    my ($transaction, $target_conf) = @_;
    return if !$transaction;
    die "PVN clone transaction has no port list\n"
        if ref($transaction) ne 'HASH' || ref($transaction->{ports}) ne 'ARRAY';
    my %expected;
    for my $port (@{ $transaction->{ports} }) {
        die "PVN clone transaction contains an invalid port identity\n"
            if ref($port) ne 'HASH' || !defined($port->{nic})
            || !defined($port->{mac_address})
            || $port->{mac_address} !~ /\A[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}\z/
            || !defined($port->{port_id}) || $port->{port_id} eq ''
            || !defined($port->{ownership_digest}) || $port->{ownership_digest} eq '';
        my $nic = $port->{nic};
        die "PVN clone transaction contains a duplicate NIC\n" if $expected{$nic};
        $expected{$nic} = $port;
    }
    my $current_nics = _pvn_nics($target_conf);
    die "committed clone PVN NIC set changed before activation\n"
        if @$current_nics != scalar(keys %expected);
    for my $current (@$current_nics) {
        my $port = $expected{$current->{nic}};
        die "committed clone PVN NIC set changed before activation\n"
            if !$port || lc($current->{mac_address}) ne lc($port->{mac_address});
    }
    for my $port (@{ $transaction->{ports} }) {
        my $nic = $port->{nic};
        my ($parts, $options) = _net_parts($target_conf->{$nic});
        die "committed cloned PVN NIC $nic is missing or moved off br-int\n"
            if !$parts || ($options->{bridge} // '') ne 'br-int';
        my ($model) = split(/=/, $parts->[0], 2);
        my $mac = $options->{macaddr} // $options->{$model};
        die "committed cloned PVN NIC $nic MAC changed before activation\n"
            if !defined($mac) || lc($mac) ne lc($port->{mac_address} // '');
        my $link_down_fields = 0;
        for my $index (1 .. $#$parts) {
            next if $parts->[$index] !~ /^link_down=/;
            $link_down_fields++;
            die "committed cloned PVN NIC $nic was activated before PVN commit\n"
                if $parts->[$index] ne 'link_down=1';
            $parts->[$index] = 'link_down=0';
        }
        die "committed cloned PVN NIC $nic has no activation guard\n"
            if $link_down_fields != 1;
        $target_conf->{$nic} = join(',', @$parts);
    }
    return 1;
}

sub _node_name {
    return $NODE_OVERRIDE->() if $NODE_OVERRIDE;
    require PVE::INotify;
    my $node = PVE::INotify::nodename();
    die "PVE node name is unavailable\n"
        if !defined($node) || $node !~ /\A[A-Za-z0-9][A-Za-z0-9._-]{0,127}\z/;
    return $node;
}

sub _lifecycle_id {
    my ($kind, @parts) = @_;
    return $LIFECYCLE_ID_OVERRIDE->($kind, @parts) if $LIFECYCLE_ID_OVERRIDE;
    my $seed = join("\0", $kind, @parts, time(), $$, rand());
    return "pve-$kind-" . substr(sha256_hex($seed), 0, 32);
}

sub snapshot_epoch {
    my ($conf) = @_;
    die "PVE snapshot configuration has no immutable snaptime\n"
        if ref($conf) ne 'HASH' || !defined($conf->{snaptime})
        || $conf->{snaptime} !~ /\A[1-9]\d*\z/;
    return int($conf->{snaptime});
}

sub _snapshot_refs {
    my ($conf) = @_;
    return [] if !defined($conf->{snapshots});
    die "PVE snapshot collection is invalid\n" if ref($conf->{snapshots}) ne 'HASH';
    my @snapshots;
    for my $snapshot_id (sort keys %{ $conf->{snapshots} }) {
        die "PVE snapshot name is invalid\n"
            if $snapshot_id eq '' || $snapshot_id =~ /[\x00-\x1f\x7f]/;
        my $snapshot_conf = $conf->{snapshots}->{$snapshot_id};
        next if !@{ _pvn_nics($snapshot_conf) };
        push @snapshots, {
            snapshot_id => "$snapshot_id",
            snapshot_epoch => snapshot_epoch($snapshot_conf),
        };
    }
    return \@snapshots;
}

sub _iso8601_epoch {
    my ($value) = @_;
    return if !defined($value);
    my ($year, $month, $day, $hour, $minute, $second, $zone) =
        $value =~ /\A(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-]\d{2}:\d{2})\z/;
    return if !defined($zone);
    my $epoch = eval { timegm($second, $minute, $hour, $day, $month - 1, $year) };
    return if $@;
    if ($zone ne 'Z') {
        my ($sign, $zone_hour, $zone_minute) = $zone =~ /\A([+-])(\d{2}):(\d{2})\z/;
        my $offset = ($zone_hour * 60 + $zone_minute) * 60;
        $epoch += $sign eq '+' ? -$offset : $offset;
    }
    return $epoch;
}

sub _loopback_agent_health {
    my $socket = IO::Socket::INET->new(
        PeerAddr => $AGENT_HEALTH_HOST,
        PeerPort => $AGENT_HEALTH_PORT,
        Proto => 'tcp',
        Timeout => 2,
    );
    die "PVN agent health endpoint is unavailable\n" if !$socket;
    my ($raw, $error);
    eval {
        $raw = _with_timeout(2, 'PVN agent health request timed out', sub {
            _write_all($socket, "GET /healthz HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n");
            return _read_all($socket);
        });
    };
    $error = $@;
    close($socket);
    die $error if $error;
    my $response = HTTP::Response->parse($raw);
    die "PVN agent health endpoint returned an invalid response\n"
        if !$response || !$response->code;
    die "PVN agent has not completed a fresh healthy binding scan\n"
        if !$response->is_success;
    my $decoded = eval { decode_json($response->content) };
    die "PVN agent health endpoint returned invalid JSON\n"
        if $@ || ref($decoded) ne 'HASH';
    return $decoded;
}

sub assert_node_ready {
    for my $unit (@REQUIRED_UNITS) {
        my $active = $UNIT_CHECK_OVERRIDE
            ? $UNIT_CHECK_OVERRIDE->($unit)
            : system('/usr/bin/systemctl', 'is-active', '--quiet', $unit) == 0;
        die "required PVN service $unit is not active\n" if !$active;
    }
    my $health = $AGENT_HEALTH_OVERRIDE
        ? $AGENT_HEALTH_OVERRIDE->()
        : _loopback_agent_health();
    die "PVN agent health payload is invalid\n"
        if ref($health) ne 'HASH' || ref($health->{report}) ne 'HASH';
    my $errors = $health->{report}->{errors};
    my $last_error = $health->{last_error};
    die "PVN agent binding scan contains errors\n"
        if !defined($errors) || ref($errors) || "$errors" ne '0'
        || (defined($last_error) && (ref($last_error) || $last_error ne ''));
    my $last_success = _iso8601_epoch($health->{last_success});
    die "PVN agent last-success timestamp is invalid\n"
        if !defined($last_success) || time() - $last_success < -300;
    # A 2xx /healthz response is the authoritative freshness check. The agent
    # derives its deadline from max(3 * configured poll interval, 30 seconds),
    # so duplicating a fixed age here would reject valid slower deployments.
    return 1;
}

sub _ha_managed {
    my ($vmid) = @_;
    return $HA_MANAGED_OVERRIDE->($vmid) ? 1 : 0 if $HA_MANAGED_OVERRIDE;
    require PVE::HA::Config;
    return PVE::HA::Config::service_is_configured("vm:$vmid") ? 1 : 0;
}

sub _ha_runtime_state {
    my ($node) = @_;
    return $HA_RUNTIME_OVERRIDE->($node) if $HA_RUNTIME_OVERRIDE;

    require PVE::RPCEnvironment;
    my $rpcenv = PVE::RPCEnvironment::get();
    die "PVE RPC environment is unavailable\n"
        if !ref($rpcenv) || !$rpcenv->isa('PVE::RPCEnvironment');
    my $origin = $rpcenv->{type} // '';
    my $user = $rpcenv->get_user(1) // '';
    my %known_origin = map { $_ => 1 } qw(cli pub priv ha);
    die "PVE RPC environment type is invalid\n"
        if ref($origin) || !$known_origin{$origin};
    die "PVE RPC user identity is invalid\n"
        if ref($user) || $user eq '' || $user =~ /[\x00-\x1f\x7f]/;
    return { origin => "$origin", user => "$user" } if $origin ne 'ha';

    require PVE::Cluster;
    my $quorate = eval { PVE::Cluster::check_cfs_quorum(); 1 };
    die "PVE HA start has no CFS quorum\n" if !$quorate;

    require PVE::HA::Config;
    my $manager_status = PVE::HA::Config::read_manager_status();
    my $lrm_status = PVE::HA::Config::read_lrm_status($node);
    my $agent_lock = "/etc/pve/priv/lock/ha_agent_${node}_lock";
    my @agent_lock_stat = lstat($agent_lock);
    die "PVE HA agent lock is unavailable\n" if !@agent_lock_stat;

    return {
        origin => "$origin",
        user => "$user",
        quorate => 1,
        manager_status => $manager_status,
        lrm_status => $lrm_status,
        agent_lock => {
            mode => $agent_lock_stat[2],
            mtime => $agent_lock_stat[9],
        },
    };
}

sub _fresh_ha_epoch {
    my ($label, $value, $now) = @_;
    die "$label is invalid\n"
        if !defined($value) || ref($value) || $value !~ /\A[1-9]\d*\z/;
    my $age = $now - int($value);
    die "$label is from the future\n" if $age < -$HA_FUTURE_TOLERANCE;
    die "$label is stale\n" if $age > $HA_STATE_FRESHNESS;
    return int($value);
}

sub _ha_start_proof {
    my ($vmid, $node) = @_;
    my $runtime = _ha_runtime_state($node);
    die "PVE HA runtime proof is invalid\n" if ref($runtime) ne 'HASH';
    my $origin = $runtime->{origin};
    die "PVE HA runtime origin is invalid\n"
        if !defined($origin) || ref($origin)
        || $origin !~ /\A(?:cli|pub|priv|ha)\z/;
    return if $origin ne 'ha';
    die "PVE HA worker identity is invalid\n"
        if ($runtime->{user} // '') ne 'root@pam'
        || !defined($runtime->{quorate}) || ref($runtime->{quorate})
        || "$runtime->{quorate}" ne '1';

    my $manager = $runtime->{manager_status};
    my $lrm = $runtime->{lrm_status};
    my $agent_lock = $runtime->{agent_lock};
    die "PVE HA manager status is invalid\n"
        if ref($manager) ne 'HASH'
        || ref($manager->{service_status}) ne 'HASH'
        || ref($manager->{node_status}) ne 'HASH';
    die "PVE HA LRM status is invalid\n" if ref($lrm) ne 'HASH';
    die "PVE HA agent lock metadata is invalid\n"
        if ref($agent_lock) ne 'HASH'
        || !defined($agent_lock->{mode}) || ref($agent_lock->{mode})
        || !S_ISDIR($agent_lock->{mode});

    my $now = int(time());
    my $manager_epoch = _fresh_ha_epoch(
        'PVE HA manager timestamp', $manager->{timestamp}, $now,
    );
    my $lrm_epoch = _fresh_ha_epoch('PVE HA LRM timestamp', $lrm->{timestamp}, $now);
    my $agent_lock_epoch = _fresh_ha_epoch(
        'PVE HA agent lock timestamp', $agent_lock->{mtime}, $now,
    );

    my $service_id = "vm:$vmid";
    my $service = $manager->{service_status}->{$service_id};
    die "PVE HA manager does not authorize this VM start\n" if ref($service) ne 'HASH';
    my $service_uid = $service->{uid};
    die "PVE HA service command UID is invalid\n"
        if !defined($service_uid) || ref($service_uid)
        || $service_uid !~ /\A[A-Za-z0-9+\/_-]{8,128}\z/;
    die "PVE HA manager does not assign this VM to the local node\n"
        if ($service->{state} // '') ne 'started' || ($service->{node} // '') ne $node;
    die "PVE HA manager does not mark the local node online\n"
        if ($manager->{node_status}->{$node} // '') ne 'online';
    die "PVE HA LRM is not the active local start authority\n"
        if ($lrm->{state} // '') ne 'active' || ($lrm->{mode} // '') ne 'active';

    my %node_states;
    my %valid_node_state = map { $_ => 1 } qw(online maintenance unknown fence gone);
    for my $name (sort keys %{ $manager->{node_status} }) {
        my $state = $manager->{node_status}->{$name};
        die "PVE HA manager contains an invalid node identity\n"
            if $name !~ /\A[A-Za-z0-9][A-Za-z0-9._-]{0,127}\z/;
        die "PVE HA manager contains an invalid node state\n"
            if !defined($state) || ref($state) || !$valid_node_state{$state};
        $node_states{$name} = "$state";
    }

    return {
        origin => 'ha',
        service_id => $service_id,
        manager_epoch => $manager_epoch,
        service_uid => "$service_uid",
        service_node => "$node",
        service_state => 'started',
        node_states => \%node_states,
        lrm_node => "$node",
        lrm_epoch => $lrm_epoch,
        lrm_state => 'active',
        lrm_mode => 'active',
        agent_lock_epoch => $agent_lock_epoch,
    };
}

sub _ha_lifecycle_id {
    my ($vmid, $node, $service_uid) = @_;
    return 'pve-ha-' . sha256_hex(join("\0", int($vmid), $node, $service_uid));
}

sub pre_start {
    my ($vmid, $conf, $migratedfrom) = @_;
    my $nics = _pvn_nics($conf);
    return if !@$nics;
    assert_node_ready();
    my $ha_managed = !$migratedfrom && _ha_managed($vmid);
    my $node = _node_name();
    my $ha_proof = $ha_managed ? _ha_start_proof($vmid, $node) : undef;
    my $payload = {
        vmid => int($vmid),
        node => $node,
        nics => $nics,
        ha_managed => $ha_managed ? JSON::PP::true : JSON::PP::false,
    };
    if ($ha_managed) {
        $payload->{lifecycle_id} = $ha_proof
            ? _ha_lifecycle_id($vmid, $node, $ha_proof->{service_uid})
            : _lifecycle_id('ha-start', $vmid);
        $payload->{ha_proof} = $ha_proof if $ha_proof;
    }
    $payload->{migration_source} = "$migratedfrom" if defined($migratedfrom) && $migratedfrom ne '';
    my $result = _request_bounded_retry('start', $payload, 3);
    die "PVN manager did not mark VM $vmid networking ready\n" if !$result->{ready};
    return $result;
}

sub clone_prepare {
    my ($source_vmid, $target_vmid, $target_node, $source_conf, $target_conf, $snapshot_id) = @_;
    # qemu-server passes the selected snapshot as source_conf/oldconf. Its
    # immutable MACs prove the durable source manifest; target_conf/newconf
    # already contains throwaway random MACs that the manager will replace.
    my $nics = _pvn_nics($source_conf);
    return if !@$nics;
    my $payload = {
        clone_id => _lifecycle_id('clone', $source_vmid, $target_vmid),
        source_vmid => int($source_vmid),
        source_node => _node_name(),
        source_template => $source_conf->{template} ? JSON::PP::true : JSON::PP::false,
        target_vmid => int($target_vmid),
        target_node => "$target_node",
        nics => $nics,
    };
    if (defined($snapshot_id) && $snapshot_id ne '') {
    $payload->{snapshot_id} = "$snapshot_id";
        $payload->{snapshot_epoch} = snapshot_epoch($source_conf);
    }
    my $transaction = _request_bounded_retry('clone_prepare', $payload, 3);
    eval { _rewrite_clone_nics($target_conf, $nics, $transaction->{ports}); };
    if (my $rewrite_error = $@) {
        eval { _request_bounded_retry('clone_abort', $transaction, 3); };
        my $abort_error = $@;
        die "$rewrite_error; PVN clone prepare rollback failed: $abort_error"
            if $abort_error;
        die $rewrite_error;
    }
    return $transaction;
}

sub clone_commit {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('clone_commit', $transaction, 3);
}

sub clone_abort {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('clone_abort', $transaction, 3);
}

sub template_prepare {
    my ($vmid, $conf) = @_;
    my $nics = _pvn_nics($conf);
    return if !@$nics;
    return _request_bounded_retry('template_prepare', {
        lifecycle_id => _lifecycle_id('template', $vmid),
        vmid => int($vmid),
        nics => $nics,
    }, 3);
}

sub template_commit {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('template_commit', $transaction, 3);
}

sub template_abort {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('template_abort', $transaction, 3);
}

sub snapshot_create {
    my ($vmid, $snapshot_id, $conf) = @_;
    my $nics = _pvn_nics($conf);
    return if !@$nics;
    return _request_bounded_retry('snapshot_create', {
        lifecycle_id => _lifecycle_id('snapshot', $vmid, $snapshot_id),
        vmid => int($vmid),
        snapshot_id => "$snapshot_id",
        snapshot_epoch => snapshot_epoch($conf),
        nics => $nics,
    }, 3);
}

sub snapshot_prepare {
    my ($vmid, $snapshot_id, $conf, $action) = @_;
    die "PVE snapshot transition action is invalid\n"
        if !defined($action) || ($action ne 'rollback' && $action ne 'delete');
    my $nics = _pvn_nics($conf);
    return if !@$nics;
    my $payload = {
        lifecycle_id => _lifecycle_id("snapshot-$action", $vmid, $snapshot_id),
        action => "$action",
        vmid => int($vmid),
        snapshot_id => "$snapshot_id",
        snapshot_epoch => snapshot_epoch($conf),
    };
    $payload->{nics} = $nics if $action eq 'rollback';
    return _request_bounded_retry('snapshot_prepare', $payload, 3);
}

sub snapshot_commit {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('snapshot_commit', $transaction, 3);
}

sub snapshot_abort {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('snapshot_abort', $transaction, 3);
}

sub snapshot_cleanup {
    my ($vmid, $snapshot_id, $snapshot_epoch) = @_;
    die "PVE snapshot cleanup has no immutable snapshot epoch\n"
        if !defined($snapshot_epoch) || $snapshot_epoch !~ /\A[1-9]\d*\z/;
    return _request_bounded_retry('snapshot_cleanup', {
        vmid => int($vmid),
        snapshot_id => "$snapshot_id",
        snapshot_epoch => int($snapshot_epoch),
    }, 3);
}

sub destroy_capture {
    my ($vmid, $conf) = @_;
    my $nics = _pvn_nics($conf);
    my $snapshots = _snapshot_refs($conf);
    return if !@$nics && !@$snapshots;
    return _request_bounded_retry('destroy_capture', {
        lifecycle_id => _lifecycle_id('destroy', $vmid),
        vmid => int($vmid),
        nics => $nics,
        template => $conf->{template} ? JSON::PP::true : JSON::PP::false,
        snapshots => $snapshots,
    }, 3);
}

sub destroy_commit {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('destroy_commit', $transaction, 3);
}

sub destroy_abort {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('destroy_abort', $transaction, 3);
}

sub _read_local_mtu {
    return int($LOCAL_MTU_OVERRIDE->()) if $LOCAL_MTU_OVERRIDE;
    open(my $handle, '<', '/sys/class/net/br-int/mtu')
        or die "read local br-int MTU: $!\n";
    my $value = <$handle>;
    close($handle);
    die "local br-int MTU is invalid\n" if !defined($value) || $value !~ /\A(\d+)\s*\z/;
    return int($1);
}

sub _read_remote_mtu {
    my ($migration) = @_;
    return int($REMOTE_MTU_OVERRIDE->($migration)) if $REMOTE_MTU_OVERRIDE;
    die "PVE migration has no restricted remote command transport\n"
        if ref($migration->{rem_ssh}) ne 'ARRAY' || !@{ $migration->{rem_ssh} };
    require PVE::Tools;
    my $output = '';
    PVE::Tools::run_command(
        [@{ $migration->{rem_ssh} }, 'cat', '/sys/class/net/br-int/mtu'],
        outfunc => sub { $output .= "$_[0]\n"; },
        errfunc => sub { },
    );
    die "target br-int MTU is invalid\n" if $output !~ /\A(\d+)\s*\z/;
    return int($1);
}

sub migration_begin {
    my ($migration, $vmid, $conf) = @_;
    my $nics = _pvn_nics($conf);
    return if !@$nics;
    my $source = _node_name();
    my $target = $migration->{node};
    die "PVE migration target node is unavailable\n"
        if !defined($target) || $target !~ /\A[A-Za-z0-9][A-Za-z0-9._-]{0,127}\z/;
    my $online = $migration->{running} ? JSON::PP::true : JSON::PP::false;
    return _request_bounded_retry('migration_begin', {
        lifecycle_id => _lifecycle_id('migration', $vmid, $source, $target),
        vmid => int($vmid),
        source_node => $source,
        target_node => "$target",
        online => $online,
        source_stopped => $online ? JSON::PP::false : JSON::PP::true,
        source_mtu => _read_local_mtu(),
        target_mtu => _read_remote_mtu($migration),
        nics => $nics,
    }, 3);
}

sub migration_finalize {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('migration_finalize', $transaction, 3);
}

sub migration_abort {
    my ($transaction) = @_;
    return if !$transaction;
    return _request_bounded_retry('migration_abort', $transaction, 3);
}

1;
