package ovnnb

import (
	"context"
	"strings"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestRendererUsesRARPOnlyForDualChassisMigrationIntent(t *testing.T) {
	ctx := context.Background()
	store := controlstore.NewMemory()
	network := mustCreate(t, store, &model.Network{Metadata: model.Metadata{ID: "migration-network"}, Name: "migration-private"}).(*model.Network)
	group := mustCreate(t, store, &model.SecurityGroup{Metadata: model.Metadata{ID: "migration-sg"}, Name: "migration-baseline"}).(*model.SecurityGroup)
	source := mustCreate(t, store, &model.Node{Metadata: model.Metadata{ID: "migration-source"}, Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}).(*model.Node)
	target := mustCreate(t, store, &model.Node{Metadata: model.Metadata{ID: "migration-target"}, Name: "pve-b", ChassisID: "chassis-b", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}).(*model.Node)
	port := mustCreate(t, store, &model.Port{
		Metadata: model.Metadata{ID: "migration-port"}, NetworkID: network.ID, Name: "vm100-net0", MACAddress: "02:00:00:00:00:64",
		SecurityGroupIDs: []string{group.ID}, AdminStateUp: true, BindingStatus: model.PortBinding,
		NodeID: source.ID, VMID: 100, NIC: "net0", Generation: 3, RequestedChassis: source.ChassisID + "," + target.ChassisID,
	}).(*model.Port)
	runner := &recordingRunner{}
	client, err := NewClient(ClientConfig{Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}, WaitForSync: true})
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := NewRenderer(client, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []model.Resource{network, group, port} {
		if err := renderer.Render(ctx, resource); err != nil {
			t.Fatal(err)
		}
	}
	if !runner.contains("lsp-set-options", "requested-chassis=chassis-a,chassis-b", "activation-strategy=rarp") {
		t.Fatalf("dual-chassis render omitted activation strategy: %v", runner.calls)
	}

	desired := *port
	desired.Metadata = model.Metadata{ID: port.ID}
	desired.NodeID = target.ID
	desired.RequestedChassis = target.ChassisID
	updated, _, err := store.Update(ctx, &desired, port.Revision, "migration-finalize")
	if err != nil {
		t.Fatal(err)
	}
	before := len(runner.calls)
	if err := renderer.Render(ctx, updated); err != nil {
		t.Fatal(err)
	}
	var optionCommand string
	for _, call := range runner.calls[before:] {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "lsp-set-options") {
			optionCommand = joined
		}
	}
	if optionCommand == "" || !strings.Contains(optionCommand, "requested-chassis=chassis-b") || strings.Contains(optionCommand, "activation-strategy") {
		t.Fatalf("finalize did not clear activation strategy: %q", optionCommand)
	}
}
