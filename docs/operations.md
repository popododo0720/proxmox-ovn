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

This locally built DEB is for controlled staging and validation only. It is
not a canonical public artifact and must not be uploaded to GitHub Releases.

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

Package configuration runs
`pvnctl doctor --check corosync-runtime-config` before `daemon-reload` or any
PVN service action. This configuration-independent check applies equally to an
active node, an inactive topology-only node, and a standalone node. A clustered
node fails closed if `corosync-cmapctl` is unavailable or its live membership,
addresses, or version differ from `/etc/pve/corosync.conf`; a standalone node
without that file passes.

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
ledger after every node verifies the desired state. A provider NIC carrying
any Corosync ring is rejected.

If a selected Geneve NIC carries a Corosync ring, migration uses two fenced
stages. The existing N/N Geneve ring remains the safety path while a management
address is added on an unused KNET ring. Only after every daemon has loaded the
dual-ring configuration and the management ring is N/N connected does PVN
remove the Geneve ring and converge every daemon to the final configuration.
Every candidate and rollback configuration passes `corosync -t` on every node
before use. Each shared write is a compare-and-swap from the exact preceding
SHA-256 boundary; unrelated operator drift stops the transaction.

A cluster-wide reload is followed by an exact persisted/runtime sweep. If a
daemon did not load the candidate, PVN restarts Corosync on at most one node at
a time, non-coordinator nodes first and the coordinator last. It requires the
safety ring to remain N/N before and after each restart. A lost command response
counts as success only when a fresh cluster-wide probe proves the exact target.
On convergence failure, PVN restores a validated safety boundary when that
rollback can be proven; a recovery stage with no safe rollback remains on its
exact rerunnable boundary. Failure to prove either condition stops for operator
audit. No host-network change has begun at any of these boundaries.

A rerun also recognizes the v0.2.13 stale-runtime failure shape in which the
shared file names a single management ring while every daemon still uniformly
runs the old Geneve topology. Recovery is allowed only when the live node IDs,
membership, joined status, rings, bind addresses, and runtime version boundary
form one consistent fully connected state. PVN then keeps that live Geneve ring
as the safety path and adds management on a different unused ring; it never
publishes or reloads an intermediate single-ring address replacement. Mixed or
ambiguous stale state fails closed.

Immediately before network staging, PVN again requires one identical shared
Corosync file and an exact loaded runtime on every node, with no ring using the
Geneve addresses. The final root-private topology ledger pins that file's
SHA-256, `config_version`, and complete ring mapping. Any network failure rolls
back all network changes owned by the transaction. Apply never creates an
activation marker or starts PVN/OVN.

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

Every `ovn-northd` process reads the complete ordered NB/SB TLS endpoint set
and certificate paths directly from `/etc/pve/pvn/config.json`; it never uses
the node-local NB/SB Unix sockets. The launcher rejects non-TLS, duplicate,
even-sized, differently ordered Control/NB/SB voter sets and the vendor
`ovn-northd-db-params.conf` bypass. `pvn-ovn-northd-ready.service` blocks
`pvn-central.target` readiness until local northd reports both IDLs connected,
an `active` or `standby` role, and exact equality of `NB_Global.nb_cfg`,
`NB_Global.sb_cfg`, and `SB_Global.nb_cfg`. There is one bounded rolling-upgrade
exception: while a root-only `central-restart-pending` marker safely matches the
installed package, the readiness unit may admit a connected clustered
`standby` with `cfg=pending`. It never admits an active northd, a standalone
node, an unreadable cfg value, or an unsafe/mismatched marker through that
exception. The diagnostic `status` action is always strict and never consults
the marker:

```sh
/usr/lib/pvn/pvn-ovn-northd status
```

The central-target `wait` gate and the transport-target `node-ready` gate have
different transition scopes. `wait` still requires connected NB/SB IDLs and
synchronized cfg; its marker exception admits only a connected clustered
standby with `cfg=pending`. Without a marker, `node-ready` is also strict. With
an exact installed-version marker it accepts only a valid local `active` or
`standby` role and reports both IDLs as unchecked so an already-running split
northd cannot deadlock a node-only package restart. While dpkg is
`half-configured`, that relaxation additionally requires a matching root-only
live package lock; stale, unlocked, unsafe, or wrong-version authorization is
rejected.

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

