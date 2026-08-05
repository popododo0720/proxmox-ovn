package model

import "testing"

func TestValidateIPv4AllocationAddress(t *testing.T) {
	tests := []struct {
		name    string
		subnet  *Subnet
		address string
		valid   bool
	}{
		{name: "usable host", subnet: &Subnet{CIDR: "10.0.0.0/24"}, address: "10.0.0.2", valid: true},
		{name: "network", subnet: &Subnet{CIDR: "10.0.0.0/24"}, address: "10.0.0.0"},
		{name: "implicit gateway", subnet: &Subnet{CIDR: "10.0.0.0/24"}, address: "10.0.0.1"},
		{name: "explicit gateway", subnet: &Subnet{CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.254"}, address: "10.0.0.254"},
		{name: "broadcast", subnet: &Subnet{CIDR: "10.0.0.0/24"}, address: "10.0.0.255"},
		{name: "outside subnet", subnet: &Subnet{CIDR: "10.0.0.0/24"}, address: "10.0.1.2"},
		{name: "inside pool", subnet: &Subnet{CIDR: "10.0.0.0/24", AllocationPools: []IPRange{{Start: "10.0.0.10", End: "10.0.0.20"}}}, address: "10.0.0.20", valid: true},
		{name: "outside pool", subnet: &Subnet{CIDR: "10.0.0.0/24", AllocationPools: []IPRange{{Start: "10.0.0.10", End: "10.0.0.20"}}}, address: "10.0.0.21"},
		{name: "second pool", subnet: &Subnet{CIDR: "10.0.0.0/24", AllocationPools: []IPRange{{Start: "10.0.0.10", End: "10.0.0.20"}, {Start: "10.0.0.30", End: "10.0.0.40"}}}, address: "10.0.0.30", valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateIPv4AllocationAddress(test.subnet, test.address)
			if test.valid && err != nil {
				t.Fatalf("ValidateIPv4AllocationAddress() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("ValidateIPv4AllocationAddress() accepted an unusable address")
			}
		})
	}
}

func TestIPv4PrefixesOverlap(t *testing.T) {
	tests := []struct {
		left, right string
		overlap     bool
	}{
		{left: "10.0.0.0/24", right: "10.0.0.128/25", overlap: true},
		{left: "10.0.0.0/25", right: "10.0.0.128/25"},
		{left: "10.0.0.1/24", right: "10.0.0.128/25", overlap: true},
		{left: "invalid", right: "10.0.0.0/24"},
	}
	for _, test := range tests {
		if got := IPv4PrefixesOverlap(test.left, test.right); got != test.overlap {
			t.Errorf("IPv4PrefixesOverlap(%q, %q) = %v, want %v", test.left, test.right, got, test.overlap)
		}
	}
}
