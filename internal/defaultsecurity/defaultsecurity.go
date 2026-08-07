// Package defaultsecurity owns the cluster-global system-managed security
// policy. Every port that uses the default security group is part of one routed
// self-ingress trust domain: the baseline allows all IPv4 egress and IPv4
// ingress from every other port attached to that same global group.
package defaultsecurity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	DefaultSecurityGroupName        = "default"
	DefaultSecurityGroupDescription = "PVN managed cluster default security group"
	DefaultEgressDescription        = "Allow all IPv4 egress"
	DefaultIngressDescription       = "Allow IPv4 ingress from all ports in the cluster default trust domain"
)

// Reconciler realizes one desired resource in OVN.
type Reconciler interface {
	Reconcile(context.Context, model.Kind, string) error
}

// Manager creates and repairs the deterministic cluster-global default policy.
type Manager struct {
	store      controlstore.Store
	reconciler Reconciler
}

// BlockedReason identifies policy states that Ensure must not overwrite.
type BlockedReason string

const (
	BlockedNone           BlockedReason = ""
	BlockedNameCollision  BlockedReason = "default_name_collision"
	BlockedMalformedGroup BlockedReason = "deterministic_group_malformed"
	BlockedMalformedRule  BlockedReason = "deterministic_rule_malformed"
)

// Inspection is a read-only assessment of the cluster baseline. Missing
// resources are repairable; BlockedReason requires operator intervention.
type Inspection struct {
	GroupID            string        `json:"group_id"`
	MissingResourceIDs []string      `json:"missing_resource_ids,omitempty"`
	Ready              bool          `json:"ready"`
	BlockedReason      BlockedReason `json:"blocked_reason,omitempty"`
	Detail             string        `json:"detail,omitempty"`
}

func New(store controlstore.Store, reconciler Reconciler) *Manager {
	return &Manager{store: store, reconciler: reconciler}
}

// DefaultSecurityGroupID is stable across all managers and installations.
func DefaultSecurityGroupID() string {
	return deterministicUUID("pvn/default-security-group/cluster/v1")
}

// DefaultEgressRuleID identifies the system-managed IPv4 egress rule.
func DefaultEgressRuleID() string {
	return deterministicUUID("pvn/default-security-group-rule/cluster-egress-ipv4/v1")
}

// DefaultIngressRuleID identifies the system-managed IPv4 self-ingress rule.
func DefaultIngressRuleID() string {
	return deterministicUUID("pvn/default-security-group-rule/cluster-ingress-self-ipv4/v1")
}

