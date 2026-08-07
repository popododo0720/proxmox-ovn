package model

import (
	"encoding/json"
	"strings"
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
		{"operation with payload", &Operation{Action: "compute-clone", TargetKind: KindPort, TargetID: "port-id", TargetRevision: 1, IdempotencyKey: "clone:port-id:1", OperationStatus: OperationQueued, Payload: `{"generation":1,"vmid":101}`}, true},
		{"operation with non-object payload", &Operation{Action: "compute-clone", TargetKind: KindPort, TargetID: "port-id", TargetRevision: 1, IdempotencyKey: "clone:port-id:1", OperationStatus: OperationQueued, Payload: `[]`}, false},
		{"operation with noncanonical payload", &Operation{Action: "compute-clone", TargetKind: KindPort, TargetID: "port-id", TargetRevision: 1, IdempotencyKey: "clone:port-id:1", OperationStatus: OperationQueued, Payload: `{ "vmid": 101 }`}, false},
		{"operation with oversized payload", &Operation{Action: "compute-clone", TargetKind: KindPort, TargetID: "port-id", TargetRevision: 1, IdempotencyKey: "clone:port-id:1", OperationStatus: OperationQueued, Payload: `{"data":"` + strings.Repeat("x", MaxOperationPayloadBytes) + `"}`}, false},
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
	subnet := &Subnet{DNSDomain: "Guest.Example.", DNSSearchDomains: []string{"SVC.Example."}}
	SetDefaults(subnet)
	if subnet.DNSDomain != "guest.example" || subnet.DNSSearchDomains[0] != "svc.example" {
		t.Fatalf("subnet DNS defaults = %#v", subnet)
	}
	router := &Router{StaticRoutes: []StaticRoute{{Destination: "10.20.0.3/16", NextHop: "10.0.0.002"}}}
	SetDefaults(router)
	if router.StaticRoutes[0].Destination != "10.20.0.0/16" {
		t.Fatalf("router route defaults = %#v", router.StaticRoutes)
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

func TestOperationPayloadHelpersAreCanonicalStrictAndPrivate(t *testing.T) {
	type manifest struct {
		VMID       int    `json:"vmid"`
		Generation int64  `json:"generation"`
		Mode       string `json:"mode,omitempty"`
	}
	payload, err := MarshalOperationPayload(manifest{VMID: 101, Generation: 7})
	if err != nil {
		t.Fatal(err)
	}
	if payload != `{"generation":7,"vmid":101}` {
		t.Fatalf("canonical payload=%q", payload)
	}
	var decoded manifest
	if err := UnmarshalOperationPayload(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.VMID != 101 || decoded.Generation != 7 {
		t.Fatalf("decoded payload=%#v", decoded)
	}
	for name, invalidPayload := range map[string]string{
		"malformed":      `{"vmid":`,
		"trailing":       `{"vmid":101}{}`,
		"array":          `[{"vmid":101}]`,
		"whitespace":     `{ "vmid":101}`,
		"unsorted":       `{"vmid":101,"generation":7}`,
		"duplicate keys": `{"vmid":101,"vmid":101}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateOperationPayload(invalidPayload); err == nil {
				t.Fatalf("ValidateOperationPayload(%q) unexpectedly succeeded", invalidPayload)
			}
		})
	}
	if err := UnmarshalOperationPayload(`{"generation":7,"unknown":true,"vmid":101}`, &decoded); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error=%v", err)
	}
	if _, err := MarshalOperationPayload([]int{1, 2}); err == nil {
		t.Fatal("array payload unexpectedly marshaled")
	}
	operation := &Operation{Payload: payload}
	clonedResource, err := Clone(operation)
	if err != nil {
		t.Fatal(err)
	}
	if clonedResource.(*Operation).Payload != payload {
		t.Fatalf("clone lost payload: %#v", clonedResource)
	}
	publicJSON, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicJSON), "payload") || strings.Contains(string(publicJSON), "generation") {
		t.Fatalf("internal payload leaked into public JSON: %s", publicJSON)
	}
}
