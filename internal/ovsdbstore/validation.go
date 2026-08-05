package ovsdbstore

import (
	"fmt"
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
		if value.DefaultSegmentID == "" {
			return nil
		}
		segmentResource, err := require(current, model.KindProviderSegment, value.DefaultSegmentID, "default_segment_id")
		if err != nil {
			return err
		}
		if segmentResource.(*model.ProviderSegment).ProviderNetworkID != value.ID {
			return storeError(controlstore.ErrConflict, "default segment belongs to a different provider network")
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
		portNetwork := network.(*model.Network)
		if portNetwork.ProjectID != value.ProjectID {
			return storeError(controlstore.ErrConflict, "network belongs to a different project")
		}
		if portNetwork.External || portNetwork.ProviderNetworkID != "" {
			return storeError(controlstore.ErrConflict, "tenant ports cannot use an external or provider-backed network")
		}
		for _, fixed := range value.FixedIPs {
			subnetResource, err := require(current, model.KindSubnet, fixed.SubnetID, "fixed_ips.subnet_id")
			if err != nil {
				return err
			}
			subnet := subnetResource.(*model.Subnet)
			if subnet.NetworkID != value.NetworkID {
				return storeError(controlstore.ErrConflict, "fixed IP subnet belongs to a different network")
			}
			if subnet.ProjectID != value.ProjectID {
				return storeError(controlstore.ErrConflict, "fixed IP subnet belongs to a different project")
			}
			if fixed.Address != "" {
				if addressErr := model.ValidateIPv4AllocationAddress(subnet, fixed.Address); addressErr != nil {
					return storeError(controlstore.ErrConflict, "fixed IP address is not allocatable on its subnet: %v", addressErr)
				}
			}
		}
		for _, groupID := range value.SecurityGroupIDs {
			groupResource, err := require(current, model.KindSecurityGroup, groupID, "security_group_ids")
			if err != nil {
				return err
			}
			if groupResource.(*model.SecurityGroup).ProjectID != value.ProjectID {
				return storeError(controlstore.ErrConflict, "security group belongs to a different project")
			}
		}
		if value.NodeID != "" {
			nodeResource, err := require(current, model.KindNode, value.NodeID, "node_id")
			if err != nil {
				return err
			}
			if value.RequestedChassis != "" && nodeResource.(*model.Node).ChassisID != value.RequestedChassis {
				return storeError(controlstore.ErrConflict, "requested chassis does not match the selected node")
			}
		} else if value.RequestedChassis != "" {
			return storeError(controlstore.ErrConflict, "requested chassis requires a selected node")
		}
	case *model.IPAllocation:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		subnetResource, err := require(current, model.KindSubnet, value.SubnetID, "subnet_id")
		if err != nil {
			return err
		}
		subnet := subnetResource.(*model.Subnet)
		if subnet.ProjectID != value.ProjectID {
			return storeError(controlstore.ErrConflict, "subnet belongs to a different project")
		}
		if addressErr := model.ValidateIPv4AllocationAddress(subnet, value.Address); addressErr != nil {
			return storeError(controlstore.ErrConflict, "allocated address is not allocatable on its subnet: %v", addressErr)
		}
		if value.PortID != "" {
			portResource, err := require(current, model.KindPort, value.PortID, "port_id")
			if err != nil {
				return err
			}
			port := portResource.(*model.Port)
			if port.ProjectID != value.ProjectID {
				return storeError(controlstore.ErrConflict, "port belongs to a different project")
			}
			if port.NetworkID != subnet.NetworkID {
				return storeError(controlstore.ErrConflict, "port belongs to a different network than the allocation subnet")
			}
			if !portHasFixedIP(port, value.SubnetID, value.Address) {
				return storeError(controlstore.ErrConflict, "allocated address is not assigned to the port on this subnet")
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
			providerResource, err := require(current, model.KindProviderNetwork, external.ProviderNetworkID, "external_network_id.provider_network_id")
			if err != nil {
				return err
			}
			if external.ProjectID != value.ProjectID && !providerResource.(*model.ProviderNetwork).Shared {
				return storeError(controlstore.ErrConflict, "external network belongs to another project and is not shared")
			}
			subnetResource, err := require(current, model.KindSubnet, value.ExternalSubnetID, "external_subnet_id")
			if err != nil {
				return err
			}
			subnet := subnetResource.(*model.Subnet)
			if subnet.NetworkID != value.ExternalNetworkID || subnet.ProjectID != external.ProjectID {
				return storeError(controlstore.ErrConflict, "external subnet belongs to a different network")
			}
			if addressErr := model.ValidateIPv4AllocationAddress(subnet, value.ExternalIPAddress); addressErr != nil {
				return storeError(controlstore.ErrConflict, "external IP address is not allocatable on the external subnet: %v", addressErr)
			}
		}
	case *model.RouterInterface:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		routerResource, err := require(current, model.KindRouter, value.RouterID, "router_id")
		if err != nil {
			return err
		}
		if routerResource.(*model.Router).ProjectID != value.ProjectID {
			return storeError(controlstore.ErrConflict, "router belongs to a different project")
		}
		subnetResource, err := require(current, model.KindSubnet, value.SubnetID, "subnet_id")
		if err != nil {
			return err
		}
		subnet := subnetResource.(*model.Subnet)
		if subnet.ProjectID != value.ProjectID {
			return storeError(controlstore.ErrConflict, "subnet belongs to a different project")
		}
		networkResource, err := require(current, model.KindNetwork, subnet.NetworkID, "subnet_id.network_id")
		if err != nil {
			return err
		}
		network := networkResource.(*model.Network)
		if network.ProjectID != value.ProjectID {
			return storeError(controlstore.ErrConflict, "router interface network belongs to a different project")
		}
		if network.External {
			return storeError(controlstore.ErrConflict, "router interfaces can only use internal networks")
		}
		if value.PortID != "" {
			portResource, err := require(current, model.KindPort, value.PortID, "port_id")
			if err != nil {
				return err
			}
			port := portResource.(*model.Port)
			if port.ProjectID != value.ProjectID || port.NetworkID != subnet.NetworkID {
				return storeError(controlstore.ErrConflict, "router interface port belongs to a different project or network")
			}
			if !portHasSubnet(port, value.SubnetID) {
				return storeError(controlstore.ErrConflict, "router interface port has no fixed IP on the interface subnet")
			}
		}
	case *model.FloatingIP:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		providerResource, err := require(current, model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id")
		if err != nil {
			return err
		}
		if !providerHasAllocatableAddress(current, value.ProviderNetworkID, value.Address, "") {
			return storeError(controlstore.ErrConflict, "floating IP address is not allocatable on an external subnet for its provider network")
		}
		if (value.PortID == "") != (value.FixedIPAddress == "") {
			return storeError(controlstore.ErrConflict, "port and fixed IP address must be configured together")
		}
		if value.PortID != "" && value.RouterID == "" {
			return storeError(controlstore.ErrConflict, "an associated floating IP requires a router")
		}
		if value.RouterID != "" {
			routerResource, err := require(current, model.KindRouter, value.RouterID, "router_id")
			if err != nil {
				return err
			}
			router := routerResource.(*model.Router)
			if router.ProjectID != value.ProjectID {
				return storeError(controlstore.ErrConflict, "router belongs to a different project")
			}
			if router.ExternalNetworkID == "" || router.ExternalSubnetID == "" {
				return storeError(controlstore.ErrConflict, "router has no external gateway")
			}
			externalNetworkResource, err := require(current, model.KindNetwork, router.ExternalNetworkID, "router_id.external_network_id")
			if err != nil {
				return err
			}
			externalNetwork := externalNetworkResource.(*model.Network)
			if !externalNetwork.External || externalNetwork.ProviderNetworkID != value.ProviderNetworkID {
				return storeError(controlstore.ErrConflict, "floating IP provider does not match the router external network")
			}
			if externalNetwork.ProjectID != value.ProjectID && !providerResource.(*model.ProviderNetwork).Shared {
				return storeError(controlstore.ErrConflict, "router external network belongs to another project and is not shared")
			}
			externalSubnetResource, err := require(current, model.KindSubnet, router.ExternalSubnetID, "router_id.external_subnet_id")
			if err != nil {
				return err
			}
			externalSubnet := externalSubnetResource.(*model.Subnet)
			if externalSubnet.NetworkID != externalNetwork.ID || externalSubnet.ProjectID != externalNetwork.ProjectID {
				return storeError(controlstore.ErrConflict, "router external subnet does not belong to its external network")
			}
			if addressErr := model.ValidateIPv4AllocationAddress(externalSubnet, value.Address); addressErr != nil {
				return storeError(controlstore.ErrConflict, "floating IP address is not allocatable on the router external subnet: %v", addressErr)
			}
		}
		if value.PortID != "" {
			portResource, err := require(current, model.KindPort, value.PortID, "port_id")
			if err != nil {
				return err
			}
			port := portResource.(*model.Port)
			if port.ProjectID != value.ProjectID {
				return storeError(controlstore.ErrConflict, "port belongs to a different project")
			}
			if !portHasAddress(port, value.FixedIPAddress) {
				return storeError(controlstore.ErrConflict, "fixed IP address is not assigned to the port")
			}
			if !routerReachesPortAddress(current, value.RouterID, port, value.FixedIPAddress) {
				return storeError(controlstore.ErrConflict, "router has no interface on the floating IP port subnet")
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
		groupResource, err := require(current, model.KindSecurityGroup, value.SecurityGroupID, "security_group_id")
		if err != nil {
			return err
		}
		if groupResource.(*model.SecurityGroup).ProjectID != value.ProjectID {
			return storeError(controlstore.ErrConflict, "security group belongs to a different project")
		}
		if value.RemoteGroupID != "" {
			remoteResource, err := require(current, model.KindSecurityGroup, value.RemoteGroupID, "remote_group_id")
			if err != nil {
				return err
			}
			if remoteResource.(*model.SecurityGroup).ProjectID != value.ProjectID {
				return storeError(controlstore.ErrConflict, "remote security group belongs to a different project")
			}
		}
	}
	return nil
}

func providerHasAllocatableAddress(current *snapshot, providerID, address, ignoredSubnetID string) bool {
	for _, networkEntry := range current.resources[model.KindNetwork] {
		network := networkEntry.resource.(*model.Network)
		if network.State == model.ResourceDeleting || !network.External || network.ProviderNetworkID != providerID {
			continue
		}
		for subnetID, subnetEntry := range current.resources[model.KindSubnet] {
			if subnetID == ignoredSubnetID {
				continue
			}
			subnet := subnetEntry.resource.(*model.Subnet)
			if subnet.State != model.ResourceDeleting && subnet.NetworkID == network.ID && model.ValidateIPv4AllocationAddress(subnet, address) == nil {
				return true
			}
		}
	}
	return false
}

func portHasFixedIP(port *model.Port, subnetID, address string) bool {
	for _, fixed := range port.FixedIPs {
		if fixed.SubnetID == subnetID && fixed.Address == address {
			return true
		}
	}
	return false
}

func portHasSubnet(port *model.Port, subnetID string) bool {
	for _, fixed := range port.FixedIPs {
		if fixed.SubnetID == subnetID {
			return true
		}
	}
	return false
}

func portHasAddress(port *model.Port, address string) bool {
	for _, fixed := range port.FixedIPs {
		if fixed.Address == address {
			return true
		}
	}
	return false
}

func routerReachesPortAddress(current *snapshot, routerID string, port *model.Port, address string) bool {
	for _, entry := range current.resources[model.KindRouterInterface] {
		routerInterface := entry.resource.(*model.RouterInterface)
		if routerInterface.RouterID != routerID {
			continue
		}
		if portHasFixedIP(port, routerInterface.SubnetID, address) {
			return true
		}
	}
	return false
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
	for _, candidateClaim := range externalAddressClaims(current, candidate) {
		for _, kind := range []model.Kind{model.KindSubnet, model.KindRouter, model.KindFloatingIP} {
			for id, entry := range current.resources[kind] {
				if kind == candidate.ResourceKind() && id == ignoredID {
					continue
				}
				for _, existingClaim := range externalAddressClaims(current, entry.resource) {
					if candidateClaim == existingClaim {
						return storeError(controlstore.ErrAlreadyExists, "%s conflicts with existing %s %q on provider network address", candidate.ResourceKind(), kind, id)
					}
				}
			}
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
	if err := validateSnapshotUnique(current); err != nil {
		return storeError(controlstore.ErrConflict, "updating %s %q would violate uniqueness: %v", candidate.ResourceKind(), id, err)
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
	seenExternal := make(map[string]string)
	for _, kind := range []model.Kind{model.KindSubnet, model.KindRouter, model.KindFloatingIP} {
		for id, entry := range current.resources[kind] {
			for _, key := range externalAddressClaims(current, entry.resource) {
				owner := fmt.Sprintf("%s %q", kind, id)
				if existing, duplicate := seenExternal[key]; duplicate {
					return fmt.Errorf("stored %s conflicts with %s on provider network address", owner, existing)
				}
				seenExternal[key] = owner
			}
		}
	}
	subnetIDs := make([]string, 0, len(current.resources[model.KindSubnet]))
	for id := range current.resources[model.KindSubnet] {
		subnetIDs = append(subnetIDs, id)
	}
	sortStrings(subnetIDs)
	for leftIndex, leftID := range subnetIDs {
		left := current.resources[model.KindSubnet][leftID].resource.(*model.Subnet)
		for _, rightID := range subnetIDs[leftIndex+1:] {
			right := current.resources[model.KindSubnet][rightID].resource.(*model.Subnet)
			if left.NetworkID == right.NetworkID && model.IPv4PrefixesOverlap(left.CIDR, right.CIDR) {
				return fmt.Errorf("stored subnet %q overlaps %q on network_id,cidr", leftID, rightID)
			}
		}
	}
	return nil
}

func externalAddressClaims(current *snapshot, resource model.Resource) []string {
	providerID, address := "", ""
	switch value := resource.(type) {
	case *model.Subnet:
		if networkEntry, exists := current.resources[model.KindNetwork][value.NetworkID]; exists {
			providerID = networkEntry.resource.(*model.Network).ProviderNetworkID
			if gateway, err := model.EffectiveIPv4Gateway(value); err == nil {
				address = gateway.String()
			}
		}
	case *model.Router:
		if networkEntry, exists := current.resources[model.KindNetwork][value.ExternalNetworkID]; exists {
			providerID = networkEntry.resource.(*model.Network).ProviderNetworkID
			address = value.ExternalIPAddress
		}
	case *model.FloatingIP:
		providerID, address = value.ProviderNetworkID, value.Address
	}
	if providerID == "" || address == "" {
		return nil
	}
	return []string{providerID + "\x00" + address}
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
		for _, fixed := range value.FixedIPs {
			if fixed.Address != "" {
				result = append(result, "fixed_ips.subnet_id,address="+fixed.SubnetID+"\x00"+fixed.Address)
			}
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
		if left.NetworkID == right.NetworkID && model.IPv4PrefixesOverlap(left.CIDR, right.CIDR) {
			return "network_id,cidr-overlap"
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
		for _, leftIP := range left.FixedIPs {
			if leftIP.Address == "" {
				continue
			}
			for _, rightIP := range right.FixedIPs {
				if leftIP.SubnetID == rightIP.SubnetID && leftIP.Address == rightIP.Address {
					return "fixed_ips.subnet_id,address"
				}
			}
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
	if kind == model.KindProviderSegment {
		segmentEntry, exists := current.resources[kind][id]
		if exists {
			segment := segmentEntry.resource.(*model.ProviderSegment)
			providerEntry, providerExists := current.resources[model.KindProviderNetwork][segment.ProviderNetworkID]
			if providerExists && providerEntry.resource.(*model.ProviderNetwork).DefaultSegmentID == "" {
				matching := 0
				for _, entry := range current.resources[model.KindProviderSegment] {
					if entry.resource.(*model.ProviderSegment).ProviderNetworkID == segment.ProviderNetworkID {
						matching++
					}
				}
				if matching == 1 {
					networkIDs := make([]string, 0, len(current.resources[model.KindNetwork]))
					for networkID := range current.resources[model.KindNetwork] {
						networkIDs = append(networkIDs, networkID)
					}
					sortStrings(networkIDs)
					for _, networkID := range networkIDs {
						if current.resources[model.KindNetwork][networkID].resource.(*model.Network).ProviderNetworkID == segment.ProviderNetworkID {
							return fmt.Sprintf("%s %q", model.KindNetwork, networkID)
						}
					}
				}
			}
		}
	}
	if kind == model.KindRouterInterface {
		routerInterfaceEntry, ok := current.resources[kind][id]
		if ok {
			routerInterface := routerInterfaceEntry.resource.(*model.RouterInterface)
			ids := make([]string, 0, len(current.resources[model.KindFloatingIP]))
			for floatingID := range current.resources[model.KindFloatingIP] {
				ids = append(ids, floatingID)
			}
			sortStrings(ids)
			for _, floatingID := range ids {
				floating := current.resources[model.KindFloatingIP][floatingID].resource.(*model.FloatingIP)
				if floating.RouterID != routerInterface.RouterID || floating.PortID == "" {
					continue
				}
				portEntry, exists := current.resources[model.KindPort][floating.PortID]
				if exists && portHasFixedIP(portEntry.resource.(*model.Port), routerInterface.SubnetID, floating.FixedIPAddress) {
					return fmt.Sprintf("%s %q", model.KindFloatingIP, floatingID)
				}
			}
		}
	}
	if kind == model.KindSubnet {
		subnetEntry, exists := current.resources[kind][id]
		if exists {
			subnet := subnetEntry.resource.(*model.Subnet)
			networkEntry, networkExists := current.resources[model.KindNetwork][subnet.NetworkID]
			if networkExists {
				network := networkEntry.resource.(*model.Network)
				if network.External && network.ProviderNetworkID != "" {
					ids := make([]string, 0, len(current.resources[model.KindFloatingIP]))
					for floatingID := range current.resources[model.KindFloatingIP] {
						ids = append(ids, floatingID)
					}
					sortStrings(ids)
					for _, floatingID := range ids {
						floating := current.resources[model.KindFloatingIP][floatingID].resource.(*model.FloatingIP)
						if floating.ProviderNetworkID == network.ProviderNetworkID && model.ValidateIPv4AllocationAddress(subnet, floating.Address) == nil && !providerHasAllocatableAddress(current, network.ProviderNetworkID, floating.Address, id) {
							return fmt.Sprintf("%s %q", model.KindFloatingIP, floatingID)
						}
					}
				}
			}
		}
	}
	return ""
}

func references(resource model.Resource, kind model.Kind, id string) bool {
	switch value := resource.(type) {
	case *model.ProviderNetwork:
		return kind == model.KindProviderSegment && value.DefaultSegmentID == id
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
