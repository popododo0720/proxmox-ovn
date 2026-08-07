package ovsdbstore

import (
	"context"
	"fmt"
	"sort"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/popododo0720/proxmox-ovn/internal/controlschema"
	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

var _ controlstore.RuntimePortLookup = (*Store)(nil)

// LookupRuntimePorts uses one OVSDB transaction so node aliases, reference
// mappings, and matching VM NIC rows all come from the same database view.
func (s *Store) LookupRuntimePorts(ctx context.Context, nodeIdentity string, vmid int, nic string) ([]*model.Port, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	raw, err := s.database.lookupRuntimePorts(ctx, vmid, nic)
	if err != nil {
		return nil, err
	}
	return decodeRuntimePortLookup(raw, nodeIdentity)
}

func decodeRuntimePortLookup(raw rawRuntimePortLookup, nodeIdentity string) ([]*model.Port, error) {
	nodeIDs := make(map[string]string, len(raw.nodes))
	acceptedNodes := map[string]bool{nodeIdentity: true}
	for index, row := range raw.nodes {
		uuid, uuidErr := rowUUID(row, "_uuid")
		id, idErr := rowString(row, "id")
		name, nameErr := rowString(row, "name")
		chassis, chassisErr := rowString(row, "chassis_id")
		if err := firstError(uuidErr, idErr, nameErr, chassisErr); err != nil {
			return nil, rowError(controlschema.NodeTable, index, err)
		}
		if id == "" {
			return nil, rowError(controlschema.NodeTable, index, fmt.Errorf("node id is empty"))
		}
		if _, duplicate := nodeIDs[uuid]; duplicate {
			return nil, rowError(controlschema.NodeTable, index, fmt.Errorf("duplicate node UUID %q", uuid))
		}
		nodeIDs[uuid] = id
		if id == nodeIdentity || name == nodeIdentity || chassis == nodeIdentity {
			acceptedNodes[id], acceptedNodes[name], acceptedNodes[chassis] = true, true, true
		}
	}

	ports := make([]*model.Port, 0, len(raw.ports))
	for index, row := range raw.ports {
		port, err := decodeRuntimePort(row, nodeIDs)
		if err != nil {
			return nil, rowError(controlschema.PortTable, index, err)
		}
		if acceptedNodes[port.NodeID] || acceptedNodes[port.RequestedChassis] {
			ports = append(ports, port)
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].ID < ports[j].ID })
	return ports, nil
}

func decodeRuntimePort(row ovsdb.Row, nodeIDs map[string]string) (*model.Port, error) {
	id, e1 := rowString(row, "id")
	revision, e2 := rowInt64(row, "revision")
	appliedRevision, e3 := rowInt64(row, "applied_revision")
	stateValue, e4 := rowString(row, "state")
	nodeUUID, e5 := rowReference(row, "node", true)
	vmid, e6 := rowInt64(row, "vmid")
	nic, e7 := rowString(row, "nic")
	lspName, e8 := rowString(row, "lsp_name")
	generation, e9 := rowInt64(row, "generation")
	requestedChassis, e10 := rowString(row, "requested_chassis")
	macAddress, e11 := rowString(row, "mac_address")
	adminStateUp, e12 := rowBool(row, "admin_state_up")
	bindingStatus, e13 := rowString(row, "binding_status")
	if err := firstError(e1, e2, e3, e4, e5, e6, e7, e8, e9, e10, e11, e12, e13); err != nil {
		return nil, err
	}
	nodeID := ""
	if nodeUUID != "" {
		var nodeExists bool
		nodeID, nodeExists = nodeIDs[nodeUUID]
		if !nodeExists {
			return nil, fmt.Errorf("column node references unknown node UUID %q", nodeUUID)
		}
	}
	state := model.ResourceState(stateValue)
	if id == "" || revision < 1 || appliedRevision < 0 || appliedRevision > revision {
		return nil, fmt.Errorf("invalid metadata for runtime port %q", id)
	}
	if state != model.ResourcePending && state != model.ResourceReady && state != model.ResourceError && state != model.ResourceDeleting {
		return nil, fmt.Errorf("runtime port %q has invalid state %q", id, state)
	}
	return &model.Port{
		Metadata: model.Metadata{
			ID: id, Revision: revision, AppliedRevision: appliedRevision, State: state,
		},
		MACAddress:       macAddress,
		AdminStateUp:     adminStateUp,
		BindingStatus:    model.PortBindingStatus(bindingStatus),
		NodeID:           nodeID,
		VMID:             int(vmid),
		NIC:              nic,
		LSPName:          lspName,
		Generation:       generation,
		RequestedChassis: requestedChassis,
	}, nil
}
