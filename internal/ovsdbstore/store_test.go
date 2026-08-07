package ovsdbstore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/popododo0720/proxmox-ovn/internal/controlschema"
	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type fakeDatabase struct {
	mu       sync.Mutex
	rows     rawDatabase
	epoch    int64
	sequence int64
	loads    int
	lookups  int
	closed   bool
}

func newFakeDatabase() *fakeDatabase {
	rows := make(rawDatabase)
	for _, table := range allTables() {
		rows[table] = nil
	}
	return &fakeDatabase{rows: rows}
}

func (f *fakeDatabase) load(ctx context.Context) (rawDatabase, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errors.New("database closed")
	}
	f.loads++
	return cloneRawDatabase(f.rows), nil
}

func (f *fakeDatabase) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

func (f *fakeDatabase) lookupRuntimePorts(ctx context.Context, vmid int, nic string) (rawRuntimePortLookup, error) {
	if err := contextError(ctx); err != nil {
		return rawRuntimePortLookup{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return rawRuntimePortLookup{}, errors.New("database closed")
	}
	f.lookups++
	operations := runtimePortLookupOperations(vmid, nic)
	selectRows := func(rows []ovsdb.Row, operation ovsdb.Operation) ([]ovsdb.Row, error) {
		result := make([]ovsdb.Row, 0, len(rows))
		for _, row := range rows {
			if operation.Table == controlschema.PortTable {
				rowVMID, err := rowInt64(row, "vmid")
				if err != nil {
					return nil, err
				}
				rowNIC, err := rowString(row, "nic")
				if err != nil {
					return nil, err
				}
				if rowVMID != int64(vmid) || rowNIC != nic {
					continue
				}
			}
			selected := make(ovsdb.Row, len(operation.Columns))
			for _, column := range operation.Columns {
				if value, exists := row[column]; exists {
					selected[column] = cloneRow(ovsdb.Row{column: value})[column]
				}
			}
			result = append(result, selected)
		}
		return result, nil
	}
	nodes, err := selectRows(f.rows[operations[0].Table], operations[0])
	if err != nil {
		return rawRuntimePortLookup{}, err
	}
	ports, err := selectRows(f.rows[operations[1].Table], operations[1])
	if err != nil {
		return rawRuntimePortLookup{}, err
	}
	return rawRuntimePortLookup{nodes: nodes, ports: ports}, nil
}

func (f *fakeDatabase) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

func (f *fakeDatabase) initialize(ctx context.Context, row ovsdb.Row) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.epoch != 0 {
		return errSerialization
	}
	copyRow := cloneRow(row)
	copyRow["_uuid"] = f.nextUUID()
	f.rows[kindTables[model.KindOperation]] = append(f.rows[kindTables[model.KindOperation]], copyRow)
	f.epoch = 1
	return nil
}

func (f *fakeDatabase) commit(ctx context.Context, epoch int64, changes []change, updatedAt string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if epoch != f.epoch {
		return errSerialization
	}
	if !controlschema.Schema().ValidateOperations(buildOperations(epoch, changes, updatedAt)...) {
		return errors.New("transaction does not match the generated PVN_Control schema")
	}
	next := cloneRawDatabase(f.rows)
	for _, item := range changes {
		rows := next[item.table]
		index := findRow(rows, item.id)
		switch item.type_ {
		case changeInsert:
			if index >= 0 {
				return &constraintError{cause: errors.New("duplicate id")}
			}
			copyRow := cloneRow(item.row)
			copyRow["_uuid"] = f.nextUUID()
			next[item.table] = append(rows, copyRow)
		case changeUpdate:
			if index < 0 || mustRowRevision(rows[index]) != item.expectedRevision {
				return errSerialization
			}
			copyRow := cloneRow(item.row)
			copyRow["_uuid"] = rows[index]["_uuid"]
			next[item.table][index] = copyRow
		case changeDelete:
			if index < 0 || mustRowRevision(rows[index]) != item.expectedRevision {
				return errSerialization
			}
			next[item.table] = append(rows[:index:index], rows[index+1:]...)
		default:
			return fmt.Errorf("unknown change type %d", item.type_)
		}
	}
	operationTable := kindTables[model.KindOperation]
	lockIndex := findRow(next[operationTable], storeLockID)
	if lockIndex < 0 {
		return errors.New("store lock disappeared")
	}
	next[operationTable][lockIndex]["revision"] = epoch + 1
	next[operationTable][lockIndex]["updated_at"] = updatedAt
	f.rows = next
	f.epoch++
	return nil
}

func (f *fakeDatabase) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *fakeDatabase) nextUUID() ovsdb.UUID {
	f.sequence++
	return ovsdb.UUID{GoUUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.sequence)}
}

func findRow(rows []ovsdb.Row, id string) int {
	for index, row := range rows {
		if row["id"] == id {
			return index
		}
	}
	return -1
}

func mustRowRevision(row ovsdb.Row) int64 {
	value, err := rowInt64(row, "revision")
	if err != nil {
		panic(err)
	}
	return value
}

func cloneRawDatabase(source rawDatabase) rawDatabase {
	result := make(rawDatabase, len(source))
	for table, rows := range source {
		result[table] = make([]ovsdb.Row, len(rows))
		for index, row := range rows {
			result[table][index] = cloneRow(row)
		}
	}
	return result
}

func cloneRow(source ovsdb.Row) ovsdb.Row {
	result := make(ovsdb.Row, len(source))
	for key, value := range source {
		switch typed := value.(type) {
		case ovsdb.OvsSet:
			result[key] = ovsdb.OvsSet{GoSet: append([]interface{}(nil), typed.GoSet...)}
		case ovsdb.OvsMap:
			values := make(map[interface{}]interface{}, len(typed.GoMap))
			for mapKey, mapValue := range typed.GoMap {
				values[mapKey] = mapValue
			}
			result[key] = ovsdb.OvsMap{GoMap: values}
		default:
			result[key] = value
		}
	}
	return result
}

func deterministicStore(database database) *Store {
	var sequence atomic.Int64
	return newStore(database,
		WithClock(func() time.Time { return time.Date(2026, 8, 5, 1, 2, 3, 456, time.UTC) }),
		WithIDGenerator(func() string { return fmt.Sprintf("id-%03d", sequence.Add(1)) }),
	)
}

func mustCreate(t *testing.T, store controlstore.Store, resource model.Resource, key string) model.Resource {
	t.Helper()
	if port, ok := resource.(*model.Port); ok && len(port.SecurityGroupIDs) == 0 {
		const groupID = "00000000-0000-5000-8000-000000000001"
		if _, err := store.Get(context.Background(), model.KindSecurityGroup, groupID); errors.Is(err, controlstore.ErrNotFound) {
			if _, _, err := store.Create(context.Background(), &model.SecurityGroup{
				Metadata: model.Metadata{ID: groupID}, Name: "test-baseline",
			}, ""); err != nil {
				t.Fatalf("Create(test security group): %v", err)
			}
		}
		port.SecurityGroupIDs = []string{groupID}
	}
	created, replayed, err := store.Create(context.Background(), resource, key)
	if err != nil || replayed {
		t.Fatalf("Create(%s) replayed=%v: %v", resource.ResourceKind(), replayed, err)
	}
	return created
}

