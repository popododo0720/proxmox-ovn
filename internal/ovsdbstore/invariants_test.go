package ovsdbstore

import (
	"context"
	"errors"
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
	topology.externalSubnet = mustCreate(t, store, &model.Subnet{ProjectID: topology.project.ID, NetworkID: topology.externalNetwork.ID, Name: "external-a-v4", CIDR: "192.0.2.0/24", GatewayIP: "192.0.2.1"}, "inv-external-a-subnet").(*model.Subnet)
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
	if _, _, err := store.Create(context.Background(), &model.Port{
		ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "duplicate-fixed-ip",
		MACAddress: "02:00:00:00:30:00", FixedIPs: []model.FixedIP{{SubnetID: topology.privateSubnet.ID, Address: "10.0.0.10"}},
	}, "inv-duplicate-fixed-ip"); !errors.Is(err, controlstore.ErrAlreadyExists) {
		t.Fatalf("duplicate fixed IP error=%v", err)
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
		{"port security group crosses project", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-sg", MACAddress: "02:00:00:00:30:02", SecurityGroupIDs: []string{topology.otherGroup.ID}}},
		{"port chassis has no node", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-node", MACAddress: "02:00:00:00:30:03", RequestedChassis: topology.node.ChassisID}},
		{"port chassis mismatches node", &model.Port{ProjectID: topology.project.ID, NetworkID: topology.privateNetwork.ID, Name: "bad-port-chassis", MACAddress: "02:00:00:00:30:04", NodeID: topology.node.ID, RequestedChassis: "another-chassis"}},
		{"allocation outside subnet", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, Address: "10.9.0.10", State: model.IPReserved}},
		{"allocation crosses project", &model.IPAllocation{ProjectID: topology.otherProject.ID, SubnetID: topology.privateSubnet.ID, Address: "10.0.0.20", State: model.IPReserved}},
		{"allocation port crosses network", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.otherPort.ID, Address: "10.0.0.20", State: model.IPAllocated}},
		{"allocation address differs from port", &model.IPAllocation{ProjectID: topology.project.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.port.ID, Address: "10.0.0.20", State: model.IPAllocated}},
		{"router subnet differs from external network", &model.Router{ProjectID: topology.project.ID, Name: "bad-router-subnet", ExternalNetworkID: topology.externalNetwork.ID, ExternalSubnetID: topology.privateSubnet.ID, ExternalIPAddress: "10.0.0.2"}},
		{"router address outside external subnet", &model.Router{ProjectID: topology.project.ID, Name: "bad-router-ip", ExternalNetworkID: topology.externalNetwork.ID, ExternalSubnetID: topology.externalSubnet.ID, ExternalIPAddress: "203.0.113.2"}},
		{"router interface crosses router project", &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.otherRouter.ID, SubnetID: topology.privateSubnet.ID}},
		{"router interface uses external network", &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.router.ID, SubnetID: topology.externalSubnet.ID}},
		{"router interface port crosses network", &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.router.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.otherPort.ID}},
		{"router interface port lacks subnet", &model.RouterInterface{ProjectID: topology.project.ID, RouterID: topology.router.ID, SubnetID: topology.privateSubnet.ID, PortID: topology.alternatePort.ID}},
		{"floating IP provider differs from router", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.otherProvider.ID, Address: "192.0.2.20", RouterID: topology.router.ID}},
		{"floating IP outside external subnet", &model.FloatingIP{ProjectID: topology.project.ID, ProviderNetworkID: topology.provider.ID, Address: "203.0.113.20", RouterID: topology.router.ID}},
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
	if _, err := store.Delete(context.Background(), model.KindRouterInterface, routerInterface.ID, routerInterface.Revision, "inv-delete-interface"); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("delete router interface used by floating IP error=%v", err)
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

	narrow := mustCreate(t, store, &model.Subnet{ProjectID: topology.project.ID, NetworkID: topology.externalNetwork.ID, Name: "external-narrow", CIDR: "192.0.2.0/28", GatewayIP: "192.0.2.1"}, "inv-external-narrow").(*model.Subnet)
	router := *topology.router
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
