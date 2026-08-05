package ovsdbstore

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/pvnstack/proxmox-ovn/internal/controlstore"
	"github.com/pvnstack/proxmox-ovn/internal/model"
)

func require(current *snapshot, kind model.Kind, id, field string) (model.Resource, error) {
	entry, exists := current.resources[kind][id]
	if !exists {
		return nil, storeError(controlstore.ErrConflict, "%s references missing %s %q", field, kind, id)
	}
	return entry.resource, nil
}

func validateReferences(current *snapshot, resource model.Resource) error {
	project := func(id, field string) error {
		_, err := require(current, model.KindProject, id, field)
		return err
	}
	switch value := resource.(type) {
	case *model.Project, *model.Node, *model.Operation:
		return nil
	case *model.ProviderNetwork:
		if value.DefaultSegmentID != "" {
			_, err := require(current, model.KindProviderSegment, value.DefaultSegmentID, "default_segment_id")
			return err
		}
	case *model.Network:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if value.ProviderNetworkID != "" {
			_, err := require(current, model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id")
			return err
		}
	case *model.Subnet:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		network, err := require(current, model.KindNetwork, value.NetworkID, "network_id")
		if err != nil {
			return err
		}
		if network.(*model.Network).ProjectID != value.ProjectID {
			return storeError(controlstore.ErrConflict, "network belongs to a different project")
		}
	case *model.Port:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		network, err := require(current, model.KindNetwork, value.NetworkID, "network_id")
		if err != nil {
			return err
		}
		if network.(*model.Network).ProjectID != value.ProjectID {
			return storeError(controlstore.ErrConflict, "network belongs to a different project")
		}
		for _, fixed := range value.FixedIPs {
			subnet, err := require(current, model.KindSubnet, fixed.SubnetID, "fixed_ips.subnet_id")
			if err != nil {
				return err
			}
			if subnet.(*model.Subnet).NetworkID != value.NetworkID {
				return storeError(controlstore.ErrConflict, "fixed IP subnet belongs to a different network")
			}
		}
		for _, groupID := range value.SecurityGroupIDs {
			if _, err := require(current, model.KindSecurityGroup, groupID, "security_group_ids"); err != nil {
				return err
			}
		}
		if value.NodeID != "" {
			if _, err := require(current, model.KindNode, value.NodeID, "node_id"); err != nil {
				return err
			}
		}
	case *model.IPAllocation:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if _, err := require(current, model.KindSubnet, value.SubnetID, "subnet_id"); err != nil {
			return err
		}
		if value.PortID != "" {
			if _, err := require(current, model.KindPort, value.PortID, "port_id"); err != nil {
				return err
			}
		}
	case *model.Router:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if value.ExternalNetworkID != "" {
			externalResource, err := require(current, model.KindNetwork, value.ExternalNetworkID, "external_network_id")
			if err != nil {
				return err
			}
			external := externalResource.(*model.Network)
			if !external.External || external.ProviderNetworkID == "" {
				return storeError(controlstore.ErrConflict, "external network must be external and provider-backed")
			}
			subnetResource, err := require(current, model.KindSubnet, value.ExternalSubnetID, "external_subnet_id")
			if err != nil {
				return err
			}
			subnet := subnetResource.(*model.Subnet)
			if subnet.NetworkID != value.ExternalNetworkID {
				return storeError(controlstore.ErrConflict, "external subnet belongs to a different network")
			}
			prefix, prefixErr := netip.ParsePrefix(subnet.CIDR)
			address, addressErr := netip.ParseAddr(value.ExternalIPAddress)
			if prefixErr != nil || addressErr != nil || !address.Is4() || !prefix.Contains(address) {
				return storeError(controlstore.ErrConflict, "external IP address must belong to the external subnet")
			}
			if subnet.GatewayIP != "" && value.ExternalIPAddress == subnet.GatewayIP {
				return storeError(controlstore.ErrConflict, "external IP address must differ from the subnet gateway")
			}
		}
	case *model.RouterInterface:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if _, err := require(current, model.KindRouter, value.RouterID, "router_id"); err != nil {
			return err
		}
		if _, err := require(current, model.KindSubnet, value.SubnetID, "subnet_id"); err != nil {
			return err
		}
		if value.PortID != "" {
			if _, err := require(current, model.KindPort, value.PortID, "port_id"); err != nil {
				return err
			}
		}
	case *model.FloatingIP:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if _, err := require(current, model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id"); err != nil {
			return err
		}
		if value.PortID != "" {
			if _, err := require(current, model.KindPort, value.PortID, "port_id"); err != nil {
				return err
			}
		}
		if value.RouterID != "" {
			if _, err := require(current, model.KindRouter, value.RouterID, "router_id"); err != nil {
				return err
			}
		}
	case *model.ProviderSegment:
		if _, err := require(current, model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id"); err != nil {
			return err
		}
	case *model.SecurityGroup:
		return project(value.ProjectID, "project_id")
	case *model.SecurityGroupRule:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if _, err := require(current, model.KindSecurityGroup, value.SecurityGroupID, "security_group_id"); err != nil {
			return err
		}
		if value.RemoteGroupID != "" {
			if _, err := require(current, model.KindSecurityGroup, value.RemoteGroupID, "remote_group_id"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateUnique(current *snapshot, candidate model.Resource, ignoredID string) error {
	for id, entry := range current.resources[candidate.ResourceKind()] {
		if id == ignoredID {
			continue
		}
		if field := conflictField(candidate, entry.resource); field != "" {
			return storeError(controlstore.ErrAlreadyExists, "%s conflicts with existing %s %q on %s", candidate.ResourceKind(), entry.resource.ResourceKind(), id, field)
		}
	}
	return nil
}

func validateReplacement(current *snapshot, candidate model.Resource) error {
	id := candidate.GetMetadata().ID
	entries := current.resources[candidate.ResourceKind()]
	previous, exists := entries[id]
	if !exists {
		return storeError(controlstore.ErrNotFound, "%s %q was not found", candidate.ResourceKind(), id)
	}
	entries[id] = storedResource{uuid: previous.uuid, resource: candidate}
	defer func() { entries[id] = previous }()
	for _, kind := range model.Kinds() {
		for dependentID, entry := range current.resources[kind] {
			if err := validateReferences(current, entry.resource); err != nil {
				return storeError(controlstore.ErrConflict, "updating %s %q would invalidate %s %q: %v", candidate.ResourceKind(), id, kind, dependentID, err)
			}
		}
	}
	return nil
}

func validateSnapshotUnique(current *snapshot) error {
	for _, kind := range model.Kinds() {
		seen := make(map[string]string)
		for id, entry := range current.resources[kind] {
			for _, key := range uniqueKeys(entry.resource) {
				if existingID, duplicate := seen[key]; duplicate {
					return fmt.Errorf("stored %s %q conflicts with %q on %s", kind, id, existingID, key)
				}
				seen[key] = id
			}
		}
	}
	return nil
}

func uniqueKeys(resource model.Resource) []string {
	switch value := resource.(type) {
	case *model.Project:
		return []string{"pool_id=" + value.PoolID, "name=" + value.Name}
	case *model.Network:
		return []string{"project_id,name=" + value.ProjectID + "\x00" + value.Name}
	case *model.Subnet:
		return []string{
			"network_id,cidr=" + value.NetworkID + "\x00" + value.CIDR,
			"network_id,name=" + value.NetworkID + "\x00" + value.Name,
		}
	case *model.Port:
		result := []string{"mac_address=" + strings.ToLower(value.MACAddress), "lsp_name=" + value.LSPName}
		if value.NodeID != "" && value.VMID > 0 && value.NIC != "" {
			result = append(result, fmt.Sprintf("node_id,vmid,nic=%s\x00%d\x00%s", value.NodeID, value.VMID, value.NIC))
		}
		return result
	case *model.IPAllocation:
		return []string{"subnet_id,address=" + value.SubnetID + "\x00" + value.Address}
	case *model.Router:
		return []string{"project_id,name=" + value.ProjectID + "\x00" + value.Name}
	case *model.RouterInterface:
		return []string{"router_id,subnet_id=" + value.RouterID + "\x00" + value.SubnetID}
	case *model.FloatingIP:
		return []string{"provider_network_id,address=" + value.ProviderNetworkID + "\x00" + value.Address}
	case *model.ProviderNetwork:
		return []string{"name=" + value.Name}
	case *model.ProviderSegment:
		return []string{
			"provider_network_id,name=" + value.ProviderNetworkID + "\x00" + value.Name,
			fmt.Sprintf("physical_network,network_type,vlan_id=%s\x00%s\x00%d", value.PhysicalNetwork, value.NetworkType, value.VLANID),
		}
	case *model.SecurityGroup:
		return []string{"project_id,name=" + value.ProjectID + "\x00" + value.Name}
	case *model.Operation:
		return []string{
			"idempotency_key=" + value.IdempotencyKey,
			fmt.Sprintf("target_kind,target_id,target_revision=%s\x00%s\x00%d", value.TargetKind, value.TargetID, value.TargetRevision),
		}
	case *model.Node:
		return []string{"name=" + value.Name, "chassis_id=" + value.ChassisID}
	default:
		return nil
	}
}

func conflictField(candidate, existing model.Resource) string {
	switch left := candidate.(type) {
	case *model.Project:
		right := existing.(*model.Project)
		if left.PoolID == right.PoolID {
			return "pool_id"
		}
		if left.Name == right.Name {
			return "name"
		}
	case *model.Network:
		right := existing.(*model.Network)
		if left.ProjectID == right.ProjectID && left.Name == right.Name {
			return "project_id,name"
		}
	case *model.Subnet:
		right := existing.(*model.Subnet)
		if left.NetworkID == right.NetworkID && left.CIDR == right.CIDR {
			return "network_id,cidr"
		}
		if left.NetworkID == right.NetworkID && left.Name == right.Name {
			return "network_id,name"
		}
	case *model.Port:
		right := existing.(*model.Port)
		if strings.EqualFold(left.MACAddress, right.MACAddress) {
			return "mac_address"
		}
		if left.LSPName != "" && left.LSPName == right.LSPName {
			return "lsp_name"
		}
		if left.NodeID != "" && left.VMID > 0 && left.NIC != "" && left.NodeID == right.NodeID && left.VMID == right.VMID && left.NIC == right.NIC {
			return "node_id,vmid,nic"
		}
	case *model.IPAllocation:
		right := existing.(*model.IPAllocation)
		if left.SubnetID == right.SubnetID && left.Address == right.Address {
			return "subnet_id,address"
		}
	case *model.Router:
		right := existing.(*model.Router)
		if left.ProjectID == right.ProjectID && left.Name == right.Name {
			return "project_id,name"
		}
	case *model.RouterInterface:
		right := existing.(*model.RouterInterface)
		if left.RouterID == right.RouterID && left.SubnetID == right.SubnetID {
			return "router_id,subnet_id"
		}
	case *model.FloatingIP:
		right := existing.(*model.FloatingIP)
		if left.ProviderNetworkID == right.ProviderNetworkID && left.Address == right.Address {
			return "provider_network_id,address"
		}
	case *model.ProviderNetwork:
		right := existing.(*model.ProviderNetwork)
		if left.Name == right.Name {
			return "name"
		}
	case *model.ProviderSegment:
		right := existing.(*model.ProviderSegment)
		if left.ProviderNetworkID == right.ProviderNetworkID && left.Name == right.Name {
			return "provider_network_id,name"
		}
		if left.PhysicalNetwork == right.PhysicalNetwork && left.NetworkType == right.NetworkType && left.VLANID == right.VLANID {
			return "physical_network,network_type,vlan_id"
		}
	case *model.SecurityGroup:
		right := existing.(*model.SecurityGroup)
		if left.ProjectID == right.ProjectID && left.Name == right.Name {
			return "project_id,name"
		}
	case *model.Operation:
		right := existing.(*model.Operation)
		if left.IdempotencyKey == right.IdempotencyKey {
			return "idempotency_key"
		}
		if left.TargetKind == right.TargetKind && left.TargetID == right.TargetID && left.TargetRevision == right.TargetRevision {
			return "target_kind,target_id,target_revision"
		}
	case *model.Node:
		right := existing.(*model.Node)
		if left.Name == right.Name {
			return "name"
		}
		if left.ChassisID == right.ChassisID {
			return "chassis_id"
		}
	}
	return ""
}

func firstReference(current *snapshot, kind model.Kind, id string) string {
	for _, candidateKind := range model.Kinds() {
		ids := make([]string, 0, len(current.resources[candidateKind]))
		for candidateID := range current.resources[candidateKind] {
			ids = append(ids, candidateID)
		}
		sortStrings(ids)
		for _, candidateID := range ids {
			if candidateKind == kind && candidateID == id {
				continue
			}
			if references(current.resources[candidateKind][candidateID].resource, kind, id) {
				return fmt.Sprintf("%s %q", candidateKind, candidateID)
			}
		}
	}
	return ""
}

func references(resource model.Resource, kind model.Kind, id string) bool {
	switch value := resource.(type) {
	case *model.Network:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindProviderNetwork && value.ProviderNetworkID == id)
	case *model.Subnet:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindNetwork && value.NetworkID == id)
	case *model.Port:
		if (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindNetwork && value.NetworkID == id) {
			return true
		}
		if kind == model.KindSubnet {
			for _, fixed := range value.FixedIPs {
				if fixed.SubnetID == id {
					return true
				}
			}
		}
		if kind == model.KindSecurityGroup {
			for _, groupID := range value.SecurityGroupIDs {
				if groupID == id {
					return true
				}
			}
		}
	case *model.IPAllocation:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindSubnet && value.SubnetID == id) || (kind == model.KindPort && value.PortID == id)
	case *model.Router:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindNetwork && value.ExternalNetworkID == id) || (kind == model.KindSubnet && value.ExternalSubnetID == id)
	case *model.RouterInterface:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindRouter && value.RouterID == id) || (kind == model.KindSubnet && value.SubnetID == id) || (kind == model.KindPort && value.PortID == id)
	case *model.FloatingIP:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindProviderNetwork && value.ProviderNetworkID == id) || (kind == model.KindPort && value.PortID == id) || (kind == model.KindRouter && value.RouterID == id)
	case *model.ProviderSegment:
		return kind == model.KindProviderNetwork && value.ProviderNetworkID == id
	case *model.SecurityGroup:
		return kind == model.KindProject && value.ProjectID == id
	case *model.SecurityGroupRule:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindSecurityGroup && (value.SecurityGroupID == id || value.RemoteGroupID == id))
	}
	return false
}

