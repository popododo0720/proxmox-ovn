package ovsdbstore

import (
	"context"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestStorePersistsDualChassisAndResolvesMigrationTarget(t *testing.T) {
	database := newFakeDatabase()
	store := deterministicStore(database)
	network := mustCreate(t, store, &model.Network{Name: "migration-private"}, "migration-network").(*model.Network)
	group := mustCreate(t, store, &model.SecurityGroup{Name: "migration-baseline"}, "migration-group").(*model.SecurityGroup)
	source := mustCreate(t, store, &model.Node{Metadata: model.Metadata{ID: "node-a"}, Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "migration-source").(*model.Node)
	target := mustCreate(t, store, &model.Node{Metadata: model.Metadata{ID: "node-b"}, Name: "pve-b", ChassisID: "chassis-b", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "migration-target").(*model.Node)
	port := mustCreate(t, store, &model.Port{
		NetworkID: network.ID, Name: "vm100-net0", MACAddress: "02:00:00:00:00:64", SecurityGroupIDs: []string{group.ID},
		AdminStateUp: true, BindingStatus: model.PortBinding, NodeID: source.ID, VMID: 100, NIC: "net0",
		Generation: 3, RequestedChassis: source.ChassisID + "," + target.ChassisID,
	}, "migration-port").(*model.Port)
	for _, identity := range []string{source.Name, target.Name, source.ChassisID, target.ChassisID} {
		matches, err := store.LookupRuntimePorts(context.Background(), identity, 100, "net0")
		if err != nil || len(matches) != 1 || matches[0].ID != port.ID || matches[0].RequestedChassis != "chassis-a,chassis-b" {
			t.Fatalf("dual-chassis lookup %q matches=%#v err=%v", identity, matches, err)
		}
	}
}
