package ovsdbstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/popododo0720/proxmox-ovn/internal/controlschema"
	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	storeLockID                = "__pvn_store_lock__"
	internalIDPrefix           = "__pvn_internal_"
	internalScopeKey           = "__pvn_internal_idem_scope__/"
	internalTypeKey            = "pvn:internal-type"
	internalLockType           = "store-lock"
	internalIdemType           = "idempotency"
	idemFingerprint            = "pvn:fingerprint"
	idemResource               = "pvn:resource"
	idemResourceKind           = "pvn:resource-kind"
	idemDeleted                = "pvn:deleted"
	subnetDNSDomainKey         = "pvn:dns-domain"
	subnetDNSSearchDomainsKey  = "pvn:dns-search-domains"
	routerStaticRoutesKey      = "pvn:static-routes"
	maxStoredErrorLen          = 16 * 1024
	maxResourceExternalJSONLen = 64 * 1024
)

type idempotencyRecord struct {
	fingerprint [sha256.Size]byte
	resource    model.Resource
	deleted     bool
}

type storedResource struct {
	uuid     string
	resource model.Resource
}

type snapshot struct {
	epoch       int64
	hasLock     bool
	resources   map[model.Kind]map[string]storedResource
	uuidToID    map[model.Kind]map[string]string
	idempotency map[string]idempotencyRecord
}

func newSnapshot() *snapshot {
	result := &snapshot{
		resources:   make(map[model.Kind]map[string]storedResource),
		uuidToID:    make(map[model.Kind]map[string]string),
		idempotency: make(map[string]idempotencyRecord),
	}
	for _, kind := range model.Kinds() {
		result.resources[kind] = make(map[string]storedResource)
		result.uuidToID[kind] = make(map[string]string)
	}
	return result
}

var kindTables = map[model.Kind]string{
	model.KindNetwork:           controlschema.NetworkTable,
	model.KindSubnet:            controlschema.SubnetTable,
	model.KindPort:              controlschema.PortTable,
	model.KindIPAllocation:      controlschema.IPAllocationTable,
	model.KindRouter:            controlschema.RouterTable,
	model.KindRouterInterface:   controlschema.RouterInterfaceTable,
	model.KindFloatingIP:        controlschema.FloatingIPTable,
	model.KindProviderNetwork:   controlschema.ProviderNetworkTable,
	model.KindProviderSegment:   controlschema.ProviderSegmentTable,
	model.KindSecurityGroup:     controlschema.SecurityGroupTable,
	model.KindSecurityGroupRule: controlschema.SecurityGroupRuleTable,
	model.KindNode:              controlschema.NodeTable,
	model.KindOperation:         controlschema.OperationTable,
}

var tableKinds = func() map[string]model.Kind {
	result := make(map[string]model.Kind, len(kindTables))
	for kind, table := range kindTables {
		result[table] = kind
	}
	return result
}()

func allTables() []string {
	result := make([]string, 0, len(kindTables))
	for _, table := range kindTables {
		result = append(result, table)
	}
	sort.Strings(result)
	return result
}

func tableForKind(kind model.Kind) (string, error) {
	table, ok := kindTables[kind]
	if !ok {
		return "", storeError(controlstore.ErrNotFound, "unknown resource kind %q", kind)
	}
	return table, nil
}

