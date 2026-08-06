# PVN

PVN is a Proxmox VE 9 network manager backed by Open Virtual Network (OVN).
It exposes an NSX/Neutron-style cloud networking model while keeping Proxmox
as the compute control plane.

The installer puts the same PVN package on every online Proxmox node. Each
node runs the manager API/UI, a local TAP binding agent, Open vSwitch, and
`ovn-controller`. The current bootstrap supports either one standalone PVE
node or any quorate, odd-sized PVE cluster with at least three nodes. In
clustered mode every PVE node is a database voter and transport node;
even-sized clusters are rejected before topology changes.

## Initial feature set

- PVE Pool to PVN Project mapping
- Geneve tenant networks and IPv4 subnets
- OVN native DHCP
- logical routers, centralized SNAT, and floating IPs
- flat and VLAN provider networks
- stateful security groups with a reserved Neutron-style project default
- running VM NIC attach and detach
- embedded `PVN` page in the Proxmox web interface

Normal PVN tables, selectors, confirmations, and bounded error messages resolve
known resource references to human names. Unknown UUIDs are redacted there;
open **Details** when an operator needs the full, copyable internal UUID.

PVE built-in SDN, BGP, IPv6, LXC, load balancing, metadata service, and live
migration coordination are intentionally outside the first release.

## Install

Run this as `root` on any node in the target PVE cluster:

```sh
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh) && bash -c "$pvn_bootstrap"
```

The `&&` prevents Bash from executing an incomplete response when `curl`
fails; the inner Bash still inherits the terminal used by interactive prompts.

The script discovers native PVE membership, requires every member to be online
and quorate, downloads the release assets over HTTPS, verifies their SHA-256
manifest, and runs a read-only preflight. Type the exact cluster name to install
the package on every node. It then offers to configure the three-NIC topology
and activate PVN; answering `n` leaves the package and Proxmox UI extension
installed but all PVN/OVN services inactive.

Full setup needs three distinct IPv4 networks:

- the existing PVE management network;
- a dedicated Geneve network; and
- a provider network whose outer OpenStack ports permit arbitrary guest/router
  MAC and IP addresses.

The provider acknowledgement is deliberately required because full setup
removes the provider NIC's host IP and attaches that NIC to `br-provider`.
`br-int` and `br-provider` are created automatically. The installer migrates a
Corosync ring off the Geneve NIC to the management network before changing
host networking.

Package-only non-interactive install:

```sh
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh) && \
  bash -c "$pvn_bootstrap" pvn-install.sh install --apply --confirm CLUSTER_NAME
```

Non-interactive full setup:

```sh
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh) && \
  bash -c "$pvn_bootstrap" pvn-install.sh install --apply --confirm CLUSTER_NAME --full \
  --geneve-cidr 192.168.100.0/24 \
  --provider-cidr 192.168.200.0/24 \
  --guest-mtu 1300 \
  --provider-port-ready OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP
```

`preflight` and an `install` invocation without `--apply` are always read-only.
Cluster SSH uses Proxmox's native root key and per-node host-key pins; password
authentication is disabled. Cluster-wide leases prevent two nodes from
starting the same install, topology, or control-plane operation concurrently.

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