Every supported active deployment keeps the two local roles paired:
`pvn-node.target` may be active only while `pvn-central.target` is already
active on the same PVE node. Control-plane apply starts all central voters
before any transport target, and package recovery refuses to resume a node
against an inactive or transitional central target.

The marker is the explicit local opt-in; enabling the target without it starts
nothing. `pvn-node-ready.service` allows up to roughly 90 seconds for the
manager runtime socket, required services, the agent's loopback `/healthz`
endpoint, OVN controller Southbound status, the installed PVE UI hook, and
`pvnctl doctor` to become healthy. The controller must report exactly
`connected`; a running process is insufficient. The agent endpoint remains
unhealthy until at least one TAP binding scan succeeds; manager socket creation
also proves that PVN Control opened and that the complete OVN NB voter set
served a read. This startup-only NB probe intentionally omits `--wait=sb`,
allowing a node package restart while preserved old northd processes are
temporarily split. It does not authorize writes: manager `/health`, every
OVN-backed render/delete before and after mutation, and
`pvnctl recovery reconcile-ovn` retain the strict NB-to-SB synchronization
fence. Keep the packaged `PVN_HEALTH_LISTEN` and `PVN_AGENT_HEALTH_URL` values
unchanged. This is a bounded local startup check, not continuous health
monitoring.

The full `pvnctl doctor` invocation always reads node-local identity from
`/etc/pvn/node.env`, even when invoked directly by an operator, the
control-plane, or the updater. The
shared `/etc/pve/pvn/config.json` intentionally leaves `networking.encap_ip`
empty. Use `--node-env /absolute/test/path` only for an isolated test fixture;
the file is parsed as a strict, non-shell `KEY=VALUE` allowlist and fails closed
when it is missing, malformed, duplicated, symlinked, or unsafely writable.
The doctor also compares that effective address and `geneve` type with OVS
`ovn-encap-ip`/`ovn-encap-type`, then verifies the address is present on a local
interface whose MTU can carry the configured guest MTU plus encapsulation.
On a clustered node it also compares `/etc/pve/corosync.conf` with one live
`corosync-cmapctl` snapshot: `config_version`, the exact name/node-ID/ring
mapping, every joined member's version and link addresses, and the local bind
address must agree. A config written to pmxcfs but not loaded by the Corosync
runtime therefore makes the doctor, node-readiness gate, and rolling-update
health check fail before the node is treated as safe to reboot. A standalone
node with no `corosync.conf` skips only this cluster-specific check.
`pvnctl doctor --check corosync-runtime-config` runs exactly that persisted/live
comparison without loading PVN configuration; package safety gates use this
form before PVN may be configured or activated. Any other check name, repeated
`--check`, malformed output, command failure, or failed check is rejected.

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

## Default security policy and legacy backfill

Each project owns a reserved `default` security group. New ports that omit an
explicit security-group selection receive it automatically. Its baseline
allows IPv4 egress and ingress from other ports in the same default group;
other IPv4 ingress is dropped. The group and its two managed rules are visible
by name in the Proxmox PVN page, but cannot be edited or deleted independently.
Authorized tenants may add separate rules to extend the group without changing
the managed baseline.

Ports created by older PVN releases with no security-group assignment remain
unrestricted until an administrator migrates them. Open **Datacenter → PVN →
Ports → Legacy security policy backfill**, refresh the plan, and run the dry
run. The page lists projects, ports, nodes, and attached VM NICs by human name.
Applying requires typing the exact PVE cluster name and changes policy for
attached traffic immediately. Review any required external ingress rules
before applying. On a standalone host, the exact confirmation is
`standalone-NODENAME`. The manager reads this name from a protected systemd
credential copy of `/etc/pve/.members`, not from the PVN installation UUID.

The same display rule applies throughout the PVN page: normal tables,
selectors, confirmations, diagnostics, and bounded errors resolve references
to human names when the referenced object is visible. Unknown UUIDs are
redacted instead of becoming an operator-facing label. Open **Details** for the
raw, copyable UUIDs needed to correlate API or database records.