func TestStorePersistsEveryResourceKindAndFiltersInternalRows(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	subject := mustCreate(t, store, &model.ProviderNetwork{Name: "tenant"}, "subject-provider").(*model.ProviderNetwork)
	provider := mustCreate(t, store, &model.ProviderNetwork{Name: "public"}, "provider").(*model.ProviderNetwork)
	segment := mustCreate(t, store, &model.ProviderSegment{ProviderNetworkID: provider.ID, Name: "public-vlan", PhysicalNetwork: "physnet1", NetworkType: model.ProviderVLAN, VLANID: 100}, "segment").(*model.ProviderSegment)
	provider.DefaultSegmentID = segment.ID
	providerResource, _, err := store.Update(context.Background(), provider, provider.Revision, "provider-default")
	if err != nil {
		t.Fatal(err)
	}
	provider = providerResource.(*model.ProviderNetwork)
	external := mustCreate(t, store, &model.Network{Name: "external", External: true, ProviderNetworkID: provider.ID}, "external-network").(*model.Network)
	externalSubnet := mustCreate(t, store, &model.Subnet{NetworkID: external.ID, Name: "external-v4", CIDR: "198.51.100.0/24", GatewayIP: "198.51.100.1"}, "external-subnet").(*model.Subnet)
	network := mustCreate(t, store, &model.Network{Name: "private"}, "network").(*model.Network)
	subnet := mustCreate(t, store, &model.Subnet{NetworkID: network.ID, Name: "private-v4", CIDR: "10.10.0.0/24", EnableDHCP: true, DNSNameservers: []string{"1.1.1.1"}, AllocationPools: []model.IPRange{{Start: "10.10.0.10", End: "10.10.0.20"}}}, "subnet").(*model.Subnet)
	node := mustCreate(t, store, &model.Node{Name: "pve-a", ChassisID: "chassis-a", ManagementAddress: "192.0.2.10", Roles: []model.NodeRole{model.NodeRoleCompute, model.NodeRoleGateway}, Enabled: true}, "node").(*model.Node)
	group := mustCreate(t, store, &model.SecurityGroup{Name: "default"}, "group").(*model.SecurityGroup)
	remoteGroup := mustCreate(t, store, &model.SecurityGroup{Name: "web"}, "remote-group").(*model.SecurityGroup)
	rule := mustCreate(t, store, &model.SecurityGroupRule{SecurityGroupID: group.ID, Direction: model.DirectionIngress, Protocol: "tcp", PortRangeMin: 443, PortRangeMax: 443, RemoteGroupID: remoteGroup.ID}, "rule")
	port := mustCreate(t, store, &model.Port{NetworkID: network.ID, Name: "vm-port", MACAddress: "02:00:00:00:00:10", FixedIPs: []model.FixedIP{{SubnetID: subnet.ID, Address: "10.10.0.10"}}, SecurityGroupIDs: []string{group.ID}, AdminStateUp: true, NodeID: node.ID, VMID: 100, NIC: "net0", RequestedChassis: node.ChassisID}, "port").(*model.Port)
	allocation := mustCreate(t, store, &model.IPAllocation{SubnetID: subnet.ID, PortID: port.ID, Address: "10.10.0.10", State: model.IPAllocated}, "allocation")
	router := mustCreate(t, store, &model.Router{Name: "router", ExternalNetworkID: external.ID, ExternalSubnetID: externalSubnet.ID, ExternalIPAddress: "198.51.100.2", EnableSNAT: true}, "router").(*model.Router)
	interfaceResource := mustCreate(t, store, &model.RouterInterface{RouterID: router.ID, SubnetID: subnet.ID, PortID: port.ID}, "router-interface")
	floating := mustCreate(t, store, &model.FloatingIP{ProviderNetworkID: provider.ID, Address: "198.51.100.10", PortID: port.ID, FixedIPAddress: "10.10.0.10", RouterID: router.ID}, "floating").(*model.FloatingIP)
	if floating.FloatingStatus != model.FloatingIPDown || floating.State != model.ResourcePending {
		t.Fatalf("new floating IP state=%s status=%s", floating.State, floating.FloatingStatus)
	}
	realizedFloating, err := store.MarkReconciled(context.Background(), model.KindFloatingIP, floating.ID, floating.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	floating = realizedFloating.(*model.FloatingIP)
	if floating.FloatingStatus != model.FloatingIPActive || floating.State != model.ResourceReady {
		t.Fatalf("realized floating IP state=%s status=%s", floating.State, floating.FloatingStatus)
	}
	pendingFloating, _, err := store.Update(context.Background(), floating, floating.Revision, "floating-update")
	if err != nil {
		t.Fatal(err)
	}
	floating = pendingFloating.(*model.FloatingIP)
	if floating.FloatingStatus != model.FloatingIPDown || floating.State != model.ResourcePending {
		t.Fatalf("updated floating IP state=%s status=%s", floating.State, floating.FloatingStatus)
	}
	failedFloating, err := store.MarkReconciled(context.Background(), model.KindFloatingIP, floating.ID, floating.Revision, errors.New("OVN unavailable"))
	if err != nil {
		t.Fatal(err)
	}
	floating = failedFloating.(*model.FloatingIP)
	if floating.FloatingStatus != model.FloatingIPError || floating.State != model.ResourceError {
		t.Fatalf("failed floating IP state=%s status=%s", floating.State, floating.FloatingStatus)
	}
	realizedFloating, err = store.MarkReconciled(context.Background(), model.KindFloatingIP, floating.ID, floating.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	floating = realizedFloating.(*model.FloatingIP)
	operation := mustCreate(t, store, &model.Operation{Action: "bind", TargetKind: model.KindPort, TargetID: port.ID, TargetRevision: port.Revision}, "operation")
	if operation.(*model.Operation).IdempotencyKey != "operation" {
		t.Fatalf("operation idempotency key=%q", operation.(*model.Operation).IdempotencyKey)
	}

	created := []model.Resource{subject, provider, segment, external, externalSubnet, network, subnet, node, group, remoteGroup, rule, port, allocation, router, interfaceResource, floating, operation}
	seen := make(map[model.Kind]bool)
	for _, expected := range created {
		loaded, err := store.Get(context.Background(), expected.ResourceKind(), expected.GetMetadata().ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", expected.ResourceKind(), err)
		}
		if loaded.GetMetadata().Revision < 1 || loaded.GetMetadata().CreatedAt.IsZero() {
			t.Fatalf("invalid decoded metadata for %s: %#v", expected.ResourceKind(), loaded.GetMetadata())
		}
		seen[expected.ResourceKind()] = true
	}
	for _, kind := range model.Kinds() {
		if !seen[kind] {
			t.Fatalf("test topology omitted kind %s", kind)
		}
	}
	operations, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].GetMetadata().ID != operation.GetMetadata().ID {
		t.Fatalf("internal lock/idempotency rows leaked into Operation list: %#v", operations)
	}
	ports, err := store.List(context.Background(), model.KindPort, controlstore.ListOptions{NetworkID: network.ID, NodeID: node.ID, VMID: 100, NIC: "net0"})
	if err != nil || len(ports) != 1 || ports[0].GetMetadata().ID != port.ID {
		t.Fatalf("filtered ports=%#v err=%v", ports, err)
	}
}

func TestStoreLookupRuntimePortsUsesTargetedReadAndResolvesAliases(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	network := mustCreate(t, store, &model.Network{Name: "private"}, "runtime-network").(*model.Network)
	node := mustCreate(t, store, &model.Node{
		Metadata: model.Metadata{ID: "node-a"}, Name: "pve-a", ChassisID: "chassis-a", Enabled: true,
	}, "runtime-node").(*model.Node)
	otherNode := mustCreate(t, store, &model.Node{
		Metadata: model.Metadata{ID: "node-b"}, Name: "pve-b", ChassisID: "chassis-b", Enabled: true,
	}, "runtime-other-node").(*model.Node)
	port := mustCreate(t, store, &model.Port{
		NetworkID: network.ID, Name: "vm-100-net0", MACAddress: "02:00:00:00:00:0a",
		AdminStateUp: true, BindingStatus: model.PortBinding, NodeID: node.ID, VMID: 100, NIC: "net0",
		LSPName: "lsp-a", Generation: 9, RequestedChassis: node.ChassisID,
	}, "runtime-port").(*model.Port)
	mustCreate(t, store, &model.Port{
		NetworkID: network.ID, Name: "vm-100-net0-other-node", MACAddress: "02:00:00:00:00:0b",
		AdminStateUp: true, BindingStatus: model.PortBinding, NodeID: otherNode.ID, VMID: 100, NIC: "net0",
		LSPName: "lsp-other", Generation: 2, RequestedChassis: otherNode.ChassisID,
	}, "runtime-other-port")

	loadsBefore := database.loadCount()
	for _, identity := range []string{node.ID, node.Name, node.ChassisID} {
		matches, err := store.LookupRuntimePorts(context.Background(), identity, 100, "net0")
		if err != nil || len(matches) != 1 {
			t.Fatalf("LookupRuntimePorts(%q) matches=%#v err=%v", identity, matches, err)
		}
		match := matches[0]
		if match.ID != port.ID || match.NodeID != node.ID || match.MACAddress != port.MACAddress ||
			match.LSPName != port.LSPName || match.Generation != port.Generation || match.BindingStatus != port.BindingStatus {
			t.Fatalf("targeted runtime port=%#v", match)
		}
	}
	if database.loadCount() != loadsBefore || database.lookupCount() != 3 {
		t.Fatalf("runtime lookup reloaded full database: loads before=%d after=%d targeted=%d", loadsBefore, database.loadCount(), database.lookupCount())
	}
}

func TestRuntimePortLookupOperationsAreTargeted(t *testing.T) {
	operations := runtimePortLookupOperations(100, "net0")
	if len(operations) != 2 || !controlschema.Schema().ValidateOperations(operations...) {
		t.Fatalf("invalid runtime lookup operations: %#v", operations)
	}
	for _, operation := range operations {
		if operation.Op != ovsdb.OperationSelect {
			t.Fatalf("runtime lookup operation is not select: %#v", operation)
		}
	}
	if len(operations[0].Columns) != 4 || len(operations[1].Where) != 2 {
		t.Fatalf("runtime lookup is not narrowly projected/filtered: %#v", operations)
	}
	portColumns := make(map[string]bool, len(operations[1].Columns))
	for _, column := range operations[1].Columns {
		portColumns[column] = true
	}
	for _, required := range []string{
		"id", "node", "revision", "applied_revision", "state", "binding_status",
		"lsp_name", "generation", "mac_address", "admin_state_up", "requested_chassis", "vmid", "nic",
	} {
		if !portColumns[required] {
			t.Fatalf("runtime port select omitted %q: %v", required, operations[1].Columns)
		}
	}
	for _, unrelated := range []string{"fixed_ips", "security_groups", "external_ids", "created_at", "updated_at"} {
		if portColumns[unrelated] {
			t.Fatalf("runtime port select includes unrelated %q: %v", unrelated, operations[1].Columns)
		}
	}
}

func TestOperationRequiresAnIdempotencyKey(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	_, _, err := store.Create(context.Background(), &model.Operation{Action: "render", TargetKind: model.KindProviderNetwork, TargetID: "provider-a", TargetRevision: 1}, "")
	if err == nil {
		t.Fatal("operation without an idempotency key was accepted")
	}
	mustCreate(t, store, &model.Operation{Action: "render", TargetKind: model.KindProviderNetwork, TargetID: "provider-a", TargetRevision: 1, IdempotencyKey: "operation-row-key"}, "request-a")
	_, _, err = store.Create(context.Background(), &model.Operation{Action: "delete", TargetKind: model.KindProviderNetwork, TargetID: "provider-b", TargetRevision: 1, IdempotencyKey: "operation-row-key"}, "request-b")
	if !errors.Is(err, controlstore.ErrAlreadyExists) {
		t.Fatalf("duplicate Operation idempotency key error=%v", err)
	}
	_, _, err = store.Create(context.Background(), &model.Operation{Action: "delete", TargetKind: model.KindProviderNetwork, TargetID: "provider-a", TargetRevision: 1, IdempotencyKey: "operation-other-key"}, "request-c")
	if !errors.Is(err, controlstore.ErrAlreadyExists) {
		t.Fatalf("duplicate Operation target error=%v", err)
	}
}

func TestStoreObserveNodeHeartbeatDoesNotChangeDesiredMetadata(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	node := mustCreate(t, store, &model.Node{Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "node").(*model.Node)
	readyResource, err := store.MarkReconciled(context.Background(), model.KindNode, node.ID, node.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready := readyResource.(*model.Node)
	observedAt := ready.UpdatedAt.Add(time.Minute)
	observed, err := store.ObserveNodeHeartbeat(context.Background(), node.ID, node.Revision, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Revision != ready.Revision || observed.AppliedRevision != ready.AppliedRevision || observed.State != ready.State || !observed.UpdatedAt.Equal(ready.UpdatedAt) {
		t.Fatalf("observation changed desired metadata: ready=%#v observed=%#v", ready.Metadata, observed.Metadata)
	}
	if observed.LastSeenAt == nil || !observed.LastSeenAt.Equal(observedAt) {
		t.Fatalf("last_seen_at=%v want %v", observed.LastSeenAt, observedAt)
	}
	loaded, err := store.Get(context.Background(), model.KindNode, node.ID)
	if err != nil || loaded.GetMetadata().Revision != node.Revision {
		t.Fatalf("persisted observed node=%#v err=%v", loaded, err)
	}
	if _, err := store.ObserveNodeHeartbeat(context.Background(), node.ID, node.Revision+1, observedAt.Add(time.Minute)); !errors.Is(err, controlstore.ErrPrecondition) {
		t.Fatalf("stale desired revision error=%v", err)
	}
}

func TestStoreListRecentFirstAndLimit(t *testing.T) {
	database := newFakeDatabase()
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	var sequence atomic.Int64
	store := newStore(database,
		WithClock(func() time.Time { return now }),
		WithIDGenerator(func() string { return fmt.Sprintf("operation-%d", sequence.Add(1)) }),
	)
	for revision := int64(1); revision <= 4; revision++ {
		mustCreate(t, store, &model.Operation{Action: "bind", TargetKind: model.KindPort, TargetID: "port-a", TargetRevision: revision}, fmt.Sprintf("operation-%d", revision))
		now = now.Add(time.Second)
	}
	resources, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{RecentFirst: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 || resources[0].GetMetadata().ID != "operation-4" || resources[1].GetMetadata().ID != "operation-3" {
		t.Fatalf("recent limited operations=%#v", resources)
	}
	if _, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{Limit: -1}); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("negative limit error=%v", err)
	}
}

func TestStoreSnapshotLoadsDatabaseOnceAndClonesAllKinds(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	network := mustCreate(t, store, &model.Network{Name: "private"}, "network")
	provider := mustCreate(t, store, &model.ProviderNetwork{Name: "provider"}, "provider")
	before := database.loadCount()
	snapshot, err := store.Snapshot(context.Background(), []model.Kind{model.KindNetwork, model.KindProviderNetwork, model.KindOperation, model.KindNetwork}, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if loads := database.loadCount() - before; loads != 1 {
		t.Fatalf("Snapshot() database loads=%d want 1", loads)
	}
	if len(snapshot) != 3 || len(snapshot[model.KindNetwork]) != 1 || snapshot[model.KindNetwork][0].GetMetadata().ID != network.GetMetadata().ID {
		t.Fatalf("network snapshot=%#v", snapshot[model.KindNetwork])
	}
	if len(snapshot[model.KindProviderNetwork]) != 1 || snapshot[model.KindProviderNetwork][0].GetMetadata().ID != provider.GetMetadata().ID {
		t.Fatalf("provider snapshot=%#v", snapshot[model.KindProviderNetwork])
	}
	if len(snapshot[model.KindOperation]) != 0 {
		t.Fatalf("internal operation rows leaked into snapshot: %#v", snapshot[model.KindOperation])
	}
	snapshot[model.KindNetwork][0].(*model.Network).Name = "caller-mutated"
	loaded, err := store.Get(context.Background(), model.KindNetwork, network.GetMetadata().ID)
	if err != nil || loaded.(*model.Network).Name != "private" {
		t.Fatalf("snapshot mutation leaked into store: network=%#v err=%v", loaded, err)
	}
}

func TestStorePrunesOperationAndDurableReplayTokenTogether(t *testing.T) {
	database := newFakeDatabase()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	store := newStore(database, WithClock(func() time.Time { return now }))
	subject := mustCreate(t, store, &model.ProviderNetwork{Name: "tenant"}, "subject-provider").(*model.ProviderNetwork)
	for revision := int64(2); revision <= 4; revision++ {
		subject.Description = fmt.Sprintf("revision-%d", revision)
		updated, _, err := store.Update(context.Background(), subject, subject.Revision, fmt.Sprintf("provider-%d", revision))
		if err != nil {
			t.Fatal(err)
		}
		subject = updated.(*model.ProviderNetwork)
	}
	for revision := int64(1); revision <= 3; revision++ {
		now = now.Add(time.Hour)
		key := fmt.Sprintf("reconcile:%s:%d", subject.ID, revision)
		operation := mustCreate(t, store, &model.Operation{
			Action: "reconcile", TargetKind: model.KindProviderNetwork, TargetID: subject.ID, TargetRevision: revision,
			OperationStatus: model.OperationQueued,
		}, key).(*model.Operation)
		completed := now
		operation.CompletedAt = &completed
		operation.OperationStatus = model.OperationSucceeded
		if _, _, err := store.Update(context.Background(), operation, operation.Revision, ""); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(48 * time.Hour)
	database.mu.Lock()
	rowsBefore := len(database.rows[kindTables[model.KindOperation]])
	database.mu.Unlock()
	pruned, err := store.PruneOperations(context.Background(), now.Add(-24*time.Hour), 1)
	if err != nil || pruned != 2 {
		t.Fatalf("PruneOperations() pruned=%d err=%v", pruned, err)
	}
	operations, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil || len(operations) != 1 || operations[0].(*model.Operation).TargetRevision != 3 {
		t.Fatalf("retained operations=%#v err=%v", operations, err)
	}
	database.mu.Lock()
	operationRows := cloneRawDatabase(database.rows)[kindTables[model.KindOperation]]
	database.mu.Unlock()
	// Each pruned public operation must remove its paired durable idempotency
	// row; unrelated resource replay tokens remain untouched.
	if len(operationRows) != rowsBefore-4 {
		t.Fatalf("Operation table rows=%d want %d", len(operationRows), rowsBefore-4)
	}
}

func TestStoreIdempotencyIsDurableAcrossManagers(t *testing.T) {
	database := newFakeDatabase()
	first := deterministicStore(database)
	second := deterministicStore(database)
	request := &model.ProviderNetwork{Name: "tenant"}
	created, replayed, err := first.Create(context.Background(), request, "same-request")
	if err != nil || replayed {
		t.Fatalf("first Create replayed=%v err=%v", replayed, err)
	}
	replayedResource, replayed, err := second.Create(context.Background(), request, "same-request")
	if err != nil || !replayed || replayedResource.GetMetadata().ID != created.GetMetadata().ID {
		t.Fatalf("second Create resource=%#v replayed=%v err=%v", replayedResource, replayed, err)
	}
	_, _, err = second.Create(context.Background(), &model.ProviderNetwork{Name: "other"}, "same-request")
	if !errors.Is(err, controlstore.ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict error=%v", err)
	}
}

func TestStoreOptimisticLifecycleAndDeleteReplay(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	created := mustCreate(t, store, &model.ProviderNetwork{Name: "tenant"}, "create").(*model.ProviderNetwork)
	created.Description = "updated"
	updatedResource, replayed, err := store.Update(context.Background(), created, 1, "update")
	if err != nil || replayed {
		t.Fatalf("Update replayed=%v err=%v", replayed, err)
	}
	updated := updatedResource.(*model.ProviderNetwork)
	if updated.Revision != 2 || updated.Description != "updated" {
		t.Fatalf("updated=%#v", updated)
	}
	if _, _, err := store.Update(context.Background(), created, 1, "stale"); !errors.Is(err, controlstore.ErrPrecondition) {
		t.Fatalf("stale Update error=%v", err)
	}
	failed, err := store.MarkReconciled(context.Background(), model.KindProviderNetwork, updated.ID, 2, errors.New("OVN unavailable"))
	if err != nil || failed.GetMetadata().State != model.ResourceError {
		t.Fatalf("failed reconcile=%#v err=%v", failed, err)
	}
	ready, err := store.MarkReconciled(context.Background(), model.KindProviderNetwork, updated.ID, 2, nil)
	if err != nil || ready.GetMetadata().State != model.ResourceReady || ready.GetMetadata().AppliedRevision != 2 {
		t.Fatalf("ready reconcile=%#v err=%v", ready, err)
	}
	replayed, err = store.Delete(context.Background(), model.KindProviderNetwork, updated.ID, 2, "delete")
	if err != nil || replayed {
		t.Fatalf("Delete replayed=%v err=%v", replayed, err)
	}
	replayed, err = store.Delete(context.Background(), model.KindProviderNetwork, updated.ID, 2, "delete")
	if err != nil || !replayed {
		t.Fatalf("Delete replay replayed=%v err=%v", replayed, err)
	}
	if _, err := store.Get(context.Background(), model.KindProviderNetwork, updated.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("Get after Delete error=%v", err)
	}
}

func TestStoreReconcileClaimFencesPurgeAndRecoversExpiredLease(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	subject := mustCreate(t, store, &model.ProviderNetwork{Name: "tenant"}, "create-fenced-provider").(*model.ProviderNetwork)
	operation := mustCreate(t, store, &model.Operation{
		Action:          "reconcile",
		TargetKind:      model.KindProviderNetwork,
		TargetID:        subject.ID,
		TargetRevision:  subject.Revision,
		OperationStatus: model.OperationQueued,
	}, "reconcile-fenced-provider").(*model.Operation)
	started := time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-ovsdb", started, started.Add(-2*time.Minute))
	if err != nil || claimed.OperationStatus != model.OperationRunning || claimed.StartedAt == nil || !claimed.StartedAt.Equal(started) {
		t.Fatalf("ClaimReconcile() operation=%#v err=%v", claimed, err)
	}
	if _, err := store.RenewOperationLease(context.Background(), claimed.ID, claimed.Revision, "lease-other", started.Add(time.Second)); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("wrong-owner RenewOperationLease() error=%v", err)
	}
	renewedAt := started.Add(time.Minute)
	claimed, err = store.RenewOperationLease(context.Background(), claimed.ID, claimed.Revision, "lease-ovsdb", renewedAt)
	if err != nil || claimed.Revision != operation.Revision+2 || claimed.StartedAt == nil || !claimed.StartedAt.Equal(started) || !claimed.UpdatedAt.Equal(renewedAt) {
		t.Fatalf("RenewOperationLease() operation=%#v err=%v", claimed, err)
	}
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProviderNetwork, subject.ID, subject.Revision, "delete-fenced-provider")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProviderNetwork, subject.ID, tombstone.GetMetadata().Revision); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Purge() error=%v, want reconcile fence", err)
	}
	active, recovered, err := store.FenceReconciles(context.Background(), model.KindProviderNetwork, subject.ID, renewedAt.Add(-time.Minute), renewedAt.Add(time.Minute))
	if err != nil || !active || recovered {
		t.Fatalf("live FenceReconciles() active=%v recovered=%v err=%v", active, recovered, err)
	}
	active, recovered, err = store.FenceReconciles(context.Background(), model.KindProviderNetwork, subject.ID, renewedAt.Add(time.Second), renewedAt.Add(3*time.Minute))
	if err != nil || active || !recovered {
		t.Fatalf("expired FenceReconciles() active=%v recovered=%v err=%v", active, recovered, err)
	}
	stored, err := store.Get(context.Background(), model.KindOperation, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := stored.(*model.Operation)
	if failed.OperationStatus != model.OperationFailed || failed.CompletedAt == nil || failed.Error == "" {
		t.Fatalf("expired operation=%#v", failed)
	}
	if err := store.Purge(context.Background(), model.KindProviderNetwork, subject.ID, tombstone.GetMetadata().Revision); err != nil {
		t.Fatal(err)
	}
}

func TestStorePurgeRejectsReferenceCreatedAfterBeginDelete(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	provider := mustCreate(t, store, &model.ProviderNetwork{Name: "provider-race"}, "create-race").(*model.ProviderNetwork)
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProviderNetwork, provider.ID, provider.Revision, "delete-race")
	if err != nil {
		t.Fatal(err)
	}
	segment := mustCreate(t, store, &model.ProviderSegment{
		ProviderNetworkID: provider.ID, Name: "late-reference", PhysicalNetwork: "phys-late", NetworkType: model.ProviderFlat,
	}, "late-reference").(*model.ProviderSegment)
	if err := store.Purge(context.Background(), model.KindProviderNetwork, provider.ID, tombstone.GetMetadata().Revision); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Purge() error=%v want late-reference conflict", err)
	}
	if _, err := store.Delete(context.Background(), model.KindProviderSegment, segment.ID, segment.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProviderNetwork, provider.ID, tombstone.GetMetadata().Revision); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRecoverExpiredOperationsIsBoundedAndIncludesSupersededTargets(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	base := time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC)
	createRunningReconcile := func(name string, started time.Time) (*model.ProviderNetwork, *model.Operation) {
		subject := mustCreate(t, store, &model.ProviderNetwork{Name: name}, "provider-"+name).(*model.ProviderNetwork)
		operation := mustCreate(t, store, &model.Operation{
			Action: "reconcile", TargetKind: model.KindProviderNetwork, TargetID: subject.ID,
			TargetRevision: subject.Revision, OperationStatus: model.OperationQueued,
		}, "operation-"+name).(*model.Operation)
		claimed, err := store.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-"+name, started, started.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return subject, claimed
	}

	supersededSubject, superseded := createRunningReconcile("superseded", base)
	supersededSubject.Description = "new desired revision"
	if _, _, err := store.Update(context.Background(), supersededSubject, supersededSubject.Revision, "supersede-provider"); err != nil {
		t.Fatal(err)
	}
	_, current := createRunningReconcile("current", base.Add(time.Minute))

	deleteSubject := mustCreate(t, store, &model.ProviderNetwork{Name: "deleting"}, "provider-deleting").(*model.ProviderNetwork)
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProviderNetwork, deleteSubject.ID, deleteSubject.Revision, "begin-delete")
	if err != nil {
		t.Fatal(err)
	}
	deleteOperation := mustCreate(t, store, &model.Operation{
		Action: "delete", TargetKind: model.KindProviderNetwork, TargetID: deleteSubject.ID,
		TargetRevision: tombstone.GetMetadata().Revision, OperationStatus: model.OperationQueued,
	}, "operation-delete").(*model.Operation)
	deleting, err := store.ClaimDelete(context.Background(), deleteOperation.ID, deleteOperation.Revision, "lease-delete", base.Add(2*time.Minute), base)
	if err != nil {
		t.Fatal(err)
	}
	liveSubject, live := createRunningReconcile("live", base.Add(2*time.Hour))
	liveSubject.Description = "superseded while lease remains live"
	if _, _, err := store.Update(context.Background(), liveSubject, liveSubject.Revision, "supersede-live-provider"); err != nil {
		t.Fatal(err)
	}

	recoveredAt := base.Add(3 * time.Hour)
	recovered, err := store.RecoverExpiredOperations(context.Background(), base.Add(time.Hour), recoveredAt, 2)
	if err != nil || recovered != 2 {
		t.Fatalf("first recovery count=%d err=%v", recovered, err)
	}
	for _, operationID := range []string{superseded.ID, current.ID} {
		resource, getErr := store.Get(context.Background(), model.KindOperation, operationID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		operation := resource.(*model.Operation)
		if operation.OperationStatus != model.OperationFailed || operation.CompletedAt == nil || !operation.CompletedAt.Equal(recoveredAt) || operation.Error == "" {
			t.Fatalf("recovered operation=%#v", operation)
		}
	}
	for _, operationID := range []string{deleting.ID, live.ID} {
		resource, getErr := store.Get(context.Background(), model.KindOperation, operationID)
		if getErr != nil || resource.(*model.Operation).OperationStatus != model.OperationRunning {
			t.Fatalf("unrecovered operation=%#v err=%v", resource, getErr)
		}
	}
	recovered, err = store.RecoverExpiredOperations(context.Background(), base.Add(time.Hour), recoveredAt.Add(time.Minute), 10)
	if err != nil || recovered != 1 {
		t.Fatalf("second recovery count=%d err=%v", recovered, err)
	}
	resource, err := store.Get(context.Background(), model.KindOperation, deleting.ID)
	if err != nil || resource.(*model.Operation).OperationStatus != model.OperationFailed {
		t.Fatalf("delete recovery operation=%#v err=%v", resource, err)
	}
	resource, err = store.Get(context.Background(), model.KindOperation, live.ID)
	if err != nil || resource.(*model.Operation).OperationStatus != model.OperationRunning {
		t.Fatalf("live operation=%#v err=%v", resource, err)
	}
	if _, err := store.RecoverExpiredOperations(context.Background(), base, recoveredAt, 0); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("zero-limit recovery error=%v", err)
	}
}

