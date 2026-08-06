package ovnnb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

// managedAuditTable describes every OVN 25.03 Northbound table that can carry
// external_ids. Keeping the complete list here makes a pvn-managed marker in a
// table PVN does not understand a hard audit failure instead of an invisible
// object. The audit is deliberately read-only: v0.2.14 has no safe cluster
// identity marker with which to authorize a global prune.
type managedAuditTable struct {
	name    string
	columns []string
}

var managedAuditTables = []managedAuditTable{
	{name: "NB_Global", columns: []string{"_uuid", "external_ids"}},
	{name: "Sample_Collector", columns: []string{"_uuid", "external_ids"}},
	{name: "Copp", columns: []string{"_uuid", "external_ids"}},
	{name: "Logical_Switch", columns: []string{"_uuid", "name", "external_ids", "ports"}},
	{name: "Logical_Switch_Port", columns: []string{"_uuid", "name", "type", "external_ids", "options", "dhcpv4_options"}},
	{name: "Forwarding_Group", columns: []string{"_uuid", "external_ids"}},
	{name: "Address_Set", columns: []string{"_uuid", "external_ids"}},
	{name: "Port_Group", columns: []string{"_uuid", "name", "external_ids", "ports", "acls"}},
	{name: "Load_Balancer", columns: []string{"_uuid", "external_ids"}},
	{name: "Load_Balancer_Health_Check", columns: []string{"_uuid", "external_ids"}},
	{name: "ACL", columns: []string{"_uuid", "external_ids"}},
	{name: "QoS", columns: []string{"_uuid", "external_ids"}},
	{name: "Mirror", columns: []string{"_uuid", "external_ids"}},
	{name: "Meter", columns: []string{"_uuid", "external_ids"}},
	{name: "Meter_Band", columns: []string{"_uuid", "external_ids"}},
	{name: "Logical_Router", columns: []string{"_uuid", "name", "external_ids", "ports", "static_routes", "nat"}},
	{name: "Logical_Router_Port", columns: []string{"_uuid", "name", "external_ids"}},
	{name: "Logical_Router_Static_Route", columns: []string{"_uuid", "external_ids"}},
	{name: "Logical_Router_Policy", columns: []string{"_uuid", "external_ids"}},
	{name: "NAT", columns: []string{"_uuid", "external_ids"}},
	{name: "DHCP_Options", columns: []string{"_uuid", "external_ids"}},
	{name: "DHCP_Relay", columns: []string{"_uuid", "external_ids"}},
	{name: "Connection", columns: []string{"_uuid", "external_ids"}},
	{name: "DNS", columns: []string{"_uuid", "external_ids"}},
	{name: "SSL", columns: []string{"_uuid", "external_ids"}},
	{name: "Gateway_Chassis", columns: []string{"_uuid", "external_ids"}},
	{name: "HA_Chassis", columns: []string{"_uuid", "external_ids"}},
	{name: "HA_Chassis_Group", columns: []string{"_uuid", "external_ids"}},
	{name: "BFD", columns: []string{"_uuid", "external_ids"}},
	{name: "Chassis_Template_Var", columns: []string{"_uuid", "external_ids"}},
	{name: "Sampling_App", columns: []string{"_uuid", "external_ids"}},
}

type managedAuditRow struct {
	table       string
	uuid        string
	name        string
	rowType     string
	externalIDs map[string]string
	options     map[string]string
	references  map[string][]string
}

type managedAuditInventory map[string]map[string]*managedAuditRow

type managedExpectedRow struct {
	key              string
	label            string
	table            string
	preferredUUID    string
	name             string
	rowType          string
	identity         map[string]string
	requiredExternal map[string]string
	requiredOptions  map[string]string
}

type managedReferenceExpectation struct {
	label       string
	childKey    string
	parentTable string
	column      string
	parentKeys  []string
}

type managedAuditPlan struct {
	rows       map[string]managedExpectedRow
	references []managedReferenceExpectation
}

type managedDesiredIndex struct {
	networks       map[string]*model.Network
	subnets        map[string]*model.Subnet
	ports          map[string]*model.Port
	routers        map[string]*model.Router
	interfaces     map[string]*model.RouterInterface
	floatingIPs    map[string]*model.FloatingIP
	providers      map[string]*model.ProviderNetwork
	segments       map[string]*model.ProviderSegment
	groups         map[string]*model.SecurityGroup
	rules          map[string]*model.SecurityGroupRule
	resourceErrors []error
}

type managedTableJSON struct {
	Headings []string            `json:"headings"`
	Data     [][]json.RawMessage `json:"data"`
}

