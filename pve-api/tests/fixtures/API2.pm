package PVE::API2;

use strict;
use warnings;

use PVE::RESTHandler;
use base qw(PVE::RESTHandler);

use PVE::API2::Cluster;
use PVE::API2::Nodes;
use PVE::API2::Pool;
use PVE::API2::AccessControl;
use PVE::API2::Storage::Config;

__PACKAGE__->register_method({
    subclass => "PVE::API2::Cluster",
    path => 'cluster',
});

__PACKAGE__->register_method({
    subclass => "PVE::API2::Pool",
    path => 'pools',
});

1;
