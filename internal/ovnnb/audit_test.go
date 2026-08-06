package ovnnb

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type managedAuditRunner struct {
	mu       sync.Mutex
	rows     managedAuditInventory
	calls    [][]string
	headings func([]string) []string
}

func newManagedAuditRunner() *managedAuditRunner {
	return &managedAuditRunner{rows: make(managedAuditInventory)}
}

func (runner *managedAuditRunner) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls = append(runner.calls, append([]string(nil), arguments...))

	var columns []string
	var table string
	for index, argument := range arguments {
		if strings.HasPrefix(argument, "--columns=") {
			columns = strings.Split(strings.TrimPrefix(argument, "--columns="), ",")
		}
		if argument == "list" && index+1 < len(arguments) {
			table = arguments[index+1]
		}
	}
	if table == "" || len(columns) == 0 {
		return nil, fmt.Errorf("managed audit issued a non-list command: %v", arguments)
	}
	headings := append([]string(nil), columns...)
	if runner.headings != nil {
		headings = runner.headings(headings)
	}

	rows := runner.rows[table]
	uuids := make([]string, 0, len(rows))
	for uuid := range rows {
		uuids = append(uuids, uuid)
	}
	sort.Strings(uuids)
	data := make([][]any, 0, len(uuids))
	for _, uuid := range uuids {
		row := rows[uuid]
		cells := make([]any, 0, len(headings))
		for _, heading := range headings {
			switch heading {
			case "_uuid":
				cells = append(cells, auditTestUUID(row.uuid))
			case "name":
				cells = append(cells, row.name)
			case "type":
				cells = append(cells, row.rowType)
			case "external_ids":
				cells = append(cells, auditTestMap(row.externalIDs))
			case "options":
				cells = append(cells, auditTestMap(row.options))
			default:
				cells = append(cells, auditTestUUIDSet(row.references[heading]))
			}
		}
		data = append(data, cells)
	}
	return json.Marshal(struct {
		Headings []string `json:"headings"`
		Data     [][]any  `json:"data"`
	}{Headings: headings, Data: data})
}

func (runner *managedAuditRunner) put(row *managedAuditRow) {
	if runner.rows[row.table] == nil {
		runner.rows[row.table] = make(map[string]*managedAuditRow)
	}
	runner.rows[row.table][row.uuid] = row
}

func auditTestUUID(uuid string) []any {
	return []any{"uuid", uuid}
}

func auditTestUUIDSet(uuids []string) []any {
	members := make([]any, 0, len(uuids))
	for _, uuid := range uuids {
		members = append(members, auditTestUUID(uuid))
	}
	return []any{"set", members}
}

func auditTestMap(values map[string]string) []any {
	keys := sortedAuditMapKeys(values)
	pairs := make([]any, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, []any{key, values[key]})
	}
	return []any{"map", pairs}
}

func auditTestCopyMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func seedManagedAuditPlan(t *testing.T, runner *managedAuditRunner, snapshot controlstore.ResourceSnapshot) (managedAuditPlan, map[string]string) {
	t.Helper()
	plan, err := buildManagedAuditPlan(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	actualByKey := make(map[string]string, len(plan.rows))
	for _, key := range sortedAuditMapKeys(plan.rows) {
		expected := plan.rows[key]
		uuid := deterministicUUID("restored-managed-audit:" + key)
		externalIDs := auditTestCopyMap(expected.requiredExternal)
		for identityKey, value := range expected.identity {
			externalIDs[identityKey] = value
		}
		runner.put(&managedAuditRow{
			table: expected.table, uuid: uuid, name: expected.name, rowType: expected.rowType,
			externalIDs: externalIDs, options: auditTestCopyMap(expected.requiredOptions), references: make(map[string][]string),
		})
		actualByKey[key] = uuid
	}
	for _, reference := range plan.references {
		childUUID := actualByKey[reference.childKey]
		if childUUID == "" {
			t.Fatalf("audit reference %q has absent child %q", reference.label, reference.childKey)
		}
		for _, parentKey := range reference.parentKeys {
			parent := plan.rows[parentKey]
			parentUUID := actualByKey[parentKey]
			if parentUUID == "" {
				t.Fatalf("audit reference %q has absent parent %q", reference.label, parentKey)
			}
			row := runner.rows[parent.table][parentUUID]
			row.references[reference.column] = append(row.references[reference.column], childUUID)
			sort.Strings(row.references[reference.column])
		}
	}
	return plan, actualByKey
}

func comprehensiveManagedAuditFixture(t *testing.T) (*controlstore.Memory, controlstore.ResourceSnapshot) {
	t.Helper()
	ctx := context.Background()
	store, fixture := newNorthSouthFixture(t)

	subnetUpdate := *fixture.internalSubnet
	subnetUpdate.EnableDHCP = true
	updatedSubnet, _, err := store.Update(ctx, &subnetUpdate, fixture.internalSubnet.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.internalSubnet = updatedSubnet.(*model.Subnet)
	group := mustCreate(t, store, &model.SecurityGroup{
		Metadata: model.Metadata{ID: "sg-audit"}, ProjectID: fixture.project.ID, Name: "audit",
	}).(*model.SecurityGroup)
	_ = mustCreate(t, store, &model.SecurityGroupRule{
		Metadata: model.Metadata{ID: "rule-audit"}, ProjectID: fixture.project.ID, SecurityGroupID: group.ID,
		Direction: model.DirectionIngress, EtherType: model.EtherTypeIPv4, Protocol: "tcp",
		PortRangeMin: 443, PortRangeMax: 443, RemoteCIDR: "0.0.0.0/0", Action: model.ActionAllow,
	})
	port := mustCreate(t, store, &model.Port{
		Metadata: model.Metadata{ID: "port-audit"}, ProjectID: fixture.project.ID, NetworkID: fixture.internalNetwork.ID,
		Name: "vm-audit-net0", LSPName: "pvn-port-audit", MACAddress: "02:00:00:00:00:55",
		FixedIPs:         []model.FixedIP{{SubnetID: fixture.internalSubnet.ID, Address: "10.42.0.55"}},
		SecurityGroupIDs: []string{group.ID}, AdminStateUp: true, BindingStatus: model.PortBound,
	}).(*model.Port)
	_ = mustCreate(t, store, &model.FloatingIP{
		Metadata: model.Metadata{ID: "fip-audit"}, ProjectID: fixture.project.ID,
		ProviderNetworkID: fixture.provider.ID, Address: "192.0.2.55", RouterID: fixture.router.ID,
		PortID: port.ID, FixedIPAddress: "10.42.0.55", FloatingStatus: model.FloatingIPActive,
	})

	snapshot, err := store.Snapshot(ctx, managedAuditControlKinds(), controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return store, snapshot
}

func TestAuditManagedGraphAcceptsCompleteRestoredGraphReadOnly(t *testing.T) {
	store, snapshot := comprehensiveManagedAuditFixture(t)
	runner := newManagedAuditRunner()
	_, _ = seedManagedAuditPlan(t, runner, snapshot)
	// Router-interface peer LSPs are currently unmarked renderer glue. They are
	// deliberately outside managed-row ownership until PVN gives them identity.
	runner.put(&managedAuditRow{
		table: "Logical_Switch_Port", uuid: deterministicUUID("unmarked-router-interface-peer"), name: "pvn-lsp-router-interface-1",
		externalIDs: map[string]string{}, options: map[string]string{}, references: map[string][]string{},
	})
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.AuditManagedGraph(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != len(managedAuditTables) {
		t.Fatalf("audit made %d calls, want one read for each of %d tables", len(runner.calls), len(managedAuditTables))
	}
	seen := make(map[string]bool, len(managedAuditTables))
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if !strings.Contains(joined, " list ") {
			t.Fatalf("audit issued a mutating/non-list command: %v", call)
		}
		for index, argument := range call {
			if argument == "list" && index+1 < len(call) {
				seen[call[index+1]] = true
			}
		}
	}
	for _, table := range managedAuditTables {
		if !seen[table.name] {
			t.Errorf("audit did not scan %s", table.name)
		}
	}
}

func TestAuditManagedGraphRejectsUnexpectedManagedTableRow(t *testing.T) {
	store, snapshot := comprehensiveManagedAuditFixture(t)
	runner := newManagedAuditRunner()
	_, _ = seedManagedAuditPlan(t, runner, snapshot)
	runner.put(&managedAuditRow{
		table: "Address_Set", uuid: deterministicUUID("orphan-address-set"),
		externalIDs: map[string]string{"pvn-managed": "true"}, options: map[string]string{}, references: map[string][]string{},
	})
	renderer := newTestRenderer(t, runner, store)

	err := renderer.AuditManagedGraph(context.Background())
	if err == nil || !strings.Contains(err.Error(), "managed Address_Set row") || !strings.Contains(err.Error(), "orphaned or has a malformed stable identity") {
		t.Fatalf("unexpected audit error: %v", err)
	}
}

func TestAuditManagedGraphRejectsDuplicateStableIdentity(t *testing.T) {
	store, snapshot := comprehensiveManagedAuditFixture(t)
	runner := newManagedAuditRunner()
	plan, actual := seedManagedAuditPlan(t, runner, snapshot)
	expected := plan.rows["network/internal-1"]
	original := runner.rows[expected.table][actual[expected.key]]
	runner.put(&managedAuditRow{
		table: expected.table, uuid: deterministicUUID("duplicate-network-identity"), name: original.name,
		externalIDs: auditTestCopyMap(original.externalIDs), options: map[string]string{}, references: map[string][]string{},
	})
	renderer := newTestRenderer(t, runner, store)

	err := renderer.AuditManagedGraph(context.Background())
	if err == nil || !strings.Contains(err.Error(), "network internal-1 has duplicate stable identities") {
		t.Fatalf("unexpected audit error: %v", err)
	}
}

func TestAuditManagedGraphRejectsNameConflict(t *testing.T) {
	store, snapshot := comprehensiveManagedAuditFixture(t)
	runner := newManagedAuditRunner()
	plan, _ := seedManagedAuditPlan(t, runner, snapshot)
	expected := plan.rows["network/internal-1"]
	runner.put(&managedAuditRow{
		table: expected.table, uuid: deterministicUUID("foreign-network-name"), name: expected.name,
		externalIDs: map[string]string{}, options: map[string]string{}, references: map[string][]string{},
	})
	renderer := newTestRenderer(t, runner, store)

	err := renderer.AuditManagedGraph(context.Background())
	if err == nil || !strings.Contains(err.Error(), "expected name") || !strings.Contains(err.Error(), "conflicts with UUIDs") {
		t.Fatalf("unexpected audit error: %v", err)
	}
}

func TestAuditManagedGraphRejectsParentConflict(t *testing.T) {
	store, snapshot := comprehensiveManagedAuditFixture(t)
	runner := newManagedAuditRunner()
	plan, actual := seedManagedAuditPlan(t, runner, snapshot)
	portUUID := actual["port/port-audit"]
	network := plan.rows["network/internal-1"]
	networkRow := runner.rows[network.table][actual[network.key]]
	networkRow.references["ports"] = removeAuditTestUUID(networkRow.references["ports"], portUUID)
	foreignUUID := deterministicUUID("foreign-parent-switch")
	runner.put(&managedAuditRow{
		table: "Logical_Switch", uuid: foreignUUID, name: "foreign-parent",
		externalIDs: map[string]string{}, options: map[string]string{}, references: map[string][]string{"ports": {portUUID}},
	})
	renderer := newTestRenderer(t, runner, store)

	err := renderer.AuditManagedGraph(context.Background())
	if err == nil || !strings.Contains(err.Error(), "port logical switch") || !strings.Contains(err.Error(), foreignUUID) {
		t.Fatalf("unexpected audit error: %v", err)
	}
}

func TestAuditManagedGraphRejectsManagedChildrenOnAlternateOVN711References(t *testing.T) {
	tests := []struct {
		name        string
		parentTable string
		column      string
		childTable  string
	}{
		{name: "logical switch ACL", parentTable: "Logical_Switch", column: "acls", childTable: "ACL"},
		{name: "IPv6 DHCP options", parentTable: "Logical_Switch_Port", column: "dhcpv6_options", childTable: "DHCP_Options"},
		{name: "NAT gateway port", parentTable: "NAT", column: "gateway_port", childTable: "Logical_Router_Port"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, snapshot := comprehensiveManagedAuditFixture(t)
			runner := newManagedAuditRunner()
			plan, actual := seedManagedAuditPlan(t, runner, snapshot)
			parentKey := firstManagedAuditPlanKey(t, plan, test.parentTable)
			childKey := firstManagedAuditPlanKey(t, plan, test.childTable)
			parent := plan.rows[parentKey]
			parentRow := runner.rows[parent.table][actual[parentKey]]
			parentRow.references[test.column] = append(parentRow.references[test.column], actual[childKey])
			renderer := newTestRenderer(t, runner, store)

			err := renderer.AuditManagedGraph(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.parentTable+"."+test.column) {
				t.Fatalf("unexpected alternate-reference audit error: %v", err)
			}
		})
	}
}

func TestAuditManagedGraphRejectsForeignChildrenOnEveryExactOVN711Reference(t *testing.T) {
	for _, reference := range managedAuditReferences {
		if !reference.exactParentChildren {
			continue
		}
		name := reference.parentTable + "." + reference.column
		t.Run(name, func(t *testing.T) {
			store, snapshot := comprehensiveManagedAuditFixture(t)
			runner := newManagedAuditRunner()
			plan, actual := seedManagedAuditPlan(t, runner, snapshot)
			parentKey := firstManagedAuditPlanKey(t, plan, reference.parentTable)
			parent := plan.rows[parentKey]
			parentRow := runner.rows[parent.table][actual[parentKey]]
			foreignUUID := deterministicUUID("foreign-reference:" + name)
			runner.put(&managedAuditRow{
				table: reference.childTable, uuid: foreignUUID, externalIDs: map[string]string{},
				options: map[string]string{}, references: map[string][]string{},
			})
			parentRow.references[reference.column] = append(parentRow.references[reference.column], foreignUUID)
			renderer := newTestRenderer(t, runner, store)

			err := renderer.AuditManagedGraph(context.Background())
			if err == nil || !strings.Contains(err.Error(), name+" children") || !strings.Contains(err.Error(), foreignUUID) {
				t.Fatalf("unexpected foreign-reference audit error: %v", err)
			}
		})
	}
}

func firstManagedAuditPlanKey(t *testing.T, plan managedAuditPlan, table string) string {
	t.Helper()
	for _, key := range sortedAuditMapKeys(plan.rows) {
		if plan.rows[key].table == table {
			return key
		}
	}
	t.Fatalf("managed audit fixture has no %s row", table)
	return ""
}

func removeAuditTestUUID(values []string, target string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

func TestDecodeManagedAuditTableAcceptsReorderedHeadings(t *testing.T) {
	uuid := deterministicUUID("reordered-headings")
	output, err := json.Marshal(struct {
		Headings []string `json:"headings"`
		Data     [][]any  `json:"data"`
	}{
		Headings: []string{"external_ids", "name", "_uuid", "ports"},
		Data: [][]any{{
			auditTestMap(map[string]string{"pvn-managed": "true"}), "switch", auditTestUUID(uuid), auditTestUUIDSet(nil),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := decodeManagedAuditTable(managedAuditTable{
		name: "Logical_Switch", columns: []string{"_uuid", "name", "external_ids", "ports"},
	}, output)
	if err != nil {
		t.Fatal(err)
	}
	if rows[uuid] == nil || rows[uuid].name != "switch" || rows[uuid].externalIDs["pvn-managed"] != "true" {
		t.Fatalf("unexpected decoded rows: %#v", rows)
	}
}

func TestDecodeManagedAuditTableRejectsDuplicateHeadings(t *testing.T) {
	output := []byte(`{"headings":["_uuid","_uuid"],"data":[]}`)
	_, err := decodeManagedAuditTable(managedAuditTable{name: "ACL", columns: []string{"_uuid", "external_ids"}}, output)
	if err == nil || !strings.Contains(err.Error(), "unexpected headings") {
		t.Fatalf("unexpected decode error: %v", err)
	}
}

func TestAuditManagedGraphRequiresConfiguration(t *testing.T) {
	var renderer *Renderer
	if err := renderer.AuditManagedGraph(context.Background()); err == nil {
		t.Fatal("nil renderer unexpectedly audited")
	}
	renderer = &Renderer{}
	if err := renderer.AuditManagedGraph(context.Background()); err == nil || !strings.Contains(err.Error(), "auditor is not configured") {
		t.Fatalf("unexpected configuration error: %v", err)
	}
}
