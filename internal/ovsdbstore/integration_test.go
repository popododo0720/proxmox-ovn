package ovsdbstore

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ovn-org/libovsdb/database/inmemory"
	libmodel "github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/libovsdb/ovsdb/serverdb"
	"github.com/ovn-org/libovsdb/server"
	"github.com/popododo0720/proxmox-ovn/internal/controlschema"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

// This self-contained integration test exercises the actual libovsdb JSON-RPC
// codec, monitor setup, wait-CAS transaction and durable commit operation. It
// does not need an installed or externally running ovsdb-server.
func TestOpenAgainstInMemoryOVSDBServer(t *testing.T) {
	controlClientModel, err := controlschema.FullDatabaseModel()
	if err != nil {
		t.Fatal(err)
	}
	serverClientModel, err := serverdb.FullDatabaseModel()
	if err != nil {
		t.Fatal(err)
	}
	database := inmemory.NewDatabase(map[string]libmodel.ClientDBModel{
		controlschema.Schema().Name: controlClientModel,
		serverdb.Schema().Name:      serverClientModel,
	})
	controlModel, modelErrors := libmodel.NewDatabaseModel(controlschema.Schema(), controlClientModel)
	if len(modelErrors) != 0 {
		t.Fatalf("control database model errors: %v", modelErrors)
	}
	serverModel, modelErrors := libmodel.NewDatabaseModel(serverdb.Schema(), serverClientModel)
	if len(modelErrors) != 0 {
		t.Fatalf("server database model errors: %v", modelErrors)
	}
	ovsServer, err := server.NewOvsdbServer(database, controlModel, serverModel)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "control.sock")
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- ovsServer.Serve("unix", socket) }()
	t.Cleanup(ovsServer.Close)
	deadline := time.Now().Add(2 * time.Second)
	for !ovsServer.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !ovsServer.Ready() {
		t.Fatal("in-memory OVSDB server did not become ready")
	}
	select {
	case err := <-serveErrors:
		t.Fatalf("serve in-memory OVSDB: %v", err)
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	live, err := openDatabase(ctx, Config{Endpoints: []string{fmt.Sprintf("unix:%s", socket)}})
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(&inMemoryTestDatabase{ovsDatabase: live})
	t.Cleanup(store.Close)
	created, replayed, err := store.Create(ctx, &model.Project{Name: "tenant", PoolID: "pool-a"}, "create-tenant")
	if err != nil || replayed {
		t.Fatalf("Create replayed=%v err=%v", replayed, err)
	}
	loaded, err := store.Get(ctx, model.KindProject, created.GetMetadata().ID)
	if err != nil || loaded.(*model.Project).Name != "tenant" {
		t.Fatalf("Get loaded=%#v err=%v", loaded, err)
	}
	replay, replayed, err := store.Create(ctx, &model.Project{Name: "tenant", PoolID: "pool-a"}, "create-tenant")
	if err != nil || !replayed || replay.GetMetadata().ID != created.GetMetadata().ID {
		t.Fatalf("replay resource=%#v replayed=%v err=%v", replay, replayed, err)
	}
	project := created.(*model.Project)
	networkResource, _, err := store.Create(ctx, &model.Network{ProjectID: project.ID, Name: "private"}, "create-network")
	if err != nil {
		t.Fatal(err)
	}
	network := networkResource.(*model.Network)
	subnetResource, _, err := store.Create(ctx, &model.Subnet{ProjectID: project.ID, NetworkID: network.ID, Name: "private-v4", CIDR: "10.0.0.0/24"}, "create-subnet")
	if err != nil {
		t.Fatal(err)
	}
	groupResource, _, err := store.Create(ctx, &model.SecurityGroup{ProjectID: project.ID, Name: "default"}, "create-group")
	if err != nil {
		t.Fatal(err)
	}
	nodeResource, _, err := store.Create(ctx, &model.Node{Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "create-node")
	if err != nil {
		t.Fatal(err)
	}
	portResource, _, err := store.Create(ctx, &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "vm-port", MACAddress: "02:00:00:00:00:01",
		FixedIPs:         []model.FixedIP{{SubnetID: subnetResource.GetMetadata().ID, Address: "10.0.0.10"}},
		SecurityGroupIDs: []string{groupResource.GetMetadata().ID}, NodeID: nodeResource.GetMetadata().ID,
		VMID: 100, NIC: "net0", AdminStateUp: true,
	}, "create-port")
	if err != nil {
		t.Fatal(err)
	}
	loadedPort, err := store.Get(ctx, model.KindPort, portResource.GetMetadata().ID)
	if err != nil {
		t.Fatal(err)
	}
	port := loadedPort.(*model.Port)
	if len(port.FixedIPs) != 1 || port.FixedIPs[0].SubnetID != subnetResource.GetMetadata().ID || len(port.SecurityGroupIDs) != 1 || port.NodeID != nodeResource.GetMetadata().ID {
		t.Fatalf("reference/map/set round trip failed: %#v", port)
	}
}

// libovsdb's in-memory server does not implement the RFC 7047 durable commit
// operation. Strip only that final operation in this test adapter; production
// ovsdb-server transactions still use ovsDatabase's durable commit path.
type inMemoryTestDatabase struct{ ovsDatabase *ovsDatabase }

func (d *inMemoryTestDatabase) load(ctx context.Context) (rawDatabase, error) {
	return d.ovsDatabase.load(ctx)
}

func (d *inMemoryTestDatabase) initialize(ctx context.Context, row ovsdb.Row) error {
	operations := []ovsdb.Operation{{Op: ovsdb.OperationInsert, Table: controlschema.OperationTable, Row: row}}
	results, err := d.ovsDatabase.client.Transact(ctx, operations...)
	if err != nil {
		return err
	}
	if _, err := ovsdb.CheckOperationResults(results, operations); err != nil {
		return err
	}
	return nil
}

func (d *inMemoryTestDatabase) commit(ctx context.Context, epoch int64, changes []change, updatedAt string) error {
	operations := buildOperations(epoch, changes, updatedAt)
	operations = operations[:len(operations)-1]
	results, err := d.ovsDatabase.client.Transact(ctx, operations...)
	if err != nil {
		return err
	}
	if _, err := ovsdb.CheckOperationResults(results, operations); err != nil {
		return err
	}
	return nil
}

func (d *inMemoryTestDatabase) close() { d.ovsDatabase.close() }