func TestStoreRecoversSupersededQueuedReconcilesWithReplayAndRetention(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	request := func(subject *model.ProviderNetwork, revision int64) *model.Operation {
		return &model.Operation{
			Action: "reconcile", TargetKind: model.KindProviderNetwork, TargetID: subject.ID,
			TargetRevision: revision, OperationStatus: model.OperationQueued,
		}
	}
	createSuperseded := func(name, key string) (*model.ProviderNetwork, *model.Operation) {
		subject := mustCreate(t, store, &model.ProviderNetwork{Name: name}, "provider-"+name).(*model.ProviderNetwork)
		operation := mustCreate(t, store, request(subject, subject.Revision), key).(*model.Operation)
		subject.Description = "new desired revision"
		updated, _, err := store.Update(context.Background(), subject, subject.Revision, "supersede-"+name)
		if err != nil {
			t.Fatal(err)
		}
		return updated.(*model.ProviderNetwork), operation
	}

	firstSubject, first := createSuperseded("first", "queued-first")
	_, second := createSuperseded("second", "queued-second")
	currentSubject := mustCreate(t, store, &model.ProviderNetwork{Name: "current"}, "provider-current").(*model.ProviderNetwork)
	current := mustCreate(t, store, request(currentSubject, currentSubject.Revision), "queued-current").(*model.Operation)

	base := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	recovered, err := store.RecoverExpiredOperations(context.Background(), base.Add(-time.Hour), base, 1)
	if err != nil || recovered != 1 {
		t.Fatalf("first recovery count=%d err=%v", recovered, err)
	}
	firstResource, err := store.Get(context.Background(), model.KindOperation, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := firstResource.(*model.Operation)
	if failed.OperationStatus != model.OperationFailed || failed.Error != supersededQueuedOperationError || failed.CompletedAt == nil || !failed.CompletedAt.Equal(base) || failed.Revision != first.Revision+1 {
		t.Fatalf("recovered queued operation=%#v", failed)
	}
	for _, operationID := range []string{second.ID, current.ID} {
		resource, getErr := store.Get(context.Background(), model.KindOperation, operationID)
		if getErr != nil || resource.(*model.Operation).OperationStatus != model.OperationQueued {
			t.Fatalf("bounded recovery changed operation=%#v err=%v", resource, getErr)
		}
	}

	replayedResource, replayed, err := store.Create(context.Background(), request(firstSubject, first.TargetRevision), "queued-first")
	if err != nil || !replayed || replayedResource.GetMetadata().ID != first.ID {
		t.Fatalf("failed operation replay=%#v replayed=%v err=%v", replayedResource, replayed, err)
	}
	latest, err := store.Get(context.Background(), model.KindOperation, replayedResource.GetMetadata().ID)
	if err != nil || latest.(*model.Operation).OperationStatus != model.OperationFailed {
		t.Fatalf("durable operation after replay=%#v err=%v", latest, err)
	}
	recovered, err = store.RecoverExpiredOperations(context.Background(), base.Add(-time.Hour), base.Add(time.Minute), 10)
	if err != nil || recovered != 1 {
		t.Fatalf("second recovery count=%d err=%v", recovered, err)
	}
	recovered, err = store.RecoverExpiredOperations(context.Background(), base.Add(-time.Hour), base.Add(90*time.Second), 10)
	if err != nil || recovered != 0 {
		t.Fatalf("idempotent recovery count=%d err=%v", recovered, err)
	}
	firstResource, err = store.Get(context.Background(), model.KindOperation, first.ID)
	if err != nil || firstResource.GetMetadata().Revision != failed.Revision {
		t.Fatalf("terminal operation changed on retry: operation=%#v err=%v", firstResource, err)
	}
	currentResource, err := store.Get(context.Background(), model.KindOperation, current.ID)
	if err != nil || currentResource.(*model.Operation).OperationStatus != model.OperationQueued {
		t.Fatalf("claimable queued operation=%#v err=%v", currentResource, err)
	}
	claimed, err := store.ClaimReconcile(context.Background(), current.ID, currentResource.GetMetadata().Revision, "lease-current", base.Add(90*time.Second), base.Add(-time.Hour))
	if err != nil || claimed.OperationStatus != model.OperationRunning {
		t.Fatalf("claim current queued operation=%#v err=%v", claimed, err)
	}

	database.mu.Lock()
	rowsBefore := len(database.rows[kindTables[model.KindOperation]])
	database.mu.Unlock()
	pruned, err := store.PruneOperations(context.Background(), base, 0)
	if err != nil || pruned != 0 {
		t.Fatalf("retention age pruned=%d err=%v", pruned, err)
	}
	pruned, err = store.PruneOperations(context.Background(), base.Add(2*time.Minute), 2)
	if err != nil || pruned != 0 {
		t.Fatalf("retention keep pruned=%d err=%v", pruned, err)
	}
	pruned, err = store.PruneOperations(context.Background(), base.Add(2*time.Minute), 0)
	if err != nil || pruned != 2 {
		t.Fatalf("PruneOperations() pruned=%d err=%v", pruned, err)
	}
	database.mu.Lock()
	rowsAfter := len(database.rows[kindTables[model.KindOperation]])
	database.mu.Unlock()
	if rowsAfter != rowsBefore-4 {
		t.Fatalf("operation and replay rows after prune=%d want %d", rowsAfter, rowsBefore-4)
	}
	if _, err := store.Get(context.Background(), model.KindOperation, first.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("pruned operation error=%v", err)
	}
	recreated, replayed, err := store.Create(context.Background(), request(firstSubject, first.TargetRevision), "queued-first")
	if err != nil || replayed || recreated.GetMetadata().ID == first.ID {
		t.Fatalf("pruned replay token remained: resource=%#v replayed=%v err=%v", recreated, replayed, err)
	}
}

func TestStoreConcurrentRecoveryOfSupersededQueuedReconcileIsSerialized(t *testing.T) {
	for iteration := 0; iteration < 16; iteration++ {
		database := newFakeDatabase()
		first := deterministicStore(database)
		second := deterministicStore(database)
		subject := mustCreate(t, first, &model.ProviderNetwork{Name: "tenant"}, "subject-provider").(*model.ProviderNetwork)
		operation := mustCreate(t, first, &model.Operation{
			Action: "reconcile", TargetKind: model.KindProviderNetwork, TargetID: subject.ID,
			TargetRevision: subject.Revision, OperationStatus: model.OperationQueued,
		}, "queued-reconcile").(*model.Operation)
		subject.Description = "superseded"
		if _, _, err := first.Update(context.Background(), subject, subject.Revision, "supersede-provider"); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		type result struct {
			count int
			err   error
		}
		results := make(chan result, 2)
		for _, candidate := range []*Store{first, second} {
			go func(store *Store) {
				<-start
				count, err := store.RecoverExpiredOperations(context.Background(), time.Now().Add(-time.Hour), time.Now(), 1)
				results <- result{count: count, err: err}
			}(candidate)
		}
		close(start)
		left, right := <-results, <-results
		if left.err != nil || right.err != nil || left.count+right.count != 1 {
			t.Fatalf("iteration %d concurrent recovery left=%#v right=%#v", iteration, left, right)
		}
		stored, err := first.Get(context.Background(), model.KindOperation, operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		recovered := stored.(*model.Operation)
		if recovered.OperationStatus != model.OperationFailed || recovered.Error != supersededQueuedOperationError || recovered.Revision != operation.Revision+1 {
			t.Fatalf("iteration %d recovered operation=%#v", iteration, recovered)
		}
	}
}

func TestStoreExpiredRecoveryAndHeartbeatAreSerializedAcrossManagers(t *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		database := newFakeDatabase()
		first := deterministicStore(database)
		second := deterministicStore(database)
		subject := mustCreate(t, first, &model.ProviderNetwork{Name: fmt.Sprintf("tenant-%d", iteration)}, "subject-provider").(*model.ProviderNetwork)
		operation := mustCreate(t, first, &model.Operation{
			Action: "reconcile", TargetKind: model.KindProviderNetwork, TargetID: subject.ID,
			TargetRevision: subject.Revision, OperationStatus: model.OperationQueued,
		}, "operation").(*model.Operation)
		started := time.Now().UTC()
		claimed, err := first.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-race", started, started.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		renewResult := make(chan error, 1)
		recoverResult := make(chan struct {
			count int
			err   error
		}, 1)
		go func() {
			<-start
			_, renewErr := first.RenewOperationLease(context.Background(), claimed.ID, claimed.Revision, claimed.LeaseOwner, started.Add(time.Second))
			renewResult <- renewErr
		}()
		go func() {
			<-start
			count, recoverErr := second.RecoverExpiredOperations(context.Background(), started, started.Add(2*time.Second), 1)
			recoverResult <- struct {
				count int
				err   error
			}{count, recoverErr}
		}()
		close(start)
		renewErr, recovery := <-renewResult, <-recoverResult
		if recovery.err != nil {
			t.Fatalf("iteration %d recovery error=%v", iteration, recovery.err)
		}
		stored, getErr := first.Get(context.Background(), model.KindOperation, claimed.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		status := stored.(*model.Operation).OperationStatus
		switch recovery.count {
		case 0:
			if renewErr != nil || status != model.OperationRunning {
				t.Fatalf("iteration %d renewal winner err=%v status=%s", iteration, renewErr, status)
			}
		case 1:
			if renewErr == nil || status != model.OperationFailed {
				t.Fatalf("iteration %d recovery winner renewErr=%v status=%s", iteration, renewErr, status)
			}
		default:
			t.Fatalf("iteration %d recovery count=%d", iteration, recovery.count)
		}
	}
}

func TestStoreReconcileCannotStartAfterTombstone(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	subject := mustCreate(t, store, &model.ProviderNetwork{Name: "tenant"}, "create-claim-provider").(*model.ProviderNetwork)
	operation := mustCreate(t, store, &model.Operation{
		Action:          "reconcile",
		TargetKind:      model.KindProviderNetwork,
		TargetID:        subject.ID,
		TargetRevision:  subject.Revision,
		OperationStatus: model.OperationQueued,
	}, "reconcile-claim-provider").(*model.Operation)
	if _, _, err := store.BeginDelete(context.Background(), model.KindProviderNetwork, subject.ID, subject.Revision, "delete-before-claim"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC)
	if _, err := store.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-ovsdb", now, now.Add(-2*time.Minute)); !errors.Is(err, controlstore.ErrPrecondition) {
		t.Fatalf("ClaimReconcile() error=%v, want inactive target precondition", err)
	}
	stored, err := store.Get(context.Background(), model.KindOperation, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	queued := stored.(*model.Operation)
	if queued.OperationStatus != model.OperationQueued {
		t.Fatalf("operation changed despite rejected claim: %#v", queued)
	}
	queued.OperationStatus = model.OperationRunning
	if _, _, err := store.Update(context.Background(), queued, queued.Revision, ""); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("generic running transition error=%v", err)
	}
}

func TestStoreDeleteLeaseBlocksPurgeUntilOwnerCompletes(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	subject := mustCreate(t, store, &model.ProviderNetwork{Name: "delete-tenant"}, "delete-provider").(*model.ProviderNetwork)
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProviderNetwork, subject.ID, subject.Revision, "begin-delete")
	if err != nil {
		t.Fatal(err)
	}
	operation := mustCreate(t, store, &model.Operation{Action: "delete", TargetKind: model.KindProviderNetwork, TargetID: subject.ID, TargetRevision: tombstone.GetMetadata().Revision, OperationStatus: model.OperationQueued}, "delete-operation").(*model.Operation)
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimDelete(context.Background(), operation.ID, operation.Revision, "lease-delete", now, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProviderNetwork, subject.ID, tombstone.GetMetadata().Revision); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Purge() error=%v, want delete lease fence", err)
	}
	claimed, err = store.RenewOperationLease(context.Background(), claimed.ID, claimed.Revision, "lease-delete", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	claimed.OperationStatus = model.OperationSucceeded
	completed := now.Add(2 * time.Second)
	claimed.CompletedAt = &completed
	if _, _, err := store.Update(context.Background(), claimed, claimed.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProviderNetwork, subject.ID, tombstone.GetMetadata().Revision); err != nil {
		t.Fatal(err)
	}
}