func decodeDatabase(raw rawDatabase) (*snapshot, error) {
	result := newSnapshot()

	// Resolve every UUID before decoding rows so references are independent of
	// table and transaction result ordering.
	for _, table := range allTables() {
		kind := tableKinds[table]
		rows, exists := raw[table]
		if !exists {
			return nil, fmt.Errorf("PVN control snapshot omitted table %s", table)
		}
		for rowIndex, row := range rows {
			id, err := rowString(row, "id")
			if err != nil {
				return nil, rowError(table, rowIndex, err)
			}
			uuid, err := rowUUID(row, "_uuid")
			if err != nil {
				return nil, rowError(table, rowIndex, err)
			}
			if _, duplicate := result.uuidToID[kind][uuid]; duplicate {
				return nil, fmt.Errorf("PVN control snapshot has duplicate %s UUID %q", table, uuid)
			}
			result.uuidToID[kind][uuid] = id
		}
	}

	for _, table := range allTables() {
		kind := tableKinds[table]
		for rowIndex, row := range raw[table] {
			if kind == model.KindOperation {
				handled, err := decodeInternalOperation(row, result)
				if err != nil {
					return nil, rowError(table, rowIndex, err)
				}
				if handled {
					continue
				}
			}
			resource, err := decodeResource(kind, row, result)
			if err != nil {
				return nil, rowError(table, rowIndex, err)
			}
			id := resource.GetMetadata().ID
			if strings.HasPrefix(id, internalIDPrefix) || id == storeLockID {
				return nil, fmt.Errorf("resource %s %q uses a reserved internal id", kind, id)
			}
			if _, duplicate := result.resources[kind][id]; duplicate {
				return nil, fmt.Errorf("PVN control snapshot has duplicate %s id %q", kind, id)
			}
			uuid, _ := rowUUID(row, "_uuid")
			result.resources[kind][id] = storedResource{uuid: uuid, resource: resource}
		}
	}
	for _, kind := range model.Kinds() {
		ids := make([]string, 0, len(result.resources[kind]))
		for id := range result.resources[kind] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			resource := result.resources[kind][id].resource
			if err := resource.Validate(); err != nil {
				return nil, fmt.Errorf("stored %s %q is invalid: %w", kind, id, err)
			}
			if err := validateReferences(result, resource); err != nil {
				return nil, fmt.Errorf("stored %s %q has invalid references: %w", kind, id, err)
			}
		}
	}
	if err := validateSnapshotUnique(result); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeInternalOperation(row ovsdb.Row, result *snapshot) (bool, error) {
	external, err := rowStringMap(row, "external_ids")
	if err != nil {
		return false, err
	}
	typeName := external[internalTypeKey]
	if typeName == "" {
		return false, nil
	}
	id, err := rowString(row, "id")
	if err != nil {
		return false, err
	}
	switch typeName {
	case internalLockType:
		if id != storeLockID || result.hasLock {
			return false, fmt.Errorf("invalid or duplicate PVN store lock row %q", id)
		}
		action, actionErr := rowString(row, "action")
		targetKind, targetKindErr := rowString(row, "target_kind")
		targetID, targetIDErr := rowString(row, "target_id")
		key, keyErr := rowString(row, "idempotency_key")
		status, statusErr := rowString(row, "operation_status")
		state, stateErr := rowString(row, "state")
		if err := firstError(actionErr, targetKindErr, targetIDErr, keyErr, statusErr, stateErr); err != nil {
			return false, fmt.Errorf("invalid PVN store lock: %w", err)
		}
		if action != "internal-lock" || targetKind != "internal" || targetID != storeLockID || key != storeLockID || status != string(model.OperationSucceeded) || state != string(model.ResourceReady) {
			return false, fmt.Errorf("PVN store lock has invalid identity or state")
		}
		epoch, err := rowInt64(row, "revision")
		if err != nil {
			return false, fmt.Errorf("invalid PVN store lock revision: %w", err)
		}
		if epoch < 1 {
			return false, fmt.Errorf("invalid PVN store lock revision %d", epoch)
		}
		result.epoch = epoch
		result.hasLock = true
		return true, nil
	case internalIdemType:
		storedScope, err := rowString(row, "idempotency_key")
		if err != nil {
			return false, fmt.Errorf("invalid idempotency scope: %w", err)
		}
		if !strings.HasPrefix(storedScope, internalScopeKey) || len(storedScope) == len(internalScopeKey) {
			return false, fmt.Errorf("idempotency scope has an invalid internal namespace")
		}
		scope := strings.TrimPrefix(storedScope, internalScopeKey)
		fingerprintBytes, err := hex.DecodeString(external[idemFingerprint])
		if err != nil || len(fingerprintBytes) != sha256.Size {
			return false, fmt.Errorf("invalid idempotency fingerprint for %q", scope)
		}
		kind := model.Kind(external[idemResourceKind])
		if !kind.Valid() {
			return false, fmt.Errorf("invalid idempotency resource kind %q", kind)
		}
		resource, err := model.New(kind)
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal([]byte(external[idemResource]), resource); err != nil {
			return false, fmt.Errorf("decode idempotency result for %q: %w", scope, err)
		}
		meta := resource.GetMetadata()
		if meta.ID == "" || meta.Revision < 1 || meta.AppliedRevision < 0 || meta.AppliedRevision > meta.Revision || meta.CreatedAt.IsZero() || meta.UpdatedAt.IsZero() {
			return false, fmt.Errorf("idempotency result for %q has invalid metadata", scope)
		}
		if meta.State != model.ResourcePending && meta.State != model.ResourceReady && meta.State != model.ResourceError && meta.State != model.ResourceDeleting {
			return false, fmt.Errorf("idempotency result for %q has invalid state %q", scope, meta.State)
		}
		if err := resource.Validate(); err != nil {
			return false, fmt.Errorf("idempotency result for %q is invalid: %w", scope, err)
		}
		if _, duplicate := result.idempotency[scope]; duplicate {
			return false, fmt.Errorf("duplicate idempotency scope %q", scope)
		}
		var fingerprint [sha256.Size]byte
		copy(fingerprint[:], fingerprintBytes)
		deleted, err := strconv.ParseBool(external[idemDeleted])
		if err != nil {
			return false, fmt.Errorf("invalid idempotency deletion marker for %q", scope)
		}
		result.idempotency[scope] = idempotencyRecord{fingerprint: fingerprint, resource: resource, deleted: deleted}
		return true, nil
	default:
		return false, fmt.Errorf("unknown internal operation type %q", typeName)
	}
}

