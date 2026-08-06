# PVN operations

PVN targets Proxmox VE 9 on Debian 13 with OVN 25.03. Install the same
`pvn-node` package on every online PVE node. Its network services remain
inactive after installation: a root-owned local activation marker plus
`pvn-node.target` starts the per-node manager/controller/agent stack, and
`pvn-central.target` starts that node's central voter only after its separate
activation marker exists. The supported automated bootstrap makes every PVE
member a voter. The shared pmxcfs configuration does not activate a node.

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
supports one standalone PVE node or any odd-sized, fully online and quorate
PVE cluster with at least three nodes. Every clustered PVE node becomes a
central voter.

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
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh) && bash -c "$pvn_bootstrap"
```

The assignment returns `curl`'s failure status, so `&&` prevents Bash from
executing partial response data. The inner Bash still inherits `/dev/tty` for
the interactive confirmations below.

The bootstrap accepts only HTTPS, downloads the versioned DEB, cluster
installer, and native PVE lease helper into a private temporary directory,
verifies every executable against `SHA256SUMS`, and removes the directory on
exit. It first prints the full discovered membership. Press Enter to stop; only
the exact cluster name enters the package-apply phase. After package install it
offers the separate topology/control-plane phase.

Use the hosted bootstrap's `install` phase first as a dry run:

```sh
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh) && \
  bash -c "$pvn_bootstrap" pvn-install.sh install
```

After reviewing every target and the printed cluster name, repeat with both
write gates:

```sh
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh) && \
  bash -c "$pvn_bootstrap" pvn-install.sh install --apply --confirm CLUSTER_NAME
```

To run every phase non-interactively, first make the outer OpenStack provider
ports trusted/port-security-disabled for arbitrary guest MAC/IP traffic, then
use all write gates:

```sh
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-install.sh) && \
  bash -c "$pvn_bootstrap" pvn-install.sh install --apply --confirm CLUSTER_NAME --full \
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

One PVE node uses standalone databases. Any supported cluster has an odd node
count of at least three and uses every PVE node as a Raft voter. The apply process
initializes the deterministic seed, joins one voter at a time, verifies exact
membership and cluster IDs after every step, then activates transport nodes
one at a time. A durable phase ledger makes a safe rerun converge forward
without deleting or regenerating existing database or key material.

Every new PVN Control join passes the ledger-pinned Raft cluster ID to
`ovsdb-tool --cid`, so it cannot discover or adopt a different cluster. A
legacy join stub created before that pinning was added may temporarily report
`cluster ID not yet known`; it is accepted only while inactive, with its
record-0 remote equal to the deterministic seed on port 6646 and a valid local
identity. Activation is followed by the same bounded exact-membership/CID
convergence gate. A foreign CID, remote, duplicate server ID, malformed log,
or convergence timeout fails closed and no automatic delete or leave occurs.

The OVN units use clustered NB/SB database ports 6643/6644 and publish
mutual-TLS client listeners on 6641/6642. PVN Control uses client port 6645 and
Raft port 6646. Allow 6643, 6644, and 6646 only among voters; allow 6641, 6642,
and 6645 only from PVN nodes. Port 8443 is the same-node PVN web/API endpoint.
No insecure TCP listener is created.

On first use, a browser may trust the PVE UI on port 8006 but still reject the
embedded manager on port 8443. The PVN panel toolbar provides **Trust local PVN
certificate**, which opens only the current node's manager origin in a new
`noopener,noreferrer` tab. Review and accept the certificate warning, return to
the panel, and select **Reload PVN**. Production clients should trust the PVE CA
or use a publicly trusted node certificate so this onboarding step is not
required.

NB/SB client listeners are process-local `ovsdb-server` remotes, applied again
after every database-process restart. PVN removes only the packaged replicated
`db:...connections` reference from that process during migration and refuses
any other unexpected remote; node-local listener addresses are never written
to replicated `NB_Global` or `SB_Global` rows.

The Debian 13 SB vendor unit spells its PID path as `%t/run/ovn/ovnsb_db.pid`,
which expands to `/run/run/ovn/ovnsb_db.pid`. The PVN drop-in pins the actual
process path, `/run/ovn/ovnsb_db.pid`, so systemd can track SB startup exactly.

