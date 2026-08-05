# PVN operations

PVN targets Proxmox VE 9 on Debian 13 with OVN 25.03. Install the same
`pvn-node` package on every online PVE node. Its network services remain
inactive after installation: a root-owned local activation marker plus
`pvn-node.target` starts the per-node manager/controller/agent stack, and
`pvn-central.target` starts a selected central voter only after its separate
activation marker exists. The shared pmxcfs configuration does not activate a
node.

The package owns the local OVN deployment. Its maintainer script masks the
Debian `ovn-host.service` and `ovn-central.service` aggregate units; PVN starts
their individual controller, database, and northd units instead. If OVN was
already used by another system, do not install PVN until that ownership is
resolved.

## 1. Native cluster discovery

Run the public installer on any target PVE node. The default mode reads native
PVE membership, requires quorum and every declared member online, and uses
Proxmox's root cluster key plus each node's own `ssh_known_hosts` pin. It never
asks for or uses a password. The current automated control-plane bootstrap
supports one standalone PVE node or exactly three clustered PVE nodes.

Before writing configuration, the topology preflight discovers on every node:

- PVE hostname, cluster membership, quorum, and PVE major version;
- management and intended Geneve addresses, routes, and underlay MTU;
- current OVS/OVN packages, services, bridges, ports, and external IDs; and
- the intended provider NIC, routes, and any existing bridge ownership.

Package discovery is read-only:

```sh
./deploy/scripts/pvn-cluster-install preflight --local-pve
```

Topology planning is also read-only and identifies Geneve/provider NICs only
from explicitly supplied CIDRs:

```sh
/usr/lib/pvn/pvn-topology plan \
  --geneve-cidr 192.168.100.0/24 \
  --provider-cidr 192.168.200.0/24
```

An external inventory and identity file remain available as an advanced
package-distribution mode. They do not perform topology/control-plane setup.
The example is installed as `inventory/pve-cluster.example`; never put a
password or private key in that file.

## 2. Safe cluster package stage

The public release entry point is:

```sh
bash -c "$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh)"
```

The bootstrap accepts only HTTPS, downloads the versioned DEB, cluster
installer, and native PVE lease helper into a private temporary directory,
verifies every executable against `SHA256SUMS`, and removes the directory on
exit. It first prints the full discovered membership. Press Enter to stop; only
the exact cluster name enters the package-apply phase. After package install it
offers the separate topology/control-plane phase.

Use the hosted bootstrap's `install` phase first as a dry run:

```sh
bash -c "$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh)" \
  pvn-install.sh install
```

After reviewing every target and the printed cluster name, repeat with both
write gates:

```sh
bash -c "$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh)" \
  pvn-install.sh install --apply --confirm CLUSTER_NAME
```

To run every phase non-interactively, first make the outer OpenStack provider
ports trusted/port-security-disabled for arbitrary guest MAC/IP traffic, then
use all write gates:

```sh
bash -c "$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh)" \
  pvn-install.sh install --apply --confirm CLUSTER_NAME --full \
  --geneve-cidr 192.168.100.0/24 \
  --provider-cidr 192.168.200.0/24 \
  --guest-mtu 1300 \
  --provider-port-ready OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP
```

From a source checkout, build the DEB, set the three local paths, and run the
underlying installer directly:

```sh
make deb
PVN_INVENTORY=deploy/inventory/pve-cluster.example
PVN_IDENTITY=/root/.ssh/id_ed25519
PVN_DEB=dist/pvn-node_VERSION_amd64.deb

./deploy/scripts/pvn-cluster-install preflight \
  --inventory "$PVN_INVENTORY" --identity "$PVN_IDENTITY"
```

Preflight is always read-only. It connects to every inventory node and rejects
missing quorum, a non-PVE-9 node, inconsistent cluster names or architectures,
an incomplete inventory, active PVN/OVN services, enabled PVN targets,
activation markers, incomplete `pvn-node` package state, or an unsupported PVE
UI template. All nodes must pass before the command succeeds.