func decodeResource(kind model.Kind, row ovsdb.Row, refs *snapshot) (model.Resource, error) {
	meta, err := decodeMetadata(row)
	if err != nil {
		return nil, err
	}
	ref := func(target model.Kind, column string, optional bool) (string, error) {
		uuid, err := rowReference(row, column, optional)
		if err != nil || uuid == "" {
			return "", err
		}
		id, exists := refs.uuidToID[target][uuid]
		if !exists {
			return "", fmt.Errorf("column %s references unknown %s UUID %q", column, target, uuid)
		}
		return id, nil
	}
	stringValue := func(column string) (string, error) { return rowString(row, column) }
	boolValue := func(column string) (bool, error) { return rowBool(row, column) }
	intValue := func(column string) (int, error) {
		value, err := rowInt64(row, column)
		return int(value), err
	}

	switch kind {
	case model.KindNetwork:
		name, e1 := stringValue("name")
		description, e2 := stringValue("description")
		mtu, e3 := intValue("mtu")
		external, e4 := boolValue("external")
		providerID, e5 := ref(model.KindProviderNetwork, "provider_network", true)
		return &model.Network{Metadata: meta, Name: name, Description: description, MTU: mtu, External: external, ProviderNetworkID: providerID}, firstError(e1, e2, e3, e4, e5)
	case model.KindSubnet:
		networkID, e1 := ref(model.KindNetwork, "network", false)
		name, e2 := stringValue("name")
		cidr, e3 := stringValue("cidr")
		gateway, e4 := stringValue("gateway_ip")
		dhcp, e5 := boolValue("enable_dhcp")
		dns, e6 := rowStringSet(row, "dns_nameservers")
		pools, e7 := decodeIPRanges(row, "allocation_pools")
		external, e8 := rowStringMap(row, "external_ids")
		searchDomains, e9 := decodeExternalJSON[[]string](external, subnetDNSSearchDomainsKey)
		return &model.Subnet{Metadata: meta, NetworkID: networkID, Name: name, CIDR: cidr, GatewayIP: gateway, EnableDHCP: dhcp, DNSNameservers: dns, DNSDomain: external[subnetDNSDomainKey], DNSSearchDomains: searchDomains, AllocationPools: pools}, firstError(e1, e2, e3, e4, e5, e6, e7, e8, e9)
	case model.KindPort:
		networkID, e1 := ref(model.KindNetwork, "network", false)
		name, e2 := stringValue("name")
		mac, e3 := stringValue("mac_address")
		fixed, e4 := decodeFixedIPs(row, refs)
		groups, e5 := decodeReferenceSet(row, "security_groups", model.KindSecurityGroup, refs)
		adminUp, e6 := boolValue("admin_state_up")
		binding, e7 := stringValue("binding_status")
		nodeID, e8 := ref(model.KindNode, "node", true)
		vmid, e9 := intValue("vmid")
		nic, e10 := stringValue("nic")
		lsp, e11 := stringValue("lsp_name")
		generation, e12 := rowInt64(row, "generation")
		requested, e13 := stringValue("requested_chassis")
		return &model.Port{Metadata: meta, NetworkID: networkID, Name: name, MACAddress: mac, FixedIPs: fixed, SecurityGroupIDs: groups, AdminStateUp: adminUp, BindingStatus: model.PortBindingStatus(binding), NodeID: nodeID, VMID: vmid, NIC: nic, LSPName: lsp, Generation: generation, RequestedChassis: requested}, firstError(e1, e2, e3, e4, e5, e6, e7, e8, e9, e10, e11, e12, e13)
	case model.KindIPAllocation:
		subnetID, e1 := ref(model.KindSubnet, "subnet", false)
		portID, e2 := ref(model.KindPort, "port", true)
		address, e3 := stringValue("address")
		state, e4 := stringValue("allocation_state")
		return &model.IPAllocation{Metadata: meta, SubnetID: subnetID, PortID: portID, Address: address, State: model.IPAllocationState(state)}, firstError(e1, e2, e3, e4)
	case model.KindRouter:
		name, e1 := stringValue("name")
		description, e2 := stringValue("description")
		externalID, e3 := ref(model.KindNetwork, "external_network", true)
		externalSubnetID, e4 := ref(model.KindSubnet, "external_subnet", true)
		externalIPAddress, e5 := stringValue("external_ip_address")
		snat, e6 := boolValue("enable_snat")
		external, e7 := rowStringMap(row, "external_ids")
		staticRoutes, e8 := decodeExternalJSON[[]model.StaticRoute](external, routerStaticRoutesKey)
		return &model.Router{Metadata: meta, Name: name, Description: description, ExternalNetworkID: externalID, ExternalSubnetID: externalSubnetID, ExternalIPAddress: externalIPAddress, EnableSNAT: snat, StaticRoutes: staticRoutes}, firstError(e1, e2, e3, e4, e5, e6, e7, e8)
	case model.KindRouterInterface:
		routerID, e1 := ref(model.KindRouter, "router", false)
		subnetID, e2 := ref(model.KindSubnet, "subnet", false)
		portID, e3 := ref(model.KindPort, "port", true)
		return &model.RouterInterface{Metadata: meta, RouterID: routerID, SubnetID: subnetID, PortID: portID}, firstError(e1, e2, e3)
	case model.KindFloatingIP:
		providerID, e1 := ref(model.KindProviderNetwork, "provider_network", false)
		address, e2 := stringValue("address")
		portID, e3 := ref(model.KindPort, "port", true)
		fixed, e4 := stringValue("fixed_ip_address")
		routerID, e5 := ref(model.KindRouter, "router", true)
		status, e6 := stringValue("floating_status")
		return &model.FloatingIP{Metadata: meta, ProviderNetworkID: providerID, Address: address, PortID: portID, FixedIPAddress: fixed, RouterID: routerID, FloatingStatus: model.FloatingIPStatus(status)}, firstError(e1, e2, e3, e4, e5, e6)
	case model.KindProviderNetwork:
		name, e1 := stringValue("name")
		description, e2 := stringValue("description")
		segmentID, e3 := ref(model.KindProviderSegment, "default_segment", true)
		return &model.ProviderNetwork{Metadata: meta, Name: name, Description: description, DefaultSegmentID: segmentID}, firstError(e1, e2, e3)
	case model.KindProviderSegment:
		providerID, e1 := ref(model.KindProviderNetwork, "provider_network", false)
		name, e2 := stringValue("name")
		physical, e3 := stringValue("physical_network")
		networkType, e4 := stringValue("network_type")
		vlanID, e5 := intValue("vlan_id")
		return &model.ProviderSegment{Metadata: meta, ProviderNetworkID: providerID, Name: name, PhysicalNetwork: physical, NetworkType: model.ProviderNetworkType(networkType), VLANID: vlanID}, firstError(e1, e2, e3, e4, e5)
	case model.KindSecurityGroup:
		name, e1 := stringValue("name")
		description, e2 := stringValue("description")
		stateful, e3 := boolValue("stateful")
		return &model.SecurityGroup{Metadata: meta, Name: name, Description: description, Stateful: stateful}, firstError(e1, e2, e3)
	case model.KindSecurityGroupRule:
		groupID, e1 := ref(model.KindSecurityGroup, "security_group", false)
		direction, e2 := stringValue("direction")
		ethertype, e3 := stringValue("ethertype")
		protocol, e4 := stringValue("protocol")
		minPort, e5 := intValue("port_range_min")
		maxPort, e6 := intValue("port_range_max")
		remoteCIDR, e7 := stringValue("remote_cidr")
		remoteGroup, e8 := ref(model.KindSecurityGroup, "remote_group", true)
		action, e9 := stringValue("action")
		description, e10 := stringValue("description")
		return &model.SecurityGroupRule{Metadata: meta, SecurityGroupID: groupID, Direction: model.RuleDirection(direction), EtherType: model.EtherType(ethertype), Protocol: protocol, PortRangeMin: minPort, PortRangeMax: maxPort, RemoteCIDR: remoteCIDR, RemoteGroupID: remoteGroup, Action: model.RuleAction(action), Description: description}, firstError(e1, e2, e3, e4, e5, e6, e7, e8, e9, e10)
	case model.KindNode:
		name, e1 := stringValue("name")
		chassis, e2 := stringValue("chassis_id")
		address, e3 := stringValue("management_address")
		roleValues, e4 := rowStringSet(row, "roles")
		enabled, e5 := boolValue("enabled")
		lastSeen, e6 := rowOptionalTime(row, "last_seen_at")
		roles := make([]model.NodeRole, len(roleValues))
		for index := range roleValues {
			roles[index] = model.NodeRole(roleValues[index])
		}
		return &model.Node{Metadata: meta, Name: name, ChassisID: chassis, ManagementAddress: address, Roles: roles, Enabled: enabled, LastSeenAt: lastSeen}, firstError(e1, e2, e3, e4, e5, e6)
	case model.KindOperation:
		action, e1 := stringValue("action")
		targetKind, e2 := stringValue("target_kind")
		targetID, e3 := stringValue("target_id")
		targetRevision, e4 := rowInt64(row, "target_revision")
		status, e5 := stringValue("operation_status")
		key, e6 := stringValue("idempotency_key")
		errorText, e7 := stringValue("error")
		leaseOwner, e8 := stringValue("lease_owner")
		started, e9 := rowOptionalTime(row, "started_at")
		completed, e10 := rowOptionalTime(row, "completed_at")
		return &model.Operation{Metadata: meta, Action: action, TargetKind: model.Kind(targetKind), TargetID: targetID, TargetRevision: targetRevision, OperationStatus: model.OperationStatus(status), IdempotencyKey: key, Error: errorText, LeaseOwner: leaseOwner, StartedAt: started, CompletedAt: completed}, firstError(e1, e2, e3, e4, e5, e6, e7, e8, e9, e10)
	default:
		return nil, fmt.Errorf("unsupported resource kind %q", kind)
	}
}

