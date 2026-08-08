#!/usr/bin/python3
"""Keep packaged Perl lifecycle calls bijective with the Go root-socket API."""

from __future__ import annotations

import pathlib
import re


REPO = pathlib.Path(__file__).resolve().parents[2]
perl = (REPO / "pve-api/PVN/ComputeLifecycle.pm").read_text(encoding="utf-8")
go_lifecycle = (REPO / "internal/api/compute_lifecycle.go").read_text(encoding="utf-8")
go_resources = (REPO / "internal/api/compute_resources.go").read_text(encoding="utf-8")
go = go_lifecycle + "\n" + go_resources

perl_paths = set(
    re.findall(
        r"^\s*[a-z_]+\s*=>\s*'(/api/v1/runtime/compute/[^']+)'",
        perl,
        flags=re.MULTILINE,
    )
)
go_constants = dict(
    re.findall(
        r"\b(compute[A-Za-z]+Path)\s*=\s*\"(/api/v1/runtime/compute/[^\"]+)\"",
        go,
    )
)
route_cases = set(re.findall(r"\bcase\s+(compute[A-Za-z]+Path):", go_lifecycle))

assert perl_paths, "Perl compute endpoint table is empty"
assert len(go_constants) == len(set(go_constants.values())), "Go compute paths are duplicated"
assert perl_paths == set(go_constants.values()), (
    "Perl/Go compute endpoint drift",
    sorted(perl_paths - set(go_constants.values())),
    sorted(set(go_constants.values()) - perl_paths),
)
assert set(go_constants) == route_cases, (
    "Go compute endpoint constant lacks an exact switch route",
    sorted(set(go_constants) - route_cases),
    sorted(route_cases - set(go_constants)),
)

for field in ("source_node", "source_template", "snapshot_epoch"):
    assert re.search(rf"\b{field}\s*=>", perl), f"Perl never sends required {field}"
    assert f'json:"{field}' in go, f"Go request/transaction schema lacks {field}"

ha_proof_fields = (
    "origin",
    "service_id",
    "manager_epoch",
    "service_uid",
    "service_node",
    "service_state",
    "node_states",
    "lrm_node",
    "lrm_epoch",
    "lrm_state",
    "lrm_mode",
    "agent_lock_epoch",
)
assert 'json:"ha_proof,omitempty"' in go_lifecycle, "Go start schema lacks HA proof"
for field in ha_proof_fields:
    assert re.search(rf"\b{field}\s*=>", perl), f"Perl HA proof omits {field}"
    assert f'json:"{field}"' in go_lifecycle, f"Go HA proof omits {field}"
assert "sha256_hex(join(\"\\0\", int($vmid), $node, $service_uid))" in perl, (
    "Perl canonical HA lifecycle digest changed"
)
assert 'fmt.Sprint(vmid) + "\\x00" + target + "\\x00" + serviceUID' in go_lifecycle, (
    "Go canonical HA lifecycle digest changed"
)

for action in ("snapshot_prepare", "snapshot_commit", "snapshot_abort", "snapshot_cleanup"):
    assert re.search(rf"^sub {action} \{{", perl, flags=re.MULTILINE), (
        f"Perl lacks {action} lifecycle helper"
    )
assert "lifecycle_id => _lifecycle_id(\"snapshot-$action\"" in perl, (
    "snapshot prepare lacks one client-generated retry identity"
)
assert 'action => "$action"' in perl and "$payload->{nics} = $nics if $action eq 'rollback'" in perl, (
    "Perl snapshot mutation payload does not distinguish rollback proof from identity-only delete"
)
for field in ("lifecycle_id", "action", "operation_id", "payload_hash"):
    assert f'json:"{field}' in go_resources, f"Go snapshot transaction schema lacks {field}"
assert "snapshot_verify =>" not in perl and "snapshot_delete =>" not in perl, (
    "legacy unfenced snapshot endpoints remain packaged"
)
assert "$payload->{snapshot_epoch} = snapshot_epoch($source_conf)" in perl, (
    "snapshot clone does not pin selected PVE snaptime"
)
assert "snapshots => $snapshots" in perl, "Perl destroy capture omits snapshot identities"
assert 'json:"snapshots' in go_resources, "Go destroy schema lacks snapshot identities"

print("pvn compute Perl/Go contract tests passed")
