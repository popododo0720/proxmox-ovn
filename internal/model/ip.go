package model

import (
	"fmt"
	"net/netip"
)

// EffectiveIPv4Gateway returns the gateway PVN renders and reserves. IPv4
// subnets in v1 require at least a /30; /31 and /32 do not leave space for the
// conventional network, gateway, guest, and broadcast roles PVN implements.
func EffectiveIPv4Gateway(subnet *Subnet) (netip.Addr, error) {
	if subnet == nil {
		return netip.Addr{}, fmt.Errorf("subnet is required")
	}
	prefix, err := netip.ParsePrefix(subnet.CIDR)
	if err != nil || !prefix.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("must use a valid IPv4 prefix")
	}
	prefix = prefix.Masked()
	if prefix.Bits() > 30 {
		return netip.Addr{}, fmt.Errorf("must be /30 or larger")
	}
	network, broadcast := ipv4SubnetBounds(prefix)

	gateway := network.Next()
	if subnet.GatewayIP != "" {
		gateway, err = netip.ParseAddr(subnet.GatewayIP)
		if err != nil || !gateway.Is4() || !prefix.Contains(gateway) {
			return netip.Addr{}, fmt.Errorf("must be an IPv4 address inside cidr")
		}
	}
	if gateway == network || gateway == broadcast {
		return netip.Addr{}, fmt.Errorf("must be a usable host address")
	}
	return gateway, nil
}

// ValidateIPv4AllocationAddress verifies that address can be allocated on
// subnet. Allocation pools, when configured, are authoritative for both
// automatic and explicitly requested addresses.
func ValidateIPv4AllocationAddress(subnet *Subnet, address string) error {
	if subnet == nil {
		return fmt.Errorf("subnet is required")
	}
	prefix, err := netip.ParsePrefix(subnet.CIDR)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("subnet must use a valid IPv4 prefix")
	}
	prefix = prefix.Masked()
	addr, err := netip.ParseAddr(address)
	if err != nil || !addr.Is4() {
		return fmt.Errorf("must be a valid IPv4 address")
	}
	if !prefix.Contains(addr) {
		return fmt.Errorf("must belong to the subnet")
	}

	network, broadcast := ipv4SubnetBounds(prefix)
	gateway, err := EffectiveIPv4Gateway(subnet)
	if err != nil {
		return fmt.Errorf("subnet gateway is invalid: %w", err)
	}
	if addr == network || addr == broadcast || addr == gateway {
		return fmt.Errorf("must be a usable host address distinct from the subnet gateway")
	}

	if len(subnet.AllocationPools) == 0 {
		return nil
	}
	for _, pool := range subnet.AllocationPools {
		start, startErr := netip.ParseAddr(pool.Start)
		end, endErr := netip.ParseAddr(pool.End)
		if startErr != nil || endErr != nil || !start.Is4() || !end.Is4() || !prefix.Contains(start) || !prefix.Contains(end) || start.Compare(end) > 0 {
			return fmt.Errorf("subnet allocation pool is invalid")
		}
		if start.Compare(addr) <= 0 && addr.Compare(end) <= 0 {
			return nil
		}
	}
	return fmt.Errorf("must belong to a subnet allocation pool")
}

// IPv4PrefixesOverlap reports whether two valid IPv4 CIDRs share any address.
func IPv4PrefixesOverlap(leftCIDR, rightCIDR string) bool {
	left, leftErr := netip.ParsePrefix(leftCIDR)
	right, rightErr := netip.ParsePrefix(rightCIDR)
	if leftErr != nil || rightErr != nil || !left.Addr().Is4() || !right.Addr().Is4() {
		return false
	}
	left = left.Masked()
	right = right.Masked()
	return left.Contains(right.Addr()) || right.Contains(left.Addr())
}

func ipv4SubnetBounds(prefix netip.Prefix) (netip.Addr, netip.Addr) {
	network := prefix.Masked().Addr()
	broadcastValue := network.As4()
	hostBits := 32 - prefix.Bits()
	mask := uint32((uint64(1) << hostBits) - 1)
	broadcastNumber := uint32(broadcastValue[0])<<24 | uint32(broadcastValue[1])<<16 | uint32(broadcastValue[2])<<8 | uint32(broadcastValue[3])
	broadcastNumber |= mask
	broadcast := netip.AddrFrom4([4]byte{byte(broadcastNumber >> 24), byte(broadcastNumber >> 16), byte(broadcastNumber >> 8), byte(broadcastNumber)})
	return network, broadcast
}