func encodeResource(resource model.Resource, refs *snapshot) (ovsdb.Row, error) {
	if resource == nil {
		return nil, fmt.Errorf("resource is nil")
	}
	row, err := encodeMetadata(resource.GetMetadata())
	if err != nil {
		return nil, err
	}
	ref := func(kind model.Kind, id, field string, optional bool) (any, error) {
		if id == "" && optional {
			return ovsdb.OvsSet{GoSet: []interface{}{}}, nil
		}
		entry, exists := refs.resources[kind][id]
		if !exists {
			return nil, storeError(controlstore.ErrConflict, "%s references missing %s %q", field, kind, id)
		}
		value := ovsdb.UUID{GoUUID: entry.uuid}
		if optional {
			return ovsdb.OvsSet{GoSet: []interface{}{value}}, nil
		}
		return value, nil
	}
	setRef := func(column string, kind model.Kind, id, field string, optional bool) {
		if err != nil {
			return
		}
		row[column], err = ref(kind, id, field, optional)
	}

	externalIDs := make(map[string]string)
	switch value := resource.(type) {
	case *model.Network:
		row["name"], row["description"], row["mtu"], row["external"] = value.Name, value.Description, value.MTU, value.External
		setRef("provider_network", model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id", true)
	case *model.Subnet:
		setRef("network", model.KindNetwork, value.NetworkID, "network_id", false)
		row["name"], row["cidr"], row["gateway_ip"], row["enable_dhcp"] = value.Name, value.CIDR, value.GatewayIP, value.EnableDHCP
		if err == nil {
			row["dns_nameservers"], err = encodeUniqueStringSet(value.DNSNameservers, "dns_nameservers")
		}
		if err == nil {
			row["allocation_pools"], err = encodeIPRanges(value.AllocationPools)
		}
		if value.DNSDomain != "" {
			externalIDs[subnetDNSDomainKey] = value.DNSDomain
		}
		if err == nil {
			err = encodeExternalJSON(externalIDs, subnetDNSSearchDomainsKey, value.DNSSearchDomains)
		}
	case *model.Port:
		setRef("network", model.KindNetwork, value.NetworkID, "network_id", false)
		row["name"], row["mac_address"] = value.Name, value.MACAddress
		if err == nil {
			row["fixed_ips"], err = encodeFixedIPs(value.FixedIPs, refs)
		}
		if err == nil {
			row["security_groups"], err = encodeReferenceSet(value.SecurityGroupIDs, model.KindSecurityGroup, "security_group_ids", refs)
		}
		row["admin_state_up"], row["binding_status"] = value.AdminStateUp, string(value.BindingStatus)
		setRef("node", model.KindNode, value.NodeID, "node_id", true)
		row["vmid"], row["nic"], row["lsp_name"], row["generation"], row["requested_chassis"] = value.VMID, value.NIC, value.LSPName, value.Generation, value.RequestedChassis
	case *model.IPAllocation:
		setRef("subnet", model.KindSubnet, value.SubnetID, "subnet_id", false)
		setRef("port", model.KindPort, value.PortID, "port_id", true)
		row["address"], row["allocation_state"] = value.Address, string(value.State)
	case *model.Router:
		row["name"], row["description"] = value.Name, value.Description
		setRef("external_network", model.KindNetwork, value.ExternalNetworkID, "external_network_id", true)
		setRef("external_subnet", model.KindSubnet, value.ExternalSubnetID, "external_subnet_id", true)
		row["external_ip_address"] = value.ExternalIPAddress
		row["enable_snat"] = value.EnableSNAT
		if err == nil {
			err = encodeExternalJSON(externalIDs, routerStaticRoutesKey, value.StaticRoutes)
		}
	case *model.RouterInterface:
		setRef("router", model.KindRouter, value.RouterID, "router_id", false)
		setRef("subnet", model.KindSubnet, value.SubnetID, "subnet_id", false)
		setRef("port", model.KindPort, value.PortID, "port_id", true)
	case *model.FloatingIP:
		setRef("provider_network", model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id", false)
		row["address"] = value.Address
		setRef("port", model.KindPort, value.PortID, "port_id", true)
		row["fixed_ip_address"] = value.FixedIPAddress
		setRef("router", model.KindRouter, value.RouterID, "router_id", true)
		row["floating_status"] = string(value.FloatingStatus)
	case *model.ProviderNetwork:
		row["name"], row["description"] = value.Name, value.Description
		setRef("default_segment", model.KindProviderSegment, value.DefaultSegmentID, "default_segment_id", true)
	case *model.ProviderSegment:
		setRef("provider_network", model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id", false)
		row["name"], row["physical_network"], row["network_type"], row["vlan_id"] = value.Name, value.PhysicalNetwork, string(value.NetworkType), value.VLANID
	case *model.SecurityGroup:
		row["name"], row["description"], row["stateful"] = value.Name, value.Description, value.Stateful
	case *model.SecurityGroupRule:
		setRef("security_group", model.KindSecurityGroup, value.SecurityGroupID, "security_group_id", false)
		row["direction"], row["ethertype"], row["protocol"] = string(value.Direction), string(value.EtherType), value.Protocol
		row["port_range_min"], row["port_range_max"], row["remote_cidr"] = value.PortRangeMin, value.PortRangeMax, value.RemoteCIDR
		setRef("remote_group", model.KindSecurityGroup, value.RemoteGroupID, "remote_group_id", true)
		row["action"], row["description"] = string(value.Action), value.Description
	case *model.Node:
		row["name"], row["chassis_id"], row["management_address"] = value.Name, value.ChassisID, value.ManagementAddress
		roles := make([]string, len(value.Roles))
		for index := range value.Roles {
			roles[index] = string(value.Roles[index])
		}
		row["roles"], row["enabled"], row["last_seen_at"] = encodeStringSet(roles), value.Enabled, encodeOptionalTime(value.LastSeenAt)
	case *model.Operation:
		row["action"], row["target_kind"], row["target_id"], row["target_revision"] = value.Action, string(value.TargetKind), value.TargetID, value.TargetRevision
		row["operation_status"], row["idempotency_key"], row["error"], row["lease_owner"] = string(value.OperationStatus), value.IdempotencyKey, value.Error, value.LeaseOwner
		row["started_at"], row["completed_at"] = encodeOptionalTime(value.StartedAt), encodeOptionalTime(value.CompletedAt)
	default:
		return nil, fmt.Errorf("unsupported resource type %T", resource)
	}
	if err != nil {
		return nil, err
	}
	row["external_ids"] = encodeStringMap(externalIDs)
	return row, nil
}

func encodeExternalJSON[T any](externalIDs map[string]string, key string, values []T) error {
	if len(values) == 0 {
		return nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode external_ids %s: %w", key, err)
	}
	if len(encoded) > maxResourceExternalJSONLen {
		return fmt.Errorf("encode external_ids %s: payload exceeds %d bytes", key, maxResourceExternalJSONLen)
	}
	externalIDs[key] = string(encoded)
	return nil
}

func decodeExternalJSON[T any](externalIDs map[string]string, key string) (T, error) {
	var result T
	encoded := externalIDs[key]
	if encoded == "" {
		return result, nil
	}
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		return result, fmt.Errorf("external_ids %s contains malformed JSON: %w", key, err)
	}
	return result, nil
}

func encodeMetadata(meta *model.Metadata) (ovsdb.Row, error) {
	if meta == nil || meta.ID == "" || meta.Revision < 1 || meta.CreatedAt.IsZero() || meta.UpdatedAt.IsZero() {
		return nil, fmt.Errorf("resource has incomplete server metadata")
	}
	return ovsdb.Row{
		"id":               meta.ID,
		"revision":         meta.Revision,
		"applied_revision": meta.AppliedRevision,
		"state":            string(meta.State),
		"last_error":       meta.LastError,
		"created_at":       formatTime(meta.CreatedAt),
		"updated_at":       formatTime(meta.UpdatedAt),
	}, nil
}

func decodeMetadata(row ovsdb.Row) (model.Metadata, error) {
	id, e1 := rowString(row, "id")
	revision, e2 := rowInt64(row, "revision")
	applied, e3 := rowInt64(row, "applied_revision")
	state, e4 := rowString(row, "state")
	lastError, e5 := rowString(row, "last_error")
	created, e6 := rowTime(row, "created_at")
	updated, e7 := rowTime(row, "updated_at")
	if err := firstError(e1, e2, e3, e4, e5, e6, e7); err != nil {
		return model.Metadata{}, err
	}
	if id == "" || revision < 1 || applied < 0 || applied > revision {
		return model.Metadata{}, fmt.Errorf("invalid metadata for resource %q", id)
	}
	resourceState := model.ResourceState(state)
	if resourceState != model.ResourcePending && resourceState != model.ResourceReady && resourceState != model.ResourceError && resourceState != model.ResourceDeleting {
		return model.Metadata{}, fmt.Errorf("resource %q has invalid state %q", id, state)
	}
	return model.Metadata{ID: id, Revision: revision, AppliedRevision: applied, State: resourceState, LastError: lastError, CreatedAt: created, UpdatedAt: updated}, nil
}

func encodeLockRow(now time.Time) ovsdb.Row {
	timestamp := formatTime(now)
	return ovsdb.Row{
		"id": storeLockID, "action": "internal-lock", "target_kind": "internal",
		"target_id": storeLockID, "target_revision": int64(1), "operation_status": string(model.OperationSucceeded),
		"idempotency_key": storeLockID, "error": "", "started_at": "", "completed_at": "",
		"revision": int64(1), "applied_revision": int64(1), "state": string(model.ResourceReady),
		"last_error": "", "created_at": timestamp, "updated_at": timestamp,
		"external_ids": encodeStringMap(map[string]string{internalTypeKey: internalLockType}),
	}
}

func encodeIdempotencyRow(scope string, fingerprint [sha256.Size]byte, resource model.Resource, deleted bool, now time.Time) (ovsdb.Row, error) {
	encoded, err := json.Marshal(resource)
	if err != nil {
		return nil, err
	}
	id := idempotencyRowID(scope)
	timestamp := formatTime(now)
	return ovsdb.Row{
		"id": id, "action": "idempotency", "target_kind": string(resource.ResourceKind()),
		"target_id": id, "target_revision": resource.GetMetadata().Revision, "operation_status": string(model.OperationSucceeded),
		"idempotency_key": internalScopeKey + scope, "error": "", "started_at": "", "completed_at": timestamp,
		"revision": int64(1), "applied_revision": int64(1), "state": string(model.ResourceReady),
		"last_error": "", "created_at": timestamp, "updated_at": timestamp,
		"external_ids": encodeStringMap(map[string]string{
			internalTypeKey:  internalIdemType,
			idemFingerprint:  hex.EncodeToString(fingerprint[:]),
			idemResource:     string(encoded),
			idemResourceKind: string(resource.ResourceKind()),
			idemDeleted:      strconv.FormatBool(deleted),
		}),
	}, nil
}

func idempotencyRowID(scope string) string {
	hash := sha256.Sum256([]byte(scope))
	return internalIDPrefix + "idem_" + hex.EncodeToString(hash[:])
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func encodeOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatTime(*value)
}

func encodeStringSet(values []string) ovsdb.OvsSet {
	set := make([]interface{}, len(values))
	for index := range values {
		set[index] = values[index]
	}
	return ovsdb.OvsSet{GoSet: set}
}

func encodeUniqueStringSet(values []string, field string) (ovsdb.OvsSet, error) {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			return ovsdb.OvsSet{}, storeError(controlstore.ErrConflict, "%s contains duplicate value %q", field, value)
		}
		seen[value] = struct{}{}
	}
	return encodeStringSet(values), nil
}

