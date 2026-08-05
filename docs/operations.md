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

## 1. Inventory and read-only discovery

The example three-node deployment inventory is installed as
`inventory/pve-cluster.example`. It must list the full PVE cluster
membership exactly once in `PVN_TARGET_NODES`; the installer parses that field
and never sources the file. Entries are management SSH destinations only.
Pass the root SSH private key separately with `--identity`; do not store a
password or private key in the inventory.

Before running the installer, verify and populate the deployment host's SSH
`known_hosts` entries through a trusted channel. The installer requires
strict host-key checking and root public-key authentication. It disables
password and keyboard-interactive authentication, uses only the supplied key,
and never prompts for credentials.

The package-safety preflight is not a topology-readiness check. Before writing
configuration, separately discover on every node:

- PVE hostname, cluster membership, quorum, and PVE major version;
- management and intended Geneve addresses, routes, and underlay MTU;
- current OVS/OVN packages, services, bridges, ports, and external IDs; and
- the operator-owned provider bridge, uplink, VLAN policy, and gateway path.

Do not infer a Geneve address, provider bridge, or physical uplink from the
management addresses. PVN never creates a provider bridge, attaches a physical
NIC, changes a host address, or rewrites `/etc/network/interfaces`.

After key-based SSH access is available, collect the same read-only topology
report from the example inventory nodes before activation:

```sh
PVN_IDENTITY=/root/.ssh/id_ed25519
mkdir -p ./pvn-discovery
for host in pve-a.example.net pve-b.example.net pve-c.example.net; do
  ssh -i "$PVN_IDENTITY" -o BatchMode=yes \
    -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no \
    -o PubkeyAuthentication=yes -o IdentitiesOnly=yes \
    -o StrictHostKeyChecking=yes root@"$host" 'sh -s' \
    < deploy/scripts/pvn-host-discover > "./pvn-discovery/$host.txt"
done
```

PVN never infers a Geneve address, provider bridge, physical uplink, VLAN
policy, or gateway path from a management address. No package or installer
phase auto-wires a provider network.

## 2. Safe cluster package stage

The intended release entry point is:

```sh
bash -c "$(curl -fsSL https://RELEASE_HOST/pvn-install.sh)"
```

`RELEASE_HOST` is a placeholder, not a live public endpoint. When hosting a
release locally, publish `pvn-install.sh` and a release directory containing
the versioned `pvn-node` DEB, `pvn-cluster-install`, and `SHA256SUMS`. The
bootstrap accepts only an HTTPS release directory, downloads the three release
files into a private temporary directory, verifies the DEB and cluster
installer against `SHA256SUMS`, and removes the directory on exit. With no
arguments it prompts on the terminal for the inventory and private-key paths,
then runs the read-only preflight first. After a successful preflight it asks
for the exact PVE cluster name. Press Enter to stop without installing; only
the matching cluster name enters the package-apply phase.

Use the hosted bootstrap's `install` phase first as a dry run:

```sh
bash -c "$(curl -fsSL https://RELEASE_HOST/pvn-install.sh)" pvn-install.sh \
  install --inventory /path/to/inventory --identity /root/.ssh/id_ed25519 \
  --release-base-url https://RELEASE_HOST/releases/v0.1.1
```

After reviewing every target and the printed cluster name, repeat with both
write gates:

```sh
bash -c "$(curl -fsSL https://RELEASE_HOST/pvn-install.sh)" pvn-install.sh \
  install --inventory /path/to/inventory --identity /root/.ssh/id_ed25519 \
  --release-base-url https://RELEASE_HOST/releases/v0.1.1 \
  --apply --confirm CLUSTER_ID
```

From a source checkout, build the DEB, set the three local paths, and run the
underlying installer directly:

```sh
make deb
PVN_INVENTORY=deploy/inventory/pve-cluster.example
PVN_IDENTITY=/root/.ssh/id_ed25519
PVN_DEB=dist/pvn-node_0.1.1_amd64.deb

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

## 3. Prepare the host and PKI

Each node needs a distinct, stable Geneve IPv4 address. With a 1500-byte
underlay, PVN's default tenant MTU is 1400. The operator must create and test
both OVS bridges before PVN activation:

- `br-int`, the OVN integration bridge; and
- the configured provider bridge, such as `br-provider`, already connected to
  the intended physical/provider path.

Create the CA once on a protected deployment host, never on a PVE node. Keep
`ca-key.pem` off the cluster:

```sh
pvnctl pki init-ca --config ./config.json \
  --directory ./pki/ca --confirm CLUSTER_ID
pvnctl pki issue-node --config ./config.json \
  --ca-cert ./pki/ca/ca.pem --ca-key ./pki/ca/ca-key.pem \
  --directory ./pki/nodes --name pve-a --dns pve-a \
  --ips CONTROL_IP,GENEVE_IP --confirm CLUSTER_ID
