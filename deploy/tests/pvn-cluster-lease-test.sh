#!/bin/sh
set -eu

TEST_DIR=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
REPO=$(CDPATH= cd -P "$TEST_DIR/../.." && pwd)
LEASE=$REPO/deploy/scripts/pvn-cluster-lease
WORK=$(mktemp -d)

cleanup() {
    rm -rf "$WORK"
}
trap cleanup 0 HUP INT TERM

mkdir -m 0700 "$WORK/leases" "$WORK/perl"
mkdir -p "$WORK/perl/PVE"
cat > "$WORK/perl/PVE/Cluster.pm" <<'EOF'
package PVE::Cluster;
use strict;
use warnings;
sub cfs_update { return 1; }
sub cfs_lock_domain {
    my ($domain, $timeout, $code) = @_;
    die "unsafe test domain\n" if $domain !~ /\Adomain-/ && $domain !~ /\Apvn-lease-/;
    return $code->();
}
1;
EOF

export PERL5LIB=$WORK/perl
export PVN_CLUSTER_LEASE_DIR=$WORK/leases
TOKEN=0123456789abcdef0123456789abcdef0123456789abcdef
OTHER=abcdef0123456789abcdef0123456789abcdef0123456789

printf '%s\n' \
    "{\"domain\":\"topology\",\"node\":\"prox1\",\"token\":\"$TOKEN\"}" |
    "$LEASE" acquire topology "$TOKEN" > "$WORK/acquire.out"
[ -f "$WORK/leases/pvn-topology.lease" ]
[ "$(stat -c %a "$WORK/leases/pvn-topology.lease")" = 600 ]
"$LEASE" show topology > "$WORK/show.out"
grep -Fq '"node":"prox1"' "$WORK/show.out"

if printf '%s\n' \
    "{\"domain\":\"topology\",\"node\":\"prox2\",\"token\":\"$OTHER\"}" |
    "$LEASE" acquire topology "$OTHER" > "$WORK/double.out" 2>&1
then
    echo "pvn-cluster-lease allowed a second owner" >&2
    exit 1
fi
if "$LEASE" release topology "$OTHER" > "$WORK/wrong-release.out" 2>&1; then
    echo "pvn-cluster-lease released another owner's token" >&2
    exit 1
fi
[ -f "$WORK/leases/pvn-topology.lease" ]
"$LEASE" release topology "$TOKEN"
[ ! -e "$WORK/leases/pvn-topology.lease" ]

if printf '%s\n' \
    "{\"domain\":\"other\",\"token\":\"$TOKEN\"}" |
    "$LEASE" acquire topology "$TOKEN" > "$WORK/domain-mismatch.out" 2>&1
then
    echo "pvn-cluster-lease accepted a mismatched owner domain" >&2
    exit 1
fi

echo "pvn-cluster-lease tests passed"