// AuditManagedGraph proves that every pvn-managed OVN Northbound row belongs
// to the current PVN Control snapshot and that every managed child has exactly
// its desired parents. It never mutates OVN. Recovery callers must still keep
// all managers and other writers frozen for the duration of this audit.
func (renderer *Renderer) AuditManagedGraph(ctx context.Context) error {
	if renderer == nil || renderer.client == nil || renderer.store == nil {
		return errors.New("OVN managed graph auditor is not configured")
	}
	kinds := managedAuditControlKinds()
	before, err := renderer.store.Snapshot(ctx, kinds, controlstore.ListOptions{})
	if err != nil {
		return fmt.Errorf("read PVN Control snapshot for OVN audit: %w", err)
	}
	digestBefore, err := managedSnapshotDigest(before)
	if err != nil {
		return fmt.Errorf("digest PVN Control snapshot for OVN audit: %w", err)
	}
	plan, err := buildManagedAuditPlan(before)
	if err != nil {
		return fmt.Errorf("build expected OVN managed graph: %w", err)
	}
	inventory, err := renderer.readManagedAuditInventory(ctx)
	if err != nil {
		return err
	}
	failures := auditManagedInventory(plan, inventory)
	after, snapshotErr := renderer.store.Snapshot(ctx, kinds, controlstore.ListOptions{})
	if snapshotErr != nil {
		failures = append(failures, fmt.Errorf("re-read PVN Control snapshot after OVN audit: %w", snapshotErr))
	} else if digestAfter, digestErr := managedSnapshotDigest(after); digestErr != nil {
		failures = append(failures, fmt.Errorf("digest post-audit PVN Control snapshot: %w", digestErr))
	} else if digestAfter != digestBefore {
		failures = append(failures, errors.New("PVN Control changed during the OVN managed graph audit"))
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("OVN managed graph audit failed: %s", limitedAuditFailures(failures))
}

func managedAuditControlKinds() []model.Kind {
	kinds := model.Kinds()
	result := make([]model.Kind, 0, len(kinds)-1)
	for _, kind := range kinds {
		if kind != model.KindOperation {
			result = append(result, kind)
		}
	}
	return result
}

func managedSnapshotDigest(snapshot controlstore.ResourceSnapshot) (string, error) {
	entries := make([]string, 0)
	for kind, resources := range snapshot {
		if kind == model.KindOperation {
			continue
		}
		for _, resource := range resources {
			if nilResource(resource) || resource.ResourceKind() != kind {
				return "", fmt.Errorf("snapshot kind %s contains invalid resource %T", kind, resource)
			}
			encoded, err := json.Marshal(resource)
			if err != nil {
				return "", fmt.Errorf("encode %s %q: %w", kind, resource.GetMetadata().ID, err)
			}
			entries = append(entries, kind.String()+"\x00"+resource.GetMetadata().ID+"\x00"+string(encoded))
		}
	}
	sort.Strings(entries)
	digest := sha256.Sum256([]byte(strings.Join(entries, "\x00")))
	return hex.EncodeToString(digest[:]), nil
}

func (renderer *Renderer) readManagedAuditInventory(ctx context.Context) (managedAuditInventory, error) {
	inventory := make(managedAuditInventory, len(managedAuditTables))
	for _, table := range managedAuditTables {
		arguments := []string{"--format=json", "--columns=" + strings.Join(table.columns, ","), "list", table.name}
		output, err := renderer.client.run(ctx, arguments...)
		if err != nil {
			return nil, fmt.Errorf("read OVN %s rows for managed graph audit: %w", table.name, err)
		}
		rows, err := decodeManagedAuditTable(table, output)
		if err != nil {
			return nil, fmt.Errorf("decode OVN %s rows for managed graph audit: %w", table.name, err)
		}
		inventory[table.name] = rows
	}
	return inventory, nil
}

func decodeManagedAuditTable(spec managedAuditTable, output []byte) (map[string]*managedAuditRow, error) {
	var table managedTableJSON
	if err := json.Unmarshal(output, &table); err != nil {
		return nil, err
	}
	if len(table.Headings) != len(spec.columns) {
		return nil, fmt.Errorf("unexpected headings %v", table.Headings)
	}
	wanted := make(map[string]bool, len(spec.columns))
	for _, column := range spec.columns {
		wanted[column] = true
	}
	seen := make(map[string]bool, len(table.Headings))
	for _, heading := range table.Headings {
		if !wanted[heading] || seen[heading] {
			return nil, fmt.Errorf("unexpected headings %v", table.Headings)
		}
		seen[heading] = true
	}
	rows := make(map[string]*managedAuditRow, len(table.Data))
	for index, cells := range table.Data {
		if len(cells) != len(table.Headings) {
			return nil, fmt.Errorf("row %d has %d cells for %d headings", index, len(cells), len(table.Headings))
		}
		row := &managedAuditRow{
			table: spec.name, externalIDs: make(map[string]string), options: make(map[string]string), references: make(map[string][]string),
		}
		for cellIndex, heading := range table.Headings {
			cell := cells[cellIndex]
			var err error
			switch heading {
			case "_uuid":
				row.uuid, err = decodeAuditUUID(cell)
			case "name":
				err = json.Unmarshal(cell, &row.name)
			case "type":
				err = json.Unmarshal(cell, &row.rowType)
			case "external_ids":
				row.externalIDs, err = decodeAuditStringMap(cell)
			case "options":
				row.options, err = decodeAuditStringMap(cell)
			default:
				row.references[heading], err = decodeAuditUUIDSet(cell)
			}
			if err != nil {
				return nil, fmt.Errorf("row %d column %s: %w", index, heading, err)
			}
		}
		if row.uuid == "" {
			return nil, fmt.Errorf("row %d has no UUID", index)
		}
		if _, duplicate := rows[row.uuid]; duplicate {
			return nil, fmt.Errorf("duplicate row UUID %q", row.uuid)
		}
		rows[row.uuid] = row
	}
	return rows, nil
}

func decodeAuditUUID(raw json.RawMessage) (string, error) {
	var value []json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || len(value) != 2 {
		return "", errors.New("expected OVSDB UUID atom")
	}
	var tag, uuid string
	if err := json.Unmarshal(value[0], &tag); err != nil || tag != "uuid" {
		return "", errors.New("expected OVSDB UUID atom")
	}
	if err := json.Unmarshal(value[1], &uuid); err != nil {
		return "", errors.New("expected OVSDB UUID string")
	}
	if err := safeUUID(uuid); err != nil {
		return "", err
	}
	return strings.ToLower(uuid), nil
}

func decodeAuditUUIDSet(raw json.RawMessage) ([]string, error) {
	if uuid, err := decodeAuditUUID(raw); err == nil {
		return []string{uuid}, nil
	}
	var value []json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || len(value) != 2 {
		return nil, errors.New("expected OVSDB UUID set")
	}
	var tag string
	if err := json.Unmarshal(value[0], &tag); err != nil || tag != "set" {
		return nil, errors.New("expected OVSDB UUID set")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(value[1], &items); err != nil {
		return nil, errors.New("expected OVSDB UUID set members")
	}
	seen := make(map[string]bool, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		uuid, err := decodeAuditUUID(item)
		if err != nil {
			return nil, err
		}
		if seen[uuid] {
			return nil, fmt.Errorf("duplicate UUID %q in set", uuid)
		}
		seen[uuid] = true
		result = append(result, uuid)
	}
	sort.Strings(result)
	return result, nil
}

func decodeAuditStringMap(raw json.RawMessage) (map[string]string, error) {
	var value []json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || len(value) != 2 {
		return nil, errors.New("expected OVSDB string map")
	}
	var tag string
	if err := json.Unmarshal(value[0], &tag); err != nil || tag != "map" {
		return nil, errors.New("expected OVSDB string map")
	}
	var pairs [][]json.RawMessage
	if err := json.Unmarshal(value[1], &pairs); err != nil {
		return nil, errors.New("expected OVSDB string map pairs")
	}
	result := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		if len(pair) != 2 {
			return nil, errors.New("expected OVSDB string map pair")
		}
		var key, item string
		if err := json.Unmarshal(pair[0], &key); err != nil {
			return nil, errors.New("expected OVSDB string map key")
		}
		if err := json.Unmarshal(pair[1], &item); err != nil {
			return nil, errors.New("expected OVSDB string map value")
		}
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("duplicate OVSDB map key %q", key)
		}
		result[key] = item
	}
	return result, nil
}

