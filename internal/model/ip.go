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
	network := prefix.Addr()
	broadcastValue := network.As4()
	hostBits := 32 - prefix.Bits()
	mask := uint32((uint64(1) << hostBits) - 1)
	broadcastNumber := uint32(broadcastValue[0])<<24 | uint32(broadcastValue[1])<<16 | uint32(broadcastValue[2])<<8 | uint32(broadcastValue[3])
	broadcastNumber |= mask
	broadcast := netip.AddrFrom4([4]byte{byte(broadcastNumber >> 24), byte(broadcastNumber >> 16), byte(broadcastNumber >> 8), byte(broadcastNumber)})

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