The dry run issues an opaque plan token covering the exact candidate ports and
their revisions. Apply accepts only that preview: if a port is added or changes
first, PVN rejects the whole request and the page refreshes the plan for a new
review and dry run. The token is intentionally not displayed in the normal UI.
The operation remains revision-fenced and rerunnable. It repairs the reserved
baseline first, migrates only ports whose security-group list is still empty,
and reports concurrent or blocked ports separately for a later retry. Planning
requires global `SDN.Audit`; applying requires global `SDN.Allocate` and
`Sys.Modify`. The normal Proxmox session and PVN CSRF protection apply, with no
second login.

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

The updater takes the shared PVE `mutation` lease and pins the exact online
name/ID/management-IP membership and cluster configuration version. Before it
stages any DEB, its own package-independent Corosync verifier checks every
node; it does not trust the `pvnctl` binary from the package being replaced.
Clustered nodes must expose one exact Corosync package version plus identical
persisted and loaded membership, rings, addresses, and `config_version`.
Standalone mode instead requires both `corosync.conf` and a running Corosync
daemon to be absent. Helper commands have a 20-second deadline and separate
4-MiB stdout/stderr limits; timeout, overflow, non-UTF-8 output, or a changed
root-owned input file fails closed.

The updater repeats that same embedded snapshot immediately before staging,
before and after every package transaction, and in the final sweep. Package
`postinst` separately runs the newly installed
`pvnctl doctor --check corosync-runtime-config` before its first possible
service restart, and already-updated nodes must keep passing that package gate.
The remaining rollout checks require PVE quorum, consistent package state, the
PVE UI hook, node readiness/`pvnctl doctor` for active transport nodes, and
healthy mode-specific database status for active central nodes: exact local
schema identity over all three Unix sockets in standalone mode, or local Raft
status in clustered mode. Thus a stale inactive topology-only node also stops
a 1-, 3-, 5-, or larger supported odd-node rollout before another package
mutation. Existing `/etc/pvn`, shared PVN configuration, and database paths are
preserved; an unexpected configuration change fails the rollout. A failed node
stops the sequence. Nodes already completed remain upgraded, and a later run
safely verifies/skips them while continuing the one remaining older version.

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
repeats the proof under the cluster mutation lease and atomically replaces the
eligible durable snapshot. A normal package repin may differ only by uniform
forward package versions. The exact legacy package/topology/cluster-version
bridge is described below. Apply then converges or revalidates forward; it
never adopts an unpinned database, regenerates keys, removes state, or consumes
the restart marker. Any other mixed/downgraded package, membership/topology
difference, unexpected progress, CID, marker, database, doctor, or service
state remains a hard failure; do not edit the ledger to bypass it.

Before stopping an active node stack, `postinst` creates or validates
`central-restart-pending`, then atomically writes the root-only mode-0600
`/var/lib/pvn-node/node-restart-pending` intent. The record pins the exact
package version and the original PVN Control, OVN NB, OVN SB, and northd PIDs.
Only configuration of that same package may resume an inactive or failed node
target from the intent. A first install, an intentional clean stop, or a legacy
failed state without the intent remains inert. Success requires all six node
units active plus unchanged final central state and PIDs; only then is the
intent consumed. Failure invalidates node readiness and retains the intent. Do
not edit or delete it: fix the cause and rerun configuration of the same
package version.

Package installation restarts only an already-active per-node
manager/agent/controller stack. The updater deliberately does not restart
active PVN Control, OVN NB/SB, or northd processes and verifies that their PIDs
did not change. The package records a root-only durable restart-required marker
at `/var/lib/pvn-node/central-restart-pending` for each active central target,
and both plan/apply report the affected nodes.
After the package rollout and full Raft convergence, restart those central
services separately, one healthy voter at a time. Dynamically restart every
current standby before the current active northd, and retain **every** restart
marker until the complete restart wave passes the all-node strict northd gate.
Never activate a previously inactive target or restart enough voters
concurrently to lose quorum. Mixed-version compatibility is required for the
duration of this rolling window; use a maintenance window for releases that
declare a breaking database or wire-protocol change.

While a restart marker exists, updater probes deliberately preserve and inspect
the old central PID instead of requiring the newly installed northd launcher;
this keeps the package/schema repin prerequisite acyclic. The subsequent
central restart must pass `pvn-ovn-northd-ready.service`. Its marker-scoped
standby exception exists only to move the standby voters onto clustered
endpoints before the old active northd releases the SB lock. Once no active
voter has a restart marker, the all-node updater health sweep additionally
requires strict cfg synchronization, exactly one `active` northd role, and
every remaining northd in `standby`.