func buildManagedAuditPlan(snapshot controlstore.ResourceSnapshot) (managedAuditPlan, error) {
	index := indexManagedDesired(snapshot)
	if len(index.resourceErrors) != 0 {
		return managedAuditPlan{}, errors.Join(index.resourceErrors...)
	}
	plan := managedAuditPlan{rows: make(map[string]managedExpectedRow)}

	for _, id := range sortedAuditMapKeys(index.networks) {
		network := index.networks[id]
		key := "network/" + network.ID
		if err := plan.add(managedExpectedRow{
			key: key, label: "network " + network.ID, table: "Logical_Switch", preferredUUID: logicalSwitchUUID(network.ID), name: logicalSwitch(network.ID),
			identity:         auditIdentity(model.KindNetwork.String(), "pvn-id", network.ID),
			requiredExternal: auditResourceExternal(network, map[string]string{"pvn-project": network.ProjectID}),
		}); err != nil {
			return managedAuditPlan{}, err
		}
		if network.ProviderNetworkID == "" {
			continue
		}
		segment, err := index.defaultProviderSegment(network.ProviderNetworkID)
		if err != nil {
			return managedAuditPlan{}, fmt.Errorf("network %q provider port: %w", network.ID, err)
		}
		portKey := "provider-port/" + network.ID
		portName := "pvn-localnet-" + compact(network.ID)
		identity := auditIdentity(model.KindProviderSegment.String(), "pvn-network", network.ID)
		if err := plan.add(managedExpectedRow{
			key: portKey, label: "provider port for network " + network.ID, table: "Logical_Switch_Port", name: portName, rowType: "localnet", identity: identity,
			requiredExternal: auditResourceExternal(segment, map[string]string{"pvn-network": network.ID}),
			requiredOptions:  map[string]string{"network_name": segment.PhysicalNetwork},
		}); err != nil {
			return managedAuditPlan{}, err
		}
		plan.expectParents("provider port logical switch", portKey, "Logical_Switch", "ports", key)
		plan.expectParents("provider port security groups", portKey, "Port_Group", "ports")
	}

	for _, id := range sortedAuditMapKeys(index.subnets) {
		subnet := index.subnets[id]
		if !subnet.EnableDHCP {
			continue
		}
		if err := plan.add(managedExpectedRow{
			key: "dhcp/" + subnet.ID, label: "DHCP options " + subnet.ID, table: "DHCP_Options", preferredUUID: deterministicUUID("dhcp-options:" + subnet.ID),
			identity:         auditIdentity(model.KindSubnet.String(), "pvn-id", subnet.ID),
			requiredExternal: auditResourceExternal(subnet, map[string]string{"pvn-project": subnet.ProjectID}),
		}); err != nil {
			return managedAuditPlan{}, err
		}
	}

	for _, id := range sortedAuditMapKeys(index.groups) {
		group := index.groups[id]
		groupKey := "security-group/" + group.ID
		if err := plan.add(managedExpectedRow{
			key: groupKey, label: "security group " + group.ID, table: "Port_Group", preferredUUID: deterministicUUID("port-group:" + group.ID), name: portGroup(group.ID),
			identity:         auditIdentity(model.KindSecurityGroup.String(), "pvn-id", group.ID),
			requiredExternal: auditResourceExternal(group, map[string]string{"pvn-project": group.ProjectID}),
		}); err != nil {
			return managedAuditPlan{}, err
		}
		for _, owner := range []string{
			group.ID + ":dhcpv4-client",
			group.ID + ":dhcpv4-server",
			group.ID + ":default-drop:to-lport",
			group.ID + ":default-drop:from-lport",
		} {
			aclKey := "acl/" + owner
			if err := plan.add(managedExpectedRow{
				key: aclKey, label: "implicit ACL " + owner, table: "ACL", preferredUUID: deterministicUUID("acl:" + owner),
				identity:         map[string]string{"pvn-managed": "true", "pvn-owner": owner},
				requiredExternal: map[string]string{"pvn-managed": "true", "pvn-owner": owner, "pvn-revision": strconv.FormatInt(group.Revision, 10)},
			}); err != nil {
				return managedAuditPlan{}, err
			}
			plan.expectParents("implicit ACL port group", aclKey, "Port_Group", "acls", groupKey)
		}
	}

	for _, id := range sortedAuditMapKeys(index.rules) {
		rule := index.rules[id]
		if index.groups[rule.SecurityGroupID] == nil {
			return managedAuditPlan{}, fmt.Errorf("security group rule %q references absent group %q", rule.ID, rule.SecurityGroupID)
		}
		aclKey := "acl/" + rule.ID
		if err := plan.add(managedExpectedRow{
			key: aclKey, label: "security group rule ACL " + rule.ID, table: "ACL", preferredUUID: deterministicUUID("acl:" + rule.ID),
			identity:         map[string]string{"pvn-managed": "true", "pvn-owner": rule.ID},
			requiredExternal: map[string]string{"pvn-managed": "true", "pvn-owner": rule.ID, "pvn-revision": strconv.FormatInt(rule.Revision, 10)},
		}); err != nil {
			return managedAuditPlan{}, err
		}
		plan.expectParents("security group rule ACL port group", aclKey, "Port_Group", "acls", "security-group/"+rule.SecurityGroupID)
	}

	for _, id := range sortedAuditMapKeys(index.routers) {
		router := index.routers[id]
		routerKey := "router/" + router.ID
		if err := plan.add(managedExpectedRow{
			key: routerKey, label: "router " + router.ID, table: "Logical_Router", preferredUUID: logicalRouterUUID(router.ID), name: logicalRouter(router.ID),
			identity:         auditIdentity(model.KindRouter.String(), "pvn-id", router.ID),
			requiredExternal: auditResourceExternal(router, map[string]string{"pvn-project": router.ProjectID}),
		}); err != nil {
			return managedAuditPlan{}, err
		}
		if router.ExternalNetworkID == "" {
			continue
		}
		if index.networks[router.ExternalNetworkID] == nil {
			return managedAuditPlan{}, fmt.Errorf("router %q references absent external network %q", router.ID, router.ExternalNetworkID)
		}
		lrpKey := "gateway-lrp/" + router.ID
		lrpName := gatewayRouterPort(router.ID)
		lrpExtra := map[string]string{"pvn-project": router.ProjectID, "pvn-role": "external-gateway"}
		if err := plan.add(managedExpectedRow{
			key: lrpKey, label: "router gateway LRP " + router.ID, table: "Logical_Router_Port", name: lrpName,
			identity:         map[string]string{"pvn-managed": "true", "pvn-kind": model.KindRouter.String(), "pvn-id": router.ID, "pvn-role": "external-gateway"},
			requiredExternal: auditResourceExternal(router, lrpExtra),
		}); err != nil {
			return managedAuditPlan{}, err
		}
		plan.expectParents("router gateway LRP parent", lrpKey, "Logical_Router", "ports", routerKey)

		lspKey := "gateway-lsp/" + router.ID
		lspExtra := map[string]string{"pvn-network": router.ExternalNetworkID, "pvn-project": router.ProjectID, "pvn-role": "external-gateway"}
		if err := plan.add(managedExpectedRow{
			key: lspKey, label: "router gateway LSP " + router.ID, table: "Logical_Switch_Port", name: gatewaySwitchPort(router.ID), rowType: "router",
			identity:         map[string]string{"pvn-managed": "true", "pvn-kind": model.KindRouter.String(), "pvn-id": router.ID, "pvn-network": router.ExternalNetworkID, "pvn-role": "external-gateway"},
			requiredExternal: auditResourceExternal(router, lspExtra), requiredOptions: map[string]string{"router-port": lrpName},
		}); err != nil {
			return managedAuditPlan{}, err
		}
		plan.expectParents("router gateway LSP parent", lspKey, "Logical_Switch", "ports", "network/"+router.ExternalNetworkID)
		plan.expectParents("router gateway LSP security groups", lspKey, "Port_Group", "ports")

		routeKey := "default-route/" + router.ID
		if err := plan.add(managedExpectedRow{
			key: routeKey, label: "router default route " + router.ID, table: "Logical_Router_Static_Route", preferredUUID: routerDefaultRouteUUID(router.ID),
			identity:         map[string]string{"pvn-managed": "true", "pvn-kind": "router-default-route", "pvn-router": router.ID},
			requiredExternal: map[string]string{"pvn-managed": "true", "pvn-kind": "router-default-route", "pvn-router": router.ID, "pvn-revision": strconv.FormatInt(router.Revision, 10)},
		}); err != nil {
			return managedAuditPlan{}, err
		}
		plan.expectParents("router default route parent", routeKey, "Logical_Router", "static_routes", routerKey)
	}

	for _, id := range sortedAuditMapKeys(index.interfaces) {
		routerInterface := index.interfaces[id]
		if index.routers[routerInterface.RouterID] == nil {
			return managedAuditPlan{}, fmt.Errorf("router interface %q references absent router %q", routerInterface.ID, routerInterface.RouterID)
		}
		lrpKey := "router-interface-lrp/" + routerInterface.ID
		if err := plan.add(managedExpectedRow{
			key: lrpKey, label: "router interface LRP " + routerInterface.ID, table: "Logical_Router_Port", name: "pvn-lrp-" + compact(routerInterface.ID),
			identity:         auditIdentity(model.KindRouterInterface.String(), "pvn-id", routerInterface.ID),
			requiredExternal: auditResourceExternal(routerInterface, map[string]string{"pvn-project": routerInterface.ProjectID}),
		}); err != nil {
			return managedAuditPlan{}, err
		}
		plan.expectParents("router interface LRP parent", lrpKey, "Logical_Router", "ports", "router/"+routerInterface.RouterID)

		router := index.routers[routerInterface.RouterID]
		if router.EnableSNAT && router.ExternalNetworkID != "" {
			snatKey := "snat/" + router.ID + "/" + routerInterface.ID
			if err := plan.add(managedExpectedRow{
				key: snatKey, label: "router SNAT " + routerInterface.ID, table: "NAT", preferredUUID: routerSNATUUID(router.ID, routerInterface.ID),
				identity: map[string]string{"pvn-managed": "true", "pvn-kind": "router-snat", "pvn-router": router.ID, "pvn-router-interface": routerInterface.ID},
				requiredExternal: map[string]string{
					"pvn-managed": "true", "pvn-kind": "router-snat", "pvn-router": router.ID, "pvn-router-interface": routerInterface.ID,
					"pvn-revision": strconv.FormatInt(router.Revision, 10), "pvn-interface-revision": strconv.FormatInt(routerInterface.Revision, 10),
				},
			}); err != nil {
				return managedAuditPlan{}, err
			}
			plan.expectParents("router SNAT parent", snatKey, "Logical_Router", "nat", "router/"+router.ID)
		}
	}

	dhcpPorts := make(map[string][]string)
	for _, id := range sortedAuditMapKeys(index.ports) {
		port := index.ports[id]
		if index.networks[port.NetworkID] == nil {
			return managedAuditPlan{}, fmt.Errorf("port %q references absent network %q", port.ID, port.NetworkID)
		}
		portKey := "port/" + port.ID
		if err := plan.add(managedExpectedRow{
			key: portKey, label: "port " + port.ID, table: "Logical_Switch_Port", preferredUUID: deterministicUUID("logical-switch-port:" + port.ID), name: port.LSPName,
			identity:         auditIdentity(model.KindPort.String(), "pvn-id", port.ID),
			requiredExternal: auditResourceExternal(port, map[string]string{"pvn-project": port.ProjectID, "pvn-network": port.NetworkID}),
		}); err != nil {
			return managedAuditPlan{}, err
		}
		plan.expectParents("port logical switch", portKey, "Logical_Switch", "ports", "network/"+port.NetworkID)
		groupKeys := make([]string, 0, len(port.SecurityGroupIDs))
		for _, groupID := range port.SecurityGroupIDs {
			if index.groups[groupID] == nil {
				return managedAuditPlan{}, fmt.Errorf("port %q references absent security group %q", port.ID, groupID)
			}
			groupKeys = append(groupKeys, "security-group/"+groupID)
		}
		plan.expectParents("port security groups", portKey, "Port_Group", "ports", groupKeys...)
		for _, fixed := range port.FixedIPs {
			subnet := index.subnets[fixed.SubnetID]
			if subnet == nil {
				return managedAuditPlan{}, fmt.Errorf("port %q references absent subnet %q", port.ID, fixed.SubnetID)
			}
			if subnet.EnableDHCP {
				dhcpPorts["dhcp/"+subnet.ID] = append(dhcpPorts["dhcp/"+subnet.ID], portKey)
				break
			}
		}
	}
	for _, id := range sortedAuditMapKeys(index.subnets) {
		if !index.subnets[id].EnableDHCP {
			continue
		}
		key := "dhcp/" + id
		plan.expectParents("DHCP logical switch ports", key, "Logical_Switch_Port", "dhcpv4_options", dhcpPorts[key]...)
	}

	for _, id := range sortedAuditMapKeys(index.floatingIPs) {
		floatingIP := index.floatingIPs[id]
		if floatingIP.RouterID == "" || floatingIP.FixedIPAddress == "" {
			continue
		}
		if index.routers[floatingIP.RouterID] == nil {
			return managedAuditPlan{}, fmt.Errorf("floating IP %q references absent router %q", floatingIP.ID, floatingIP.RouterID)
		}
		key := "floating-ip/" + floatingIP.ID
		if err := plan.add(managedExpectedRow{
			key: key, label: "floating IP NAT " + floatingIP.ID, table: "NAT", preferredUUID: deterministicUUID("floating-ip-nat:" + floatingIP.ID),
			identity:         auditIdentity(model.KindFloatingIP.String(), "pvn-id", floatingIP.ID),
			requiredExternal: auditResourceExternal(floatingIP, map[string]string{"pvn-project": floatingIP.ProjectID}),
		}); err != nil {
			return managedAuditPlan{}, err
		}
		plan.expectParents("floating IP NAT parent", key, "Logical_Router", "nat", "router/"+floatingIP.RouterID)
	}

	return plan, nil
}