```

Install the public CA as `/etc/pvn/pki/ca.pem` and the node's unique pair as
`node.pem` and `node-key.pem`. Use `root:pvn`, mode `0644` for certificates and
`0640` for the private key. Never copy a node key between hosts.

## 4. Stage shared and node-local configuration

Copy `examples/config.json` to `/etc/pve/pvn/config.json` on one quorate node.
Set a stable cluster UUID and all three PVN Control, OVN NB, and OVN SB client
endpoints. Do not weaken pmxcfs permissions for the `pvn` service account.
At each manager activation, systemd reads this file as root and supplies an
immutable credential copy that only the unprivileged manager can read. The
root agent and host-configuration service continue to read pmxcfs directly.
Consequently, a manager restart is required to load a later shared-config
edit.

On every node, install `examples/node.env` as `/etc/pvn/node.env` and set its
real hostname, Geneve address, local PVE URL, and roles. Install
`examples/ovn-host.env` as `/etc/pvn/ovn-host.env`. Both files are local, not
pmxcfs data, and must be `root:pvn` mode `0640`.

`pvn-ovn-host-config.service` only sets the OVN remote, Geneve endpoint, and
the requested physnet-to-bridge mapping in OVS. It fails if either bridge is
missing or the physnet already maps to a different bridge.

Do not create `/etc/pvn/node-enabled` or enable `pvn-node.target` yet. A
production manager fails closed when PVN Control or OVN NB is unavailable, so
bootstrap the central voters first.

## 5. Select and bootstrap central voters

Preview deterministic 1/3/5 placement:

```sh
pvnctl central plan --nodes pve-a,pve-b,pve-c
```

For the three-node deployment, all three nodes are Raft voters. Complete the
first voter before joining the next one. On the first voter:

1. Install `examples/control-db.env`, `examples/ovn-central-seed.env`, and
   `examples/ovn-listeners.env` under `/etc/pvn/central/`; use node-specific
   addresses in the first two files, owner `root:pvn`, and mode `0640`.
2. Initialize PVN Control while every central unit is stopped.
3. Create the marker last, then enable the target.

```sh
pvnctl central init-control --mode raft \
  --local ssl:FIRST_CONTROL_IP:6646 --confirm CLUSTER_ID
touch /etc/pvn/central/enabled
systemctl enable --now pvn-central.target
systemctl --no-pager --full status pvn-central.target
```

The seed environment makes Debian's OVN units create clustered NB and SB
databases at ports 6643 and 6644. `pvn-ovn-db-listeners.service` then publishes
mutual-TLS client listeners on 6641 and 6642. No insecure TCP listener is
created. OVN stores these passive listeners in replicated Connection rows, so
they bind all local addresses on every voter; host firewall policy is required.

On the second voter, use `examples/ovn-central-join.env`, point both OVN remote
cluster addresses at the first voter, and join PVN Control:

```sh
pvnctl central init-control --mode raft \
  --local ssl:SECOND_CONTROL_IP:6646 \
  --join ssl:FIRST_CONTROL_IP:6646 --confirm CLUSTER_ID
touch /etc/pvn/central/enabled
systemctl enable --now pvn-central.target
```

Repeat for the third voter with its own local addresses and certificate. Start
only one new voter at a time and verify membership/quorum for PVN Control, OVN
NB, and OVN SB before continuing. Allow 6643, 6644, and 6646 only among voters;
allow 6641, 6642, and 6645 only from PVN nodes. Port 8443 is the same-node PVN
web/API endpoint.

PVN Control's client endpoint is fixed at 6645, its Raft endpoint is fixed at
6646, and startup rejects other values. Its node-local control socket lives in
`/run/pvn-control`, separate from the manager's `/run/pvn` runtime directory;
restarting a central database therefore cannot remove the live manager socket.

The preflight unit refuses missing configuration, a mode mismatch, an existing
standalone OVN database, insecure OVN listeners, or a join configuration with
no remote seed. Nodes that are not voters never get the activation marker and
keep all central services inactive.

### One- or two-node installations

Use one selected central node and `init-control --mode standalone`; set
`PVN_CONTROL_MODE=standalone` and use `examples/ovn-central-standalone.env`.
Promotion to Raft is a maintenance operation, never
automatic. Back up all three databases, stop managers and central services,
run `pvnctl central promote-control --local ssl:IP:6646 --confirm CLUSTER_ID
--apply`, then convert OVN NB/SB using the matching OVN 25.03 procedure before
joining additional voters.

## 6. Activate every transport node

After all selected central voters have quorum, activate PVE nodes one at a
time:

```sh
install -o root -g root -m 0644 /dev/null /etc/pvn/node-enabled
systemctl enable --now pvn-node.target
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

Upgrade one PVE node at a time. The package restarts an already-active
per-node stack and fails package configuration if any restart or readiness
check fails; inspect the unit status printed by `postinst` before retrying. It
deliberately does not restart active OVN central database units. After checking
Raft health, restart central services one voter at a time during a maintenance
window. Never upgrade enough voters concurrently to lose quorum.

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
