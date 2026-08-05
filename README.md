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

## Development

```sh
make test
make build
```

The detailed architecture and lab procedures live under `docs/` as the
corresponding components are implemented.