PVN Control's client endpoint is fixed at 6645, its Raft endpoint is fixed at
6646, and startup rejects other values. Its node-local control socket lives in
`/run/pvn-control`, separate from the manager's `/run/pvn` runtime directory;
restarting a central database therefore cannot remove the live manager socket.

The preflight unit refuses missing configuration, a mode mismatch, an existing
standalone OVN database, insecure OVN listeners, or a join configuration with
no remote seed. Each voter gets its activation marker only at its serialized
bootstrap step and keeps all central services inactive before then.

### One-node installations

The automated bootstrap selects standalone mode. Promotion to Raft is a
maintenance operation and is never automatic. Two-node control-plane bootstrap
is intentionally rejected because it cannot provide majority availability.

## 6. Activate every transport node

The control-plane apply activates PVE transport nodes only after all central
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

`pvnctl doctor` always reads node-local identity from `/etc/pvn/node.env`, even
when invoked directly by an operator, the control-plane, or the updater. The
shared `/etc/pve/pvn/config.json` intentionally leaves `networking.encap_ip`
empty. Use `--node-env /absolute/test/path` only for an isolated test fixture;
the file is parsed as a strict, non-shell `KEY=VALUE` allowlist and fails closed
when it is missing, malformed, duplicated, symlinked, or unsafely writable.
The doctor also compares that effective address and `geneve` type with OVS
`ovn-encap-ip`/`ovn-encap-type`, then verifies the address is present on a local
interface whose MTU can carry the configured guest MTU plus encapsulation.

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
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-update.sh) && bash -c "$pvn_bootstrap"
```

For non-interactive automation, plan first and then explicitly apply:

```sh
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-update.sh) && \
  bash -c "$pvn_bootstrap" pvn-update.sh plan