For an already `complete` schema-2 ledger, run `pvn-control-plane plan` and its
confirmed `apply` immediately after the package rollout, while every restart
marker still exists. The successful apply repins the durable package snapshot.
Only then perform the one-voter-at-a-time restart sequence below and consume
the markers after its global proof; clearing any marker before the repin or
during the restart wave intentionally makes recovery fail closed.

### One-time v0.2.16 half-configured recovery to v0.2.17

This bridge applies only to the captured v0.2.16 state: dpkg reports
`half-configured`, `pvn-node.target` is inactive, no
`node-restart-pending` intent exists, and the still-active central target has
kept its original four PIDs. The hosted updater correctly rejects this
incomplete dpkg state. First record those PIDs, download the v0.2.17 DEB and
manifest from the public release, and verify the exact DEB entry:

```sh
central_before=$(for unit in pvn-control-db.service \
  ovn-ovsdb-server-nb.service ovn-ovsdb-server-sb.service \
  ovn-northd.service; do
    systemctl show --property=MainPID --value "$unit"
  done | paste -sd, -)

curl --fail --show-error --location --remote-name \
  https://github.com/popododo0720/proxmox-ovn/releases/download/v0.2.17/pvn-node_0.2.17_amd64.deb
curl --fail --show-error --location --remote-name \
  https://github.com/popododo0720/proxmox-ovn/releases/download/v0.2.17/SHA256SUMS
grep '  pvn-node_0.2.17_amd64.deb$' SHA256SUMS | sha256sum --check --strict -
```

Install that verified package directly. Because this legacy interruption has
no durable node-restart intent, its `postinst` deliberately leaves the node
target stopped; explicitly start it after dpkg succeeds:

```sh
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  --only-upgrade --no-remove -- ./pvn-node_0.2.17_amd64.deb
systemctl start pvn-node.target
```

Require dpkg to report v0.2.17 fully installed, both targets and every node
service to be active, `central-restart-pending` to contain exactly v0.2.17,
and the four central PIDs to equal `central_before`:

```sh
test "$(dpkg-query -W -f='${db:Status-Abbrev} ${Version}' pvn-node)" = \
  'ii  0.2.17'
systemctl is-active --quiet pvn-central.target pvn-node.target \
  pvn-control-db.service ovn-ovsdb-server-nb.service \
  ovn-ovsdb-server-sb.service ovn-northd.service ovn-controller.service \
  pvn-manager.service pvn-agent.service pvn-ovn-host-config.service \
  pvn-node-ready.service
test "$(cat /var/lib/pvn-node/central-restart-pending)" = 0.2.17
central_after=$(for unit in pvn-control-db.service \
  ovn-ovsdb-server-nb.service ovn-ovsdb-server-sb.service \
  ovn-northd.service; do
    systemctl show --property=MainPID --value "$unit"
  done | paste -sd, -)
test "$central_after" = "$central_before"
```

Do not use this bridge for a v0.2.17-or-newer interruption. Those retries must
resume only through their matching durable `node-restart-pending` intent.

### Active v0.2.13 schema-1 upgrade to v0.2.14

An active cluster whose complete v0.2.13 control snapshot still pins a
canonical schema-1 topology needs two repins after the v0.2.14 rolling package
update. Use this exact order:

1. Finish the v0.2.14 rolling updater on every node. Keep every central target
   running and retain every `central-restart-pending` marker. Do not begin the
   one-voter restart sequence yet.
2. Reuse the exact Geneve/provider CIDRs, guest MTU, provider acknowledgement,
   and cluster name from the completed topology transaction. Review and apply
   the active ledger-only upgrade:

   ```sh
   /usr/lib/pvn/pvn-topology plan \
     --geneve-cidr EXACT_GENEVE_CIDR --provider-cidr EXACT_PROVIDER_CIDR \
     --guest-mtu EXACT_GUEST_MTU

   /usr/lib/pvn/pvn-topology apply \
     --geneve-cidr EXACT_GENEVE_CIDR --provider-cidr EXACT_PROVIDER_CIDR \
     --guest-mtu EXACT_GUEST_MTU \
     --provider-port-ready OPENSTACK_PROVIDER_PORTS_ALLOW_ARBITRARY_MAC_IP \
     --confirm CLUSTER_NAME
   ```

   The plan must report `Upgrade readiness: READY`. Apply re-proves every
   active node, complete topology journal, live doctor/Raft state, restart
   marker, raw complete control ledger, and persisted/runtime Corosync pin. It
   performs one shared topology-ledger compare-and-swap from the canonical
   schema-1 file to its schema-2 projection. It does not change networking,
   Corosync, journals, services, or markers.