func deterministicUUID(domain string) string {
	digest := sha256.Sum256([]byte(domain))
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(digest[:16])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// IsReserved reports whether resource is one of the deterministic policy
// objects. API mutation paths use this to prevent the baseline from being
// renamed, weakened, or deleted.
func IsReserved(resource model.Resource) bool {
	if resource == nil {
		return false
	}
	switch resource.ResourceKind() {
	case model.KindSecurityGroup:
		return resource.GetMetadata().ID == DefaultSecurityGroupID()
	case model.KindSecurityGroupRule:
		id := resource.GetMetadata().ID
		return id == DefaultEgressRuleID() || id == DefaultIngressRuleID()
	default:
		return false
	}
}

// Inspect assesses the deterministic baseline without creating, updating, or
// reconciling any resource.
func (m *Manager) Inspect(ctx context.Context) (Inspection, error) {
	inspection := Inspection{GroupID: DefaultSecurityGroupID(), Ready: true}
	if m == nil || m.store == nil {
		return inspection, errors.New("default security policy store is not configured")
	}

	desiredGroup := desiredGroup()
	groups, err := m.store.List(ctx, model.KindSecurityGroup, controlstore.ListOptions{})
	if err != nil {
		return inspection, err
	}
	for _, resource := range groups {
		group := resource.(*model.SecurityGroup)
		if group.Name == DefaultSecurityGroupName && group.ID != desiredGroup.ID {
			inspection.Ready = false
			inspection.BlockedReason = BlockedNameCollision
			inspection.Detail = fmt.Sprintf("a security group already uses the reserved name %q", DefaultSecurityGroupName)
			return inspection, nil
		}
	}

	resources := []model.Resource{desiredGroup, desiredEgressRule(), desiredIngressRule()}
	for _, desired := range resources {
		current, getErr := m.store.Get(ctx, desired.ResourceKind(), desired.GetMetadata().ID)
		if errors.Is(getErr, controlstore.ErrNotFound) {
			inspection.Ready = false
			inspection.MissingResourceIDs = append(inspection.MissingResourceIDs, desired.GetMetadata().ID)
			continue
		}
		if getErr != nil {
			return inspection, getErr
		}
		var verifyErr error
		switch value := desired.(type) {
		case *model.SecurityGroup:
			verifyErr = sameGroup(current.(*model.SecurityGroup), value)
		case *model.SecurityGroupRule:
			verifyErr = sameRule(current.(*model.SecurityGroupRule), value)
		}
		if verifyErr != nil {
			inspection.Ready = false
			if desired.ResourceKind() == model.KindSecurityGroup {
				inspection.BlockedReason = BlockedMalformedGroup
			} else {
				inspection.BlockedReason = BlockedMalformedRule
			}
			inspection.Detail = verifyErr.Error()
			return inspection, nil
		}
		if !resourceReady(current) {
			inspection.Ready = false
		}
	}
	return inspection, nil
}

// Ensure creates missing deterministic policy rows, verifies existing rows,
// and realizes each row before returning the group for port attachment. The
// restrictive group is realized before either allow rule.
func (m *Manager) Ensure(ctx context.Context) (*model.SecurityGroup, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("default security policy store is not configured")
	}
	group := desiredGroup()
	if err := m.rejectNameCollision(ctx, group); err != nil {
		return nil, err
	}
	ensuredGroup, err := m.ensureResource(ctx, group)
	if err != nil {
		return nil, err
	}
	if err := sameGroup(ensuredGroup.(*model.SecurityGroup), group); err != nil {
		return nil, err
	}
	ensuredGroup, err = m.reconcileAndRequireReady(ctx, ensuredGroup)
	if err != nil {
		return nil, err
	}

	for _, desired := range []*model.SecurityGroupRule{desiredEgressRule(), desiredIngressRule()} {
		ensured, ensureErr := m.ensureResource(ctx, desired)
		if ensureErr != nil {
			return nil, ensureErr
		}
		if verifyErr := sameRule(ensured.(*model.SecurityGroupRule), desired); verifyErr != nil {
			return nil, verifyErr
		}
		if _, reconcileErr := m.reconcileAndRequireReady(ctx, ensured); reconcileErr != nil {
			return nil, reconcileErr
		}
	}

	latest, err := m.store.Get(ctx, model.KindSecurityGroup, group.ID)
	if err != nil {
		return nil, err
	}
	return latest.(*model.SecurityGroup), nil
}

// EnsureAll retains the manager repair-loop contract while ensuring the one
// cluster-global default policy.
func (m *Manager) EnsureAll(ctx context.Context) error {
	_, err := m.Ensure(ctx)
	return err
}

// Probe reports whether the cluster-global default policy is complete and
// realized. It is read-only and suitable for the manager health endpoint.
func (m *Manager) Probe(ctx context.Context) error {
	inspection, err := m.Inspect(ctx)
	if err != nil {
		return err
	}
	if inspection.BlockedReason != BlockedNone {
		return fmt.Errorf("default security policy is blocked: %s", inspection.BlockedReason)
	}
	if !inspection.Ready {
		return errors.New("default security policy is incomplete or not realized")
	}
	return nil
}

func resourceReady(resource model.Resource) bool {
	meta := resource.GetMetadata()
	return meta.State == model.ResourceReady && meta.AppliedRevision == meta.Revision
}

func desiredGroup() *model.SecurityGroup {
	return &model.SecurityGroup{
		Metadata:    model.Metadata{ID: DefaultSecurityGroupID()},
		Name:        DefaultSecurityGroupName,
		Description: DefaultSecurityGroupDescription,
		Stateful:    true,
	}
}

func desiredEgressRule() *model.SecurityGroupRule {
	return &model.SecurityGroupRule{
		Metadata:        model.Metadata{ID: DefaultEgressRuleID()},
		SecurityGroupID: DefaultSecurityGroupID(),
		Direction:       model.DirectionEgress,
		EtherType:       model.EtherTypeIPv4,
		Action:          model.ActionAllow,
		Description:     DefaultEgressDescription,
	}
}

