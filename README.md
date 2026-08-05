# PVN

PVN is a Proxmox VE 9 network manager backed by Open Virtual Network (OVN).
It exposes an NSX/Neutron-style cloud networking model while keeping Proxmox
as the compute control plane.

The target deployment installs the same PVN node package on every Proxmox
node. Each node runs the manager API/UI, a local TAP binding agent, Open
vSwitch, and `ovn-controller`. A selected odd set of one, three, or five nodes
also hosts the clustered PVN, OVN Northbound, and OVN Southbound databases.

## Initial feature set

- PVE Pool to PVN Project mapping
- Geneve tenant networks and IPv4 subnets
- OVN native DHCP
- logical routers, centralized SNAT, and floating IPs
- flat and VLAN provider networks
- stateful security groups
- running VM NIC attach and detach
- embedded `PVN` page in the Proxmox web interface

PVE built-in SDN, BGP, IPv6, LXC, load balancing, metadata service, and live
migration coordination are intentionally outside the first release.

## Safe cluster package stage

The intended release entry point is a one-line installer hosted with the
release artifacts:

```sh
bash -c "$(curl -fsSL https://RELEASE_HOST/pvn-install.sh)"
```

`RELEASE_HOST` is a placeholder: this repository does not currently publish a
public installer URL. A locally hosted release directory must serve the
versioned `pvn-node` DEB, `pvn-cluster-install`, and `SHA256SUMS`. The bootstrap
downloads all three over HTTPS and verifies the DEB and cluster installer
against `SHA256SUMS`. With no arguments it prompts for the inventory and SSH
key paths and runs the read-only cluster preflight first. It then asks for the
exact PVE cluster name; pressing Enter stops without installing, while entering
the matching name is the explicit apply gate. The explicit hosted install form
is still a dry run unless explicitly applied:

```sh
bash -c "$(curl -fsSL https://RELEASE_HOST/pvn-install.sh)" pvn-install.sh \
  install --inventory /path/to/inventory --identity /root/.ssh/id_ed25519 \
  --release-base-url https://RELEASE_HOST/releases/v0.1.1
# Re-run only after reviewing the dry-run output:
bash -c "$(curl -fsSL https://RELEASE_HOST/pvn-install.sh)" pvn-install.sh \
  install --inventory /path/to/inventory --identity /root/.ssh/id_ed25519 \
  --release-base-url https://RELEASE_HOST/releases/v0.1.1 \
  --apply --confirm CLUSTER_ID
```

From a source checkout, build the DEB and run the underlying phased installer
directly:

```sh
make deb
PVN_INVENTORY=deploy/inventory/pve-cluster-192.168.0.example
PVN_IDENTITY=/root/.ssh/id_ed25519
PVN_DEB=dist/pvn-node_0.1.1_amd64.deb

./deploy/scripts/pvn-cluster-install preflight \
  --inventory "$PVN_INVENTORY" --identity "$PVN_IDENTITY"
./deploy/scripts/pvn-cluster-install install \
  --inventory "$PVN_INVENTORY" --identity "$PVN_IDENTITY" --deb "$PVN_DEB"
# Re-run the printed command only after reviewing every node:
./deploy/scripts/pvn-cluster-install install \
  --inventory "$PVN_INVENTORY" --identity "$PVN_IDENTITY" --deb "$PVN_DEB" \
  --apply --confirm CLUSTER_ID
```

`preflight` is always read-only, and `install` is a dry run unless both
`--apply` and the exact cluster-name confirmation are supplied. SSH is root,
public-key-only, non-interactive, and strict about known host keys. The apply
stage distributes the verified package, masks Debian's aggregate OVN units,
installs the same package on every node, and verifies that PVN remains
inactive.

This stage never chooses Geneve or control addresses, creates an OVS bridge,
attaches a provider uplink, changes host networking, creates PKI or
configuration, initializes central databases, or creates activation markers.
Topology preparation, central bootstrap, and node activation remain separate
operator-controlled phases.

Package installation does not activate PVN networking. After staging shared
config, node-local config, PKI, and operator-created OVS bridges, create the
root-owned `/etc/pvn/node-enabled` marker and enable `pvn-node.target` on every
online node. Central services use a separate marker and
`pvn-central.target`; in a three-node cluster, all three nodes normally become
Raft voters. Shared pmxcfs configuration alone never opts a node in.

## Development

```sh
make test
make build
make package-check
make deb
```

See [architecture](docs/architecture.md), [operations](docs/operations.md),
and [development](docs/development.md) for the concrete topology, voter
policy, bootstrap flow, security boundary, and current scope.