3. Run the control plan. At the exact legacy boundary it reports both
   `package_repin_required: true` and
   `topology_schema_repin_required: true`. Apply with the cluster name:

   ```sh
   /usr/lib/pvn/pvn-control-plane plan
   /usr/lib/pvn/pvn-control-plane apply --confirm CLUSTER_NAME
   ```

   The combined bridge is deliberately one-use and exact: the complete
   control snapshot must uniformly pin package `0.2.13`, cluster version `3`,
   and the canonical schema-1 digest; all live nodes must uniformly run
   `0.2.14`; and current PVE membership, the schema-2 Corosync candidate, and
   every persisted/runtime Corosync report must agree on config version `4`.
   The complete targets, PKI, database identities, N/N Raft health, doctors,
   and restart markers must also pass the normal complete-ledger proof. Apply
   repeats everything under the mutation lease and atomically pins the fresh
   schema-2/package/cluster-version snapshot.
4. Re-run both plans. The topology upgrade must be `already complete` and the
   control plan must require no repin. Only then restart central voters one at
   a time and consume their markers as described below.

This is not a general `current = pinned + 1` exception. A different package
pair, version jump, topology projection, journal, membership, or control phase
fails closed and requires operator investigation.

Do not clear **any** marker while any voter still runs an old central process.
Once every database has converged beyond the one-member seed, write each exact
voter name and management IP into `PVN_VOTERS`. Before each restart, collect a fresh
`ovn-appctl -t ovn-northd status` from every voter and require exactly one
active role. Restart an as-yet-unrestarted standby, re-read all roles, and
repeat. Restart the then-current active voter only after every other voter has
completed. A role change changes the order; a stale initial list never
authorizes restarting the new active early.

```sh
PVN_VOTERS='EXACT_VOTER_1=MANAGEMENT_IP_1 EXACT_VOTER_2=MANAGEMENT_IP_2 EXACT_VOTER_3=MANAGEMENT_IP_3'
pvn_voter_ssh() {
  voter=$1
  shift
  node=${voter%%=*}
  address=${voter#*=}
  [ "$node" != "$voter" ] && [ -n "$node" ] && [ -n "$address" ] || return 1
  timeout 150 ssh -q -F /dev/null -i /root/.ssh/id_rsa \
    -o BatchMode=yes -o PasswordAuthentication=no \
    -o KbdInteractiveAuthentication=no -o IdentitiesOnly=yes \
    -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o CheckHostIP=no \
    -o "HostKeyAlias=$node" \
    -o "UserKnownHostsFile=/etc/pve/nodes/$node/ssh_known_hosts" \
    -o GlobalKnownHostsFile=none -o ConnectTimeout=10 \
    "root@$address" "$@"
}
transition_roles() {
  transition_records=$(
    for voter in $PVN_VOTERS; do
      node=${voter%%=*}
      role=$(pvn_voter_ssh "$voter" /usr/bin/ovn-appctl -t ovn-northd status) || exit
      printf '%s %s\n' "$node" "$role"
    done
  ) || return
  PVN_TRANSITION_RECORDS="$transition_records" python3 - "$PVN_VOTERS" <<'PY'
import os, re, sys
entries = sys.argv[1].split()
if len(entries) < 3 or len(entries) % 2 == 0:
    raise SystemExit("transition requires the exact odd voter set")
names = []
addresses = []
for entry in entries:
    if entry.count("=") != 1:
        raise SystemExit("voter mapping must contain exact name=management-IP pairs")
    name, address = entry.split("=", 1)
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", name) or not address:
        raise SystemExit("unsafe voter mapping")
    names.append(name)
    addresses.append(address)
if len(set(names)) != len(names) or len(set(addresses)) != len(addresses):
    raise SystemExit("duplicate voter name or management IP")
roles = {}
for line in os.environ.get("PVN_TRANSITION_RECORDS", "").splitlines():
    fields = line.split()
    if len(fields) != 3 or fields[1] != "Status:" or fields[2] not in {"active", "standby"}:
        raise SystemExit(f"invalid northd transition role: {line!r}")
    if fields[0] in roles:
        raise SystemExit(f"duplicate northd transition role: {fields[0]}")
    roles[fields[0]] = fields[2]
if set(roles) != set(names) or list(roles.values()).count("active") != 1:
    raise SystemExit("transition requires exact voter coverage and one active")
for name in names:
    print(f"{name}={roles[name]}")
PY
}
transition_selection_gate() {
  selected=$1
  completed=$2
  roles=$(transition_roles) || return
  PVN_TRANSITION_ROLES="$roles" python3 - "$PVN_VOTERS" "$selected" "$completed" <<'PY'
import os, sys
names = [entry.split("=", 1)[0] for entry in sys.argv[1].split()]
selected = sys.argv[2]
completed = sys.argv[3].split()
if len(completed) != len(set(completed)) or not set(completed) <= set(names):
    raise SystemExit("completed voter list is invalid")
if selected not in names or selected in completed:
    raise SystemExit("selected voter is unknown or already completed")
roles = dict(line.split("=", 1) for line in os.environ["PVN_TRANSITION_ROLES"].splitlines())
unfinished = [name for name in names if name not in completed]
unfinished_standbys = [name for name in unfinished if roles[name] == "standby"]
unfinished_active = [name for name in unfinished if roles[name] == "active"]
eligible = unfinished_standbys or unfinished_active
if selected not in eligible:
    raise SystemExit(
        f"selected {selected} is not in the next eligible set: {eligible}"
    )
print(
    f"PVN transition selection: node={selected} role={roles[selected]} "
    f"unfinished={len(unfinished)}"
)
PY
}

PVN_COMPLETED=
transition_roles
# Before each voter, set its exact name and require this gate to pass:
PVN_SELECTED=EXACT_NEXT_VOTER
transition_selection_gate "$PVN_SELECTED" "$PVN_COMPLETED"
```