func indexManagedDesired(snapshot controlstore.ResourceSnapshot) managedDesiredIndex {
	index := managedDesiredIndex{
		networks: make(map[string]*model.Network), subnets: make(map[string]*model.Subnet), ports: make(map[string]*model.Port),
		routers: make(map[string]*model.Router), interfaces: make(map[string]*model.RouterInterface), floatingIPs: make(map[string]*model.FloatingIP),
		providers: make(map[string]*model.ProviderNetwork), segments: make(map[string]*model.ProviderSegment),
		groups: make(map[string]*model.SecurityGroup), rules: make(map[string]*model.SecurityGroupRule),
	}
	for kind, resources := range snapshot {
		for _, resource := range resources {
			if nilResource(resource) || resource.ResourceKind() != kind || resource.GetMetadata().ID == "" {
				index.resourceErrors = append(index.resourceErrors, fmt.Errorf("snapshot kind %s contains invalid resource %T", kind, resource))
				continue
			}
			id := resource.GetMetadata().ID
			var duplicate bool
			switch value := resource.(type) {
			case *model.Network:
				_, duplicate = index.networks[id]
				index.networks[id] = value
			case *model.Subnet:
				_, duplicate = index.subnets[id]
				index.subnets[id] = value
			case *model.Port:
				_, duplicate = index.ports[id]
				index.ports[id] = value
			case *model.Router:
				_, duplicate = index.routers[id]
				index.routers[id] = value
			case *model.RouterInterface:
				_, duplicate = index.interfaces[id]
				index.interfaces[id] = value
			case *model.FloatingIP:
				_, duplicate = index.floatingIPs[id]
				index.floatingIPs[id] = value
			case *model.ProviderNetwork:
				_, duplicate = index.providers[id]
				index.providers[id] = value
			case *model.ProviderSegment:
				_, duplicate = index.segments[id]
				index.segments[id] = value
			case *model.SecurityGroup:
				_, duplicate = index.groups[id]
				index.groups[id] = value
			case *model.SecurityGroupRule:
				_, duplicate = index.rules[id]
				index.rules[id] = value
			case *model.Project, *model.IPAllocation, *model.Node:
				continue
			default:
				index.resourceErrors = append(index.resourceErrors, fmt.Errorf("snapshot kind %s contains unexpected resource %T", kind, resource))
				continue
			}
			if duplicate {
				index.resourceErrors = append(index.resourceErrors, fmt.Errorf("snapshot contains duplicate %s ID %q", kind, id))
			}
		}
	}
	return index
}