Preview package installation without changing a node:

```sh
./deploy/scripts/pvn-cluster-install install \
  --inventory "$PVN_INVENTORY" --identity "$PVN_IDENTITY" --deb "$PVN_DEB"
```

`install` also defaults to a dry run. It prints the package version and digest,
the exact target list, and the discovered value required by `--confirm`. After
reviewing that output, perform only the package stage:

```sh
./deploy/scripts/pvn-cluster-install install \
  --inventory "$PVN_INVENTORY" --identity "$PVN_IDENTITY" --deb "$PVN_DEB" \
  --apply --confirm CLUSTER_ID
```

Immediately before each node's first change, the installer repeats its safety
preflight. It stages the DEB in a private temporary path, verifies its SHA-256,
masks `ovn-host.service` and `ovn-central.service` before apt can start them,
installs the package, verifies the installed version and PVE UI hook, confirms
that PVN remains disabled and inactive, and removes the staged file. A failure
stops before the next node; nodes already completed remain installed but
inactive, so investigate the failure and then rerun the same command.

This completes package distribution only. It does **not** create `br-int` or a
provider bridge, attach an uplink, edit `/etc/network/interfaces`, select
Geneve/control addresses, install PKI or configuration, initialize PVN/OVN
databases, create either activation marker, or enable a PVN target. Continue
with the topology, bootstrap, and activation sections below as separate
operator decisions.

### Manual single-node package command

If the phased installer cannot be used, preserve the same ordering on each
online node. To prevent Debian dependency installation from briefly starting
its aggregate OVN units, pre-mask them before installing the package and
dependencies:

```sh
systemctl mask ovn-host.service ovn-central.service
apt install ./pvn-node_VERSION_amd64.deb
```

Repeat on all online nodes before activating any one of them. Confirm that
`pvn-node.target` and `pvn-central.target` are disabled and inactive, and that
neither `/etc/pvn/node-enabled` nor `/etc/pvn/central/enabled` exists.

On PVE 9, package configuration fails if the exact tested template signature
cannot be patched or if the installed loader differs from the packaged copy.
`pvn-ui-verify` performs the same read-only check. Other PVE major versions are
left unchanged by the UI installer; they also fail the later PVE 9 doctor check
and therefore cannot be activated accidentally.

## 3. Apply the inert host topology

Each node needs a distinct, stable Geneve IPv4 address. The topology command
derives a safe guest MTU from the live Geneve/provider paths unless
`--guest-mtu` is supplied. Review its read-only plan, then use all three write
gates:

```sh
/usr/lib/pvn/pvn-topology plan \
  --geneve-cidr GENEVE_CIDR --provider-cidr PROVIDER_CIDR

/usr/lib/pvn/pvn-topology apply \
  --geneve-cidr GENEVE_CIDR --provider-cidr PROVIDER_CIDR \
  --guest-mtu GUEST_MTU \
  --provider-port-ready OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
  --confirm CLUSTER_NAME
```

The apply transaction creates `br-int` and `br-provider`, moves only the
selected provider NIC under OVS, and publishes a root-private shared topology
ledger after every node verifies the desired state. If a selected Geneve NIC
carries a Corosync ring, the command first migrates that ring to the
already-verified management address; a provider NIC carrying any Corosync ring
is rejected. Any failure rolls back all network changes owned by that
transaction. It never creates an activation marker or starts PVN/OVN.

## 4. Plan configuration and node-local PKI

`pvn-control-plane plan` validates the completed topology ledger, exact package
version, inactive targets, empty PVN databases, and absence of conflicting PKI
without writing anything:

```sh
/usr/lib/pvn/pvn-control-plane plan
```

During apply, each node generates its own Ed25519 private key locally. Only its
CSR crosses SSH to the deterministic seed node. The seed keeps the cluster CA
key only in `/var/lib/pvn-ca` (`root:root`, mode `0600`), returns public
certificates, and never places a private key in pmxcfs or sends one over SSH.
The root-private control-plane ledger pins the CA certificate and every node's
certificate/public-key fingerprints before public certificates are installed.
Legacy shared PKI or unexpected seed-CA state fails closed and requires a
manual audit.