On the selected voter, use this sequence. The JSON guard proves that the
remaining connected members can still form quorum without it. All four PID
checks must pass individually, proving that PVN Control, NB, SB, and northd
each executed the installed unit. The marker is checked but deliberately not
removed:

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
central_pid_change_check() {
  python3 - "$1" "$2" <<'PY'
import sys
names = ("PVN Control", "OVN NB", "OVN SB", "ovn-northd")
before = sys.argv[1].split(",")
after = sys.argv[2].split(",")
if len(before) != 4 or len(after) != 4:
    raise SystemExit("expected four central PIDs before and after restart")
for name, old, new in zip(names, before, after):
    if not old.isdigit() or old == "0" or not new.isdigit() or new == "0":
        raise SystemExit(f"{name} returned an invalid PID: {old!r} -> {new!r}")
    if old == new:
        raise SystemExit(f"{name} PID did not change: {old}")
PY
}
marker_check() {
  marker=/var/lib/pvn-node/central-restart-pending
  [ -d /var/lib/pvn-node ] && [ ! -L /var/lib/pvn-node ] || return 1
  [ "$(stat -c '%u:%g:%a' /var/lib/pvn-node)" = 0:0:700 ] || return 1
  installed=$(dpkg-query -W -f='${Version}' pvn-node) || return 1
  python3 - "$marker" "$installed" <<'PY'
import os, pathlib, stat, sys
path = pathlib.Path(sys.argv[1])
expected = (sys.argv[2] + "\n").encode("ascii")
before = path.lstat()
identity = (before.st_dev, before.st_ino)
if (not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode)
        or before.st_nlink != 1 or before.st_uid != 0 or before.st_gid != 0
        or stat.S_IMODE(before.st_mode) != 0o600 or before.st_size > 256):
    raise SystemExit("unsafe central restart marker")
flags = os.O_RDONLY | os.O_CLOEXEC | getattr(os, "O_NOFOLLOW", 0)
descriptor = os.open(path, flags)
try:
    opened = os.fstat(descriptor)
    if (opened.st_dev, opened.st_ino) != identity:
        raise SystemExit("central restart marker changed before open")
    raw = os.read(descriptor, 257)
    if os.read(descriptor, 1) or len(raw) > 256:
        raise SystemExit("central restart marker is oversized")