func (index managedDesiredIndex) defaultProviderSegment(providerID string) (*model.ProviderSegment, error) {
	provider := index.providers[providerID]
	if provider == nil {
		return nil, fmt.Errorf("provider network %q is absent", providerID)
	}
	if provider.DefaultSegmentID != "" {
		segment := index.segments[provider.DefaultSegmentID]
		if segment == nil || segment.ProviderNetworkID != providerID {
			return nil, fmt.Errorf("default segment %q is absent or belongs to another provider network", provider.DefaultSegmentID)
		}
		return segment, nil
	}
	var selected *model.ProviderSegment
	for _, id := range sortedAuditMapKeys(index.segments) {
		segment := index.segments[id]
		if segment.ProviderNetworkID != providerID {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("provider network %q has multiple segments but no default", providerID)
		}
		selected = segment
	}
	if selected == nil {
		return nil, fmt.Errorf("provider network %q has no segment", providerID)
	}
	return selected, nil
}

func (plan *managedAuditPlan) add(row managedExpectedRow) error {
	if row.key == "" || row.label == "" || row.table == "" || len(row.identity) == 0 {
		return errors.New("managed audit row key, label, table, and identity are required")
	}
	if previous, exists := plan.rows[row.key]; exists {
		return fmt.Errorf("desired managed identity %q is shared by %s and %s", row.key, previous.label, row.label)
	}
	plan.rows[row.key] = row
	return nil
}