The same apply renders shared `/etc/pve/pvn/config.json` and node-local
`/etc/pvn/*.env` files. At manager activation systemd supplies an immutable
credential copy of the shared configuration to the unprivileged manager. A
manager restart is required to load a later shared-config edit.

`pvn-ovn-host-config.service` only sets the OVN remote, Geneve endpoint, and
the requested physnet-to-bridge mapping in OVS. It fails if either bridge is
missing or the physnet already maps to a different bridge.

Do not create `/etc/pvn/node-enabled` or enable `pvn-node.target` yet. A
production manager fails closed when PVN Control or OVN NB is unavailable, so
bootstrap the central voters first.

## 5. Bootstrap central voters and transport nodes

Apply only after reviewing the plan, using the exact PVE cluster name:

```sh
/usr/lib/pvn/pvn-control-plane apply --confirm CLUSTER_NAME
```

One PVE node uses standalone databases. Exactly three PVE nodes use all three
as Raft voters. The apply process initializes the deterministic seed, joins
one voter at a time, verifies exact membership and cluster IDs after every
step, then activates transport nodes one at a time. A durable phase ledger
makes a safe rerun converge forward without deleting or regenerating existing
database or key material.

The OVN units use clustered NB/SB database ports 6643/6644 and publish
mutual-TLS client listeners on 6641/6642. PVN Control uses client port 6645 and
Raft port 6646. Allow 6643, 6644, and 6646 only among voters; allow 6641, 6642,
and 6645 only from PVN nodes. Port 8443 is the same-node PVN web/API endpoint.
No insecure TCP listener is created.

PVN Control's client endpoint is fixed at 6645, its Raft endpoint is fixed at
6646, and startup rejects other values. Its node-local control socket lives in
`/run/pvn-control`, separate from the manager's `/run/pvn` runtime directory;
restarting a central database therefore cannot remove the live manager socket.

The preflight unit refuses missing configuration, a mode mismatch, an existing
standalone OVN database, insecure OVN listeners, or a join configuration with
no remote seed. Nodes that are not voters never get the activation marker and
keep all central services inactive.

### One-node installations

The automated bootstrap selects standalone mode. Promotion to Raft is a
maintenance operation and is never automatic. Two-node control-plane bootstrap
is intentionally rejected because it cannot provide majority availability.

## 6. Activate every transport node

The control-plane apply activates PVE nodes only after all selected central
voters have quorum. Verify each local stack after it completes:

```sh
systemctl --no-pager --full status \
  pvn-node.target pvn-node-ready pvn-manager pvn-agent ovn-controller
```

The marker is the explicit local opt-in; enabling the target without it starts
nothing. `pvn-node-ready.service` allows up to roughly 90 seconds for the
manager runtime socket, required services, the agent's loopback `/healthz`
endpoint, OVN controller Southbound status, the installed PVE UI hook, and
`pvnctl doctor` to become healthy. The controller must report exactly
`connected`; a running process is insufficient. The agent endpoint remains
unhealthy until at least one TAP binding scan succeeds; manager socket creation
also proves its startup PVN Control and OVN NB probes passed. Keep the packaged
`PVN_HEALTH_LISTEN` and `PVN_AGENT_HEALTH_URL` values unchanged. This is a
bounded local startup check, not continuous health monitoring.

At boot, `pvn-guest-gate.service` leaves normal PVE behavior unchanged when
the local marker is absent. When the marker exists, a failed readiness check
fails that `pve-guests.service` start, so all guests on that host—not only PVN
guests—remain stopped for operator review. The gate is a one-shot and is not
coupled to later service failures, so it does not automatically stop already
running guests. It also cannot intercept a later manual VM start through the
PVE UI/API; until a future PVE start/HA integration exists, enforce operational
policy that tenant VMs start only while `pvn-node-ready.service` is active on
their node. A VM started outside that policy can boot with a disconnected TAP.

