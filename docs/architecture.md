# PVN architecture

PVN keeps Proxmox VE as the compute control plane and uses stock OVN as the
network data/control plane. Every PVE node receives the same `pvn-node`
package. There is no container runtime and no separate appliance VM.

## Per-node processes

- `pvn-manager`: local management and reconcile API over two Unix sockets. All
  managers are active and reconcile the same desired state.
- Proxmox API/UI adapter: native ExtJS screens and the authenticated
  `/api2/json/pvn` route on the existing PVE port 8006.
- `pvn-agent`: observes local QEMU TAP interfaces and binds only ports for
  which the manager returns an exact `(node, vmid, netN)` assignment.
- Open vSwitch and `ovn-controller`: implement the local datapath.
- `pvn-control-db`, OVN NB/SB, and `ovn-northd`: installed everywhere and
  enabled on every member by the supported automated bootstrap.

The current automated bootstrap has one deterministic placement policy:

| Online PVE nodes | Central voters | Mode |
| --- | ---: | --- |
| 1 | 1 | standalone |
| 3, 5, 7, ... | every node | Raft |

Even-sized clusters are rejected by the full-setup compatibility preflight
before topology changes. All listed members of a supported odd-sized cluster
become central voters and transport nodes. Changing membership after bootstrap
remains an explicit operator workflow; the package-only installer remains
usable without activating PVN.

## Source of truth

`PVN_Control` is the desired-state database. It contains tenant and provider
networks, IPv4 subnets and allocations, ports, routers, floating IPs, security
groups, nodes, and durable operations. OVN Northbound is realized
state. Reconcilers are revision- and idempotency-aware; no transaction is
assumed to span the two databases.

The cluster has one reserved default security group. A newly provisioned port
receives that group when the request omits `security_group_ids`; an explicit
selection replaces the default. The reserved group and its baseline rules are
ordinary list results in the manager API, so operators should expect to see
them alongside operator-created policy. The managed baseline cannot be
weakened. Its self-ingress rule makes all ports using the default group one
routed trust domain; use explicit groups when narrower trust is required.

The manager's operator-facing deployment name comes from a systemd credential
copy of `/etc/pve/.members`: clustered nodes use `cluster.name`, while a
standalone node uses `standalone-NODENAME`. The UUID in `cluster.id` remains
the durable PVN installation identity used by configuration and recovery
tooling; it is not used as the normal display label or confirmation text.
The production systemd unit pins the credential path directly on the manager
command line, and the bounded parser pins the current PVE 9 clustered or exact
standalone `.members` shape. Unknown fields, malformed identity metadata, or a
non-regular credential file fail manager startup instead of falling back to
the installation UUID.

PVN is one cluster-global administration domain and does not map PVE pools to
network projects. The Proxmox API adapter checks the authenticated user's
effective PVE permissions before each action:

- read: `SDN.Audit`
- network resource changes: `SDN.Allocate`
- VM attachment: global/network `SDN.Use` and VM `VM.Config.Network`
- central, provider, and gateway changes: global administrator permission

## Network resource model and UI

The control API keeps three different objects because they have different
lifecycles, while the Proxmox UI presents them in one **Networks** workspace:

- a **Logical Network** is an OVN logical switch. Tenant subnets and VM ports
  belong here; an external logical network also references a provider network;
- a **Provider Network** groups one physical north-south domain. Its detail
  view owns segments, external logical networks, and floating IP inventory;
- a **Segment** describes how that provider domain reaches the underlay:
  physical-network mapping plus flat or VLAN encapsulation. One segment is the
  provider's active default for OVN localnet realization.

Logical-network selection filters its subnet list, and subnet selection filters
the corresponding ports. Router and security-group screens use the same
master-detail pattern for interfaces and rules. Subnet allocation pools are the
shared IPv4 range used by OVN DHCP and PVN IPAM; leaving the range empty uses
the automatically derived usable CIDR range.

When DHCP is enabled, a subnet may also supply IPv4 resolvers, one guest DNS
domain, and an ordered search-domain list. The renderer maps these to OVN
DHCPv4 `dns_server`, `domain_name`, and `domain_search_list` options. DNS
values remain subnet-owned; they are not host resolver settings and do not
create an OVN authoritative DNS zone.

Additional static routes are router-owned pairs of destination CIDR and
next-hop IPv4 address. A next hop must be on-link through exactly one
non-deleting interface of that router (including its external gateway
attachment), which gives the renderer one deterministic output port. PVN owns
the external router's `0.0.0.0/0` route, so an operator route cannot replace or
duplicate it. Each additional route has a content-derived identity; reconcile
updates its exact destination, next hop, output port, ownership metadata, and
parent attachment, and removes stale rows.

