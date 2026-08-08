package PVE::API2::Qemu;
use PVE::QemuServer::DBusVMState;

sub clone_fixture {
            my ($vmid, $newid, $target, $localnode, $oldconf, $newconf, $snapname, $conffile) = @_;
            my ($storecfg, $vollist, $pool, $running, $authuser);
            my $newvollist = [];
            my $jobs = {};
            eval {
                local $SIG{INT} = local $SIG{TERM} = local $SIG{QUIT} = local $SIG{HUP} =
                    sub { die "interrupted by signal\n"; };

                PVE::Storage::activate_volumes($storecfg, $vollist, $snapname);
                if ($target) {
                    if (!$running) {
                        PVE::Storage::deactivate_volumes($storecfg, $vollist, $snapname);
                    }
                    my $newconffile = PVE::QemuConfig->config_file($newid, $target);
                    die "rename failed" if !rename($conffile, $newconffile);
                }
                PVE::AccessControl::add_vm_to_pool($newid, $pool) if $pool;
            };
            if (my $err = $@) {
                eval {
                    PVE::QemuServer::BlockJob::qemu_blockjobs_cancel(vm_qmp_peer($vmid), $jobs);
                };
                foreach my $volid (@$newvollist) {
                    PVE::Storage::vdisk_free($storecfg, $volid);
                }
                unlink $conffile; # avoid races -> last thing before die
                die "clone failed: $err";
            }

            return;
}

sub snapshot_create_fixture {
            my ($vmid, $snapname, $param, $vmconf) = @_;
            PVE::QemuConfig->snapshot_create(
                $vmid, $snapname, $param->{vmstate}, $param->{description},
            );
}

sub snapshot_rollback_fixture {
            my ($vmid, $snapname) = @_;
            PVE::QemuConfig->snapshot_rollback($vmid, $snapname);
}

sub snapshot_delete_fixture {
            my ($vmid, $snapname, $param, $authuser) = @_;
            PVE::Cluster::log_msg('info', $authuser, "delete snapshot VM $vmid: $snapname");
            PVE::QemuConfig->snapshot_delete($vmid, $snapname, $param->{force});
}

sub template_fixture {
                    my ($vmid, $conf, $disk) = @_;
                    $conf->{template} = 1;
                    PVE::QemuConfig->write_config($vmid, $conf);
                    PVE::QemuServer::template_create($vmid, $conf, $disk);
}

sub destroy_fixture {
                    my ($storecfg, $vmid, $skiplock, $purge_unreferenced) = @_;
                    PVE::QemuServer::destroy_vm(
                        $storecfg,
                        $vmid,
                        $skiplock,
                        { lock => 'destroyed' },
                        $purge_unreferenced,
                    );
                    PVE::AccessControl::remove_vm_access($vmid);
                    PVE::Firewall::remove_vmfw_conf($vmid);
                    PVE::QemuConfig->destroy_config($vmid);
}

1;
