package PVE::QemuMigrate;
use PVE::QemuServer::DBusVMState;

sub phase1 {
    my ($self, $vmid) = @_;

    $self->log('info', "starting migration of VM $vmid to node '$self->{node}' ($self->{nodeip})");

    my $conf = $self->{vmconf};
    # set migrate lock in config file
}

sub phase1_cleanup {
    my ($self, $vmid, $err) = @_;

    $self->log('info', "aborting phase 1 - cleanup resources");
}

sub phase2_cleanup {
    my ($self, $vmid, $err) = @_;

    return if !$self->{errors};
    $self->{phase2errors} = 1;
}

sub phase3_cleanup {
    my ($self, $vmid, $err) = @_;
    # always stop local VM with nocheck, since config is moved already
    PVE::QemuServer::vm_stop($self->{storecfg}, $vmid, 1, 1);
    # clear migrate lock
    $self->cmd_logerr(['qm', 'unlock', $vmid]);
    if ($self->{opts}->{remote} && $self->{opts}->{delete}) {
        eval { PVE::QemuServer::destroy_vm($self->{storecfg}, $vmid, 1, undef, 0) };
        warn "Failed to remove source VM - $@\n" if $@;
    }
}

sub final_cleanup {
    my ($self, $vmid) = @_;
}

1;
