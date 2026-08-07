package controlstore

import (
	"context"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestMemoryAcceptsRegisteredDualChassisAndResolvesBothNodes(t *testing.T) {
	store := NewMemory()
	network := mustCreate(t, store, &model.Network{Name: "dual-private"}, "dual-network").(*model.Network)
	group := mustCreate(t, store, &model.SecurityGroup{Name: "dual-baseline"}, "dual-group").(*model.SecurityGroup)
	source := mustCreate(t, store, &model.Node{Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "dual-source").(*model.Node)
	target := mustCreate(t, store, &model.Node{Name: "pve-b", ChassisID: "chassis-b", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "dual-target").(*model.Node)
	port := mustCreate(t, store, &model.Port{
		NetworkID: network.ID, Name: "vm100-net0", MACAddress: "02:00:00:00:00:64", SecurityGroupIDs: []string{group.ID},
		AdminStateUp: true, BindingStatus: model.PortBinding, NodeID: source.ID, VMID: 100, NIC: "net0",
		Generation: 3, RequestedChassis: source.ChassisID + "," + target.ChassisID,
	}, "dual-port").(*model.Port)
	for _, identity := range []string{source.Name, target.Name, source.ChassisID, target.ChassisID} {
		matches, err := store.LookupRuntimePorts(context.Background(), identity, 100, "net0")
		if err != nil || len(matches) != 1 || matches[0].ID != port.ID {
			t.Fatalf("dual lookup %q matches=%#v err=%v", identity, matches, err)
		}
	}
}

func TestMemoryRejectsUnregisteredAdditionalChassis(t *testing.T) {
	store := NewMemory()
	network := mustCreate(t, store, &model.Network{Name: "dual-private"}, "dual-network").(*model.Network)
	group := mustCreate(t, store, &model.SecurityGroup{Name: "dual-baseline"}, "dual-group").(*model.SecurityGroup)
	source := mustCreate(t, store, &model.Node{Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "dual-source").(*model.Node)
	_, _, err := store.Create(context.Background(), &model.Port{
		NetworkID: network.ID, Name: "vm100-net0", MACAddress: "02:00:00:00:00:64", SecurityGroupIDs: []string{group.ID},
		AdminStateUp: true, BindingStatus: model.PortBinding, NodeID: source.ID, VMID: 100, NIC: "net0",
		Generation: 3, RequestedChassis: source.ChassisID + ",missing-chassis",
	}, "dual-port")
	if err == nil {
		t.Fatal("port with an unregistered additional chassis was accepted")
	}
}