func TestStoreClaimAndDeleteAreSerializedAcrossManagers(t *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		database := newFakeDatabase()
		first := deterministicStore(database)
		second := deterministicStore(database)
		subject := mustCreate(t, first, &model.ProviderNetwork{Name: "tenant"}, "subject-provider").(*model.ProviderNetwork)
		operation := mustCreate(t, first, &model.Operation{
			Action:          "reconcile",
			TargetKind:      model.KindProviderNetwork,
			TargetID:        subject.ID,
			TargetRevision:  subject.Revision,
			OperationStatus: model.OperationQueued,
		}, "operation").(*model.Operation)
		start := make(chan struct{})
		claimResult := make(chan error, 1)
		deleteResult := make(chan error, 1)
		now := time.Now().UTC()
		go func() {
			<-start
			_, err := second.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-ovsdb", now, now.Add(-2*time.Minute))
			claimResult <- err
		}()
		go func() {
			<-start
			_, _, err := first.BeginDelete(context.Background(), model.KindProviderNetwork, subject.ID, subject.Revision, "delete")
			deleteResult <- err
		}()
		close(start)
		claimErr, deleteErr := <-claimResult, <-deleteResult
		if deleteErr != nil {
			t.Fatalf("iteration %d BeginDelete(): %v", iteration, deleteErr)
		}
		if claimErr != nil && !errors.Is(claimErr, controlstore.ErrPrecondition) {
			t.Fatalf("iteration %d ClaimReconcile(): %v", iteration, claimErr)
		}
		stored, err := first.Get(context.Background(), model.KindOperation, operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		status := stored.(*model.Operation).OperationStatus
		if (claimErr == nil) != (status == model.OperationRunning) {
			t.Fatalf("iteration %d claimErr=%v status=%s", iteration, claimErr, status)
		}
	}
}

