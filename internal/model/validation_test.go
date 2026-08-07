package model

import (
	"testing"
)

func TestNewSupportsEveryKind(t *testing.T) {
	for _, kind := range Kinds() {
		resource, err := New(kind)
		if err != nil {
			t.Fatalf("New(%s): %v", kind, err)
		}
		if resource.ResourceKind() != kind {
			t.Fatalf("New(%s) returned %s", kind, resource.ResourceKind())
		}
		if resource.GetMetadata() == nil {
			t.Fatalf("New(%s) has nil metadata", kind)
		}
	}
}

func TestCollectionRoundTrip(t *testing.T) {
	for _, kind := range Kinds() {
		parsed, err := ParseCollection(kind.Collection())
		if err != nil || parsed != kind {
			t.Fatalf("ParseCollection(%q) = %q, %v", kind.Collection(), parsed, err)
		}
	}
	if _, err := ParseCollection("not-real"); err == nil {
		t.Fatal("unknown collection was accepted")
	}
}

func TestResourceValidation(t *testing.T) {
	tests := []struct {
		name     string
		resource Resource
		valid    bool
	}{
		{"network", &Network{Name: "private", MTU: 1400}, true},
		{"external missing provider", &Network{Name: "public", MTU: 1500, External: true}, false},
		{"subnet", &Subnet{NetworkID: "n", Name: "v4", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1", DNSNameservers: []string{"1.1.1.1"}, DNSDomain: "guest.example", DNSSearchDomains: []string{"guest.example", "svc.example."}, AllocationPools: []IPRange{{Start: "10.0.0.10", End: "10.0.0.20"}}}, true},
		{"subnet invalid DNS domain", &Subnet{NetworkID: "n", Name: "v4", CIDR: "10.0.0.0/24", DNSDomain: "-guest.example"}, false},
		{"subnet duplicate DNS search domain", &Subnet{NetworkID: "n", Name: "v4", CIDR: "10.0.0.0/24", DNSSearchDomains: []string{"guest.example", "GUEST.EXAMPLE."}}, false},
		{"subnet implicit gateway", &Subnet{NetworkID: "n", Name: "implicit", CIDR: "10.0.1.0/24"}, true},
		{"subnet too narrow", &Subnet{NetworkID: "n", Name: "narrow", CIDR: "10.0.2.0/31"}, false},
		{"subnet gateway is network", &Subnet{NetworkID: "n", Name: "network-gateway", CIDR: "10.0.3.0/24", GatewayIP: "10.0.3.0"}, false},
		{"subnet pool outside", &Subnet{NetworkID: "n", Name: "v4", CIDR: "10.0.0.0/24", AllocationPools: []IPRange{{Start: "10.1.0.1", End: "10.1.0.2"}}}, false},
		{"ipv6 rejected", &Subnet{NetworkID: "n", Name: "v6", CIDR: "2001:db8::/64"}, false},
		{"port", &Port{NetworkID: "n", Name: "vm-100-net0", MACAddress: "02:00:00:00:00:01", NIC: "net0", VMID: 100, BindingStatus: PortUnbound}, true},
		{"port without MAC", &Port{NetworkID: "n", Name: "vm-100-net0", NIC: "net0", VMID: 100, BindingStatus: PortUnbound}, false},
		{"bad nic", &Port{NetworkID: "n", Name: "port", NIC: "eth0"}, false},
		{"allocation", &IPAllocation{SubnetID: "s", Address: "10.0.0.2", State: IPReserved}, true},
		{"allocated missing port", &IPAllocation{SubnetID: "s", Address: "10.0.0.2", State: IPAllocated}, false},
		{"router without gateway", &Router{Name: "edge", EnableSNAT: true}, true},
		{"router gateway", &Router{Name: "edge", ExternalNetworkID: "ext", ExternalSubnetID: "ext-v4", ExternalIPAddress: "192.0.2.10", EnableSNAT: true}, true},
		{"router static routes", &Router{Name: "edge", StaticRoutes: []StaticRoute{{Destination: "10.20.0.0/16", NextHop: "10.0.0.2"}, {Destination: "10.30.0.0/16", NextHop: "10.0.0.3"}}}, true},
		{"router noncanonical static route", &Router{Name: "edge", StaticRoutes: []StaticRoute{{Destination: "10.20.0.1/16", NextHop: "10.0.0.2"}}}, false},
		{"router invalid static route next hop", &Router{Name: "edge", StaticRoutes: []StaticRoute{{Destination: "10.20.0.0/16", NextHop: "0.0.0.0"}}}, false},
		{"router duplicate static route", &Router{Name: "edge", StaticRoutes: []StaticRoute{{Destination: "10.20.0.0/16", NextHop: "10.0.0.2"}, {Destination: "10.20.0.0/16", NextHop: "10.0.0.2"}}}, false},
		{"router gateway missing fixed IP", &Router{Name: "edge", ExternalNetworkID: "ext", ExternalSubnetID: "ext-v4", EnableSNAT: true}, false},
		{"flat segment", &ProviderSegment{ProviderNetworkID: "pn", Name: "flat", PhysicalNetwork: "provider", NetworkType: ProviderFlat}, true},
		{"vlan segment", &ProviderSegment{ProviderNetworkID: "pn", Name: "vlan-100", PhysicalNetwork: "provider", NetworkType: ProviderVLAN, VLANID: 100}, true},
		{"bad vlan", &ProviderSegment{ProviderNetworkID: "pn", Name: "vlan", PhysicalNetwork: "provider", NetworkType: ProviderVLAN, VLANID: 4095}, false},
		{"security rule", &SecurityGroupRule{SecurityGroupID: "sg", Direction: DirectionIngress, EtherType: EtherTypeIPv4, Protocol: "tcp", PortRangeMin: 22, PortRangeMax: 22, RemoteCIDR: "0.0.0.0/0", Action: ActionAllow}, true},
		{"security rule remote conflict", &SecurityGroupRule{SecurityGroupID: "sg", Direction: DirectionIngress, EtherType: EtherTypeIPv4, RemoteCIDR: "10.0.0.0/8", RemoteGroupID: "other", Action: ActionAllow}, false},
		{"node", &Node{Name: "pve01", ChassisID: "chassis-1", Roles: []NodeRole{NodeRoleCompute, NodeRoleGateway}}, true},
		{"node duplicate role", &Node{Name: "pve01", ChassisID: "chassis-1", Roles: []NodeRole{NodeRoleCompute, NodeRoleCompute}}, false},
		{"operation", &Operation{Action: "reconcile", TargetKind: KindNetwork, TargetID: "network-id", TargetRevision: 1, IdempotencyKey: "reconcile:network:network-id:1", OperationStatus: OperationQueued}, true},
		{"running operation", &Operation{Action: "reconcile", TargetKind: KindNetwork, TargetID: "network-id", TargetRevision: 1, IdempotencyKey: "reconcile:network:network-id:1", OperationStatus: OperationRunning, LeaseOwner: "lease-manager-1"}, true},
		{"running operation without owner", &Operation{Action: "reconcile", TargetKind: KindNetwork, TargetID: "network-id", TargetRevision: 1, IdempotencyKey: "reconcile:network:network-id:1", OperationStatus: OperationRunning}, false},
		{"operation without idempotency", &Operation{Action: "reconcile", TargetKind: KindNetwork, TargetID: "network-id", TargetRevision: 1, OperationStatus: OperationQueued}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.resource.Validate()
			if test.valid && err != nil {
				t.Fatalf("Validate(): %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestSetDefaults(t *testing.T) {
	network := &Network{}
	SetDefaults(network)
	if network.MTU != 1400 || network.State != ResourcePending {
		t.Fatalf("network defaults = %#v", network)
	}
	port := &Port{Metadata: Metadata{ID: "port-id"}, MACAddress: "02-00-00-00-00-AA"}
	SetDefaults(port)
	if port.Generation != 1 || port.BindingStatus != PortUnbound || port.LSPName != "pvn-port-id" || port.MACAddress != "02:00:00:00:00:aa" {
		t.Fatalf("port defaults = %#v", port)
	}
}

func TestCloneIsIndependent(t *testing.T) {
	original := &Port{Name: "before", FixedIPs: []FixedIP{{SubnetID: "s", Address: "10.0.0.2"}}}
	clonedResource, err := Clone(original)
	if err != nil {
		t.Fatal(err)
	}
	cloned := clonedResource.(*Port)
	cloned.Name = "after"
	cloned.FixedIPs[0].Address = "10.0.0.3"
	if original.Name != "before" || original.FixedIPs[0].Address != "10.0.0.2" {
		t.Fatal("clone mutated original")
	}
}