finally:
    os.close(descriptor)
after = path.lstat()
if (after.st_dev, after.st_ino) != identity or raw != expected:
    raise SystemExit("central restart marker changed or has the wrong version")
PY
}

marker_check
raft_drain_check
before=$(central_pids)
systemctl restart pvn-central.target
pvnctl central status
raft_drain_check
after=$(central_pids)
central_pid_change_check "$before" "$after"
marker_check
```

Stop at the first failed command. Keep every marker and repeat only after the
restarted voter and all three databases pass the guard. Back on the
coordinating node, append the completed voter exactly once and immediately
rerun the parsed role proof before selecting the next voter:

```sh
PVN_COMPLETED="${PVN_COMPLETED:+$PVN_COMPLETED }$PVN_SELECTED"
transition_roles
```

Once every voter has
completed, run this bounded all-node gate from a PVE node. It requires the
normal strict `status` output on every voter, one active role, all other roles
standby, and one common synchronized cfg value:

```sh
strict_northd_proof() {
  strict_records=$(
    for voter in $PVN_VOTERS; do
      node=${voter%%=*}
      record=$(pvn_voter_ssh "$voter" /usr/lib/pvn/pvn-ovn-northd status) || exit
      printf '%s %s\n' "$node" "$record"
    done
  ) || return
  PVN_STRICT_RECORDS="$strict_records" python3 - "$PVN_VOTERS" <<'PY'
import os, re, sys
entries = sys.argv[1].split()
if any(entry.count("=") != 1 for entry in entries):
    raise SystemExit("voter mapping must contain exact name=management-IP pairs")
expected = [entry.split("=", 1)[0] for entry in entries]
records = os.environ.get("PVN_STRICT_RECORDS", "").splitlines()
if len(expected) < 3 or len(expected) % 2 == 0 or len(records) != len(expected):
    raise SystemExit("strict proof does not cover the exact odd voter set")
roles = []
cfgs = []
seen = set()
for line in records:
    fields = line.split()
    if len(fields) != 6 or fields[1] != "PVN_NORTHD":
        raise SystemExit(f"malformed strict status: {line!r}")
    node = fields[0]
    values = dict(field.split("=", 1) for field in fields[2:])
    if node in seen or set(values) != {"role", "nb", "sb", "cfg"}:
        raise SystemExit(f"duplicate node or fields: {line!r}")
    if values["nb"] != "connected" or values["sb"] != "connected":
        raise SystemExit(f"disconnected northd: {line!r}")
    if values["role"] not in {"active", "standby"}:
        raise SystemExit(f"invalid northd role: {line!r}")
    if not re.fullmatch(r"0|[1-9][0-9]*", values["cfg"]):
        raise SystemExit(f"invalid synchronized cfg: {line!r}")
    seen.add(node)
    roles.append(values["role"])
    cfgs.append(values["cfg"])
if seen != set(expected) or roles.count("active") != 1:
    raise SystemExit("strict proof requires exactly one active over every voter")
if roles.count("standby") != len(expected) - 1 or len(set(cfgs)) != 1:
    raise SystemExit("strict proof requires all remaining standbys and one cfg")
print(f"PVN northd strict proof: voters={len(expected)} cfg={cfgs[0]}")
PY
}
strict_deadline=$(($(date +%s) + 150))
until strict_northd_proof; do
  [ "$(date +%s)" -lt "$strict_deadline" ] || {
    echo "northd did not reach one strict synchronized role set in 150s" >&2
    exit 1
  }
  sleep 2
done
```

Only after that proof may the markers be consumed. Remove all of them, not one
during the restart wave, and immediately rerun the hosted updater in plan mode
so its no-marker health path independently repeats strict cfg and one-active
validation:

```sh
for voter in $PVN_VOTERS; do
  pvn_voter_ssh "$voter" /usr/bin/python3 - <<'PVN_REMOVE_MARKER' || exit
import os, pathlib, re, stat, subprocess
directory = pathlib.Path("/var/lib/pvn-node")
marker = directory / "central-restart-pending"
directory_metadata = directory.lstat()
if (not stat.S_ISDIR(directory_metadata.st_mode)
        or stat.S_ISLNK(directory_metadata.st_mode)
        or directory_metadata.st_uid != 0 or directory_metadata.st_gid != 0
        or stat.S_IMODE(directory_metadata.st_mode) != 0o700):
    raise SystemExit("unsafe restart state directory")
