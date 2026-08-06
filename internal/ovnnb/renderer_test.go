package ovnnb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

var testOVSUUID = deterministicUUID("dhcp-options:subnet-1")

type recordingRunner struct {
	mu                   sync.Mutex
	calls                [][]string
	dhcpCreated          bool
	routerSNATFindOutput string
	gatewayChassisOutput string
}

type activeActiveRunner struct {
	mu          sync.Mutex
	initialFind int
	created     bool
	creates     int
	barrier     chan struct{}
	uuid        string
}

type movingGatewayRunner struct {
	recordingRunner
	failed bool
}

type unavailableGatewayRunner struct {
	recordingRunner
}

type providerPortRunner struct {
	recordingRunner
	uuid   string
	exists bool
	owned  bool
}

type uuidLookupRunner struct {
	arguments []string
	output    []byte
	err       error
}

type ownedRowLookupRunner struct {
	owned     []string
	named     string
	preferred string
}

type attachedRaceRunner struct {
	uuid   string
	exists bool
	calls  [][]string
}

func (runner *uuidLookupRunner) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	runner.arguments = append([]string(nil), arguments...)
	return runner.output, runner.err
}

func (runner *ownedRowLookupRunner) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "find Logical_Switch") {
		if strings.Contains(joined, stringAssignment("name", logicalSwitch("network-1"))) {
			if runner.named == "" {
				return nil, nil
			}
			return []byte(runner.named + "\n"), nil
		}
		return []byte(strings.Join(runner.owned, "\n")), nil
	}
	if strings.Contains(joined, "get Logical_Switch "+logicalSwitchUUID("network-1")+" _uuid") {
		if runner.preferred == "" {
			return nil, nil
		}
		return []byte(runner.preferred + "\n"), nil
	}
	return nil, nil
}

func (runner *attachedRaceRunner) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string(nil), arguments...))
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "get NAT "+runner.uuid+" _uuid") {
		if runner.exists {
			return []byte(runner.uuid + "\n"), nil
		}
		return nil, nil
	}
	if strings.Contains(joined, "create NAT") {
		runner.exists = true
		return []byte("transaction error: UUID already exists"), errors.New("exit status 1")
	}
	return nil, nil
}

func (runner *activeActiveRunner) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "_uuid=") {
		return nil, errors.New("unsupported _uuid find condition")
	}
	if strings.Contains(joined, "get Logical_Switch "+runner.uuid+" _uuid") {
		runner.mu.Lock()
		if runner.created {
			runner.mu.Unlock()
			return []byte(runner.uuid + "\n"), nil
		}
		runner.initialFind++
		if runner.initialFind == 2 {
			close(runner.barrier)
		}
		barrier := runner.barrier
		runner.mu.Unlock()
		<-barrier
		return nil, nil
	}
	if strings.Contains(joined, "create Logical_Switch") {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		runner.creates++
		if runner.created {
			return []byte("transaction error: UUID already exists"), errors.New("exit status 1")
		}
		runner.created = true
		return []byte(runner.uuid + "\n"), nil
	}
	return nil, nil
}

func (runner *recordingRunner) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls = append(runner.calls, append([]string(nil), arguments...))
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "_uuid=") {
		return nil, errors.New("unsupported _uuid find condition")
	}
	if strings.Contains(joined, "get DHCP_Options "+testOVSUUID+" _uuid") {
		if runner.dhcpCreated {
			return []byte(testOVSUUID + "\n"), nil
		}
		return nil, nil
	}
	if strings.Contains(joined, "create DHCP_Options") {
		runner.dhcpCreated = true
		return []byte(testOVSUUID + "\n"), nil
	}
	if strings.Contains(joined, "find NAT") && strings.Contains(joined, `external_ids:pvn-kind="router-snat"`) {
		return []byte(runner.routerSNATFindOutput), nil
	}
	if strings.Contains(joined, "lrp-get-gateway-chassis") {
		return []byte(runner.gatewayChassisOutput), nil
	}
	return nil, nil
}

func (runner *movingGatewayRunner) Run(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
	joined := strings.Join(arguments, " ")
	output, err := runner.recordingRunner.Run(ctx, binary, arguments...)
	if strings.Contains(joined, "lrp-add") && strings.Contains(joined, "lsp-add") && !strings.Contains(joined, "lsp-del") && !runner.failed {
		runner.failed = true
		return []byte("port already belongs to another logical switch"), errors.New("exit status 1")
	}
	if strings.Contains(joined, "get Logical_Router_Port") || strings.Contains(joined, "get Logical_Switch_Port") {
		return []byte("owned-port\n"), nil
	}
	return output, err
}

func (runner *unavailableGatewayRunner) Run(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
	joined := strings.Join(arguments, " ")
	_, _ = runner.recordingRunner.Run(ctx, binary, arguments...)
	if strings.Contains(joined, "lrp-add") || strings.Contains(joined, "get Logical_Router_Port") || strings.Contains(joined, "get Logical_Switch_Port") {
		return []byte("database connection failed"), errors.New("exit status 1")
	}
	return nil, nil
}

func (runner *providerPortRunner) Run(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
	output, err := runner.recordingRunner.Run(ctx, binary, arguments...)
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "find Logical_Switch_Port") {
		if strings.Contains(joined, "external_ids:pvn-managed") {
			if runner.exists && runner.owned {
				return []byte(runner.uuid + "\n"), nil
			}
			return nil, nil
		}
		if strings.Contains(joined, "name=") && runner.exists {
			return []byte(runner.uuid + "\n"), nil
		}
	}
	if strings.Contains(joined, "lsp-add") && strings.Contains(joined, "pvn-localnet-") {
		runner.exists = true
		runner.owned = true
	}
	if strings.Contains(joined, "lsp-del "+runner.uuid) {
		runner.exists = false
	}
	return output, err
}

func (runner *recordingRunner) contains(parts ...string) bool {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		matches := true
		for _, part := range parts {
			matches = matches && strings.Contains(joined, part)
		}
		if matches {
			return true
		}
	}
	return false
}