func TestStoreRejectsUpdateThatWouldBreakDependentResources(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	first := mustCreate(t, store, &model.Network{Name: "first"}, "first-network").(*model.Network)
	second := mustCreate(t, store, &model.Network{Name: "second"}, "second-network").(*model.Network)
	subnet := mustCreate(t, store, &model.Subnet{NetworkID: first.ID, Name: "private-v4", CIDR: "10.0.0.0/24"}, "subnet").(*model.Subnet)
	mustCreate(t, store, &model.Port{
		NetworkID: first.ID, Name: "dependent-port", MACAddress: "02:00:00:00:00:42",
		FixedIPs: []model.FixedIP{{SubnetID: subnet.ID, Address: "10.0.0.42"}},
	}, "dependent-port")
	subnet.NetworkID = second.ID
	_, _, err := store.Update(context.Background(), subnet, subnet.Revision, "move-subnet")
	if !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("dependency-breaking subnet update error=%v", err)
	}
}

func TestStoreSerializesConcurrentUniqueAllocation(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	network := mustCreate(t, store, &model.Network{Name: "private"}, "network").(*model.Network)
	subnet := mustCreate(t, store, &model.Subnet{NetworkID: network.ID, Name: "private-v4", CIDR: "10.0.0.0/24"}, "subnet").(*model.Subnet)
	const workers = 32
	var successes atomic.Int64
	var duplicates atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, _, err := store.Create(context.Background(), &model.IPAllocation{SubnetID: subnet.ID, Address: "10.0.0.10", State: model.IPReserved}, fmt.Sprintf("allocation-%d", index))
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, controlstore.ErrAlreadyExists):
				duplicates.Add(1)
			default:
				t.Errorf("Create allocation: %v", err)
			}
		}(index)
	}
	wait.Wait()
	if successes.Load() != 1 || duplicates.Load() != workers-1 {
		t.Fatalf("successes=%d duplicates=%d", successes.Load(), duplicates.Load())
	}
}

