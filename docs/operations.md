# PVN operations

PVN currently targets Proxmox VE 9 only. Install the same `pvn-node` Debian
package on every online node before enabling the cluster. A mixed cluster is
reported unhealthy and new PVN port attachments should be blocked.

## Host preparation

Each node needs:

- a stable management address and a distinct Geneve encapsulation address;
- Open vSwitch `br-int`;
- an operator-created provider bridge (default `br-provider`); and
- PVN CA, certificate, and private key files under `/etc/pvn/pki`.

PVN does not add physical NICs to a bridge. Prepare and test the provider path
before creating provider networks.

Copy `config.example.json` to `/etc/pve/pvn/config.json`, set a stable cluster
UUID, node-local values through `PVN_NODE_NAME` and `PVN_ENCAP_IP`, and list all
PVN Control, OVN NB, and OVN SB client endpoints. The file is cluster-wide;
node-specific environment overrides belong in systemd drop-ins.

Run the read-only preflight on every node:

```sh
pvnctl doctor --config /etc/pve/pvn/config.json
```

## Selecting central nodes

Preview the deterministic 1/3/5 placement:

```sh
pvnctl central plan --nodes pve-a,pve-b,pve-c --existing pve-a
```

Initialize a one-node or two-node installation as standalone on the selected
central node. The confirmation value must exactly match `cluster.id`:

```sh
pvnctl central init-control \
  --mode standalone \
  --confirm CLUSTER_ID
systemctl enable --now pvn-central.target
```

For a new three-or-more-node installation, seed the first voter and join the
others. Port 6646 is reserved for PVN Control Raft traffic:

```sh
# first voter
pvnctl central init-control --mode raft \
  --local ssl:10.0.0.11:6646 --confirm CLUSTER_ID

# second and subsequent voters
pvnctl central init-control --mode raft \
  --local ssl:10.0.0.12:6646 \
  --join ssl:10.0.0.11:6646 \
  --confirm CLUSTER_ID
```

Configure the distro OVN central services for the same voter set, then enable
`pvn-central.target`. Non-voters keep the units installed but disabled.

## Standalone to Raft promotion

Promotion is never automatic. Take a current external backup, schedule a short
control-plane write outage, and stop the only standalone database so every
manager fails closed instead of accepting new writes:

```sh
systemctl stop pvn-manager.service pvn-control-db.service
pvnctl central promote-control \
  --local ssl:10.0.0.11:6646 \
  --confirm CLUSTER_ID --apply
systemctl start pvn-control-db.service pvn-manager.service
```

The command verifies that the service is inactive and writes a timestamped
standalone backup beside the database before replacing it with the first Raft
member. Initialize the other voters with `init-control --mode raft --join`.
Promote OVN NB/SB using the matching OVN package procedure during the same
maintenance window.

## Node removal

Move or detach managed ports, remove the gateway role, and remove the node from
Raft membership before uninstalling. The Debian pre-removal script fails closed
unless local state says the node is drained:

```sh
pvnctl node can-remove --local
```

Package removal deletes only PVN's marked PVE UI loader reference. It does not
remove OVN databases, certificates, provider bridges, or tenant state.

## Recovery boundaries

Raft is availability, not backup. Back up PVN Control and both OVN databases
independently. Never restore only one database and assume cross-database state
is transactional; restore desired state first, then let reconciliation rebuild
OVN state while attachments remain fail-closed.
