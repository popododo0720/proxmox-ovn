// Package defaultsecurity owns the system-managed security policy that every
// PVN project receives. The policy uses deterministic UUIDs so every manager
// can safely ensure the same resources without an additional coordination row.
package defaultsecurity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	DefaultSecurityGroupName        = "default"
	DefaultSecurityGroupDescription = "PVN managed default security group"
	DefaultEgressDescription        = "Allow all IPv4 egress"
	DefaultIngressDescription       = "Allow IPv4 ingress from this security group"
)

// Reconciler realizes one desired resource in OVN.
type Reconciler interface {
	Reconcile(context.Context, model.Kind, string) error
}

// Manager creates and repairs the deterministic default policy for projects.
type Manager struct {
	store      controlstore.Store
	reconciler Reconciler
}

// BlockedReason identifies policy states that Ensure must not overwrite.
type BlockedReason string

const (
	BlockedNone            BlockedReason = ""
	BlockedNameCollision   BlockedReason = "default_name_collision"
	BlockedMalformedGroup  BlockedReason = "deterministic_group_malformed"
	BlockedMalformedRule   BlockedReason = "deterministic_rule_malformed"
	BlockedProjectDeleting BlockedReason = "project_deleting"
)

// Inspection is a read-only assessment of one project's baseline. Missing
// resources are repairable; BlockedReason requires operator intervention.
type Inspection struct {
	ProjectID          string        `json:"project_id"`
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
func DefaultSecurityGroupID(projectID string) string {
	return deterministicUUID("pvn/default-security-group/v1", projectID)
}

// DefaultEgressRuleID identifies the system-managed IPv4 egress rule.
func DefaultEgressRuleID(projectID string) string {
	return deterministicUUID("pvn/default-security-group-rule/egress-ipv4/v1", projectID)
}

// DefaultIngressRuleID identifies the system-managed IPv4 self-ingress rule.
func DefaultIngressRuleID(projectID string) string {
	return deterministicUUID("pvn/default-security-group-rule/ingress-self-ipv4/v1", projectID)
}

func deterministicUUID(domain, projectID string) string {
	digest := sha256.Sum256([]byte(domain + ":" + strings.TrimSpace(projectID)))
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(digest[:16])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// IsReserved reports whether resource is one of the deterministic policy
// objects. API mutation paths use this to prevent the baseline from being
// renamed, weakened, or deleted.
func IsReserved(resource model.Resource) bool {
	switch value := resource.(type) {
	case *model.SecurityGroup:
		return value.ID != "" && value.ID == DefaultSecurityGroupID(value.ProjectID)
	case *model.SecurityGroupRule:
		return value.ID != "" && (value.ID == DefaultEgressRuleID(value.ProjectID) || value.ID == DefaultIngressRuleID(value.ProjectID))
	default:
		return false
	}
}

// Inspect assesses the deterministic baseline without creating, updating, or
// reconciling any resource.
func (m *Manager) Inspect(ctx context.Context, projectID string) (Inspection, error) {
	inspection := Inspection{ProjectID: strings.TrimSpace(projectID), Ready: true}
	inspection.GroupID = DefaultSecurityGroupID(inspection.ProjectID)
	if m == nil || m.store == nil {
		return inspection, errors.New("default security policy store is not configured")
	}
	if inspection.ProjectID == "" {
		return inspection, &model.ValidationError{Field: "project_id", Message: "is required"}
	}
	project, err := m.store.Get(ctx, model.KindProject, inspection.ProjectID)
	if err != nil {
		return inspection, err
	}
	if project.GetMetadata().State == model.ResourceDeleting {
		inspection.Ready = false
		inspection.BlockedReason = BlockedProjectDeleting
		inspection.Detail = "project is being deleted"
		return inspection, nil
	}

	desiredGroup := desiredGroup(inspection.ProjectID)
	groups, err := m.store.List(ctx, model.KindSecurityGroup, controlstore.ListOptions{ProjectID: inspection.ProjectID})
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

	resources := []model.Resource{desiredGroup, desiredEgressRule(inspection.ProjectID), desiredIngressRule(inspection.ProjectID)}
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
		meta := current.GetMetadata()
		if meta.State != model.ResourceReady || meta.AppliedRevision != meta.Revision {
			inspection.Ready = false
		}
	}
	return inspection, nil
}

// Ensure creates any missing deterministic policy rows, verifies that existing
// rows contain exactly the baseline policy, and realizes each row before it is
// returned for attachment to a new port. Creation order is deliberately
// restrictive: the default-drop security group is realized before allow rules.
func (m *Manager) Ensure(ctx context.Context, projectID string) (*model.SecurityGroup, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("default security policy store is not configured")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, &model.ValidationError{Field: "project_id", Message: "is required"}
	}
	projectResource, err := m.store.Get(ctx, model.KindProject, projectID)
	if err != nil {
		return nil, err
	}
	if projectResource.GetMetadata().State == model.ResourceDeleting {
		return nil, conflict("project is being deleted; its default security policy cannot be ensured")
	}

	group := desiredGroup(projectID)
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

	for _, desired := range []*model.SecurityGroupRule{desiredEgressRule(projectID), desiredIngressRule(projectID)} {
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

// EnsureAll repairs default policies for every non-deleting project. It never
// reads or changes Port rows, so legacy ports with an explicit empty SG list
// remain untouched.
func (m *Manager) EnsureAll(ctx context.Context) error {
	if m == nil || m.store == nil {
		return errors.New("default security policy store is not configured")
	}
	projects, err := m.store.List(ctx, model.KindProject, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	var failures []error
	for _, resource := range projects {
		if resource.GetMetadata().State == model.ResourceDeleting {
			continue
		}
		if _, ensureErr := m.Ensure(ctx, resource.GetMetadata().ID); ensureErr != nil {
			failures = append(failures, fmt.Errorf("ensure project %q default security policy: %w", resource.GetMetadata().ID, ensureErr))
		}
	}
	return errors.Join(failures...)
}

func desiredGroup(projectID string) *model.SecurityGroup {
	return &model.SecurityGroup{
		Metadata:    model.Metadata{ID: DefaultSecurityGroupID(projectID)},
		ProjectID:   projectID,
		Name:        DefaultSecurityGroupName,
		Description: DefaultSecurityGroupDescription,
		Stateful:    true,
	}
}

func desiredEgressRule(projectID string) *model.SecurityGroupRule {
	return &model.SecurityGroupRule{
		Metadata:        model.Metadata{ID: DefaultEgressRuleID(projectID)},
		ProjectID:       projectID,
		SecurityGroupID: DefaultSecurityGroupID(projectID),
		Direction:       model.DirectionEgress,
		EtherType:       model.EtherTypeIPv4,
		Action:          model.ActionAllow,
		Description:     DefaultEgressDescription,
	}
}

func desiredIngressRule(projectID string) *model.SecurityGroupRule {
	groupID := DefaultSecurityGroupID(projectID)
	return &model.SecurityGroupRule{
		Metadata:        model.Metadata{ID: DefaultIngressRuleID(projectID)},
		ProjectID:       projectID,
		SecurityGroupID: groupID,
		Direction:       model.DirectionIngress,
		EtherType:       model.EtherTypeIPv4,
		RemoteGroupID:   groupID,
		Action:          model.ActionAllow,
		Description:     DefaultIngressDescription,
	}
}

func (m *Manager) rejectNameCollision(ctx context.Context, desired *model.SecurityGroup) error {
	groups, err := m.store.List(ctx, model.KindSecurityGroup, controlstore.ListOptions{ProjectID: desired.ProjectID})
	if err != nil {
		return err
	}
	for _, resource := range groups {
		group := resource.(*model.SecurityGroup)
		if group.Name == DefaultSecurityGroupName && group.ID != desired.ID {
			return conflict("project already has a non-PVN security group named %q; rename or remove it before creating ports", DefaultSecurityGroupName)
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
	// Another manager may have committed the same deterministic row between
	// our read and write. Only adopt that exact ID; unique-name collisions are
	// diagnosed by the caller and never treated as the default policy.
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
	if m.reconciler == nil {
		return resource, nil
	}
	meta := resource.GetMetadata()
	if err := m.reconciler.Reconcile(ctx, resource.ResourceKind(), meta.ID); err != nil {
		return nil, fmt.Errorf("realize default security policy %s %q: %w", resource.ResourceKind(), meta.ID, err)
	}
	latest, err := m.store.Get(ctx, resource.ResourceKind(), meta.ID)
	if err != nil {
		return nil, err
	}
	latestMeta := latest.GetMetadata()
	if latestMeta.State != model.ResourceReady || latestMeta.AppliedRevision != latestMeta.Revision {
		return nil, fmt.Errorf("default security policy %s %q was not fully realized", resource.ResourceKind(), latestMeta.ID)
	}
	return latest, nil
}

func sameGroup(current, desired *model.SecurityGroup) error {
	if current.State == model.ResourceDeleting {
		return conflict("the default security group is being deleted")
	}
	if current.ProjectID != desired.ProjectID || current.Name != desired.Name || current.Description != desired.Description || !current.Stateful {
		return conflict("the deterministic default security group contains non-baseline policy")
	}
	return nil
}

func sameRule(current, desired *model.SecurityGroupRule) error {
	if current.State == model.ResourceDeleting {
		return conflict("a default security group rule is being deleted")
	}
	if current.ProjectID != desired.ProjectID || current.SecurityGroupID != desired.SecurityGroupID ||
		current.Direction != desired.Direction || current.EtherType != desired.EtherType ||
		current.Protocol != "" || current.PortRangeMin != 0 || current.PortRangeMax != 0 ||
		current.RemoteCIDR != "" || current.RemoteGroupID != desired.RemoteGroupID ||
		current.Action != desired.Action || current.Description != desired.Description {
		return conflict("a deterministic default security group rule contains non-baseline policy")
	}
	return nil
}

func conflict(format string, args ...any) error {
	return &controlstore.Error{Kind: controlstore.ErrConflict, Message: fmt.Sprintf(format, args...)}
}