func TestStoreRejectsMalformedSnapshotAndCancelledContext(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	if _, err := store.Get(context.Background(), model.KindProviderNetwork, "missing"); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("missing resource error=%v", err)
	}
	database.mu.Lock()
	database.rows[kindTables[model.KindProviderNetwork]] = append(database.rows[kindTables[model.KindProviderNetwork]], ovsdb.Row{"_uuid": database.nextUUID(), "id": "broken"})
	database.mu.Unlock()
	if _, err := store.List(context.Background(), model.KindProviderNetwork, controlstore.ListOptions{}); err == nil {
		t.Fatal("malformed database row was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.List(ctx, model.KindProviderNetwork, controlstore.ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled List error=%v", err)
	}
}

func TestConnectionConfigFailsClosed(t *testing.T) {
	ca := x509.NewCertPool()
	secureTLS := &tls.Config{RootCAs: ca, Certificates: []tls.Certificate{{}}, MinVersion: tls.VersionTLS13}
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "unix", config: Config{Endpoints: []string{"unix:/run/pvn/control.sock"}}},
		{name: "ssl", config: Config{Endpoints: []string{"ssl:192.0.2.10:6645"}, TLSConfig: secureTLS}},
		{name: "no endpoints", config: Config{}, wantErr: true},
		{name: "tcp rejected", config: Config{Endpoints: []string{"tcp:192.0.2.10:6645"}}, wantErr: true},
		{name: "ssl no tls", config: Config{Endpoints: []string{"ssl:192.0.2.10:6645"}}, wantErr: true},
		{name: "insecure", config: Config{Endpoints: []string{"ssl:192.0.2.10:6645"}, TLSConfig: &tls.Config{InsecureSkipVerify: true}}, wantErr: true}, //nolint:gosec -- verifies rejection
		{name: "no client cert", config: Config{Endpoints: []string{"ssl:192.0.2.10:6645"}, TLSConfig: &tls.Config{RootCAs: ca}}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateConnectionConfig(test.config)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateConnectionConfig() error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestBuildOperationsUsesCASAndDurableCommit(t *testing.T) {
	changes := []change{{type_: changeUpdate, table: kindTables[model.KindProviderNetwork], id: "provider-a", expectedRevision: 7, row: ovsdb.Row{"revision": int64(8)}}}
	operations := buildOperations(12, changes, "2026-08-05T00:00:00Z")
	if len(operations) != 4 {
		t.Fatalf("operation count=%d", len(operations))
	}
	if operations[0].Op != ovsdb.OperationWait || operations[0].Timeout == nil || *operations[0].Timeout != 0 || operations[0].Until != string(ovsdb.WaitConditionEqual) {
		t.Fatalf("CAS wait=%#v", operations[0])
	}
	if operations[1].Op != ovsdb.OperationUpdate || operations[1].Row["revision"] != int64(13) {
		t.Fatalf("lock update=%#v", operations[1])
	}
	if operations[2].Op != ovsdb.OperationUpdate || len(operations[2].Where) != 2 {
		t.Fatalf("resource update=%#v", operations[2])
	}
	if operations[3].Op != ovsdb.OperationCommit || operations[3].Durable == nil || !*operations[3].Durable {
		t.Fatalf("durable commit=%#v", operations[3])
	}
}

func TestOperationResultErrorsAreClassifiedByConcreteType(t *testing.T) {
	operations := []ovsdb.Operation{{Op: ovsdb.OperationWait, Table: kindTables[model.KindOperation]}}
	operationErrors, transactionError := ovsdb.CheckOperationResults([]ovsdb.OperationResult{{Error: "timed out"}}, operations)
	if transactionError == nil || !hasWaitError(operationErrors, transactionError) {
		t.Fatalf("wait error was not classified: ops=%v err=%v", operationErrors, transactionError)
	}
	operationErrors, transactionError = ovsdb.CheckOperationResults([]ovsdb.OperationResult{{Error: "constraint violation"}}, operations)
	if transactionError == nil || !hasConstraintError(operationErrors, transactionError) {
		t.Fatalf("constraint error was not classified: ops=%v err=%v", operationErrors, transactionError)
	}
	operationErrors, transactionError = ovsdb.CheckOperationResults([]ovsdb.OperationResult{{Error: "referential integrity violation"}}, operations)
	if transactionError == nil || !hasConstraintError(operationErrors, transactionError) || !hasReferentialError(operationErrors, transactionError) {
		t.Fatalf("referential error was not classified: ops=%v err=%v", operationErrors, transactionError)
	}
}