func TestFindUUIDUsesOVN2503GetSemantics(t *testing.T) {
	wanted := deterministicUUID("lookup-row")
	other := deterministicUUID("other-row")
	for name, test := range map[string]struct {
		output  string
		want    string
		wantErr bool
	}{
		"missing":   {},
		"found":     {output: wanted + "\n", want: wanted},
		"malformed": {output: "not-a-uuid\n", wantErr: true},
		"mismatch":  {output: other + "\n", wantErr: true},
		"multiple":  {output: wanted + "\n" + wanted + "\n", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &uuidLookupRunner{output: []byte(test.output)}
			renderer := newTestRenderer(t, runner, controlstore.NewMemory())
			found, err := renderer.findUUID(context.Background(), "Logical_Switch", wanted)
			if (err != nil) != test.wantErr || found != test.want {
				t.Fatalf("findUUID() found=%q err=%v", found, err)
			}
			joined := strings.Join(runner.arguments, " ")
			if !strings.Contains(joined, "--bare -- --if-exists get Logical_Switch "+wanted+" _uuid") || strings.Contains(joined, "_uuid=") {
				t.Fatalf("findUUID arguments=%v", runner.arguments)
			}
		})
	}
	runner := &uuidLookupRunner{}
	renderer := newTestRenderer(t, runner, controlstore.NewMemory())
	if _, err := renderer.findUUID(context.Background(), "Logical_Switch", "not-a-uuid"); err == nil || len(runner.arguments) != 0 {
		t.Fatalf("unsafe UUID lookup err=%v arguments=%v", err, runner.arguments)
	}
}