directory_fd = os.open(
    directory,
    os.O_RDONLY | os.O_CLOEXEC | getattr(os, "O_DIRECTORY", 0)
    | getattr(os, "O_NOFOLLOW", 0),
)
try:
    try:
        before = os.stat(marker.name, dir_fd=directory_fd, follow_symlinks=False)
    except FileNotFoundError:
        print("restart marker already consumed")
        raise SystemExit(0)
    if (not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode)
            or before.st_nlink != 1 or before.st_uid != 0 or before.st_gid != 0
            or stat.S_IMODE(before.st_mode) != 0o600 or before.st_size > 256):
        raise SystemExit("unsafe central restart marker")
    result = subprocess.run(
        ["/usr/bin/dpkg-query", "-W", "-f=${Status}\\t${Version}\\n", "pvn-node"],
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        timeout=10, check=False, env={"LC_ALL": "C", "PATH": "/usr/bin:/bin"},
    )
    match = re.fullmatch(
        rb"install ok installed\t([A-Za-z0-9.+~_-]{1,128})\n", result.stdout
    )
    if result.returncode != 0 or match is None:
        raise SystemExit("cannot prove the exact installed pvn-node version")
    marker_fd = os.open(
        marker.name,
        os.O_RDONLY | os.O_CLOEXEC | getattr(os, "O_NOFOLLOW", 0),
        dir_fd=directory_fd,
    )
    try:
        opened = os.fstat(marker_fd)
        identity = (before.st_dev, before.st_ino)
        if (opened.st_dev, opened.st_ino) != identity:
            raise SystemExit("central restart marker changed before open")
        raw = os.read(marker_fd, 257)
        if os.read(marker_fd, 1) or len(raw) > 256:
            raise SystemExit("central restart marker is oversized")
    finally:
        os.close(marker_fd)
    after = os.stat(marker.name, dir_fd=directory_fd, follow_symlinks=False)
    if (after.st_dev, after.st_ino) != identity or raw != match.group(1) + b"\n":
        raise SystemExit("central restart marker changed or has the wrong version")
    os.unlink(marker.name, dir_fd=directory_fd)
finally:
    os.close(directory_fd)
PVN_REMOVE_MARKER
done
pvn_version=$(dpkg-query -W -f='${Version}' pvn-node)
case "$pvn_version" in ''|*[!A-Za-z0-9.+~_-]*) exit 1 ;; esac
for voter in $PVN_VOTERS; do
  remote_version=$(pvn_voter_ssh "$voter" /bin/sh -s <<'PVN_REMOTE_VERSION'
exec /usr/bin/dpkg-query -W -f='${Version}' pvn-node
PVN_REMOTE_VERSION
  ) || exit
  [ "$remote_version" = "$pvn_version" ] || exit
done
pvn_bootstrap=$(curl -fsSL \
  "https://github.com/popododo0720/proxmox-ovn/releases/download/v$pvn_version/pvn-update.sh") && \
  PVN_VERSION="$pvn_version" PVN_PHASE=plan bash -c "$pvn_bootstrap"
```

If marker removal stops after only some voters, do not continue from the
failed command alone. Re-run the complete all-node strict proof, verify that
already-consumed markers are absent and every remaining marker still safely
matches the same installed version, and only then rerun the idempotent removal
loop; an absent marker is counted as already completed.

The marker lives outside `/var/lib/pvn` because that standard service state
directory is writable by the unprivileged `pvn` account; restart authorization
state must remain root-only.

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

Use [`database-backup.md`](database-backup.md) for the packaged create/verify
commands and the deliberately manual, one-database-at-a-time restore runbook.
There is no automatic restore subcommand.

If an activated node blocks `pve-guests` during boot, inspect
`pvn-node-ready.service` and fix its reported local or control-plane failure.
Then restart `pvn-node.target` and `pve-guests.service`. To deliberately return
the host to non-PVN guest boot behavior, first stop and disable
`pvn-node.target`, remove `/etc/pvn/node-enabled`, and then restart
`pve-guests.service`; this is an explicit networking ownership change, not an
automatic fallback.