// desiredIngressRule is intentionally self-referential. Because every default
// port uses this one cluster-global group, all such ports form one routed
// self-ingress trust domain across all logical networks.
func desiredIngressRule() *model.SecurityGroupRule {
	groupID := DefaultSecurityGroupID()
	return &model.SecurityGroupRule{
		Metadata:        model.Metadata{ID: DefaultIngressRuleID()},
		SecurityGroupID: groupID,
		Direction:       model.DirectionIngress,
		EtherType:       model.EtherTypeIPv4,
		RemoteGroupID:   groupID,
		Action:          model.ActionAllow,
		Description:     DefaultIngressDescription,
	}
}

func (m *Manager) rejectNameCollision(ctx context.Context, desired *model.SecurityGroup) error {
	groups, err := m.store.List(ctx, model.KindSecurityGroup, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	for _, resource := range groups {
		group := resource.(*model.SecurityGroup)
		if group.Name == DefaultSecurityGroupName && group.ID != desired.ID {
			return conflict("a non-PVN security group already uses the cluster-reserved name %q", DefaultSecurityGroupName)
		}
	}
	return nil
}

func (m *Manager) ensureResource(ctx context.Context, desired model.Resource) (model.Resource, error) {
	kind, id := desired.ResourceKind(), desired.GetMetadata().ID
	existing, err := m.store.Get(ctx, kind, id)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, controlstore.ErrNotFound) {
		return nil, err
	}
	created, _, err := m.store.Create(ctx, desired, "")
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, controlstore.ErrAlreadyExists) && !errors.Is(err, controlstore.ErrConflict) {
		return nil, err
	}
	// Another manager may have committed the deterministic row between the read
	// and write. Only adopt that exact ID; unique-name collisions remain errors.
	existing, getErr := m.store.Get(ctx, kind, id)
	if getErr == nil {
		return existing, nil
	}
	if kind == model.KindSecurityGroup {
		if collisionErr := m.rejectNameCollision(ctx, desired.(*model.SecurityGroup)); collisionErr != nil {
			return nil, collisionErr
		}
	}
	return nil, err
}

func (m *Manager) reconcileAndRequireReady(ctx context.Context, resource model.Resource) (model.Resource, error) {
	meta := resource.GetMetadata()
	if meta.State == model.ResourceReady && meta.AppliedRevision == meta.Revision {
		return resource, nil
	}
	if m.reconciler == nil {
		return resource, nil
	}
	if err := m.reconciler.Reconcile(ctx, resource.ResourceKind(), meta.ID); err != nil {
		return nil, fmt.Errorf("realize default security policy %s: %w", resource.ResourceKind(), err)
	}
	latest, err := m.store.Get(ctx, resource.ResourceKind(), meta.ID)
	if err != nil {
		return nil, err
	}
	latestMeta := latest.GetMetadata()
	if latestMeta.State != model.ResourceReady || latestMeta.AppliedRevision != latestMeta.Revision {
		return nil, fmt.Errorf("default security policy %s was not fully realized", resource.ResourceKind())
	}
	return latest, nil
}

func sameGroup(current, desired *model.SecurityGroup) error {
	if current.State == model.ResourceDeleting {
		return conflict("the default security group is being deleted")
	}
	if current.Name != desired.Name || current.Description != desired.Description || !current.Stateful {
		return conflict("the deterministic default security group contains non-baseline policy")
	}
	return nil
}

func sameRule(current, desired *model.SecurityGroupRule) error {
	if current.State == model.ResourceDeleting {
		return conflict("a default security group rule is being deleted")
	}
	if current.SecurityGroupID != desired.SecurityGroupID || current.Direction != desired.Direction ||
		current.EtherType != desired.EtherType || current.Protocol != "" ||
		current.PortRangeMin != 0 || current.PortRangeMax != 0 || current.RemoteCIDR != "" ||
		current.RemoteGroupID != desired.RemoteGroupID || current.Action != desired.Action ||
		current.Description != desired.Description {
		return conflict("a deterministic default security group rule contains non-baseline policy")
	}
	return nil
}

func conflict(format string, args ...any) error {
	return &controlstore.Error{Kind: controlstore.ErrConflict, Message: fmt.Sprintf(format, args...)}
}