func TestLookupOwnedRowAdoptsOnlyOneUnambiguousRestoredRow(t *testing.T) {
	preferred := logicalSwitchUUID("network-1")
	restored := deterministicUUID("restored-logical-switch")
	foreign := deterministicUUID("foreign-logical-switch")
	row := managedRow(
		"Logical_Switch",
		preferred,
		logicalSwitch("network-1"),
		mapAssignment("external_ids", "pvn-kind", model.KindNetwork.String()),
		mapAssignment("external_ids", "pvn-id", "network-1"),
	)
	for name, test := range map[string]struct {
		runner  ownedRowLookupRunner
		want    string
		wantErr string
	}{
		"missing": {},
		"deterministic": {
			runner: ownedRowLookupRunner{owned: []string{preferred}, named: preferred, preferred: preferred},
			want:   preferred,
		},
		"restored": {
			runner: ownedRowLookupRunner{owned: []string{restored}, named: restored},
			want:   restored,
		},
		"duplicate-owned": {
			runner:  ownedRowLookupRunner{owned: []string{preferred, restored}, named: restored, preferred: preferred},
			wantErr: "duplicate PVN-owned",
		},
		"foreign-name": {
			runner:  ownedRowLookupRunner{named: foreign},
			wantErr: "not owned",
		},
		"foreign-deterministic-uuid": {
			runner:  ownedRowLookupRunner{preferred: preferred},
			wantErr: "not owned",
		},
		"restored-plus-deterministic-collision": {
			runner:  ownedRowLookupRunner{owned: []string{restored}, named: restored, preferred: preferred},
			wantErr: "conflicts with deterministic-UUID row",
		},
		"owned-name-mismatch": {
			runner:  ownedRowLookupRunner{owned: []string{restored}},
			wantErr: "does not have expected name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := test.runner
			renderer := newTestRenderer(t, &runner, controlstore.NewMemory())
			actual, err := renderer.lookupOwnedRow(context.Background(), row)
			if actual != test.want {
				t.Fatalf("lookupOwnedRow() = %q, want %q (err=%v)", actual, test.want, err)
			}
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("lookupOwnedRow() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestEnsureAttachedRowRecoversDeterministicUUIDRace(t *testing.T) {
	uuid := deterministicUUID("router-snat:race")
	parent := deterministicUUID("logical-router:race")
	runner := &attachedRaceRunner{uuid: uuid}
	renderer := newTestRenderer(t, runner, controlstore.NewMemory())
	assignments := []string{stringAssignment("type", "snat"), stringAssignment("logical_ip", "10.42.0.0/24")}
	if err := renderer.ensureAttachedRow(context.Background(), "NAT", uuid, assignments, "Logical_Router", parent, "nat"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("race calls=%v", runner.calls)
	}
	create := strings.Join(runner.calls[1], " ")
	retry := strings.Join(runner.calls[3], " ")
	if !strings.Contains(create, "--id="+uuid+" create NAT") || !strings.Contains(create, "add Logical_Router "+parent+" nat "+uuid) {
		t.Fatalf("create did not atomically attach non-root row: %v", runner.calls[1])
	}
	if !strings.Contains(retry, "set NAT "+uuid) || !strings.Contains(retry, "add Logical_Router "+parent+" nat "+uuid) {
		t.Fatalf("race retry did not update and reattach winner: %v", runner.calls[3])
	}
}

func TestRendererBuildsTenantNetworkPortAndSecurityGroup(t *testing.T) {
	ctx := context.Background()
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1"}).(*model.Project)
	network := mustCreate(t, store, &model.Network{Metadata: model.Metadata{ID: "network-1"}, ProjectID: project.ID, Name: "private", MTU: 1400}).(*model.Network)
	subnet := mustCreate(t, store, &model.Subnet{
		Metadata: model.Metadata{ID: "subnet-1"}, ProjectID: project.ID, NetworkID: network.ID,
		Name: "private-v4", CIDR: "10.42.0.0/24", GatewayIP: "10.42.0.1", EnableDHCP: true,
		DNSNameservers: []string{"1.1.1.1"},
	}).(*model.Subnet)
	group := mustCreate(t, store, &model.SecurityGroup{Metadata: model.Metadata{ID: "sg-1"}, ProjectID: project.ID, Name: "web"}).(*model.SecurityGroup)
	mustCreate(t, store, &model.Node{
		Metadata: model.Metadata{ID: "pve-a"}, Name: "pve-a", ChassisID: "chassis-a",
		Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true,
	})
	port := mustCreate(t, store, &model.Port{
		Metadata: model.Metadata{ID: "port-1"}, ProjectID: project.ID, NetworkID: network.ID, Name: "vm100-net0",
		MACAddress: "02:00:00:00:00:10", FixedIPs: []model.FixedIP{{SubnetID: subnet.ID, Address: "10.42.0.10"}},
		SecurityGroupIDs: []string{group.ID}, AdminStateUp: true, BindingStatus: model.PortBinding,
		NodeID: "pve-a", VMID: 100, NIC: "net0", RequestedChassis: "chassis-a",
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
	for _, resource := range []model.Resource{network, subnet, group, port} {
		if err := renderer.Render(ctx, resource); err != nil {
			t.Fatalf("render %s: %v", resource.ResourceKind(), err)
		}
	}

	for _, expected := range [][]string{
		{"create Logical_Switch", stringAssignment("name", logicalSwitch(network.ID)), `external_ids:pvn-id="network-1"`},
		{"create DHCP_Options", `cidr="10.42.0.0/24"`},
		{"dhcp-options-set-options", "server_id=10.42.0.1", "mtu=1400"},
		{"create Port_Group", portGroup(group.ID), `external_ids:pvn-id="sg-1"`},
		{"lsp-add " + logicalSwitchUUID(network.ID) + " pvn-port-1", "lsp-set-enabled pvn-port-1 enabled"},
		{"lsp-set-options pvn-port-1 requested-chassis=chassis-a"},
		{"lsp-set-dhcpv4-options pvn-port-1 " + testOVSUUID},
		{"get Logical_Switch_Port pvn-port-1", "add Port_Group " + portGroup(group.ID) + " ports @lsp"},
	} {
		if !runner.contains(expected...) {
			t.Errorf("no OVN command contains %v; calls=%v", expected, runner.calls)
		}
	}
}

func TestRendererDisablesUnboundPortWithOVNState(t *testing.T) {
	ctx := context.Background()
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{
		Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1",
	}).(*model.Project)
	network := mustCreate(t, store, &model.Network{
		Metadata: model.Metadata{ID: "network-1"}, ProjectID: project.ID, Name: "private", MTU: 1400,
	}).(*model.Network)
	subnet := mustCreate(t, store, &model.Subnet{
		Metadata: model.Metadata{ID: "subnet-1"}, ProjectID: project.ID, NetworkID: network.ID,
		Name: "private-v4", CIDR: "10.42.0.0/24", GatewayIP: "10.42.0.1",
	}).(*model.Subnet)
	port := mustCreate(t, store, &model.Port{
		Metadata: model.Metadata{ID: "port-1"}, ProjectID: project.ID, NetworkID: network.ID, Name: "vm100-net0",
		MACAddress: "02:00:00:00:00:10", FixedIPs: []model.FixedIP{{SubnetID: subnet.ID, Address: "10.42.0.10"}},
		AdminStateUp: true, BindingStatus: model.PortUnbound,
	}).(*model.Port)
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(ctx, port); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("lsp-set-enabled pvn-port-1 disabled") {
		t.Fatalf("unbound port did not use the OVN disabled state: %v", runner.calls)
	}
}

func TestExternalNetworkCreatesItsLocalnetPortImmediately(t *testing.T) {
	ctx := context.Background()
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1"}).(*model.Project)
	provider := mustCreate(t, store, &model.ProviderNetwork{Metadata: model.Metadata{ID: "provider-1"}, Name: "uplink"}).(*model.ProviderNetwork)
	segment := mustCreate(t, store, &model.ProviderSegment{
		Metadata: model.Metadata{ID: "segment-1"}, ProviderNetworkID: provider.ID,
		Name: "vlan-100", PhysicalNetwork: "provider", NetworkType: model.ProviderVLAN, VLANID: 100,
	}).(*model.ProviderSegment)
	network := mustCreate(t, store, &model.Network{
		Metadata: model.Metadata{ID: "external-1"}, ProjectID: project.ID, Name: "external",
		External: true, ProviderNetworkID: provider.ID,
	}).(*model.Network)

	runner := &recordingRunner{}
	client, err := NewClient(ClientConfig{Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := NewRenderer(client, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderer.Render(ctx, network); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("lsp-add "+logicalSwitchUUID(network.ID)+" pvn-localnet-", "lsp-set-type", "localnet", "network_name=provider", "tag=100") {
		t.Fatalf("external network localnet port was not rendered: %v", runner.calls)
	}
	_ = segment
}

func TestNetworkProviderChangeUpdatesOwnedLocalnetMapping(t *testing.T) {
	ctx := context.Background()
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1"}).(*model.Project)
	providerA := mustCreate(t, store, &model.ProviderNetwork{Metadata: model.Metadata{ID: "provider-a"}, Name: "uplink-a"}).(*model.ProviderNetwork)
	providerB := mustCreate(t, store, &model.ProviderNetwork{Metadata: model.Metadata{ID: "provider-b"}, Name: "uplink-b"}).(*model.ProviderNetwork)
	_ = mustCreate(t, store, &model.ProviderSegment{
		Metadata: model.Metadata{ID: "segment-a"}, ProviderNetworkID: providerA.ID,
		Name: "flat-a", PhysicalNetwork: "phys-a", NetworkType: model.ProviderFlat,
	})
	segmentB := mustCreate(t, store, &model.ProviderSegment{
		Metadata: model.Metadata{ID: "segment-b"}, ProviderNetworkID: providerB.ID,
		Name: "vlan-b", PhysicalNetwork: "phys-b", NetworkType: model.ProviderVLAN, VLANID: 222,
	}).(*model.ProviderSegment)
	network := &model.Network{
		Metadata: model.Metadata{ID: "network-1", Revision: 1}, ProjectID: project.ID, Name: "provider",
		ProviderNetworkID: providerA.ID,
	}
	runner := &providerPortRunner{uuid: deterministicUUID("localnet-row:" + network.ID)}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(ctx, network); err != nil {
		t.Fatal(err)
	}
	updated := *network
	updated.ProviderNetworkID = providerB.ID
	updated.Revision++
	if err := renderer.Render(ctx, &updated); err != nil {
		t.Fatal(err)
	}

	port := "pvn-localnet-" + compact(network.ID)
	if !runner.contains("find Logical_Switch_Port", stringAssignment("name", port),
		stringAssignment("type", "localnet"),
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", model.KindProviderSegment.String()),
		mapAssignment("external_ids", "pvn-network", network.ID)) {
		t.Fatalf("existing localnet port ownership was not checked: %v", runner.calls)
	}
	if !runner.contains("lsp-set-options "+runner.uuid+" network_name=phys-b",
		"set Logical_Switch_Port "+runner.uuid,
		mapAssignment("external_ids", "pvn-id", segmentB.ID), "tag=222") {
		t.Fatalf("provider change did not update the owned localnet row: %v", runner.calls)
	}
	lspAdds := 0
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "lsp-add "+logicalSwitchUUID(network.ID)+" "+port) {
			lspAdds++
		}
	}
	if lspAdds != 1 {
		t.Fatalf("provider change recreated the localnet port; lsp-add calls=%d calls=%v", lspAdds, runner.calls)
	}
}

func TestNetworkProviderRemovalDeletesOnlyOwnedLocalnetPort(t *testing.T) {
	ctx := context.Background()
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1"}).(*model.Project)
	provider := mustCreate(t, store, &model.ProviderNetwork{Metadata: model.Metadata{ID: "provider-1"}, Name: "uplink"}).(*model.ProviderNetwork)
	_ = mustCreate(t, store, &model.ProviderSegment{
		Metadata: model.Metadata{ID: "segment-1"}, ProviderNetworkID: provider.ID,
		Name: "flat", PhysicalNetwork: "provider", NetworkType: model.ProviderFlat,
	})
	network := &model.Network{
		Metadata: model.Metadata{ID: "network-1", Revision: 1}, ProjectID: project.ID, Name: "provider",
		ProviderNetworkID: provider.ID,
	}
	runner := &providerPortRunner{uuid: deterministicUUID("localnet-row:" + network.ID)}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(ctx, network); err != nil {
		t.Fatal(err)
	}
	overlay := *network
	overlay.ProviderNetworkID = ""
	overlay.Revision++
	if err := renderer.Render(ctx, &overlay); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("--if-exists lsp-del " + runner.uuid) {
		t.Fatalf("owned localnet port was not removed by UUID: %v", runner.calls)
	}
	if runner.exists {
		t.Fatal("localnet runner still contains the removed provider port")
	}
}

func TestNetworkProviderRemovalRefusesUnownedNameCollision(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1"}).(*model.Project)
	network := &model.Network{Metadata: model.Metadata{ID: "network-1"}, ProjectID: project.ID, Name: "overlay"}
	runner := &providerPortRunner{
		uuid: deterministicUUID("foreign-localnet-row:" + network.ID), exists: true, owned: false,
	}
	renderer := newTestRenderer(t, runner, store)

	err := renderer.Render(context.Background(), network)
	if err == nil || !strings.Contains(err.Error(), "is not owned by PVN network") {
		t.Fatalf("unowned localnet collision error = %v", err)
	}
	if runner.contains("lsp-del") {
		t.Fatalf("unowned localnet row was deleted: %v", runner.calls)
	}
}

func TestSecurityGroupDefaultDropHasOwnedDHCPv4Exceptions(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1"}).(*model.Project)
	group := mustCreate(t, store, &model.SecurityGroup{Metadata: model.Metadata{ID: "sg-1"}, ProjectID: project.ID, Name: "default"}).(*model.SecurityGroup)
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct {
		owner     string
		direction string
		match     string
	}{
		{owner: group.ID + ":dhcpv4-client", direction: "from-lport", match: "ip4 && udp && udp.src == 68 && udp.dst == 67"},
		{owner: group.ID + ":dhcpv4-server", direction: "to-lport", match: "ip4 && udp && udp.src == 67 && udp.dst == 68"},
	} {
		if !runner.contains("create ACL", stringAssignment("direction", expected.direction), "priority=3000",
			stringAssignment("match", expected.match), stringAssignment("action", "allow"),
			mapAssignment("external_ids", "pvn-managed", "true"),
			mapAssignment("external_ids", "pvn-owner", expected.owner)) {
			t.Errorf("DHCPv4 exception %q was not rendered: %v", expected.owner, runner.calls)
		}
	}
	for _, direction := range []string{"to-lport", "from-lport"} {
		owner := group.ID + ":default-drop:" + direction
		if !runner.contains("create ACL", stringAssignment("direction", direction), "priority=1000",
			stringAssignment("match", "ip4"), stringAssignment("action", "drop"),
			mapAssignment("external_ids", "pvn-owner", owner)) {
			t.Errorf("arbitrary IPv4 traffic is not default-dropped for %s: %v", direction, runner.calls)
		}
	}
}

func TestRendererUsesOVNNBCTL2503CommandBoundaries(t *testing.T) {
	ctx := context.Background()
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1"}).(*model.Project)
	network := mustCreate(t, store, &model.Network{Metadata: model.Metadata{ID: "network-1"}, ProjectID: project.ID, Name: "private"}).(*model.Network)
	group := mustCreate(t, store, &model.SecurityGroup{Metadata: model.Metadata{ID: "sg-1"}, ProjectID: project.ID, Name: "default"}).(*model.SecurityGroup)
	runner := &recordingRunner{}
	client, err := NewClient(ClientConfig{Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := NewRenderer(client, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []model.Resource{network, group} {
		if err := renderer.Render(ctx, resource); err != nil {
			t.Fatal(err)
		}
	}
	for _, call := range runner.calls {
		for index, argument := range call {
			if argument == "--may-exist" || argument == "--if-exists" || strings.HasPrefix(argument, "--id=") {
				if index == 0 || call[index-1] != "--" {
					t.Errorf("command option %q is not separated from global options: %v", argument, call)
				}
			}
			if argument == "pg-add-ports" || argument == "pg-del-ports" {
				t.Errorf("unsupported OVN 25.03 command used: %v", call)
			}
		}
	}
}

func TestRendererRejectsTypedNilAndCrossProjectPort(t *testing.T) {
	store := controlstore.NewMemory()
	runner := &recordingRunner{}
	client, err := NewClient(ClientConfig{Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := NewRenderer(client, store)
	if err != nil {
		t.Fatal(err)
	}
	var network *model.Network
	if err := renderer.Render(context.Background(), network); err == nil {
		t.Fatal("typed nil resource unexpectedly rendered")
	}

	first := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-a"}, Name: "a", PoolID: "pool-a"}).(*model.Project)
	second := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-b"}, Name: "b", PoolID: "pool-b"}).(*model.Project)
	otherNetwork := mustCreate(t, store, &model.Network{Metadata: model.Metadata{ID: "network-b"}, ProjectID: second.ID, Name: "private"}).(*model.Network)
	port := &model.Port{
		Metadata: model.Metadata{ID: "port-a"}, ProjectID: first.ID, NetworkID: otherNetwork.ID, Name: "vm100-net0",
		MACAddress: "02:00:00:00:00:01", LSPName: "pvn-port-a",
	}
	if err := renderer.Render(context.Background(), port); err == nil || !strings.Contains(err.Error(), "different projects") {
		t.Fatalf("cross-project port error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("OVN called for rejected resources: %v", runner.calls)
	}
}

func TestRouterRendersCentralizedGatewayDefaultRouteAndSNAT(t *testing.T) {
	ctx := context.Background()
	store, fixture := newNorthSouthFixture(t)
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(ctx, fixture.router); err != nil {
		t.Fatal(err)
	}

	routerPort := gatewayRouterPort(fixture.router.ID)
	switchPort := gatewaySwitchPort(fixture.router.ID)
	routeUUID := routerDefaultRouteUUID(fixture.router.ID)
	snatUUID := routerSNATUUID(fixture.router.ID, fixture.routerInterface.ID)
	for _, expected := range [][]string{
		{"lrp-add " + logicalRouterUUID(fixture.router.ID) + " " + routerPort, "192.0.2.10/24"},
		{"lsp-add " + logicalSwitchUUID(fixture.externalNetwork.ID) + " " + switchPort, "lsp-set-type " + switchPort + " router", "router-port=" + routerPort, "nat-addresses=router"},
		{"create Logical_Router_Static_Route", routeUUID, `ip_prefix="0.0.0.0/0"`, `nexthop="192.0.2.1"`, `output_port="` + routerPort + `"`},
		{"create Logical_Router_Static_Route", "add Logical_Router " + logicalRouterUUID(fixture.router.ID) + " static_routes " + routeUUID},
		{"lrp-set-gateway-chassis " + routerPort + " chassis-a 32767", "lrp-set-gateway-chassis " + routerPort + " chassis-b 32766"},
		{"create NAT", snatUUID, `type="snat"`, `external_ip="192.0.2.10"`, `logical_ip="10.42.0.0/24"`},
		{"create NAT", "add Logical_Router " + logicalRouterUUID(fixture.router.ID) + " nat " + snatUUID},
	} {
		if !runner.contains(expected...) {
			t.Errorf("no OVN command contains %v; calls=%v", expected, runner.calls)
		}
	}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "create Logical_Router_Static_Route") && !strings.Contains(joined, "add Logical_Router "+logicalRouterUUID(fixture.router.ID)+" static_routes "+routeUUID) {
			t.Fatalf("non-root route was created without its parent reference: %v", call)
		}
		if strings.Contains(joined, "create NAT") && strings.Contains(joined, `external_ids:pvn-kind="router-snat"`) && !strings.Contains(joined, "add Logical_Router "+logicalRouterUUID(fixture.router.ID)+" nat "+snatUUID) {
			t.Fatalf("non-root SNAT was created without its parent reference: %v", call)
		}
	}
	if runner.contains("chassis-disabled") {
		t.Fatalf("disabled gateway chassis was selected: %v", runner.calls)
	}
}

func TestRouterInterfaceReconcilesRouterSNAT(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(context.Background(), fixture.routerInterface); err != nil {
		t.Fatal(err)
	}
	snatUUID := routerSNATUUID(fixture.router.ID, fixture.routerInterface.ID)
	if !runner.contains("create NAT", snatUUID, `type="snat"`, `logical_ip="10.42.0.0/24"`) {
		t.Fatalf("router interface did not reconcile SNAT: %v", runner.calls)
	}
}

func TestRouterInterfaceMovesItsPortsAtomicallyWhenSubnetChanges(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	runner := &movingGatewayRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(context.Background(), fixture.routerInterface); err != nil {
		t.Fatal(err)
	}
	routerPort := "pvn-lrp-" + compact(fixture.routerInterface.ID)
	switchPort := "pvn-rsp-" + compact(fixture.routerInterface.ID)
	if !runner.contains("--if-exists lsp-del "+switchPort, "--if-exists lrp-del "+routerPort,
		"--may-exist lsp-add "+logicalSwitchUUID(fixture.internalNetwork.ID)+" "+switchPort) {
		t.Fatalf("router interface ports were not replaced in one OVN transaction: %v", runner.calls)
	}
}

func TestRouterInterfaceDoesNotDeletePortsOnDatabaseFailure(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	runner := &unavailableGatewayRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(context.Background(), fixture.routerInterface); err == nil || !strings.Contains(err.Error(), "database connection failed") {
		t.Fatalf("database failure = %v", err)
	}
	if runner.contains("lsp-del", "lrp-del") {
		t.Fatalf("router interface ports were deleted after inconclusive probes: %v", runner.calls)
	}
}

func TestRouterSNATDisableRemovesOnlyManagedRows(t *testing.T) {
	ctx := context.Background()
	store, fixture := newNorthSouthFixture(t)
	snatUUID := routerSNATUUID(fixture.router.ID, fixture.routerInterface.ID)
	runner := &recordingRunner{routerSNATFindOutput: snatUUID + "\n"}
	renderer := newTestRenderer(t, runner, store)

	update := *fixture.router
	update.EnableSNAT = false
	updated, _, err := store.Update(ctx, &update, fixture.router.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := renderer.Render(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("remove Logical_Router "+logicalRouterUUID(fixture.router.ID)+" nat "+snatUUID, "destroy NAT "+snatUUID) {
		t.Fatalf("stale managed SNAT was not removed: %v", runner.calls)
	}
	if runner.contains("lr-nat-del") {
		t.Fatalf("broad NAT deletion was used: %v", runner.calls)
	}
}

func TestRouterWithoutExternalGatewayCleansGatewayRouteAndSNAT(t *testing.T) {
	ctx := context.Background()
	store, fixture := newNorthSouthFixture(t)
	snatUUID := routerSNATUUID(fixture.router.ID, fixture.routerInterface.ID)
	runner := &recordingRunner{routerSNATFindOutput: snatUUID + "\n"}
	renderer := newTestRenderer(t, runner, store)

	update := *fixture.router
	update.ExternalNetworkID = ""
	update.ExternalSubnetID = ""
	update.ExternalIPAddress = ""
	update.EnableSNAT = false
	updated, _, err := store.Update(ctx, &update, fixture.router.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := renderer.Render(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("lsp-del "+gatewaySwitchPort(fixture.router.ID), "lrp-del "+gatewayRouterPort(fixture.router.ID), "destroy Logical_Router_Static_Route "+routerDefaultRouteUUID(fixture.router.ID), "destroy NAT "+snatUUID) {
		t.Fatalf("external gateway artifacts were not cleaned: %v", runner.calls)
	}
}

func TestRouterDeleteCleansNorthSouthArtifacts(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	snatUUID := routerSNATUUID(fixture.router.ID, fixture.routerInterface.ID)
	runner := &recordingRunner{routerSNATFindOutput: snatUUID + "\n"}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Delete(context.Background(), fixture.router); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("lsp-del "+gatewaySwitchPort(fixture.router.ID), "lrp-del "+gatewayRouterPort(fixture.router.ID), "lr-del "+logicalRouterUUID(fixture.router.ID), "destroy Logical_Router_Static_Route "+routerDefaultRouteUUID(fixture.router.ID), "destroy NAT "+snatUUID) {
		t.Fatalf("router north-south artifacts were not deleted: %v", runner.calls)
	}
}

func TestRouterInterfaceDeleteRemovesSNATAndReconciles(t *testing.T) {
	ctx := context.Background()
	store, fixture := newNorthSouthFixture(t)
	snatUUID := routerSNATUUID(fixture.router.ID, fixture.routerInterface.ID)
	runner := &recordingRunner{routerSNATFindOutput: snatUUID + "\n"}
	renderer := newTestRenderer(t, runner, store)
	tombstone, _, err := store.BeginDelete(ctx, model.KindRouterInterface, fixture.routerInterface.ID, fixture.routerInterface.Revision, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := renderer.Delete(ctx, tombstone); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("lsp-del pvn-rsp-"+compact(fixture.routerInterface.ID), "lrp-del pvn-lrp-"+compact(fixture.routerInterface.ID), "destroy NAT "+snatUUID) {
		t.Fatalf("router interface artifacts were not deleted: %v", runner.calls)
	}
}

func TestRouterGatewayChassisIsDeterministicAndRemovesStaleMembers(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	routerPort := gatewayRouterPort(fixture.router.ID)
	runner := &recordingRunner{gatewayChassisOutput: routerPort + "-chassis-old   100\n"}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(context.Background(), fixture.router); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("lrp-set-gateway-chassis "+routerPort+" chassis-a 32767", "lrp-set-gateway-chassis "+routerPort+" chassis-b 32766") {
		t.Fatalf("gateway chassis priorities are not deterministic: %v", runner.calls)
	}
	if !runner.contains("lrp-del-gateway-chassis " + routerPort + " chassis-old") {
		t.Fatalf("stale gateway chassis was not removed: %v", runner.calls)
	}
}

func TestRouterGatewayMovesItsSwitchPortAtomicallyWhenExternalNetworkChanges(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	runner := &movingGatewayRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(context.Background(), fixture.router); err != nil {
		t.Fatal(err)
	}
	port := gatewaySwitchPort(fixture.router.ID)
	if !runner.contains("--if-exists lsp-del "+port, "--may-exist lsp-add "+logicalSwitchUUID(fixture.externalNetwork.ID)+" "+port) {
		t.Fatalf("gateway switch port was not moved in one OVN transaction: %v", runner.calls)
	}
}

func TestRouterGatewayDoesNotDeletePortsOnDatabaseFailure(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	runner := &unavailableGatewayRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(context.Background(), fixture.router); err == nil || !strings.Contains(err.Error(), "database connection failed") {
		t.Fatalf("database failure = %v", err)
	}
	if runner.contains("lsp-del", "lrp-del") {
		t.Fatalf("gateway ports were deleted after an inconclusive ownership probe: %v", runner.calls)
	}
}

func TestNorthSouthRendererUsesOVN2503CompatibleCommands(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)
	if err := renderer.Render(context.Background(), fixture.router); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("lrp-set-gateway-chassis") || !runner.contains("create Logical_Router_Static_Route") || !runner.contains("create NAT") {
		t.Fatalf("expected OVN 25.03 north-south commands are absent: %v", runner.calls)
	}
	for _, call := range runner.calls {
		for index, argument := range call {
			if argument == "--may-exist" || argument == "--if-exists" || strings.HasPrefix(argument, "--id=") {
				if index == 0 || call[index-1] != "--" {
					t.Errorf("command option %q is not separated from global options: %v", argument, call)
				}
			}
			if argument == "lsp-add-router-port" || argument == "lsp-add-localnet-port" {
				t.Errorf("post-25.03.0 convenience command used: %v", call)
			}
		}
	}
}

func TestRouterExternalGatewayRequiresEnabledGatewayNode(t *testing.T) {
	ctx := context.Background()
	store, fixture := newNorthSouthFixture(t)
	for _, id := range []string{"node-a", "node-b"} {
		resource, err := store.Get(ctx, model.KindNode, id)
		if err != nil {
			t.Fatal(err)
		}
		node := resource.(*model.Node)
		node.Enabled = false
		if _, _, err := store.Update(ctx, node, node.Revision, ""); err != nil {
			t.Fatal(err)
		}
	}
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(ctx, fixture.router); err == nil || !strings.Contains(err.Error(), "no enabled gateway chassis") {
		t.Fatalf("missing gateway chassis error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("OVN was mutated before gateway placement validation: %v", runner.calls)
	}
}

func TestRouterRejectsInvalidExternalNetworkRelationBeforeOVNMutation(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)
	router := &model.Router{
		Metadata:          model.Metadata{ID: "router-invalid"},
		ProjectID:         fixture.project.ID,
		Name:              "invalid",
		ExternalNetworkID: fixture.internalNetwork.ID,
		ExternalSubnetID:  fixture.internalSubnet.ID,
		ExternalIPAddress: "10.42.0.10",
		EnableSNAT:        true,
	}

	if err := renderer.Render(context.Background(), router); err == nil || !strings.Contains(err.Error(), "not provider-backed and external") {
		t.Fatalf("invalid external relation error = %v", err)
	}
	otherExternal := mustCreate(t, store, &model.Network{
		Metadata: model.Metadata{ID: "external-2"}, ProjectID: fixture.project.ID, Name: "external-other",
		External: true, ProviderNetworkID: fixture.provider.ID,
	}).(*model.Network)
	otherSubnet := mustCreate(t, store, &model.Subnet{
		Metadata: model.Metadata{ID: "external-subnet-2"}, ProjectID: fixture.project.ID, NetworkID: otherExternal.ID,
		Name: "external-other-v4", CIDR: "198.51.100.0/24", GatewayIP: "198.51.100.1",
	}).(*model.Subnet)
	router.ExternalNetworkID = fixture.externalNetwork.ID
	router.ExternalSubnetID = otherSubnet.ID
	router.ExternalIPAddress = "198.51.100.10"
	if err := renderer.Render(context.Background(), router); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("external subnet relation error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("OVN was mutated for an invalid external relation: %v", runner.calls)
	}
}

func TestFloatingIPMustMatchRouterExternalProviderAndSubnet(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	secondProvider := mustCreate(t, store, &model.ProviderNetwork{Metadata: model.Metadata{ID: "provider-2"}, Name: "other"}).(*model.ProviderNetwork)
	_ = mustCreate(t, store, &model.ProviderSegment{
		Metadata: model.Metadata{ID: "segment-2"}, ProviderNetworkID: secondProvider.ID,
		Name: "flat-other", PhysicalNetwork: "other", NetworkType: model.ProviderFlat,
	})
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)

	mismatch := &model.FloatingIP{
		Metadata: model.Metadata{ID: "fip-mismatch"}, ProjectID: fixture.project.ID,
		ProviderNetworkID: secondProvider.ID, Address: "192.0.2.20", RouterID: fixture.router.ID,
		FixedIPAddress: "10.42.0.10",
	}
	if err := renderer.Render(context.Background(), mismatch); err == nil || !strings.Contains(err.Error(), "provider network does not match") {
		t.Fatalf("provider mismatch error = %v", err)
	}
	outOfSubnet := *mismatch
	outOfSubnet.ID = "fip-outside"
	outOfSubnet.ProviderNetworkID = fixture.provider.ID
	outOfSubnet.Address = "198.51.100.20"
	if err := renderer.Render(context.Background(), &outOfSubnet); err == nil || !strings.Contains(err.Error(), "outside router") {
		t.Fatalf("external subnet mismatch error = %v", err)
	}
	if runner.contains("create NAT") {
		t.Fatalf("inconsistent floating IP created NAT: %v", runner.calls)
	}
}

func TestFloatingIPOnRouterExternalProviderRendersNAT(t *testing.T) {
	store, fixture := newNorthSouthFixture(t)
	port := mustCreate(t, store, &model.Port{
		Metadata: model.Metadata{ID: "port-1"}, ProjectID: fixture.project.ID, NetworkID: fixture.internalNetwork.ID,
		Name: "vm100-net0", MACAddress: "02:00:00:00:00:10",
		FixedIPs:     []model.FixedIP{{SubnetID: fixture.internalSubnet.ID, Address: "10.42.0.10"}},
		AdminStateUp: true, BindingStatus: model.PortUnbound,
	}).(*model.Port)
	floatingIP := &model.FloatingIP{
		Metadata: model.Metadata{ID: "fip-1"}, ProjectID: fixture.project.ID,
		ProviderNetworkID: fixture.provider.ID, Address: "192.0.2.20", RouterID: fixture.router.ID,
		PortID: port.ID, FixedIPAddress: "10.42.0.10", FloatingStatus: model.FloatingIPActive,
	}
	runner := &recordingRunner{}
	renderer := newTestRenderer(t, runner, store)

	if err := renderer.Render(context.Background(), floatingIP); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("create NAT", `type="dnat_and_snat"`, `external_ip="192.0.2.20"`, `logical_ip="10.42.0.10"`, "add Logical_Router "+logicalRouterUUID(fixture.router.ID)) {
		t.Fatalf("valid floating IP NAT was not rendered: %v", runner.calls)
	}
}

func TestDerivedNamesDoNotAliasPunctuation(t *testing.T) {
	if logicalSwitch("a-b") == logicalSwitch("ab") || portGroup("a:b") == portGroup("a.b") {
		t.Fatal("derived OVN names alias distinct PVN identifiers")
	}
}

func TestActiveActiveNetworkCreateUsesOneDeterministicRow(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1"}).(*model.Project)
	network := mustCreate(t, store, &model.Network{Metadata: model.Metadata{ID: "network-1"}, ProjectID: project.ID, Name: "private"}).(*model.Network)
	runner := &activeActiveRunner{barrier: make(chan struct{}), uuid: logicalSwitchUUID(network.ID)}
	client, err := NewClient(ClientConfig{Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := NewRenderer(client, store)
	if err != nil {
		t.Fatal(err)
	}
	errorsByManager := make(chan error, 2)
	for range 2 {
		go func() { errorsByManager <- renderer.Render(context.Background(), network) }()
	}
	for range 2 {
		if err := <-errorsByManager; err != nil {
			t.Fatalf("active-active render failed: %v", err)
		}
	}
	if !runner.created || runner.creates != 2 {
		t.Fatalf("race was not exercised: created=%v attempts=%d", runner.created, runner.creates)
	}
}

func TestDeletePortIsIdempotentAndScoped(t *testing.T) {
	store := controlstore.NewMemory()
	runner := &recordingRunner{}
	client, err := NewClient(ClientConfig{Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := NewRenderer(client, store)
	if err != nil {
		t.Fatal(err)
	}
	port := &model.Port{Metadata: model.Metadata{ID: "port-1"}, LSPName: "pvn-port-1"}
	if err := renderer.Delete(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	if !runner.contains("--if-exists lsp-del pvn-port-1") {
		t.Fatalf("scoped idempotent delete missing: %v", runner.calls)
	}
}

func TestRendererRejectsUnsafeResourceIDBeforeExecuting(t *testing.T) {
	runner := &recordingRunner{}
	client, err := NewClient(ClientConfig{Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	store := controlstore.NewMemory()
	renderer, err := NewRenderer(client, store)
	if err != nil {
		t.Fatal(err)
	}
	resource := &model.Network{Metadata: model.Metadata{ID: "../../bad"}, ProjectID: "project-1", Name: "bad", MTU: 1400}
	if err := renderer.Render(context.Background(), resource); err == nil {
		t.Fatal("unsafe ID unexpectedly rendered")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("OVN was called for unsafe ID: %v", runner.calls)
	}
}

func mustCreate(t *testing.T, store controlstore.Store, resource model.Resource) model.Resource {
	t.Helper()
	created, _, err := store.Create(context.Background(), resource, "")
	if err != nil {
		t.Fatal(err)
	}
	return created
}

type northSouthFixture struct {
	project         *model.Project
	provider        *model.ProviderNetwork
	externalNetwork *model.Network
	externalSubnet  *model.Subnet
	internalNetwork *model.Network
	internalSubnet  *model.Subnet
	router          *model.Router
	routerInterface *model.RouterInterface
}

func newNorthSouthFixture(t *testing.T) (*controlstore.Memory, northSouthFixture) {
	t.Helper()
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{
		Metadata: model.Metadata{ID: "project-1"}, Name: "tenant", PoolID: "pool-1",
	}).(*model.Project)
	provider := mustCreate(t, store, &model.ProviderNetwork{
		Metadata: model.Metadata{ID: "provider-1"}, Name: "uplink", Shared: true,
	}).(*model.ProviderNetwork)
	_ = mustCreate(t, store, &model.ProviderSegment{
		Metadata: model.Metadata{ID: "segment-1"}, ProviderNetworkID: provider.ID,
		Name: "flat-provider", PhysicalNetwork: "provider", NetworkType: model.ProviderFlat,
	})
	externalNetwork := mustCreate(t, store, &model.Network{
		Metadata: model.Metadata{ID: "external-1"}, ProjectID: project.ID, Name: "external",
		External: true, ProviderNetworkID: provider.ID,
	}).(*model.Network)
	externalSubnet := mustCreate(t, store, &model.Subnet{
		Metadata: model.Metadata{ID: "external-subnet-1"}, ProjectID: project.ID, NetworkID: externalNetwork.ID,
		Name: "external-v4", CIDR: "192.0.2.0/24", GatewayIP: "192.0.2.1",
	}).(*model.Subnet)
	internalNetwork := mustCreate(t, store, &model.Network{
		Metadata: model.Metadata{ID: "internal-1"}, ProjectID: project.ID, Name: "private",
	}).(*model.Network)
	internalSubnet := mustCreate(t, store, &model.Subnet{
		Metadata: model.Metadata{ID: "internal-subnet-1"}, ProjectID: project.ID, NetworkID: internalNetwork.ID,
		Name: "private-v4", CIDR: "10.42.0.0/24", GatewayIP: "10.42.0.1",
	}).(*model.Subnet)
	for _, node := range []*model.Node{
		{Metadata: model.Metadata{ID: "node-b"}, Name: "pve-b", ChassisID: "chassis-b", Roles: []model.NodeRole{model.NodeRoleCompute, model.NodeRoleGateway}, Enabled: true},
		{Metadata: model.Metadata{ID: "node-a"}, Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleGateway}, Enabled: true},
		{Metadata: model.Metadata{ID: "node-disabled"}, Name: "pve-disabled", ChassisID: "chassis-disabled", Roles: []model.NodeRole{model.NodeRoleGateway}, Enabled: false},
	} {
		_ = mustCreate(t, store, node)
	}
	router := mustCreate(t, store, &model.Router{
		Metadata: model.Metadata{ID: "router-1"}, ProjectID: project.ID, Name: "edge",
		ExternalNetworkID: externalNetwork.ID, ExternalSubnetID: externalSubnet.ID,
		ExternalIPAddress: "192.0.2.10", EnableSNAT: true,
	}).(*model.Router)
	routerInterface := mustCreate(t, store, &model.RouterInterface{
		Metadata: model.Metadata{ID: "router-interface-1"}, ProjectID: project.ID,
		RouterID: router.ID, SubnetID: internalSubnet.ID,
	}).(*model.RouterInterface)
	return store, northSouthFixture{
		project: project, provider: provider, externalNetwork: externalNetwork, externalSubnet: externalSubnet,
		internalNetwork: internalNetwork, internalSubnet: internalSubnet, router: router, routerInterface: routerInterface,
	}
}

func newTestRenderer(t *testing.T, runner Runner, store controlstore.Store) *Renderer {
	t.Helper()
	client, err := NewClient(ClientConfig{Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := NewRenderer(client, store)
	if err != nil {
		t.Fatal(err)
	}
	return renderer
}