func encodeStringMap(values map[string]string) ovsdb.OvsMap {
	result := make(map[interface{}]interface{}, len(values))
	for key, value := range values {
		result[key] = value
	}
	return ovsdb.OvsMap{GoMap: result}
}

func encodeIPRanges(values []model.IPRange) (ovsdb.OvsSet, error) {
	encoded := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		item, err := json.Marshal(value)
		if err != nil {
			return ovsdb.OvsSet{}, err
		}
		encodedValue := string(item)
		if _, duplicate := seen[encodedValue]; duplicate {
			return ovsdb.OvsSet{}, storeError(controlstore.ErrConflict, "allocation_pools contains duplicate range %s", encodedValue)
		}
		seen[encodedValue] = struct{}{}
		encoded = append(encoded, encodedValue)
	}
	return encodeStringSet(encoded), nil
}

func decodeIPRanges(row ovsdb.Row, column string) ([]model.IPRange, error) {
	values, err := rowStringSet(row, column)
	if err != nil {
		return nil, err
	}
	result := make([]model.IPRange, 0, len(values))
	for _, value := range values {
		var item model.IPRange
		if err := json.Unmarshal([]byte(value), &item); err != nil {
			return nil, fmt.Errorf("column %s contains malformed allocation pool: %w", column, err)
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Start == result[j].Start {
			return result[i].End < result[j].End
		}
		return result[i].Start < result[j].Start
	})
	return result, nil
}

