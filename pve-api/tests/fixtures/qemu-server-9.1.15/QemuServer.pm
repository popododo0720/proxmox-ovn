package PVE::QemuServer;
use PVE::QemuServer::DBusVMState;

sub vm_start_nolock {
    my ($storecfg, $vmid, $conf, $params, $migrate_opts) = @_;
    my $migratedfrom = $migrate_opts->{migratedfrom};
    PVE::GuestHelpers::exec_hookscript($conf, $vmid, 'pre-start', 1);
}

1;