func matches(resource model.Resource, options controlstore.ListOptions) bool {
	projectID, networkID, nodeID, vmid, nic := resourceFields(resource)
	if options.ProjectID != "" && projectID != options.ProjectID {
		return false
	}
	if options.NetworkID != "" && networkID != options.NetworkID {
		return false
	}
	if options.NodeID != "" && nodeID != options.NodeID {
		return false
	}
	if options.VMID != 0 && vmid != options.VMID {
		return false
	}
	if options.NIC != "" && nic != options.NIC {
		return false
	}
	return true
}

func resourceFields(resource model.Resource) (projectID, networkID, nodeID string, vmid int, nic string) {
	switch value := resource.(type) {
	case *model.Network:
		projectID = value.ProjectID
	case *model.Subnet:
		projectID, networkID = value.ProjectID, value.NetworkID
	case *model.Port:
		projectID, networkID, nodeID, vmid, nic = value.ProjectID, value.NetworkID, value.NodeID, value.VMID, value.NIC
	case *model.IPAllocation:
		projectID = value.ProjectID
	case *model.Router:
		projectID = value.ProjectID
	case *model.RouterInterface:
		projectID = value.ProjectID
	case *model.FloatingIP:
		projectID = value.ProjectID
	case *model.SecurityGroup:
		projectID = value.ProjectID
	case *model.SecurityGroupRule:
		projectID = value.ProjectID
	}
	return
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
