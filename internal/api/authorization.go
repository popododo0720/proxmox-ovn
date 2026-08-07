package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	globalPath        = "/"
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

	// Authorize both the stored and proposed representations so a caller cannot
	// bypass policy by changing fields used by resource-specific checks.
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
		key := fmt.Sprintf("%s/%d/%s", port.NetworkID, port.VMID, port.NIC)
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
	return fmt.Errorf("global %s is required", privilege)
}

func (s *Server) requirePortUse(ctx context.Context, session Session, port *model.Port) error {
	if !hasPrivilege(session, globalPath, "SDN.Use") &&
		!hasPrivilege(session, networkPathPrefix+port.NetworkID, "SDN.Use") {
		return errors.New("SDN.Use is required globally or on the network")
	}
	if port.VMID < 1 || (!hasPrivilege(session, globalPath, "VM.Config.Network") &&
		!hasPrivilege(session, fmt.Sprintf("/vms/%d", port.VMID), "VM.Config.Network")) {
		return errors.New("VM.Config.Network is required on the target VM")
	}
	return nil
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