func (plan *managedAuditPlan) expectParents(label, childKey, parentTable, column string, parentKeys ...string) {
	keys := append([]string(nil), parentKeys...)
	sort.Strings(keys)
	unique := keys[:0]
	for _, key := range keys {
		if len(unique) == 0 || unique[len(unique)-1] != key {
			unique = append(unique, key)
		}
	}
	plan.references = append(plan.references, managedReferenceExpectation{
		label: label, childKey: childKey, parentTable: parentTable, column: column, parentKeys: unique,
	})
}

func auditIdentity(kind, key, value string) map[string]string {
	return map[string]string{"pvn-managed": "true", "pvn-kind": kind, key: value}
}

func auditResourceExternal(resource model.Resource, extra map[string]string) map[string]string {
	metadata := resource.GetMetadata()
	result := map[string]string{
		"pvn-managed": "true", "pvn-kind": resource.ResourceKind().String(), "pvn-id": metadata.ID,
		"pvn-revision": strconv.FormatInt(metadata.Revision, 10),
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func auditManagedInventory(plan managedAuditPlan, inventory managedAuditInventory) []error {
	failures := make([]error, 0)
	actualByKey := make(map[string]string, len(plan.rows))
	claimed := make(map[string]string, len(plan.rows))
	keys := make([]string, 0, len(plan.rows))
	for key := range plan.rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		expected := plan.rows[key]
		rows := inventory[expected.table]
		matches := make([]*managedAuditRow, 0, 1)
		for _, row := range rows {
			if auditMapContains(row.externalIDs, expected.identity) {
				matches = append(matches, row)
			}
		}
		sort.Slice(matches, func(i, j int) bool { return matches[i].uuid < matches[j].uuid })
		if len(matches) != 1 {
			if len(matches) == 0 {
				failures = append(failures, fmt.Errorf("%s is missing its exact stable identity", expected.label))
			} else {
				failures = append(failures, fmt.Errorf("%s has duplicate stable identities at UUIDs %v", expected.label, auditRowUUIDs(matches)))
			}
			continue
		}
		actual := matches[0]
		rowToken := expected.table + "\x00" + actual.uuid
		if previous, duplicate := claimed[rowToken]; duplicate {
			failures = append(failures, fmt.Errorf("managed %s row %q is claimed by both %s and %s", expected.table, actual.uuid, previous, expected.label))
			continue
		}
		claimed[rowToken] = expected.label
		actualByKey[key] = actual.uuid
		if expected.name != "" {
			if actual.name != expected.name {
				failures = append(failures, fmt.Errorf("%s UUID %q has name %q instead of %q", expected.label, actual.uuid, actual.name, expected.name))
			}
			named := auditRowsNamed(rows, expected.name)
			if len(named) != 1 || named[0].uuid != actual.uuid {
				failures = append(failures, fmt.Errorf("%s expected name %q conflicts with UUIDs %v", expected.label, expected.name, auditRowUUIDs(named)))
			}
		}
		if expected.preferredUUID != "" {
			preferred := rows[strings.ToLower(expected.preferredUUID)]
			if preferred != nil && preferred.uuid != actual.uuid {
				failures = append(failures, fmt.Errorf("%s UUID %q conflicts with deterministic UUID row %q", expected.label, actual.uuid, preferred.uuid))
			}
		}
		if expected.rowType != "" && actual.rowType != expected.rowType {
			failures = append(failures, fmt.Errorf("%s UUID %q has type %q instead of %q", expected.label, actual.uuid, actual.rowType, expected.rowType))
		}
		for externalKey, value := range expected.requiredExternal {
			if actual.externalIDs[externalKey] != value {
				failures = append(failures, fmt.Errorf("%s UUID %q has malformed external ID %s", expected.label, actual.uuid, externalKey))
			}
		}
		for option, value := range expected.requiredOptions {
			if actual.options[option] != value {
				failures = append(failures, fmt.Errorf("%s UUID %q has parent option %s=%q instead of %q", expected.label, actual.uuid, option, actual.options[option], value))
			}
		}
	}

	for _, table := range managedAuditTables {
		rows := inventory[table.name]
		uuids := make([]string, 0, len(rows))
		for uuid := range rows {
			uuids = append(uuids, uuid)
		}
		sort.Strings(uuids)
		for _, uuid := range uuids {
			row := rows[uuid]
			if row.externalIDs["pvn-managed"] != "true" {
				continue
			}
			if _, expected := claimed[table.name+"\x00"+uuid]; !expected {
				failures = append(failures, fmt.Errorf("managed %s row %q is orphaned or has a malformed stable identity", table.name, uuid))
			}
		}
	}

	for _, reference := range plan.references {
		childUUID := actualByKey[reference.childKey]
		if childUUID == "" {
			continue
		}
		expectedParents := make([]string, 0, len(reference.parentKeys))
		complete := true
		for _, parentKey := range reference.parentKeys {
			parentUUID := actualByKey[parentKey]
			if parentUUID == "" {
				complete = false
				break
			}
			expectedParents = append(expectedParents, parentUUID)
		}
		if !complete {
			continue
		}
		sort.Strings(expectedParents)
		actualParents := auditReferencingRows(inventory[reference.parentTable], reference.column, childUUID)
		if !equalAuditStrings(actualParents, expectedParents) {
			failures = append(failures, fmt.Errorf("%s for child UUID %q has parent UUIDs %v instead of %v", reference.label, childUUID, actualParents, expectedParents))
		}
	}
	return failures
}

func auditMapContains(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func auditRowsNamed(rows map[string]*managedAuditRow, name string) []*managedAuditRow {
	result := make([]*managedAuditRow, 0, 1)
	for _, row := range rows {
		if row.name == name {
			result = append(result, row)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].uuid < result[j].uuid })
	return result
}

func auditRowUUIDs(rows []*managedAuditRow) []string {
	result := make([]string, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.uuid)
	}
	sort.Strings(result)
	return result
}

func auditReferencingRows(rows map[string]*managedAuditRow, column, childUUID string) []string {
	result := make([]string, 0)
	for _, row := range rows {
		for _, reference := range row.references[column] {
			if reference == childUUID {
				result = append(result, row.uuid)
				break
			}
		}
	}
	sort.Strings(result)
	return result
}

func equalAuditStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func limitedAuditFailures(failures []error) string {
	const limit = 50
	count := len(failures)
	if count > limit {
		failures = failures[:limit]
	}
	messages := make([]string, 0, len(failures)+1)
	for _, failure := range failures {
		messages = append(messages, failure.Error())
	}
	if count > limit {
		messages = append(messages, fmt.Sprintf("and %d more", count-limit))
	}
	return strings.Join(messages, "; ")
}

func sortedAuditMapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
