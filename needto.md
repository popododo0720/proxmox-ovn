# PVN — shipped, verified, and still needed

Status snapshot for follow-up work. Reflects the intended v1 feature set in
`README.md` / `docs/architecture.md`, source-tree work for v0.2.14, the live
three-node lab evidence through v0.2.13, and gaps that still need destructive
recovery tests.

**How to use:** treat “v1 must-pass” as release gates; everything else is
prioritized backlog. Update this file when items ship or scope changes.

---

## Already in v1 (do not re-build; verify on real PVE)

- [x] PVE Pool → PVN Project mapping
- [x] Geneve tenant networks + IPv4 subnets + OVN native DHCP
- [x] Logical routers, centralized SNAT, floating IPs
- [x] Flat / VLAN provider networks
- [x] Stateful security groups (+ rules)
- [x] Running VM NIC attach / detach (fail-closed)
- [x] Embedded PVN page in Proxmox UI
- [x] Durable control store (OVSDB) + OVN NB reconcile
- [x] Cluster install / bootstrap / activation markers
- [x] Unit tests green (`make test`)

Use the checkboxes above as an **E2E verification** list on a disposable PVE 9
cluster (not as missing features).

---

## v1 must-pass (validation / release)

These are not new product features so much as **proof the current code works**.

### E2E on real hosts

- [x] Full install path: preflight → package stage → config/PKI → central →
      `node-enabled` / targets → services healthy
- [x] `pvnctl doctor` / health green on all members
- [x] Create project → network → subnet → DHCP works in guest
- [x] Attach NIC to **running** VM → link up → same-subnet ping
- [x] Detach cleanly; no orphan LSP / TAP drift
- [x] SNAT and/or floating IP path to provider network
- [x] Security group allow / deny behaves as expected
- [x] Node reboot: agent re-binds; guests recover or fail closed as designed
- [x] Package upgrade on active node (fail-closed / safe path)
- [ ] Backup/restore drill: in a separate window for each database, freeze all
      writers and independently copy / verify a fresh set, then restore exactly
      one of PVN_Control, OVN NB, or OVN SB; prove 3/3 Raft/CID, reconcile,
      security policy, and dataplane health after each (no cross-DB transaction
      assumption)

### Operator prerequisites (document + checklist in runbooks)

PVN does **not** auto-wire physical networking. Confirm every node has:

- [x] Distinct management vs Geneve encap addresses
- [x] Underlay MTU plan (1500 underlay, 1300 lab guest MTU)
- [x] Confirmed topology installer wiring for provider bridge + uplink policy
- [x] SSH root pubkey + strict known_hosts for installers
- [x] No conflicting pre-existing OVN ownership on the host

### Release packaging

- [x] Freeze and publish a release tree (`pvn-node` deb, `pvn-cluster-install`,
      `SHA256SUMS`, `pvn-install.sh`) on a real HTTPS host if using curl install
- [x] Keep GitHub release assets consistent with README

### Current product work

- [x] Show human names in tables/selectors and keep full UUIDs in Details only
- [x] Validate that a Project mapping names an existing PVE pool
- [x] Guide first-use browser trust for the same-node `:8443` manager certificate
- [x] Keep the stateful-only security-group UI honest (no ineffective toggle)
- [x] Auto-provision and continuously repair a project default security policy;
      keep legacy unrestricted ports explicit until preview-token backfill
- [x] Fence Corosync Geneve-to-management migration through an N/N dual-ring
      safety path, exact persisted/runtime gates, resumable stage boundaries,
      and strict recovery of the uniform v0.2.13 stale-runtime shape before any
      host-network mutation

---

## High priority (v1.x — real operational holes)

### Compute integration

- [ ] **Live migration coordination** — rebind chassis / port on migrate
      (largest functional gap if the cluster uses live migrate)
- [ ] **HA restart / unexpected node move** — restore or fail closed correctly
- [ ] **VM start policy** — PVE UI/API start can still boot with a disconnected
      TAP until deeper start/HA hooks exist; enforce operationally or integrate
- [ ] Clone / template / snapshot NIC + port lifecycle (avoid duplicate MACs /
      orphan ports)

### Day-2 networking

- [ ] DNS integration (guest resolver via DHCP options and/or project DNS)
- [ ] Static / extra routes beyond basic router interfaces
- [ ] Better multi-exit / gateway HA story (beyond single centralized SNAT model)
- [x] Port / chassis / binding diagnostics in UI (control, PVE NIC, runtime
      resolver, and unrestricted-policy warnings; raw IDs stay in Details)
- [ ] SG / ACL deny visibility (logging or counters for debug)

### Multitenancy & safety

- [ ] Quotas (ports, IPs, FIPs per project)
- [ ] Stronger audit trail for attach/detach and policy changes
- [ ] Isolation self-test tool (east-west matrix)

### Ops tooling

- [ ] Metrics / alerting hooks (Prometheus or equivalent)
- [ ] Safer automated Raft membership changes (today: careful manual procedures;
      leave/kick not exposed as casual CLI for good reason)
- [x] One-button or scripted backup verification (`pvn-db-backup create/verify`)

---

## Intentionally out of first release (v2+ unless scope changes)

Do not block v1 on these unless product goals change:

| Item | Notes |
| --- | --- |
| PVE built-in SDN coexistence | Use PVN instead of PVE SDN for tenant overlays |
| BGP | DC/edge routing integration |
| IPv6 / dual-stack | Tenant + provider |
| Load balancers | Octavia-class |
| Metadata service | cloud-init `169.254.169.254` style |
| LXC support | VM-only for now |
| Distributed SNAT / advanced edge services | Scale-out north-south |

---

## Suggested order while validating

1. Finish **v1 must-pass E2E** checkboxes on a lab cluster.
2. Whatever **blocks real use** during that run becomes the top of **High priority**
   (almost always migration/start/HA if you move VMs; otherwise DNS/diagnostics).
3. Only then pull from **v2+** list.

---

## Notes

- Unit tests and package-check are necessary but **not** a substitute for staged
  PVE 9 host tests (`docs/development.md`).
- Three control planes (PVN_Control, OVN NB, OVN SB) are **not** one atomic DB;
  recovery and membership changes must stay one-database-at-a-time.
- PVE UI inject is not a stable upstream ABI; re-verify on each PVE minor bump.

---

*Last written for the validation phase around PVN 0.2.x; revise as features land.*
