# PVN architecture

PVN keeps Proxmox VE as the compute control plane and uses stock OVN as the
network data/control plane. Every PVE node receives the same `pvn-node`
package. There is no container runtime and no separate appliance VM.

## Per-node processes

- `pvn-manager`: HTTPS API and static web UI on port 8443. All managers are
  active and reconcile the same desired state.
- `pvn-agent`: observes local QEMU TAP interfaces and binds only ports for
  which the manager returns an exact `(node, vmid, netN)` assignment.
- Open vSwitch and `ovn-controller`: implement the local datapath.
- `pvn-control-db`, OVN NB/SB, and `ovn-northd`: installed everywhere but
  enabled only on selected central voters.

The central placement policy is deterministic:

| Online eligible nodes | Central voters | Mode |
| --- | ---: | --- |
| 1-2 | 1 | standalone; a two-node cluster has no central HA |
| 3-4 | 3 | Raft |
| 5 or more | 5 | Raft |

Existing healthy voters are retained. Increasing from standalone to Raft is
an explicit operation because it changes the on-disk database service model.

## Source of truth

`PVN_Control` is the desired-state database. It contains projects, tenant and
provider networks, IPv4 subnets and allocations, ports, routers, floating IPs,
security groups, nodes, and durable operations. OVN Northbound is realized
state. Reconcilers are revision- and idempotency-aware; no transaction is
assumed to span the two databases.

PVE pools map to PVN projects. PVN checks the authenticated user's effective
PVE permissions before each action:

- read: `SDN.Audit`
- network resource changes: `SDN.Allocate`
- VM attachment: network `SDN.Use` at `/sdn/zones/pvn/<network-id>` (or the
  project pool/global scope) and VM `VM.Config.Network`
- central, provider, and gateway changes: global administrator permission

## VM port lifecycle

Attach is deliberately fail-closed:

1. Reserve the MAC and IPv4 address and create a disabled logical switch port.
2. Add the QEMU NIC as `virtio,bridge=br-int,firewall=0,link_down=1`, using the
   PVE config digest for optimistic concurrency.
3. The local agent sees the TAP and resolves its exact PVN assignment.
4. Set `iface-id`, `iface-id-ver`, and requested-chassis OVS external IDs.
5. Wait for OVN binding (`up` and `ovn-installed`) and clear `link_down`.

Detach performs the reverse sequence. An unknown TAP remains unbound. Direct
edits that conflict with PVN ownership are reported as drift and are not
silently overwritten.

## North-south networking

The first release follows the centralized OpenStack/Neutron OVN model.
Selected PVE nodes are gateway chassis. Logical routers provide centralized
SNAT and floating IPs toward a flat or VLAN provider network. The physical
provider bridge must be prepared by the operator; PVN validates it but never
rewires physical interfaces.

The default assumes a 1500-byte underlay and advertises a 1400-byte tenant MTU
for Geneve. BGP, IPv6, metadata service, load balancers, LXC, and coordinated
PVE live migration/HA moves are outside v1.

## UI and authentication

The Debian package adds one marked script tag after `pvemanagerlib.js`. The
loader contributes a `PVN` Datacenter menu and embeds the same-node manager UI.
It is not a stable PVE plugin ABI, so the patch is version-gated, idempotent,
and becomes a no-op on unknown PVE versions.

The browser sends the existing `PVEAuthCookie` to the same host on port 8443.
`pvn-manager` validates it against the local PVE permissions endpoint and then
issues a short-lived PVN session plus a PVN CSRF token. There is no anonymous
API and no second login. Privileged calls proxied to PVE use strict
origin/source/nonce checks and PVE's existing CSRF and audit path.

## Ports

| Port | Purpose |
| ---: | --- |
| 8006 | existing PVE API/UI |
| 8443 | PVN manager HTTPS |
| 6641 | OVN Northbound client |
| 6642 | OVN Southbound client |
| 6643 | OVN Northbound Raft |
| 6644 | OVN Southbound Raft |
| 6645 | PVN Control client |
| 6646 | PVN Control Raft |

Management and Raft listeners must be protected by host firewalls and PVN PKI.
