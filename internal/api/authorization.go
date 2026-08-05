package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/pvnstack/proxmox-ovn/internal/model"
)

const (
	globalPath        = "/"
	projectPoolPrefix = "/pool/"
	networkPathPrefix = "/sdn/zones/pvn/"
)

func (s *Server) authorizeRead(ctx context.Context, resource model.Resource) error {
	session, secured := ctx.Value(sessionContextKey{}).(Session)
	if !secured {
		return nil
	}
	return s.requireResourcePrivilege(ctx, session, resource, "SDN.Audit")
}

func (s *Server) authorizeWrite(ctx context.Context, resource, previous model.Resource) error {
	session, secured := ctx.Value(sessionContextKey{}).(Session)
	if !secured {
		return nil
	}

	// Authorize both sides of a project/pool move. Checking only the proposed
	// resource would let a user with access to pool B take an object from pool A.
	if previous != nil {
		if err := s.requireResourcePrivilege(ctx, session, previous, "SDN.Allocate"); err != nil {
			return err
		}
	}
	if err := s.requireResourcePrivilege(ctx, session, resource, "SDN.Allocate"); err != nil {
		return err
	}

	if systemResource(resource.ResourceKind()) {
		if !hasPrivilege(session, globalPath, "Sys.Modify") {
			return errors.New("global SDN.Allocate and Sys.Modify are required")
		}
	}

	checkedPorts := make(map[string]struct{}, 2)
	for _, candidate := range []model.Resource{previous, resource} {
		port, ok := candidate.(*model.Port)
		if !ok || !portAttached(port) {
			continue
		}
		key := fmt.Sprintf("%s/%s/%d/%s", port.ProjectID, port.NetworkID, port.VMID, port.NIC)
		if _, done := checkedPorts[key]; done {
			continue
		}
		checkedPorts[key] = struct{}{}
		if err := s.requirePortUse(ctx, session, port); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) requireResourcePrivilege(ctx context.Context, session Session, resource model.Resource, privilege string) error {
	if resource == nil {
		return errors.New("resource is unavailable for permission evaluation")
	}
	if hasPrivilege(session, globalPath, privilege) {
		return nil
	}
	if systemResource(resource.ResourceKind()) {
		return fmt.Errorf("global %s is required", privilege)
	}
	poolPath, err := s.resourcePoolPath(ctx, resource)
	if err != nil {
		return err
	}
	if poolPath != "" && hasPrivilege(session, poolPath, privilege) {
		return nil
	}
	return fmt.Errorf("%s is required on the resource project", privilege)
}

func (s *Server) resourcePoolPath(ctx context.Context, resource model.Resource) (string, error) {
	if project, ok := resource.(*model.Project); ok {
		if project.PoolID == "" {
			return "", errors.New("the resource project is unavailable for permission evaluation")
		}
		return projectPoolPrefix + project.PoolID, nil
	}

	if operation, ok := resource.(*model.Operation); ok {
		target, err := s.store.Get(ctx, operation.TargetKind, operation.TargetID)
		if err != nil {
			return "", errors.New("the operation target is unavailable for permission evaluation")
		}
		if systemResource(target.ResourceKind()) {
			return "", nil
		}
		return s.resourcePoolPath(ctx, target)
	}

	projectID := resourceProjectID(resource)
	if projectID == "" {
		return "", errors.New("the resource project is unavailable for permission evaluation")
	}
	projectResource, err := s.store.Get(ctx, model.KindProject, projectID)
	if err != nil {
		return "", errors.New("the resource project is unavailable for permission evaluation")
	}
	project, ok := projectResource.(*model.Project)
	if !ok || project.PoolID == "" {
		return "", errors.New("the resource project is unavailable for permission evaluation")
	}
	return projectPoolPrefix + project.PoolID, nil
}

func (s *Server) requirePortUse(ctx context.Context, session Session, port *model.Port) error {
	poolPath, err := s.resourcePoolPath(ctx, port)
	if err != nil {
		return err
	}
	if !hasPrivilege(session, globalPath, "SDN.Use") &&
		!hasPrivilege(session, poolPath, "SDN.Use") &&
		!hasPrivilege(session, networkPathPrefix+port.NetworkID, "SDN.Use") {
		return errors.New("SDN.Use is required on the network project")
	}
	if port.VMID < 1 || (!hasPrivilege(session, globalPath, "VM.Config.Network") &&
		!hasPrivilege(session, fmt.Sprintf("/vms/%d", port.VMID), "VM.Config.Network")) {
		return errors.New("VM.Config.Network is required on the target VM")
	}
	return nil
}

func resourceProjectID(resource model.Resource) string {
	switch value := resource.(type) {
	case *model.Network:
		return value.ProjectID
	case *model.Subnet:
		return value.ProjectID
	case *model.Port:
		return value.ProjectID
	case *model.IPAllocation:
		return value.ProjectID
	case *model.Router:
		return value.ProjectID
	case *model.RouterInterface:
		return value.ProjectID
	case *model.FloatingIP:
		return value.ProjectID
	case *model.SecurityGroup:
		return value.ProjectID
	case *model.SecurityGroupRule:
		return value.ProjectID
	default:
		return ""
	}
}

func systemResource(kind model.Kind) bool {
	return kind == model.KindProviderNetwork || kind == model.KindProviderSegment || kind == model.KindNode
}

func portAttached(port *model.Port) bool {
	return port.NodeID != "" || port.VMID != 0 || port.NIC != "" || port.RequestedChassis != "" ||
		(port.BindingStatus != "" && port.BindingStatus != model.PortUnbound)
}

func portBindingFieldsSet(port *model.Port) bool {
	return port.NodeID != "" || port.VMID != 0 || port.NIC != "" || port.RequestedChassis != "" ||
		port.BindingStatus != "" || port.LSPName != "" || port.Generation != 0
}

func samePortBindingFields(proposed, current *model.Port) bool {
	return proposed.NodeID == current.NodeID &&
		proposed.VMID == current.VMID &&
		proposed.NIC == current.NIC &&
		proposed.RequestedChassis == current.RequestedChassis &&
		proposed.BindingStatus == current.BindingStatus &&
		proposed.LSPName == current.LSPName &&
		proposed.Generation == current.Generation
}

func metadataEmpty(metadata *model.Metadata) bool {
	return metadata.ID == "" && metadata.Revision == 0 && metadata.AppliedRevision == 0 &&
		metadata.State == "" && metadata.LastError == "" && metadata.CreatedAt.IsZero() && metadata.UpdatedAt.IsZero()
}

func metadataUpdateAllowed(proposed, current *model.Metadata) bool {
	return (proposed.AppliedRevision == 0 || proposed.AppliedRevision == current.AppliedRevision) &&
		(proposed.State == "" || proposed.State == current.State) &&
		(proposed.LastError == "" || proposed.LastError == current.LastError) &&
		(proposed.CreatedAt.IsZero() || proposed.CreatedAt.Equal(current.CreatedAt)) &&
		(proposed.UpdatedAt.IsZero() || proposed.UpdatedAt.Equal(current.UpdatedAt))
}

func hasPrivilege(session Session, path, privilege string) bool {
	raw, ok := session.Permissions[path]
	if !ok {
		return false
	}
	switch privileges := raw.(type) {
	case map[string]bool:
		return privileges[privilege]
	case map[string]int:
		return privileges[privilege] != 0
	case map[string]any:
		switch value := privileges[privilege].(type) {
		case bool:
			return value
		case int:
			return value != 0
		case int64:
			return value != 0
		case float64:
			return value != 0
		}
	}
	return false
}