func encodeFixedIPs(values []model.FixedIP, refs *snapshot) (ovsdb.OvsMap, error) {
	result := make(map[interface{}]interface{}, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value.SubnetID]; duplicate {
			return ovsdb.OvsMap{}, storeError(controlstore.ErrConflict, "fixed_ips contains subnet %q more than once", value.SubnetID)
		}
		seen[value.SubnetID] = struct{}{}
		entry, exists := refs.resources[model.KindSubnet][value.SubnetID]
		if !exists {
			return ovsdb.OvsMap{}, storeError(controlstore.ErrConflict, "fixed_ips.subnet_id references missing subnet %q", value.SubnetID)
		}
		result[ovsdb.UUID{GoUUID: entry.uuid}] = value.Address
	}
	return ovsdb.OvsMap{GoMap: result}, nil
}

func decodeFixedIPs(row ovsdb.Row, refs *snapshot) ([]model.FixedIP, error) {
	values, err := rowMap(row, "fixed_ips")
	if err != nil {
		return nil, err
	}
	result := make([]model.FixedIP, 0, len(values))
	for key, rawAddress := range values {
		uuid, err := valueUUID(key)
		if err != nil {
			return nil, fmt.Errorf("column fixed_ips: %w", err)
		}
		subnetID, exists := refs.uuidToID[model.KindSubnet][uuid]
		if !exists {
			return nil, fmt.Errorf("column fixed_ips references unknown subnet UUID %q", uuid)
		}
		address, ok := rawAddress.(string)
		if !ok {
			return nil, fmt.Errorf("column fixed_ips has a non-string address")
		}
		result = append(result, model.FixedIP{SubnetID: subnetID, Address: address})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SubnetID < result[j].SubnetID })
	return result, nil
}

func encodeReferenceSet(ids []string, kind model.Kind, field string, refs *snapshot) (ovsdb.OvsSet, error) {
	result := make([]interface{}, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			return ovsdb.OvsSet{}, storeError(controlstore.ErrConflict, "%s contains %s %q more than once", field, kind, id)
		}
		seen[id] = struct{}{}
		entry, exists := refs.resources[kind][id]
		if !exists {
			return ovsdb.OvsSet{}, storeError(controlstore.ErrConflict, "%s references missing %s %q", field, kind, id)
		}
		result = append(result, ovsdb.UUID{GoUUID: entry.uuid})
	}
	return ovsdb.OvsSet{GoSet: result}, nil
}

func decodeReferenceSet(row ovsdb.Row, column string, kind model.Kind, refs *snapshot) ([]string, error) {
	values, err := rowSet(row, column)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		uuid, err := valueUUID(value)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", column, err)
		}
		id, exists := refs.uuidToID[kind][uuid]
		if !exists {
			return nil, fmt.Errorf("column %s references unknown %s UUID %q", column, kind, uuid)
		}
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func rowError(table string, index int, err error) error {
	return fmt.Errorf("decode PVN control table %s row %d: %w", table, index, err)
}

func firstError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
