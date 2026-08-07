package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
)

type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

var (
	namePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,126}$`)
	nicPattern      = regexp.MustCompile(`^net[0-9]+$`)
	dnsLabelPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

func validateName(field, value string) error {
	if !namePattern.MatchString(value) {
		return invalid(field, "must be 1-127 characters and contain only letters, digits, '.', '_', ':', or '-'")
	}
	return nil
}

func required(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalid(field, "is required")
	}
	return nil
}

func validateNetwork(n *Network) error {
	if err := validateName("name", n.Name); err != nil {
		return err
	}
	if n.MTU != 0 && (n.MTU < 576 || n.MTU > 9216) {
		return invalid("mtu", "must be between 576 and 9216")
	}
	if n.External && n.ProviderNetworkID == "" {
		return invalid("provider_network_id", "is required for an external network")
	}
	return nil
}

func validateSubnet(s *Subnet) error {
	if err := required("network_id", s.NetworkID); err != nil {
		return err
	}
	if err := validateName("name", s.Name); err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(s.CIDR)
	if err != nil || !prefix.Addr().Is4() {
		return invalid("cidr", "must be a valid IPv4 prefix")
	}
	prefix = prefix.Masked()
	if _, gatewayErr := EffectiveIPv4Gateway(s); gatewayErr != nil {
		return invalid("gateway_ip", "%s", gatewayErr.Error())
	}
	for i, server := range s.DNSNameservers {
		addr, parseErr := netip.ParseAddr(server)
		if parseErr != nil || !addr.Is4() {
			return invalid(fmt.Sprintf("dns_nameservers[%d]", i), "must be an IPv4 address")
		}
	}
	if s.DNSDomain != "" {
		if err := validateDNSDomain("dns_domain", s.DNSDomain); err != nil {
			return err
		}
	}
	seenSearchDomains := make(map[string]struct{}, len(s.DNSSearchDomains))
	for i, domain := range s.DNSSearchDomains {
		field := fmt.Sprintf("dns_search_domains[%d]", i)
		if err := validateDNSDomain(field, domain); err != nil {
			return err
		}
		key := strings.ToLower(strings.TrimSuffix(domain, "."))
		if _, duplicate := seenSearchDomains[key]; duplicate {
			return invalid(field, "duplicates another search domain")
		}
		seenSearchDomains[key] = struct{}{}
	}
	for i, pool := range s.AllocationPools {
		start, startErr := netip.ParseAddr(pool.Start)
		end, endErr := netip.ParseAddr(pool.End)
		if startErr != nil || endErr != nil || !prefix.Contains(start) || !prefix.Contains(end) || start.Compare(end) > 0 {
			return invalid(fmt.Sprintf("allocation_pools[%d]", i), "must be an ordered IPv4 range inside cidr")
		}
	}
	return nil
}

func validatePort(p *Port) error {
	if err := required("network_id", p.NetworkID); err != nil {
		return err
	}
	if err := validateName("name", p.Name); err != nil {
		return err
	}
	if err := required("mac_address", p.MACAddress); err != nil {
		return err
	}
	mac, err := net.ParseMAC(p.MACAddress)
	if err != nil || len(mac) != 6 {
		return invalid("mac_address", "must be a 48-bit MAC address")
	}
	for i, fixed := range p.FixedIPs {
		if fixed.SubnetID == "" {
			return invalid(fmt.Sprintf("fixed_ips[%d].subnet_id", i), "is required")
		}
		if fixed.Address != "" {
			addr, err := netip.ParseAddr(fixed.Address)
			if err != nil || !addr.Is4() {
				return invalid(fmt.Sprintf("fixed_ips[%d].address", i), "must be an IPv4 address")
			}
		}
	}
	if p.NIC != "" && !nicPattern.MatchString(p.NIC) {
		return invalid("nic", "must use the Proxmox netN form")
	}
	if p.VMID < 0 {
		return invalid("vmid", "must not be negative")
	}
	if p.Generation < 0 {
		return invalid("generation", "must not be negative")
	}
	if p.BindingStatus != "" && p.BindingStatus != PortUnbound && p.BindingStatus != PortBinding && p.BindingStatus != PortBound && p.BindingStatus != PortDetaching && p.BindingStatus != PortBindingError {
		return invalid("binding_status", "is not recognized")
	}
	return nil
}

func validateIPAllocation(a *IPAllocation) error {
	if err := required("subnet_id", a.SubnetID); err != nil {
		return err
	}
	addr, err := netip.ParseAddr(a.Address)
	if err != nil || !addr.Is4() {
		return invalid("address", "must be an IPv4 address")
	}
	if a.State != IPReserved && a.State != IPAllocated {
		return invalid("allocation_state", "must be reserved or allocated")
	}
	if a.State == IPAllocated && a.PortID == "" {
		return invalid("port_id", "is required for an allocated address")
	}
	return nil
}

func validateRouter(r *Router) error {
	if err := validateName("name", r.Name); err != nil {
		return err
	}
	for i, route := range r.StaticRoutes {
		prefix, err := netip.ParsePrefix(route.Destination)
		if err != nil || !prefix.Addr().Is4() {
			return invalid(fmt.Sprintf("static_routes[%d].destination", i), "must be a valid IPv4 prefix")
		}
		if prefix != prefix.Masked() {
			return invalid(fmt.Sprintf("static_routes[%d].destination", i), "must be a canonical IPv4 prefix")
		}
		nextHop, err := netip.ParseAddr(route.NextHop)
		if err != nil || !nextHop.Is4() || nextHop.IsUnspecified() || nextHop.IsMulticast() {
			return invalid(fmt.Sprintf("static_routes[%d].next_hop", i), "must be a unicast IPv4 address")
		}
		for previous := 0; previous < i; previous++ {
			if r.StaticRoutes[previous].Destination == route.Destination && r.StaticRoutes[previous].NextHop == route.NextHop {
				return invalid(fmt.Sprintf("static_routes[%d]", i), "duplicates another static route")
			}
		}
	}
	if r.ExternalNetworkID == "" {
		if r.ExternalSubnetID != "" || r.ExternalIPAddress != "" {
			return invalid("external_network_id", "is required when an external subnet or IP is configured")
		}
		return nil
	}
	if err := required("external_subnet_id", r.ExternalSubnetID); err != nil {
		return err
	}
	address, err := netip.ParseAddr(r.ExternalIPAddress)
	if err != nil || !address.Is4() {
		return invalid("external_ip_address", "must be a valid IPv4 address")
	}
	return nil
}

func validateDNSDomain(field, value string) error {
	domain := strings.TrimSuffix(value, ".")
	if domain == "" || len(domain) > 253 {
		return invalid(field, "must be a DNS name no longer than 253 characters")
	}
	for _, label := range strings.Split(domain, ".") {
		if !dnsLabelPattern.MatchString(label) {
			return invalid(field, "must contain valid DNS labels")
		}
	}
	return nil
}

func validateRouterInterface(r *RouterInterface) error {
	if err := required("router_id", r.RouterID); err != nil {
		return err
	}
	return required("subnet_id", r.SubnetID)
}

func validateFloatingIP(f *FloatingIP) error {
	if err := required("provider_network_id", f.ProviderNetworkID); err != nil {
		return err
	}
	addr, err := netip.ParseAddr(f.Address)
	if err != nil || !addr.Is4() {
		return invalid("address", "must be an IPv4 address")
	}
	if f.FixedIPAddress != "" {
		fixed, parseErr := netip.ParseAddr(f.FixedIPAddress)
		if parseErr != nil || !fixed.Is4() {
			return invalid("fixed_ip_address", "must be an IPv4 address")
		}
	}
	if f.FloatingStatus != "" && f.FloatingStatus != FloatingIPDown && f.FloatingStatus != FloatingIPActive && f.FloatingStatus != FloatingIPError {
		return invalid("status", "is not recognized")
	}
	return nil
}

func validateProviderNetwork(p *ProviderNetwork) error { return validateName("name", p.Name) }

func validateProviderSegment(p *ProviderSegment) error {
	if err := required("provider_network_id", p.ProviderNetworkID); err != nil {
		return err
	}
	if err := validateName("name", p.Name); err != nil {
		return err
	}
	if err := validateName("physical_network", p.PhysicalNetwork); err != nil {
		return err
	}
	if p.NetworkType != ProviderFlat && p.NetworkType != ProviderVLAN {
		return invalid("network_type", "must be flat or vlan")
	}
	if p.NetworkType == ProviderFlat && p.VLANID != 0 {
		return invalid("vlan_id", "must be zero for a flat segment")
	}
	if p.NetworkType == ProviderVLAN && (p.VLANID < 1 || p.VLANID > 4094) {
		return invalid("vlan_id", "must be between 1 and 4094 for a VLAN segment")
	}
	return nil
}

func validateSecurityGroup(s *SecurityGroup) error {
	return validateName("name", s.Name)
}

func validateSecurityGroupRule(r *SecurityGroupRule) error {
	if err := required("security_group_id", r.SecurityGroupID); err != nil {
		return err
	}
	if r.Direction != DirectionIngress && r.Direction != DirectionEgress {
		return invalid("direction", "must be ingress or egress")
	}
	if r.EtherType != EtherTypeIPv4 {
		return invalid("ethertype", "only IPv4 is supported")
	}
	protocol := strings.ToLower(r.Protocol)
	if protocol != "" && protocol != "tcp" && protocol != "udp" && protocol != "icmp" {
		return invalid("protocol", "must be tcp, udp, icmp, or empty")
	}
	if protocol == "tcp" || protocol == "udp" {
		if r.PortRangeMin < 0 || r.PortRangeMin > 65535 || r.PortRangeMax < 0 || r.PortRangeMax > 65535 || (r.PortRangeMax != 0 && r.PortRangeMin > r.PortRangeMax) {
			return invalid("port_range", "must be an ordered range between 0 and 65535")
		}
	} else if r.PortRangeMin != 0 || r.PortRangeMax != 0 {
		return invalid("port_range", "requires tcp or udp")
	}
	if r.RemoteCIDR != "" && r.RemoteGroupID != "" {
		return invalid("remote", "remote_cidr and remote_group_id are mutually exclusive")
	}
	if r.RemoteCIDR != "" {
		prefix, err := netip.ParsePrefix(r.RemoteCIDR)
		if err != nil || !prefix.Addr().Is4() {
			return invalid("remote_cidr", "must be an IPv4 prefix")
		}
	}
	if r.Action != ActionAllow && r.Action != ActionDrop {
		return invalid("action", "must be allow or drop")
	}
	return nil
}

func validateNode(n *Node) error {
	if err := validateName("name", n.Name); err != nil {
		return err
	}
	if err := required("chassis_id", n.ChassisID); err != nil {
		return err
	}
	seen := map[NodeRole]bool{}
	for _, role := range n.Roles {
		if role != NodeRoleCompute && role != NodeRoleGateway && role != NodeRoleCentral {
			return invalid("roles", "contains an unknown role %q", role)
		}
		if seen[role] {
			return invalid("roles", "contains duplicate role %q", role)
		}
		seen[role] = true
	}
	return nil
}

func validateOperation(o *Operation) error {
	if err := required("action", o.Action); err != nil {
		return err
	}
	if err := required("idempotency_key", o.IdempotencyKey); err != nil {
		return err
	}
	if !o.TargetKind.Valid() || o.TargetKind == KindOperation {
		return invalid("target_kind", "must identify a non-operation resource")
	}
	if err := required("target_id", o.TargetID); err != nil {
		return err
	}
	if o.TargetRevision < 1 {
		return invalid("target_revision", "must be positive")
	}
	if o.OperationStatus != OperationQueued && o.OperationStatus != OperationRunning && o.OperationStatus != OperationSucceeded && o.OperationStatus != OperationFailed {
		return invalid("status", "is not recognized")
	}
	if o.OperationStatus == OperationRunning && (o.Action == "reconcile" || o.Action == "delete") {
		if err := validateName("lease_owner", o.LeaseOwner); err != nil {
			return err
		}
	}
	return nil
}

func New(kind Kind) (Resource, error) {
	switch kind {
	case KindNetwork:
		return &Network{}, nil
	case KindSubnet:
		return &Subnet{}, nil
	case KindPort:
		return &Port{}, nil
	case KindIPAllocation:
		return &IPAllocation{}, nil
	case KindRouter:
		return &Router{}, nil
	case KindRouterInterface:
		return &RouterInterface{}, nil
	case KindFloatingIP:
		return &FloatingIP{}, nil
	case KindProviderNetwork:
		return &ProviderNetwork{}, nil
	case KindProviderSegment:
		return &ProviderSegment{}, nil
	case KindSecurityGroup:
		return &SecurityGroup{}, nil
	case KindSecurityGroupRule:
		return &SecurityGroupRule{}, nil
	case KindNode:
		return &Node{}, nil
	case KindOperation:
		return &Operation{}, nil
	default:
		return nil, fmt.Errorf("unknown resource kind %q", kind)
	}
}

func Clone(resource Resource) (Resource, error) {
	if resource == nil {
		return nil, errors.New("resource is nil")
	}
	copyResource, err := New(resource.ResourceKind())
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(resource)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, copyResource); err != nil {
		return nil, err
	}
	return copyResource, nil
}

// SetDefaults supplies API defaults without overwriting explicit values.
func SetDefaults(resource Resource) {
	meta := resource.GetMetadata()
	if meta.State == "" {
		meta.State = ResourcePending
	}
	switch value := resource.(type) {
	case *Network:
		if value.MTU == 0 {
			value.MTU = 1400
		}
	case *Port:
		if mac, err := net.ParseMAC(value.MACAddress); err == nil && len(mac) == 6 {
			value.MACAddress = strings.ToLower(mac.String())
		}
		if value.BindingStatus == "" {
			value.BindingStatus = PortUnbound
		}
		if value.Generation == 0 {
			value.Generation = 1
		}
		if value.LSPName == "" && value.ID != "" {
			value.LSPName = "pvn-" + value.ID
		}
	case *IPAllocation:
		if value.State == "" {
			value.State = IPReserved
		}
	case *FloatingIP:
		if value.FloatingStatus == "" {
			value.FloatingStatus = FloatingIPDown
		}
	case *SecurityGroup:
		// Security groups are stateful unless explicitly represented by a future mode.
		value.Stateful = true
	case *SecurityGroupRule:
		if value.EtherType == "" {
			value.EtherType = EtherTypeIPv4
		}
		if value.Action == "" {
			value.Action = ActionAllow
		}
	case *Operation:
		if value.OperationStatus == "" {
			value.OperationStatus = OperationQueued
		}
	}
}