These options do not change the `PVN_Control` 2.0 schema. Existing subnet
`dns_nameservers` storage remains authoritative for resolver addresses;
DNS domain/search metadata uses reserved `pvn:*` subnet `external_ids`, and
the bounded static-route JSON uses reserved router `external_ids`. Missing
keys decode as empty values, preserving existing databases.

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

### PVE QEMU lifecycle boundary

The package version-gates and signature-gates three `qemu-server` 9.1.15 files:
`PVE/QemuServer.pm`, `PVE/QemuMigrate.pm`, and `PVE/API2/Qemu.pm`. It inserts
marked calls to `PVN::ComputeLifecycle` under one root-owned injector lock and
an atomic replacement journal, then requires every patched Perl file to
compile. An unknown or modified signature fails package configuration; a
recorded interrupted replacement is recovered before another injection
attempt. PVN does not use per-VM hookscripts; its pre-start fence runs before,
and preserves the ordering of, an existing user hookscript.

The Perl module sends bounded, idempotent requests over the fixed
`/run/pvn-compute/manager.sock` Unix socket. The manager accepts only a local
UID-0 peer, pins the request to that PVE node, and never exposes this API to the
browser. Only NICs whose exact PVE bridge is `br-int` participate. Before such
a VM starts, the hook requires the local PVN runtime and node-readiness units,
a successful agent binding scan, and a manager decision for the complete NIC
set.

Compute changes use durable per-VM transactions:

- an ordinary start is rejected while any incompatible lifecycle is active;
  dual-chassis ports require the exact fresh incoming-migration transaction;
- migration begins on the source, passes `migratedfrom` to target pre-start,
  and finalizes only after source QEMU/configuration cleanup. Failures before
  completed source cleanup abort; a finalization failure preserves the durable
  intent and reports a task error. An expired or ambiguous move remains fenced
  for explicit completion rather than being inferred from an ordinary start;
- an HA-managed VM starts only from the root PVE HA worker with fresh quorate
  CRM assignment, active LRM state, node state, service command UID, and agent
  lock proof. A manual UI/API start of that HA-managed VM has no such proof and
  fails closed;
- clone, template conversion, snapshot rollback/delete, and destroy use
  prepare/capture followed by exact commit or abort. Snapshot identity includes
  immutable `snaptime`; clone ports stay disconnected until commit; and
  response-loss paths preserve their durable intent instead of guessing a
  rollback.

Snapshot creation is recorded only after PVE creates the snapshot. If that
recording fails and PVE can definitely remove the new snapshot, an idempotent
cleanup fences that exact name-and-`snaptime` generation. Destroy captures live
ports plus PVN-bearing snapshot manifests and commits only after the complete
PVE cleanup, so templates and snapshot-only identities are not orphaned.

## North-south networking

The first release follows the centralized OpenStack/Neutron OVN model.
Selected PVE nodes are gateway chassis. Logical routers provide centralized
SNAT and floating IPs toward a flat or VLAN provider network. The manager
runtime validates provider wiring and never rewires physical interfaces. The
separately confirmed full-setup installer may create `br-provider` and move the
explicitly selected uplink into it after its destructive topology preflight;
operators using package-only or manual setup must prepare that bridge instead.

The default assumes a 1500-byte underlay and advertises a 1400-byte tenant MTU
for Geneve. BGP, IPv6, metadata service, load balancers, and LXC are outside
v1. Coordinated QEMU migration and HA starts are limited to the exact packaged
PVE lifecycle boundary described above.

## UI and authentication

The Debian package adds one marked script tag after `pvemanagerlib.js`. The
loader contributes a native `PVN` Datacenter menu and ExtJS resource panels.
It is not a stable PVE plugin ABI, so the patch is version-gated, idempotent,
and becomes a no-op on unknown PVE versions.

The browser uses only the existing PVE origin on port 8006. Proxmox validates
its ticket, CSRF token and RBAC before a fixed PVN route is forwarded over a
browser-only Unix socket. No PVE cookie, authorization header or CSRF token is
forwarded to `pvn-manager`; there is no anonymous API, second login, iframe,
cross-origin request, or PVN-specific browser session.

## Ports

| Port | Purpose |
| ---: | --- |
| 8006 | existing PVE API/UI |
| 6641 | OVN Northbound client |
| 6642 | OVN Southbound client |
| 6643 | OVN Northbound Raft |
| 6644 | OVN Southbound Raft |
| 6645 | PVN Control client |
| 6646 | PVN Control Raft |

Management and Raft listeners must be protected by host firewalls and PVN PKI.
