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