After changing `/etc/pve/pvn/config.json`, refresh the credential copy and
local settings one node at a time:

```sh
systemctl restart pvn-ovn-host-config ovn-controller \
  pvn-manager pvn-agent pvn-node-ready
```

## Upgrades

Run the hosted rolling updater on any online PVE node. With no arguments it
downloads the release package, updater, and native PVE lease helper, verifies
all three against `SHA256SUMS`, prints a read-only plan, and asks for the exact
deployment ID before changing a node:

```sh
bash -c "$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-update.sh)"
```

For non-interactive automation, plan first and then explicitly apply:

```sh
bash -c "$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-update.sh)" \
  pvn-update.sh plan
bash -c "$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-update.sh)" \
  pvn-update.sh apply --confirm CLUSTER_NAME
```

The updater takes the shared PVE `mutation` lease, pins the exact online
name/ID/management-IP membership and cluster configuration version, stages and
hashes the DEB on every pending node, then upgrades nodes sequentially. Before
and after every node it requires PVE quorum, consistent package state, the PVE
UI hook, node readiness/`pvnctl doctor` for active transport nodes, and healthy
local Raft status for active central voters. Existing `/etc/pvn`, shared PVN
configuration, and database paths are preserved; an unexpected configuration
change fails the rollout. A failed node stops the sequence. Nodes already
completed remain upgraded, and a later run safely verifies/skips them while
continuing the one remaining older version.

Package installation restarts only an already-active per-node
manager/agent/controller stack. The updater deliberately does not restart
active PVN Control, OVN NB/SB, or northd processes and verifies that their PIDs
did not change. This means package files can be at the new version while those
central processes still run the previous executable until a maintenance
restart. After the package rollout, check Raft health and restart central
services one voter at a time. Never restart enough voters concurrently to lose
quorum. Mixed-version compatibility is required for the duration of this
rolling window; use a maintenance window for releases that declare a breaking
database or wire-protocol change.

A hard power loss can leave the cluster mutation lease for operator review.
Inspect it with `pvn-cluster-lease show mutation`; never remove a lease until
the recorded owner is proven dead and the partial rollout is audited.

## Node removal

Move or detach managed ports, remove the gateway role, and remove the node from
all three Raft clusters before uninstalling. Then remove the central marker and
explicitly stop and disable both PVN targets. Remove the local marker only
after the node stack is stopped:

```sh
systemctl disable --now pvn-central.target pvn-node.target
rm -f /etc/pvn/central/enabled /etc/pvn/node-enabled
apt remove pvn-node
```

The pre-removal check runs even if shared pmxcfs configuration is missing or
unavailable. It fails closed while either local/central marker remains, either
target is enabled, any PVN/OVN unit is active, OVS cannot be queried, or a
local interface still has `external_ids:managed-by=pvn`. An unavailable
systemd manager also blocks removal because service state cannot be proven.
This live guard avoids depending on a cached node-state file. It cannot prove
remote desired state, so removing the node/gateway records and Raft
memberships remains an explicit operator step.

Removal stops PVN-owned services and removes only PVN's marked PVE UI loader
reference. It preserves `/etc/pvn`, certificates, databases, provider bridges,
and tenant state. Delete preserved data manually only after a verified backup.

## Recovery boundaries

Raft is availability, not backup. Back up PVN Control and both OVN databases
independently. Never restore only one database and assume cross-database state
is transactional. Restore desired state first, then let reconciliation rebuild
OVN state while attachments remain fail-closed.

If an activated node blocks `pve-guests` during boot, inspect
`pvn-node-ready.service` and fix its reported local or control-plane failure.
Then restart `pvn-node.target` and `pve-guests.service`. To deliberately return
the host to non-PVN guest boot behavior, first stop and disable
`pvn-node.target`, remove `/etc/pvn/node-enabled`, and then restart
`pve-guests.service`; this is an explicit networking ownership change, not an
automatic fallback.