pvn_bootstrap=$(curl -fsSL https://github.com/popododo0720/proxmox-ovn/releases/latest/download/pvn-update.sh) && \
  bash -c "$pvn_bootstrap" pvn-update.sh apply --confirm CLUSTER_NAME
```

The updater takes the shared PVE `mutation` lease, pins the exact online
name/ID/management-IP membership and cluster configuration version, stages and
hashes the DEB on every pending node, then upgrades nodes sequentially. Before
and after every node it requires PVE quorum, consistent package state, the PVE
UI hook, node readiness/`pvnctl doctor` for active transport nodes, and healthy
mode-specific database status for active central nodes: exact local schema
identity over all three Unix sockets in standalone mode, or local Raft status
in clustered mode. Existing `/etc/pvn`, shared PVN configuration, and database
paths are preserved; an unexpected configuration change fails the rollout. A
failed node stops the sequence. Nodes already completed remain upgraded, and a
later run safely verifies/skips them while continuing the one remaining older
version.

Proxmox also exposes a volatile membership-view generation as the top-level
`version` in `/etc/pve/.members`. It can advance while the durable cluster is
unchanged, so PVN records it for diagnostics but does not treat it as topology
identity. Every discovery still requires quorum and all members online and
strictly pins the cluster name, corosync configuration version, exact sorted
node name/ID/management-IP set, topology digest, interfaces, addresses, MTU,
and package versions. Drift in any of those durable fields remains a hard
failure before an activation boundary.

The control-plane ledger also pins the package version on every node. Package
drift normally stops `pvn-control-plane plan`. There are only four automatic
forward-recovery exceptions:

1. A wholly untouched `planned` ledger.
2. A Raft ledger in the exact `staged` crash window where the deterministic
   seed alone is marked and active, its pinned Control DB and all three
   databases are healthy single-member clusters, and every non-seed is
   pristine.
3. An exact `central-N` ledger (`1 <= N < node count`) where the first N voters
   are marked, active, healthy N/N members of all three pinned database
   clusters; the next voter is inactive with only one record-0 PVN Control
   join stub pointing at the seed; and every later voter is pristine. The stub
   may contain the pinned CID or the legacy not-yet-known CID state.
4. An exact `complete` ledger immediately after a uniform forward package
   rollout. Every central and transport target/marker must remain active, a
   fresh `pvnctl doctor` must pass on every node, ledger-pinned PKI and seed-CA
   placement must be exact, and the online/offline identity of every database
   must agree with healthy N/N membership and the pinned CIDs. Every voter must
   still carry a root-only restart marker equal to its newly installed package.

The first three exceptions require zero transport progress. The staged and
`central-N` cases additionally require complete ledger-pinned PKI and a
root-only `central-restart-pending` marker matching the uniformly installed,
strictly newer package on every already-active voter. The `central-N` proof
also rejects a wrong/missing/extra seed remote, foreign CID, or server-ID
collision before changing the ledger. The `complete` proof additionally
cross-checks each live Raft row with the corresponding offline database SID,
CID, and local address. Plan reports the required repin without writing. Apply
repeats the proof under the cluster mutation lease, atomically replaces only
the package-version snapshot, and then converges or revalidates forward. It
never adopts an unpinned database, regenerates keys, removes state, or consumes
the restart marker. Any mixed/downgraded package, membership/topology
difference, unexpected progress, CID, marker, database, doctor, or service
state remains a hard failure; do not edit the ledger to bypass it.

Package installation restarts only an already-active per-node
manager/agent/controller stack. The updater deliberately does not restart
active PVN Control, OVN NB/SB, or northd processes and verifies that their PIDs
did not change. The package records a root-only durable restart-required marker
at `/var/lib/pvn-node/central-restart-pending` for each active central target,
and both plan/apply report the affected nodes.
After the package rollout and full Raft convergence, restart those central
services separately, one healthy voter at a time, then clear the marker only
after verifying that voter. Never activate a previously inactive target or
restart enough voters concurrently to lose quorum. Mixed-version compatibility
is required for the duration of this rolling window; use a maintenance window
for releases that declare a breaking database or wire-protocol change.

For an already `complete` ledger, run `pvn-control-plane plan` and its confirmed
`apply` immediately after the package rollout, while every restart marker still
exists. The successful apply repins the durable package snapshot. Only then
perform the one-voter-at-a-time restart sequence below and consume each marker;
clearing or consuming a marker before the repin intentionally makes recovery
fail closed.

Do not clear the marker while a voter still runs the old process. Once every
database has converged beyond the one-member seed, use this sequence on exactly
one voter at a time. The JSON guard proves that the remaining connected members
can still form quorum without the local voter; run the same guard again after
the restart before moving to the next voter:

```sh
raft_drain_check() {
  pvnctl central status | python3 -c '
import json, sys
report = json.load(sys.stdin)
databases = report.get("databases", [])
if report.get("healthy") is not True or len(databases) != 3:
    raise SystemExit("all three Raft databases must be healthy")
for database in databases:
    name = database.get("database", "unknown")
    members = database.get("member_count", 0)
    connected = database.get("connected_members", 0)
    quorum = database.get("quorum_size", 0)
    if members <= 1 or connected - 1 < quorum:
        raise SystemExit(f"unsafe to drain {name}: "
                         f"connected={connected}, quorum={quorum}")
'
}
central_pids() {
  for unit in pvn-control-db.service ovn-ovsdb-server-nb.service \
    ovn-ovsdb-server-sb.service ovn-northd.service
  do
    systemctl show --property=MainPID --value "$unit"
  done | paste -sd, -
}

raft_drain_check
before=$(central_pids)
systemctl restart pvn-central.target
pvnctl central status
raft_drain_check
after=$(central_pids)
[ "$before" != "$after" ] || { echo "central PIDs did not change" >&2; exit 1; }

marker=/var/lib/pvn-node/central-restart-pending
[ -f "$marker" ] && [ ! -L "$marker" ] || exit 1
[ "$(stat -c '%u:%g:%a' "$marker")" = 0:0:600 ] || exit 1
[ "$(cat "$marker")" = "$(dpkg-query -W -f='${Version}' pvn-node)" ] || exit 1
rm -f -- "$marker"
```

Stop at the first failed command. Repeat only after the restarted voter and all
three databases pass the guard. The marker lives outside `/var/lib/pvn`
because that standard service state directory is writable by the unprivileged
`pvn` account; restart authorization state must remain root-only.

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
