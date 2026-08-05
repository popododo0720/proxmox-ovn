package ovsdbstore

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pvnstack/proxmox-ovn/internal/controlstore"
	"github.com/pvnstack/proxmox-ovn/internal/model"
)

type invariantTopology struct {
	project, otherProject                 *model.Project
	provider, otherProvider               *model.ProviderNetwork
	segment, otherSegment                 *model.ProviderSegment
	privateNetwork, otherPrivateNetwork   *model.Network
	privateSubnet, alternateSubnet        *model.Subnet
	otherPrivateSubnet                    *model.Subnet
	externalNetwork, otherExternalNetwork *model.Network
	externalSubnet, otherExternalSubnet   *model.Subnet
	node                                  *model.Node
	group, remoteGroup, otherGroup        *model.SecurityGroup
	port, alternatePort, otherPort        *model.Port
	router, otherRouter                   *model.Router
}

func TestStoreSerializesCrossKindExternalAddressClaims(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	topology := buildInvariantTopology(t, store)
	candidates := []model.Resource{
		&model.Router{ProjectID: topology.project.ID, Name: "racing-router", ExternalNetworkID: topology.externalNetwork.ID, ExternalSubnetID: topology.externalSubnet.ID, ExternalIPAddress: "192.0.2.70"},
		&model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.70"},
	}
	start := make(chan struct{})
	errorsSeen := make(chan error, len(candidates))
	var workers sync.WaitGroup
	for index, candidate := range candidates {
		workers.Add(1)
		go func(index int, candidate model.Resource) {
			defer workers.Done()
			<-start
			_, _, err := store.Create(context.Background(), candidate, "race-create-"+string(rune('a'+index)))
			errorsSeen <- err
		}(index, candidate)
	}
	close(start)
	workers.Wait()
	close(errorsSeen)
	succeeded, conflicted := 0, 0
	for err := range errorsSeen {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, controlstore.ErrAlreadyExists):
			conflicted++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func TestStoreSerializesOverlappingSubnetCIDRs(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	project := mustCreate(t, store, &model.Project{Name: "tenant-overlap", PoolID: "pool-overlap"}, "overlap-project").(*model.Project)
	network := mustCreate(t, store, &model.Network{ProjectID: project.ID, Name: "overlap-network"}, "overlap-network").(*model.Network)
	candidates := []*model.Subnet{
		{ProjectID: project.ID, NetworkID: network.ID, Name: "larger", CIDR: "10.50.0.0/24"},
		{ProjectID: project.ID, NetworkID: network.ID, Name: "nested", CIDR: "10.50.0.0/25"},
	}
	start := make(chan struct{})
	errorsSeen := make(chan error, len(candidates))
	var workers sync.WaitGroup
	for index, candidate := range candidates {
		workers.Add(1)
		go func(index int, candidate *model.Subnet) {
			defer workers.Done()
			<-start
			_, _, err := store.Create(context.Background(), candidate, "overlap-create-"+string(rune('a'+index)))
			errorsSeen <- err
		}(index, candidate)
	}
	close(start)
	workers.Wait()
	close(errorsSeen)
	succeeded, conflicted := 0, 0
	for err := range errorsSeen {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, controlstore.ErrAlreadyExists):
			conflicted++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func buildInvariantTopology(t *testing.T, store controlstore.Store) invariantTopology {
	t.Helper()
	ctx := context.Background()
	topology := invariantTopology{}
	topology.project = mustCreate(t, store, &model.Project{Name: "tenant-a", PoolID: "pool-a"}, "inv-project-a").(*model.Project)
	topology.otherProject = mustCreate(t, store, &model.Project{Name: "tenant-b", PoolID: "pool-b"}, "inv-project-b").(*model.Project)

	topology.provider = mustCreate(t, store, &model.ProviderNetwork{Name: "provider-a"}, "inv-provider-a").(*model.ProviderNetwork)
	topology.segment = mustCreate(t, store, &model.ProviderSegment{ProviderNetworkID: topology.provider.ID, Name: "segment-a", PhysicalNetwork: "phys-a", NetworkType: model.ProviderVLAN, VLANID: 101}, "inv-segment-a").(*model.ProviderSegment)
	topology.provider.DefaultSegmentID = topology.segment.ID
	updated, _, err := store.Update(ctx, topology.provider, topology.provider.Revision, "inv-provider-a-default")
	if err != nil {
		t.Fatal(err)
	}
	topology.provider = updated.(*model.ProviderNetwork)
	topology.otherProvider = mustCreate(t, store, &model.ProviderNetwork{Name: "provider-b", Shared: true}, "inv-provider-b").(*model.ProviderNetwork)
	topology.otherSegment = mustCreate(t, store, &model.ProviderSegment{ProviderNetworkID: topology.otherProvider.ID, Name: "segment-b", PhysicalNetwork: "phys-b", NetworkType: model.ProviderVLAN, VLANID: 102}, "inv-segment-b").(*model.ProviderSegment)

	topology.privateNetwork = mustCreate(t, store, &model.Network{ProjectID: topology.project.ID, Name: "private-a"}, "inv-private-a").(*model.Network)
	topology.privateSubnet = mustCreate(t, store, &model.Subnet{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "private-a-v4", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"}, "inv-private-a-subnet").(*model.Subnet)
	topology.alternateSubnet = mustCreate(t, store, &model.Subnet{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "private-a-alt", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"}, "inv-private-a-alt").(*model.Subnet)
	topology.otherPrivateNetwork = mustCreate(t, store, &model.Network{ProjectID: topology.otherProject.ID, Name: "private-b"}, "inv-private-b").(*model.Network)
	topology.otherPrivateSubnet = mustCreate(t, store, &model.Subnet{ProjectID: topology.otherProject.ID, NetworkID: topology.otherPrivateNetwork.ID, Name: "private-b-v4", CIDR: "10.1.0.0/24", GatewayIP: "10.1.0.1"}, "inv-private-b-subnet").(*model.Subnet)

	topology.externalNetwork = mustCreate(t, store, &model.Network{ProjectID: topology.project.ID, Name: "external-a", External: true, ProviderNetworkID: topology.provider.ID}, "inv-external-a").(*model.Network)
	topology.externalSubnet = mustCreate(t, store, &model.Subnet{ProjectID: topology.project.ID, NetworkID: topology.externalNetwork.ID, Name: "external-a-v4", CIDR: "192.0.2.0/24", GatewayIP: "192.0.2.1", AllocationPools: []model.IPRange{{Start: "192.0.2.2", End: "192.0.2.200"}}}, "inv-external-a-subnet").(*model.Subnet)
	topology.otherExternalNetwork = mustCreate(t, store, &model.Network{ProjectID: topology.otherProject.ID, Name: "external-b", External: true, ProviderNetworkID: topology.otherProvider.ID}, "inv-external-b").(*model.Network)
	topology.otherExternalSubnet = mustCreate(t, store, &model.Subnet{ProjectID: topology.otherProject.ID, NetworkID: topology.otherExternalNetwork.ID, Name: "external-b-v4", CIDR: "198.51.100.0/24", GatewayIP: "198.51.100.1"}, "inv-external-b-subnet").(*model.Subnet)

	topology.node = mustCreate(t, store, &model.Node{Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "inv-node").(*model.Node)
	topology.group = mustCreate(t, store, &model.SecurityGroup{ProjectID: topology.project.ID, Name: "default"}, "inv-group").(*model.SecurityGroup)
	topology.remoteGroup = mustCreate(t, store, &model.SecurityGroup{ProjectID: topology.project.ID, Name: "remote"}, "inv-remote-group").(*model.SecurityGroup)
	topology.otherGroup = mustCreate(t, store, &model.SecurityGroup{ProjectID: topology.otherProject.ID, Name: "other"}, "inv-other-group").(*model.SecurityGroup)
	topology.port = mustCreate(t, store, &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "port-a", MACAddress: "02:00:00:00:10:01", FixedIPs: []model.FixedIP{{SubnetID: topology.privateSubnet.ID, Address: "10.0.0.10"}}, SecurityGroupIDs: []string{topology.group.ID}, NodeID: topology.node.ID, RequestedChassis: topology.node.ChassisID}, "inv-port-a").(*model.Port)
	topology.alternatePort = mustCreate(t, store, &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "port-alt", MACAddress: "02:00:00:00:10:02", FixedIPs: []model.FixedIP{{SubnetID: topology.alternateSubnet.ID, Address: "10.0.1.10"}}}, "inv-port-alt").(*model.Port)
	topology.otherPort = mustCreate(t, store, &model.Port{ProjectID: topology.otherProject.ID, NetworkID: topology.otherPrivateNetwork.ID, Name: "port-b", MACAddress: "02:00:00:00:20:01", FixedIPs: []model.FixedIP{{SubnetID: topology.otherPrivateSubnet.ID, Address: "10.1.0.10"}}}, "inv-port-b").(*model.Port)

	topology.router = mustCreate(t, store, &model.Router{ProjectID: topology.project.ID, Name: "router-a", ExternalNetworkID: topology.externalNetwork.ID, ExternalSubnetID: topology.externalSubnet.ID, ExternalIPAddress: "192.0.2.2", EnableSNAT: true}, "inv-router-a").(*model.Router)
	topology.otherRouter = mustCreate(t, store, &model.Router{ProjectID: topology.otherProject.ID, Name: "router-b", ExternalNetworkID: topology.otherExternalNetwork.ID, ExternalSubnetID: topology.otherExternalSubnet.ID, ExternalIPAddress: "198.51.100.2", EnableSNAT: true}, "inv-router-b").(*model.Router)
	return topology
}

func expectInvariantConflict(t *testing.T, store controlstore.Store, resource model.Resource, key string) {
	t.Helper()
	if _, _, err := store.Create(context.Background(), resource, key); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Create(%s) error=%v, want conflict", resource.ResourceKind(), err)
	}
}

func TestStoreEnforcesCrossResourceInvariants(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	topology := buildInvariantTopology(t, store)
	pooledSubnet := mustCreate(t, store, &model.Subnet{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "private-pooled", CIDR: "10.0.2.0/24", AllocationPools: []model.IPRange{{Start: "10.0.2.10", End: "10.0.2.20"}}}, "inv-private-pooled").(*model.Subnet)
	mustCreate(t, store, &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "pooled-port", MACAddress: "02:00:00:00:30:10", FixedIPs: []model.FixedIP{{SubnetID: pooledSubnet.ID, Address: "10.0.2.10"}}}, "inv-pooled-port")
	mustCreate(t, store, &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: pooledSubnet.ID, Address: "10.0.2.20", State: model.IPReserved}, "inv-pooled-allocation")
	if _, _, err := store.Create(context.Background(), &model.Port{
		ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "duplicate-fixed-ip",
		MACAddress: "02:00:00:00:30:00", FixedIPs: []model.FixedIP{{SubnetID: topology.privateSubnet.ID, Address: "10.0.0.10"}},
	}, "inv-duplicate-fixed-ip"); !errors.Is(err, controlstore.ErrAlreadyExists) {
		t.Fatalf("duplicate fixed IP error=%v", err)
	}
	for name, resource := range map[string]model.Resource{
		"duplicate router external IP": &model.Router{ProjectID: topology.project.ID, Name: "duplicate-router-ip", ExternalNetworkID: topology.externalNetwork.ID, ExternalSubnetID: topology.externalSubnet.ID, ExternalIPAddress: topology.router.ExternalIPAddress},
		"floating IP equals router IP": &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: topology.router.ExternalIPAddress},
	} {
		if _, _, err := store.Create(context.Background(), resource, "inv-address-"+name); !errors.Is(err, controlstore.ErrAlreadyExists) {
			t.Fatalf("%s error=%v", name, err)
		}
	}
	expectInvariantConflict(t, store, &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: topology.externalSubnet.GatewayIP}, "inv-floating-gateway")
	mustCreate(t, store, &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.50"}, "inv-reserved-floating")
	deleteProvider := mustCreate(t, store, &model.ProviderNetwork{Name: "provider-delete-check"}, "inv-provider-delete-check").(*model.ProviderNetwork)
	deleteNetwork := mustCreate(t, store, &model.Network{ProjectID: topology.project.ID, Name: "external-delete-check", External: true, ProviderNetworkID: deleteProvider.ID}, "inv-network-delete-check").(*model.Network)
	deleteSubnet := mustCreate(t, store, &model.Subnet{ProjectID: topology.project.ID, NetworkID: deleteNetwork.ID, Name: "external-delete-check", CIDR: "172.20.0.0/24"}, "inv-subnet-delete-check").(*model.Subnet)
	mustCreate(t, store, &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: deleteProvider.ID, Address: "172.20.0.10"}, "inv-floating-delete-check")
	if _, err := store.Delete(context.Background(), model.KindSubnet, deleteSubnet.ID, deleteSubnet.Revision, "inv-delete-floating-subnet"); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("delete sole floating IP subnet error=%v", err)
	}

	wrongDefault := *topology.provider
	wrongDefault.DefaultSegmentID = topology.otherSegment.ID
	if _, _, err := store.Update(context.Background(), &wrongDefault, topology.provider.Revision, "inv-wrong-default"); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("provider default segment error=%v", err)
	}

	cases := []struct {
		name     string
		resource model.Resource
	}{
		{"port fixed IP outside subnet", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-ip", MACAddress: "02:00:00:00:30:01", FixedIPs: []model.FixedIP{{SubnetID: topology.privateSubnet.ID, Address: "10.9.0.10"}}}},
		{"port fixed IP is network", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-network", MACAddress: "02:00:00:00:30:11", FixedIPs: []model.FixedIP{{SubnetID: topology.privateSubnet.ID, Address: "10.0.0.0"}}}},
		{"port fixed IP is gateway", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-gateway", MACAddress: "02:00:00:00:30:12", FixedIPs: []model.FixedIP{{SubnetID: topology.privateSubnet.ID, Address: "10.0.0.1"}}}},
		{"port fixed IP is broadcast", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-broadcast", MACAddress: "02:00:00:00:30:13", FixedIPs: []model.FixedIP{{SubnetID: topology.privateSubnet.ID, Address: "10.0.0.255"}}}},
		{"port fixed IP outside pool", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-pool", MACAddress: "02:00:00:00:30:14", FixedIPs: []model.FixedIP{{SubnetID: pooledSubnet.ID, Address: "10.0.2.21"}}}},
		{"port uses provider network", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.externalNetwork.ID, Name: "bad-provider-port", MACAddress: "02:00:00:00:30:05", FixedIPs: []model.FixedIP{{SubnetID: topology.externalSubnet.ID, Address: "192.0.2.60"}}}},
		{"port security group crosses project", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-sg", MACAddress: "02:00:00:00:30:02", SecurityGroupIDs: []string{topology.otherGroup.ID}}},
		{"port chassis has no node", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-node", MACAddress: "02:00:00:00:30:03", RequestedChassis: topology.node.ChassisID}},
		{"port chassis mismatches node", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-chassis", MACAddress: "02:00:00:00:30:04", NodeID: topology.node.ID, RequestedChassis: "another-chassis"}},
		{"allocation outside subnet", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, Address: "10.9.0.10", State: model.IPReserved}},
		{"allocation is network", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, Address: "10.0.0.0", State: model.IPReserved}},
		{"allocation is gateway", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, Address: "10.0.0.1", State: model.IPReserved}},
		{"allocation is broadcast", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, Address: "10.0.0.255", State: model.IPReserved}},
		{"allocation outside pool", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: pooledSubnet.ID, Address: "10.0.2.21", State: model.IPReserved}},
		{"allocation crosses project", &model.IPAllocation{ProjectID: topology.otherProject.ID, SubnetID: topology.privateSubnet.ID, Address: "10.0.0.20", State: model.IPReserved}},
		{"allocation port crosses network", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.otherPort.ID, Address: "10.0.0.20", State: model.IPAllocated}},
		{"allocation address differs from port", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.port.ID, Address: "10.0.0.20", State: model.IPAllocated}},
		{"router subnet differs from external network", &model.Router{ProjectID: topology.project.ID, Name: "bad-router-subnet", ExternalNetworkID: topology.externalNetwork.ID, ExternalSubnetID: topology.privateSubnet.ID, ExternalIPAddress: "10.0.0.2"}},
		{"router address outside external subnet", &model.Router{ProjectID: topology.project.ID, Name: "bad-router-ip", ExternalNetworkID: topology.externalNetwork.ID, ExternalSubnetID: topology.externalSubnet.ID, ExternalIPAddress: "203.0.113.2"}},
		{"router address outside allocation pool", &model.Router{ProjectID: topology.project.ID, Name: "bad-router-pool", ExternalNetworkID: topology.externalNetwork.ID, ExternalSubnetID: topology.externalSubnet.ID, ExternalIPAddress: "192.0.2.201"}},
		{"router interface crosses router project", &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.otherRouter.ID, SubnetID: topology.privateSubnet.ID}},
		{"router interface uses external network", &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.router.ID, SubnetID: topology.externalSubnet.ID}},
		{"router interface port crosses network", &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.router.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.otherPort.ID}},
		{"router interface port lacks subnet", &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.router.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.alternatePort.ID}},
		{"floating IP provider differs from router", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.otherProvider.ID, Address: "192.0.2.20", RouterID: topology.router.ID}},
		{"floating IP outside external subnet", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "203.0.113.20", RouterID: topology.router.ID}},
		{"floating IP outside allocation pool", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.201"}},
		{"floating IP port lacks fixed address", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.21", RouterID: topology.router.ID, PortID: topology.port.ID}},
		{"floating IP fixed address lacks port", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.22", RouterID: topology.router.ID, FixedIPAddress: "10.0.0.10"}},
		{"floating IP association lacks router", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.23", PortID: topology.port.ID, FixedIPAddress: "10.0.0.10"}},
		{"floating IP port crosses project", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.24", RouterID: topology.router.ID, PortID: topology.otherPort.ID, FixedIPAddress: "10.1.0.10"}},
		{"floating IP fixed address not on port", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.25", RouterID: topology.router.ID, PortID: topology.port.ID, FixedIPAddress: "10.0.0.99"}},
		{"floating IP router lacks port subnet", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.26", RouterID: topology.router.ID, PortID: topology.port.ID, FixedIPAddress: "10.0.0.10"}},
		{"security group rule crosses group project", &model.SecurityGroupRule{ProjectID: topology.project.ID, SecurityGroupID: topology.otherGroup.ID, Direction: model.DirectionIngress, EtherType: model.EtherTypeIPv4, Action: model.ActionAllow}},
		{"security group rule crosses remote project", &model.SecurityGroupRule{ProjectID: topology.project.ID, SecurityGroupID: topology.group.ID, RemoteGroupID: topology.otherGroup.ID, Direction: model.DirectionIngress, EtherType: model.EtherTypeIPv4, Action: model.ActionAllow}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			expectInvariantConflict(t, store, test.resource, "inv-reject-"+test.name)
		})
	}

	nonShared := mustCreate(t, store, &model.ProviderNetwork{Name: "provider-private"}, "inv-provider-private").(*model.ProviderNetwork)
	otherExternal := mustCreate(t, store, &model.Network{ProjectID: topology.otherProject.ID, Name: "external-private", External: true, ProviderNetworkID: nonShared.ID}, "inv-external-private").(*model.Network)
	otherSubnet := mustCreate(t, store, &model.Subnet{ProjectID: topology.otherProject.ID, NetworkID: otherExternal.ID, Name: "external-private-v4", CIDR: "203.0.113.0/24", GatewayIP: "203.0.113.1"}, "inv-external-private-subnet").(*model.Subnet)
	expectInvariantConflict(t, store, &model.Router{ProjectID: topology.project.ID, Name: "cross-project-private", ExternalNetworkID: otherExternal.ID, ExternalSubnetID: otherSubnet.ID, ExternalIPAddress: "203.0.113.2"}, "inv-cross-project-private")
	mustCreate(t, store, &model.Router{ProjectID: topology.project.ID, Name: "cross-project-shared", ExternalNetworkID: topology.otherExternalNetwork.ID, ExternalSubnetID: topology.otherExternalSubnet.ID, ExternalIPAddress: "198.51.100.3"}, "inv-cross-project-shared")

	if _, err := store.Delete(context.Background(), model.KindProviderSegment, topology.segment.ID, topology.segment.Revision, "inv-delete-default-segment"); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("delete default segment error=%v", err)
	}
}

func TestStoreReplacementPreservesCrossResourceInvariants(t *testing.T) {
	store := deterministicStore(newFakeDatabase())
	topology := buildInvariantTopology(t, store)
	mustCreate(t, store, &model.SecurityGroupRule{ProjectID: topology.project.ID, SecurityGroupID: topology.group.ID, RemoteGroupID: topology.remoteGroup.ID, Direction: model.DirectionIngress, EtherType: model.EtherTypeIPv4, Action: model.ActionAllow}, "inv-rule")
	mustCreate(t, store, &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.port.ID, Address: "10.0.0.10", State: model.IPAllocated}, "inv-allocation")
	routerInterface := mustCreate(t, store, &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.router.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.port.ID}, "inv-interface").(*model.RouterInterface)
	mustCreate(t, store, &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.200", RouterID: topology.router.ID, PortID: topology.port.ID, FixedIPAddress: "10.0.0.10"}, "inv-floating")
	reservedFloating := mustCreate(t, store, &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "192.0.2.50"}, "inv-reserved-floating").(*model.FloatingIP)
	if _, err := store.Delete(context.Background(), model.KindRouterInterface, routerInterface.ID, routerInterface.Revision, "inv-delete-interface"); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("delete router interface used by floating IP error=%v", err)
	}
	routerAddressCollision := *topology.router
	routerAddressCollision.ExternalIPAddress = reservedFloating.Address
	if _, _, err := store.Update(context.Background(), &routerAddressCollision, topology.router.Revision, "inv-router-address-collision"); !errors.Is(err, controlstore.ErrAlreadyExists) {
		t.Fatalf("router address collision error=%v", err)
	}
	gatewayAddressCollision := *topology.externalSubnet
	gatewayAddressCollision.GatewayIP = reservedFloating.Address
	if _, _, err := store.Update(context.Background(), &gatewayAddressCollision, topology.externalSubnet.Revision, "inv-gateway-address-collision"); !errors.Is(err, controlstore.ErrAlreadyExists) {
		t.Fatalf("gateway address collision error=%v", err)
	}
	overlappingSubnet := *topology.alternateSubnet
	overlappingSubnet.CIDR = "10.0.0.128/25"
	overlappingSubnet.GatewayIP = "10.0.0.129"
	if _, _, err := store.Update(context.Background(), &overlappingSubnet, topology.alternateSubnet.Revision, "inv-overlapping-subnet"); !errors.Is(err, controlstore.ErrAlreadyExists) {
		t.Fatalf("overlapping subnet update error=%v", err)
	}

	updates := []struct {
		name     string
		resource model.Resource
		revision int64
	}{
		{"move default segment", func() model.Resource {
			candidate := *topology.segment
			candidate.ProviderNetworkID = topology.otherProvider.ID
			return &candidate
		}(), topology.segment.Revision},
		{"change selected node chassis", func() model.Resource {
			candidate := *topology.node
			candidate.ChassisID = "new-chassis"
			return &candidate
		}(), topology.node.Revision},
		{"move security group project", func() model.Resource {
			candidate := *topology.group
			candidate.ProjectID = topology.otherProject.ID
			return &candidate
		}(), topology.group.Revision},
		{"shrink subnet around fixed IP", func() model.Resource {
			candidate := *topology.privateSubnet
			candidate.CIDR = "10.0.0.0/29"
			return &candidate
		}(), topology.privateSubnet.Revision},
		{"move gateway onto fixed IP", func() model.Resource {
			candidate := *topology.privateSubnet
			candidate.GatewayIP = "10.0.0.10"
			return &candidate
		}(), topology.privateSubnet.Revision},
		{"exclude fixed IP from allocation pools", func() model.Resource {
			candidate := *topology.privateSubnet
			candidate.AllocationPools = []model.IPRange{{Start: "10.0.0.20", End: "10.0.0.30"}}
			return &candidate
		}(), topology.privateSubnet.Revision},
		{"remove allocated port address", func() model.Resource { candidate := *topology.port; candidate.FixedIPs = nil; return &candidate }(), topology.port.Revision},
		{"move floating IP router interface", func() model.Resource {
			candidate := *routerInterface
			candidate.SubnetID = topology.alternateSubnet.ID
			candidate.PortID = topology.alternatePort.ID
			return &candidate
		}(), routerInterface.Revision},
		{"make router external network internal", func() model.Resource {
			candidate := *topology.externalNetwork
			candidate.External = false
			return &candidate
		}(), topology.externalNetwork.Revision},
	}
	for _, test := range updates {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := store.Update(context.Background(), test.resource, test.revision, "inv-update-"+test.name); !errors.Is(err, controlstore.ErrConflict) {
				t.Fatalf("Update(%s) error=%v, want conflict", test.resource.ResourceKind(), err)
			}
		})
	}

	narrowNetwork := mustCreate(t, store, &model.Network{ProjectID: topology.project.ID, Name: "external-narrow", External: true, ProviderNetworkID: topology.provider.ID}, "inv-external-narrow-network").(*model.Network)
	narrow := mustCreate(t, store, &model.Subnet{ProjectID: topology.project.ID, NetworkID: narrowNetwork.ID, Name: "external-narrow", CIDR: "192.0.2.0/28", GatewayIP: "192.0.2.14"}, "inv-external-narrow").(*model.Subnet)
	router := *topology.router
	router.ExternalNetworkID = narrowNetwork.ID
	router.ExternalSubnetID = narrow.ID
	if _, _, err := store.Update(context.Background(), &router, topology.router.Revision, "inv-narrow-router-subnet"); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("router update that strands floating IP error=%v", err)
	}

	mustCreate(t, store, &model.Router{ProjectID: topology.project.ID, Name: "shared-router", ExternalNetworkID: topology.otherExternalNetwork.ID, ExternalSubnetID: topology.otherExternalSubnet.ID, ExternalIPAddress: "198.51.100.3"}, "inv-shared-router")
	provider := *topology.otherProvider
	provider.Shared = false
	if _, _, err := store.Update(context.Background(), &provider, topology.otherProvider.Revision, "inv-disable-sharing"); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("provider update that strands cross-project router error=%v", err)
	}
}
