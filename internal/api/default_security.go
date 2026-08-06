package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

var projectScopedKinds = []model.Kind{
	model.KindNetwork,
	model.KindSubnet,
	model.KindPort,
	model.KindIPAllocation,
	model.KindRouter,
	model.KindRouterInterface,
	model.KindFloatingIP,
	model.KindSecurityGroupRule,
	model.KindSecurityGroup,
}

func (s *Server) ensureDefaultSecurityGroup(writer http.ResponseWriter, ctx context.Context, projectID string) (*model.SecurityGroup, bool) {
	group, err := s.defaultSecurity.Ensure(ctx, projectID)
	if err != nil {
		s.writeDefaultSecurityError(writer, err)
		return nil, false
	}
	return group, true
}

func (s *Server) writeDefaultSecurityError(writer http.ResponseWriter, err error) {
	if errors.Is(err, controlstore.ErrNotFound) || errors.Is(err, controlstore.ErrAlreadyExists) ||
		errors.Is(err, controlstore.ErrConflict) || errors.Is(err, controlstore.ErrPrecondition) ||
		errors.Is(err, controlstore.ErrIdempotencyConflict) {
		s.storeError(writer, err)
		return
	}
	s.logger.Error("default security policy is unavailable", "error", err)
	writeError(writer, http.StatusServiceUnavailable, "default_security_policy_unavailable", "the project default security policy is not ready", nil)
}

func (s *Server) preparePortSecurityGroups(writer http.ResponseWriter, ctx context.Context, port *model.Port, current *model.Port) bool {
	if len(port.SecurityGroupIDs) != 0 {
		return true
	}
	if current != nil && len(current.SecurityGroupIDs) == 0 {
		writeError(writer, http.StatusConflict, "legacy_port_security_unset", "this existing port has no security policy; run the default security policy backfill before updating it", nil)
		return false
	}
	group, ok := s.ensureDefaultSecurityGroup(writer, ctx, port.ProjectID)
	if !ok {
		return false
	}
	port.SecurityGroupIDs = []string{group.ID}
	return true
}

// EnsureDefaultSecurityPolicies performs a bounded repair pass when invoked by
// the manager. It does not modify any existing Port rows.
func (s *Server) EnsureDefaultSecurityPolicies(ctx context.Context) error {
	return s.defaultSecurity.EnsureAll(ctx)
}

// cleanupProjectDefaultSecurity removes only the deterministic baseline after
// proving that no tenant-owned project children remain. A concurrent child
// write causes a reference/precondition failure; a later EnsureAll pass repairs
// any restrictive partial cleanup.
func (s *Server) cleanupProjectDefaultSecurity(ctx context.Context, projectID string) error {
	snapshot, err := s.store.Snapshot(ctx, projectScopedKinds, controlstore.ListOptions{ProjectID: projectID})
	if err != nil {
		return err
	}
	reserved := map[model.Kind]map[string]struct{}{
		model.KindSecurityGroup: {
			defaultsecurity.DefaultSecurityGroupID(projectID): {},
		},
		model.KindSecurityGroupRule: {
			defaultsecurity.DefaultEgressRuleID(projectID):  {},
			defaultsecurity.DefaultIngressRuleID(projectID): {},
		},
	}
	for _, kind := range projectScopedKinds {
		for _, resource := range snapshot[kind] {
			if _, allowed := reserved[kind][resource.GetMetadata().ID]; allowed {
				continue
			}
			return &controlstore.Error{Kind: controlstore.ErrConflict, Message: "project still has tenant resources; remove them before deleting the project"}
		}
	}
	for _, target := range []struct {
		kind model.Kind
		id   string
	}{
		{kind: model.KindSecurityGroupRule, id: defaultsecurity.DefaultEgressRuleID(projectID)},
		{kind: model.KindSecurityGroupRule, id: defaultsecurity.DefaultIngressRuleID(projectID)},
		{kind: model.KindSecurityGroup, id: defaultsecurity.DefaultSecurityGroupID(projectID)},
	} {
		if err := s.deleteDefaultSecurityResource(ctx, projectID, target.kind, target.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) deleteDefaultSecurityResource(ctx context.Context, projectID string, kind model.Kind, id string) error {
	current, err := s.store.Get(ctx, kind, id)
	if errors.Is(err, controlstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if resourceProjectID(current) != projectID || !defaultsecurity.IsReserved(current) {
		return &controlstore.Error{Kind: controlstore.ErrConflict, Message: "deterministic default security policy contains non-baseline ownership"}
	}
	tombstone := current
	if state := current.GetMetadata().State; state != model.ResourceDeleting {
		created, _, beginErr := s.store.BeginDelete(ctx, kind, id, current.GetMetadata().Revision, "")
		if beginErr != nil {
			return beginErr
		}
		tombstone = created
	}
	if deletionReconciler, ok := s.reconciler.(DeletionReconciler); ok {
		if err := deletionReconciler.Delete(ctx, tombstone); err != nil {
			return fmt.Errorf("remove default security policy from OVN: %w", err)
		}
	}
	return s.store.Purge(ctx, kind, id, tombstone.GetMetadata().Revision)
}
