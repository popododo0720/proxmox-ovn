# PVN 0.3 architecture

PVN 0.3 is a breaking, projectless control plane for one Proxmox cluster. It
uses Proxmox as the compute manager and OVN as the network control and data
plane. Every Proxmox node runs the same PVN package.

## Management path

```text
browser
  |
  | HTTPS :8006 (PVE ticket, CSRF, RBAC)
  v
Proxmox ExtJS + /api2/json/pvn
  |
  | Unix socket /run/pvn-api/manager.sock
  v
local pvn-manager
  |
  | TLS OVSDB :6645
  v
PVN_Control Raft cluster
```

The browser never connects to a PVN-specific TCP port. Proxmox owns TLS and
authentication. The Proxmox API adapter accepts only fixed PVN routes, checks
the existing PVE ticket, CSRF token and RBAC, and forwards no browser
credentials to the Unix socket.

The browser socket is separate from `/run/pvn/manager.sock`. The latter remains
the runtime-only socket used by `pvn-agent` and must not expose management CRUD.

## Cluster roles

On a three-node PVE cluster, all three nodes are central voters for
`PVN_Control`, `OVN_Northbound`, and `OVN_Southbound`. They are peers, not three
independent control planes. Each node also runs a local manager, OVS, and
`ovn-controller`; clustered northd selects the active worker while the others
remain ready.

A standalone PVE host runs the same components with standalone databases. No
special single-node product mode is required.

## Resource scope

There is one cluster-global PVN management domain. `Project`, PVE pool mapping,
`project_id`, and provider-network `shared` do not exist. Network, router and
security-group names are globally unique. UUIDs remain immutable API keys, but
the UI shows names by default and exposes UUIDs in the Details window.

The reserved default security group is also cluster-global. Its self-ingress
rule therefore treats every port using that group as one routed trust domain.
Operators that need narrower trust must create explicit security groups.

## Data plane and north-south

- the existing PVE management NIC and bridge remain the management underlay;
- a dedicated Geneve interface carries tenant overlays;
- a provider interface/bridge carries external/provider networks;
- logical switches, routers, ports, DHCP, ACLs and NAT live in OVN;
- north-south networking follows the OpenStack provider/external-network model,
  without making BGP a requirement.

PVN never creates or attaches a physical provider bridge implicitly. The
topology installer validates and pins the operator-selected interfaces before
the control plane is activated.

## Upgrade boundary

PVN 0.2 and 0.3 managers cannot share a control database. A normal rolling
update refuses this schema transition, and PVN 0.3 does not ship a
state-preserving 0.2 migration tool. Existing production deployments must stay
on 0.2 until such a migrator is available.

A disposable lab may instead perform an explicitly destructive reset after a
verified backup: stop every PVN writer, install 0.3 while inactive, remove all
three PVN/OVN databases, the shared control ledger/configuration, every node's
PVN PKI, and the seed CA, then bootstrap all voters as a new PVN installation.
That reset creates a new PVN cluster UUID and new PKI. The separately verified
PVE/Corosync state, physical underlay, and topology ledger are not reset.
