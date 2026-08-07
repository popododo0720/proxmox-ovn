package ovnnb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type Renderer struct {
	client *Client
	store  controlstore.Store
}

func NewRenderer(client *Client, store controlstore.Store) (*Renderer, error) {
	if client == nil || store == nil {
		return nil, errors.New("OVN client and control store are required")
	}
	return &Renderer{client: client, store: store}, nil
}

func (renderer *Renderer) Render(ctx context.Context, resource model.Resource) error {
	if nilResource(resource) {
		return errors.New("cannot render a nil resource")
	}
	if err := safeID(resource.GetMetadata().ID); err != nil {
		return err
	}
	if err := resource.Validate(); err != nil {
		return fmt.Errorf("invalid %s %q: %w", resource.ResourceKind(), resource.GetMetadata().ID, err)
	}
	backed, err := ovnBackedResource(resource)
	if err != nil || !backed {
		return err
	}
	// The post-transaction fence below proves that this exact desired state
	// reached Southbound. This pre-transaction fence additionally prevents a
	// manager restarted during a rolling northd transition from writing into
	// Northbound while the old split northd processes cannot synchronize it.
	if err := renderer.client.sync(ctx); err != nil {
		return err
	}
	var renderErr error
	switch value := resource.(type) {
	case *model.Network:
		renderErr = renderer.network(ctx, value)
	case *model.Subnet:
		renderErr = renderer.subnet(ctx, value)
	case *model.Port:
		renderErr = renderer.port(ctx, value)
	case *model.Router:
		renderErr = renderer.router(ctx, value)
	case *model.RouterInterface:
		renderErr = renderer.routerInterface(ctx, value)
	case *model.FloatingIP:
		renderErr = renderer.floatingIP(ctx, value)
	case *model.ProviderSegment:
		renderErr = renderer.providerSegment(ctx, value)
	case *model.SecurityGroup:
		renderErr = renderer.securityGroup(ctx, value)
	case *model.SecurityGroupRule:
		renderErr = renderer.securityGroupRule(ctx, value)
	}
	if renderErr != nil {
		return renderErr
	}
	return renderer.client.sync(ctx)
}

// Delete removes only OVN rows whose names or deterministic UUIDs are owned by
// the supplied PVN resource.  Repeated calls are safe and never use a broad
// table-wide delete.
func (renderer *Renderer) Delete(ctx context.Context, resource model.Resource) error {
	if nilResource(resource) {
		return errors.New("cannot delete a nil resource")
	}
	if err := safeID(resource.GetMetadata().ID); err != nil {
		return err
	}
	backed, err := ovnBackedResource(resource)
	if err != nil || !backed {
		return err
	}
	if err := renderer.client.sync(ctx); err != nil {
		return err
	}
	var deleteErr error
	switch value := resource.(type) {
	case *model.Network:
		uuid, err := renderer.lookupOwnedRow(ctx, logicalSwitchOwnedRow(value.ID))
		if err != nil || uuid == "" {
			deleteErr = wrapRender("delete network", value.ID, err)
			break
		}
		_, err = renderer.client.run(ctx, "--", "--if-exists", "ls-del", uuid)
		deleteErr = wrapRender("delete network", value.ID, err)
	case *model.Subnet:
		uuid, err := renderer.lookupOwnedRow(ctx, dhcpOptionsOwnedRow(value.ID))
		if err != nil {
			deleteErr = wrapRender("delete subnet", value.ID, err)
			break
		}
		// Resolve the DHCP row before changing any port references. A duplicate
		// restored identity is ambiguous and must fail closed without partially
		// clearing DHCP from otherwise healthy logical switch ports.
		if err := renderer.attachDHCPToPorts(ctx, value, ""); err != nil {
			deleteErr = err
			break
		}
		if uuid != "" {
			_, err = renderer.client.run(ctx, "--", "--if-exists", "destroy", "DHCP_Options", uuid)
			deleteErr = wrapRender("delete subnet", value.ID, err)
		}
	case *model.Port:
		if err := safeID(value.LSPName); err != nil {
			return fmt.Errorf("invalid LSP name: %w", err)
		}
		uuid, err := renderer.lookupOwnedRow(ctx, logicalSwitchPortOwnedRow(value.ID, value.LSPName))
		if err != nil || uuid == "" {
			deleteErr = wrapRender("delete port", value.ID, err)
			break
		}
		_, err = renderer.client.run(ctx, "--", "--if-exists", "lsp-del", uuid)
		deleteErr = wrapRender("delete port", value.ID, err)
	case *model.Router:
		deleteErr = renderer.deleteRouter(ctx, value)
	case *model.RouterInterface:
		deleteErr = renderer.deleteRouterInterface(ctx, value)
	case *model.FloatingIP:
		deleteErr = renderer.deleteFloatingIP(ctx, value)
	case *model.ProviderSegment:
		deleteErr = renderer.deleteProviderSegment(ctx, value)
	case *model.SecurityGroup:
		uuid, err := renderer.lookupOwnedRow(ctx, portGroupOwnedRow(value.ID))
		if err != nil || uuid == "" {
			deleteErr = wrapRender("delete security group", value.ID, err)
			break
		}
		_, err = renderer.client.run(ctx, "--", "--if-exists", "destroy", "Port_Group", uuid)
		deleteErr = wrapRender("delete security group", value.ID, err)
	case *model.SecurityGroupRule:
		deleteErr = renderer.deleteACL(ctx, value.ID)
	}
	if deleteErr != nil {
		return deleteErr
	}
	return renderer.client.sync(ctx)
}

func ovnBackedResource(resource model.Resource) (bool, error) {
	switch resource.(type) {
	case *model.Network, *model.Subnet, *model.Port, *model.Router,
		*model.RouterInterface, *model.FloatingIP, *model.ProviderSegment,
		*model.SecurityGroup, *model.SecurityGroupRule:
		return true, nil
	case *model.ProviderNetwork, *model.IPAllocation,
		*model.Node, *model.Operation:
		return false, nil
	default:
		return false, fmt.Errorf("unsupported resource type %T", resource)
	}
}

func (renderer *Renderer) deleteFloatingIP(ctx context.Context, floatingIP *model.FloatingIP) error {
	uuid, err := renderer.lookupOwnedRow(ctx, floatingIPOwnedRow(floatingIP.ID))
	if err != nil || uuid == "" {
		return wrapRender("delete floating IP", floatingIP.ID, err)
	}
	routers, err := renderer.store.List(ctx, model.KindRouter, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	args := make([]string, 0, len(routers)*8+6)
	for _, resource := range routers {
		router, ok := resource.(*model.Router)
		if !ok {
			return fmt.Errorf("control store returned %T while listing routers", resource)
		}
		routerUUID, lookupErr := renderer.lookupOwnedRow(ctx, logicalRouterOwnedRow(router.ID))
		if lookupErr != nil {
			return lookupErr
		}
		if routerUUID != "" {
			args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "nat", uuid)
		}
	}
	args = append(args, "--", "--if-exists", "destroy", "NAT", uuid)
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("delete floating IP", floatingIP.ID, err)
}

func (renderer *Renderer) deleteProviderSegment(ctx context.Context, segment *model.ProviderSegment) error {
	provider, err := getResource[*model.ProviderNetwork](ctx, renderer.store, model.KindProviderNetwork, segment.ProviderNetworkID)
	if err != nil {
		return err
	}
	if provider.DefaultSegmentID != "" && provider.DefaultSegmentID != segment.ID {
		return nil
	}
	if provider.DefaultSegmentID == "" {
		segments, listErr := renderer.store.List(ctx, model.KindProviderSegment, controlstore.ListOptions{})
		if listErr != nil {
			return listErr
		}
		matching := 0
		for _, resource := range segments {
			candidate, ok := resource.(*model.ProviderSegment)
			if !ok {
				return fmt.Errorf("control store returned %T while listing provider segments", resource)
			}
			if candidate.ProviderNetworkID == segment.ProviderNetworkID {
				matching++
			}
		}
		if matching > 1 {
			return fmt.Errorf("provider network %q has multiple segments but no default segment", provider.ID)
		}
	}
	networks, err := renderer.store.List(ctx, model.KindNetwork, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	args := make([]string, 0, len(networks)*5)
	for _, resource := range networks {
		network, ok := resource.(*model.Network)
		if !ok {
			return fmt.Errorf("control store returned %T while listing networks", resource)
		}
		if network.ProviderNetworkID == segment.ProviderNetworkID {
			args = append(args, "--", "--if-exists", "lsp-del", "pvn-localnet-"+compact(network.ID))
		}
	}
	if len(args) == 0 {
		return nil
	}
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("delete provider segment", segment.ID, err)
}

func (renderer *Renderer) deleteACL(ctx context.Context, owner string) error {
	uuid, err := renderer.lookupOwnedRow(ctx, aclOwnedRow(owner))
	if err != nil || uuid == "" {
		return wrapRender("delete security group ACL", owner, err)
	}
	groups, err := renderer.store.List(ctx, model.KindSecurityGroup, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	args := make([]string, 0, len(groups)*8+6)
	for _, resource := range groups {
		group, ok := resource.(*model.SecurityGroup)
		if !ok {
			return fmt.Errorf("control store returned %T while listing security groups", resource)
		}
		groupUUID, lookupErr := renderer.lookupOwnedRow(ctx, portGroupOwnedRow(group.ID))
		if lookupErr != nil {
			return lookupErr
		}
		if groupUUID != "" {
			args = append(args, "--", "--if-exists", "remove", "Port_Group", groupUUID, "acls", uuid)
		}
	}
	args = append(args, "--", "--if-exists", "destroy", "ACL", uuid)
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("delete security group ACL", owner, err)
}

func (renderer *Renderer) network(ctx context.Context, network *model.Network) error {
	name := logicalSwitch(network.ID)
	assignments := []string{stringAssignment("name", name)}
	assignments = append(assignments, metadataAssignments(network, nil)...)
	switchUUID, err := renderer.ensureOwnedRow(ctx, logicalSwitchOwnedRow(network.ID), assignments)
	if err != nil {
		return wrapRender("network", network.ID, err)
	}
	if network.ProviderNetworkID == "" {
		return renderer.removeProviderPort(ctx, network)
	}
	segment, err := renderer.defaultProviderSegment(ctx, network.ProviderNetworkID)
	if err != nil {
		return wrapRender("network provider segment", network.ID, err)
	}
	return renderer.renderProviderPort(ctx, segment, network, switchUUID)
}

func (renderer *Renderer) subnet(ctx context.Context, subnet *model.Subnet) error {
	if err := validateReferencedIDs(subnet.NetworkID); err != nil {
		return err
	}
	if !subnet.EnableDHCP {
		return renderer.clearSubnetDHCP(ctx, subnet)
	}
	network, err := getResource[*model.Network](ctx, renderer.store, model.KindNetwork, subnet.NetworkID)
	if err != nil {
		return err
	}
	gateway, err := subnetGateway(subnet)
	if err != nil {
		return err
	}
	assignments := []string{
		stringAssignment("cidr", subnet.CIDR),
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", subnet.ResourceKind().String()),
		mapAssignment("external_ids", "pvn-id", subnet.ID),
		mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(subnet.Revision, 10)),
	}
	uuid, err := renderer.ensureOwnedRow(ctx, dhcpOptionsOwnedRow(subnet.ID), assignments)
	if err != nil {
		return wrapRender("subnet DHCP", subnet.ID, err)
	}
	options := []string{
		"server_id=" + gateway.String(),
		"server_mac=" + deterministicMAC("dhcp:"+subnet.ID),
		"lease_time=43200",
		"mtu=" + strconv.Itoa(network.MTU),
		"router=" + gateway.String(),
	}
	if len(subnet.DNSNameservers) > 0 {
		options = append(options, "dns_server={"+strings.Join(subnet.DNSNameservers, ",")+"}")
	}
	if subnet.DNSDomain != "" {
		options = append(options, "domain_name="+normalizedDNSDomain(subnet.DNSDomain))
	}
	if len(subnet.DNSSearchDomains) > 0 {
		domains := make([]string, len(subnet.DNSSearchDomains))
		for index, domain := range subnet.DNSSearchDomains {
			domains[index] = normalizedDNSDomain(domain)
		}
		options = append(options, "domain_search_list="+strings.Join(domains, ","))
	}
	args := append([]string{"dhcp-options-set-options", uuid}, options...)
	if _, err := renderer.client.run(ctx, args...); err != nil {
		return wrapRender("subnet DHCP options", subnet.ID, err)
	}
	return renderer.attachDHCPToPorts(ctx, subnet, uuid)
}

func (renderer *Renderer) clearSubnetDHCP(ctx context.Context, subnet *model.Subnet) error {
	uuid, err := renderer.lookupOwnedRow(ctx, dhcpOptionsOwnedRow(subnet.ID))
	if err != nil {
		return wrapRender("remove subnet DHCP", subnet.ID, err)
	}
	// Ownership is a preflight gate: do not detach any port if restored rows
	// make this subnet's DHCP identity ambiguous.
	if err := renderer.attachDHCPToPorts(ctx, subnet, ""); err != nil {
		return err
	}
	if uuid == "" {
		return nil
	}
	_, err = renderer.client.run(ctx, "--", "--if-exists", "destroy", "DHCP_Options", uuid)
	return wrapRender("remove subnet DHCP", subnet.ID, err)
}

func (renderer *Renderer) attachDHCPToPorts(ctx context.Context, subnet *model.Subnet, uuid string) error {
	ports, err := renderer.store.List(ctx, model.KindPort, controlstore.ListOptions{NetworkID: subnet.NetworkID})
	if err != nil {
		return err
	}
	for _, resource := range ports {
		port, ok := resource.(*model.Port)
		if !ok {
			return fmt.Errorf("control store returned %T while listing ports", resource)
		}
		usesSubnet := false
		for _, fixed := range port.FixedIPs {
			usesSubnet = usesSubnet || fixed.SubnetID == subnet.ID
		}
		if !usesSubnet || port.LSPName == "" {
			continue
		}
		if err := safeID(port.LSPName); err != nil {
			return fmt.Errorf("port %q has an unsafe LSP name: %w", port.ID, err)
		}
		portUUID, lookupErr := renderer.lookupOwnedRow(ctx, logicalSwitchPortOwnedRow(port.ID, port.LSPName))
		if lookupErr != nil {
			return wrapRender("attach subnet DHCP to port", port.ID, lookupErr)
		}
		if portUUID == "" {
			continue
		}
		var arguments []string
		if uuid == "" {
			arguments = []string{"clear", "Logical_Switch_Port", portUUID, "dhcpv4_options"}
		} else {
			arguments = []string{"lsp-set-dhcpv4-options", portUUID, uuid}
		}
		if _, err := renderer.client.run(ctx, arguments...); err != nil {
			return wrapRender("attach subnet DHCP to port", port.ID, err)
		}
	}
	return nil
}

func (renderer *Renderer) port(ctx context.Context, port *model.Port) error {
	if err := validateReferencedIDs(port.NetworkID); err != nil {
		return err
	}
	network, err := getResource[*model.Network](ctx, renderer.store, model.KindNetwork, port.NetworkID)
	if err != nil {
		return err
	}
	if err := safeID(port.LSPName); err != nil {
		return fmt.Errorf("invalid LSP name: %w", err)
	}
	if port.MACAddress == "" {
		return errors.New("port MAC address is required before OVN reconciliation")
	}
	addresses := []string{strings.ToLower(port.MACAddress)}
	for _, fixed := range port.FixedIPs {
		if err := safeID(fixed.SubnetID); err != nil {
			return fmt.Errorf("invalid fixed-IP subnet ID: %w", err)
		}
		subnet, getErr := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, fixed.SubnetID)
		if getErr != nil {
			return getErr
		}
		if subnet.NetworkID != port.NetworkID {
			return fmt.Errorf("fixed-IP subnet %q does not belong to port %q's network", subnet.ID, port.ID)
		}
		if fixed.Address != "" {
			prefix, _ := netip.ParsePrefix(subnet.CIDR)
			address, _ := netip.ParseAddr(fixed.Address)
			if !prefix.Contains(address) {
				return fmt.Errorf("fixed IP %q is outside subnet %q", fixed.Address, subnet.ID)
			}
		}
		if fixed.Address != "" {
			addresses = append(addresses, fixed.Address)
		}
	}
	switchUUID, err := renderer.requireOwnedRow(ctx, logicalSwitchOwnedRow(network.ID))
	if err != nil {
		return wrapRender("port network", port.ID, err)
	}
	enabledState := "disabled"
	if port.AdminStateUp && port.BindingStatus != model.PortUnbound && port.BindingStatus != model.PortDetaching && port.BindingStatus != model.PortBindingError {
		enabledState = "enabled"
	}
	portUUID, err := renderer.lookupOwnedRow(ctx, logicalSwitchPortOwnedRow(port.ID, port.LSPName))
	if err != nil {
		return wrapRender("port ownership", port.ID, err)
	}
	target := portUUID
	arguments := make([]string, 0, 24)
	if target == "" {
		target = port.LSPName
		arguments = append(arguments, "--", "--may-exist", "lsp-add", switchUUID, port.LSPName)
	}
	arguments = append(arguments,
		"--", "lsp-set-addresses", target, strings.Join(addresses, " "),
		"--", "lsp-set-port-security", target, strings.Join(addresses, " "),
		"--", "lsp-set-enabled", target, enabledState,
		"--", "set", "Logical_Switch_Port", target,
	)
	arguments = append(arguments, metadataAssignments(port, map[string]string{"pvn-network": port.NetworkID})...)
	if _, err := renderer.client.run(ctx, arguments...); err != nil {
		return wrapRender("port", port.ID, err)
	}
	if portUUID == "" {
		portUUID, err = renderer.requireOwnedRow(ctx, logicalSwitchPortOwnedRow(port.ID, port.LSPName))
		if err != nil {
			return wrapRender("port", port.ID, err)
		}
	}
	optionArgs := []string{"lsp-set-options", portUUID}
	if port.RequestedChassis != "" {
		if err := safeID(port.RequestedChassis); err != nil {
			return fmt.Errorf("invalid requested chassis: %w", err)
		}
		optionArgs = append(optionArgs, "requested-chassis="+port.RequestedChassis)
	}
	if _, err := renderer.client.run(ctx, optionArgs...); err != nil {
		return wrapRender("port chassis request", port.ID, err)
	}
	if err := renderer.portDHCP(ctx, port, portUUID); err != nil {
		return err
	}
	return renderer.portGroups(ctx, port, portUUID)
}

func (renderer *Renderer) portDHCP(ctx context.Context, port *model.Port, portUUID string) error {
	var selected string
	for _, fixed := range port.FixedIPs {
		subnet, err := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, fixed.SubnetID)
		if err != nil {
			return err
		}
		if !subnet.EnableDHCP {
			continue
		}
		found, err := renderer.lookupOwnedRow(ctx, dhcpOptionsOwnedRow(fixed.SubnetID))
		if err != nil {
			return err
		}
		if found != "" {
			selected = found
			break
		}
	}
	if selected == "" {
		_, err := renderer.client.run(ctx, "clear", "Logical_Switch_Port", portUUID, "dhcpv4_options")
		return wrapRender("clear port DHCP", port.ID, err)
	}
	_, err := renderer.client.run(ctx, "lsp-set-dhcpv4-options", portUUID, selected)
	return wrapRender("port DHCP", port.ID, err)
}

func (renderer *Renderer) router(ctx context.Context, router *model.Router) error {
	var external *routerExternal
	var gatewayChassis []string
	if router.ExternalNetworkID != "" {
		var err error
		external, err = renderer.validateRouterExternal(ctx, router)
		if err != nil {
			return err
		}
		gatewayChassis, err = renderer.enabledGatewayChassis(ctx)
		if err != nil {
			return err
		}
		if len(gatewayChassis) == 0 {
			return fmt.Errorf("router %q has an external gateway but no enabled gateway chassis", router.ID)
		}
	}
	name := logicalRouter(router.ID)
	assignments := []string{stringAssignment("name", name)}
	assignments = append(assignments, metadataAssignments(router, nil)...)
	routerUUID, err := renderer.ensureOwnedRow(ctx, logicalRouterOwnedRow(router.ID), assignments)
	if err != nil {
		return wrapRender("router", router.ID, err)
	}
	if external == nil {
		if err := renderer.clearRouterExternal(ctx, router, routerUUID); err != nil {
			return err
		}
	} else {
		externalSwitchUUID, err := renderer.requireOwnedRow(ctx, logicalSwitchOwnedRow(external.network.ID))
		if err != nil {
			return wrapRender("router external network", router.ID, err)
		}
		if err := renderer.renderRouterGateway(ctx, router, routerUUID, externalSwitchUUID, external, gatewayChassis); err != nil {
			return err
		}
		if err := renderer.reconcileRouterSNAT(ctx, router, routerUUID); err != nil {
			return err
		}
	}
	return renderer.reconcileRouterStaticRoutes(ctx, router, routerUUID)
}

type routerExternal struct {
	network    *model.Network
	subnet     *model.Subnet
	prefix     netip.Prefix
	gateway    netip.Addr
	externalIP netip.Addr
}

func (renderer *Renderer) validateRouterExternal(ctx context.Context, router *model.Router) (*routerExternal, error) {
	if err := validateReferencedIDs(router.ExternalNetworkID, router.ExternalSubnetID); err != nil {
		return nil, err
	}
	network, err := getResource[*model.Network](ctx, renderer.store, model.KindNetwork, router.ExternalNetworkID)
	if err != nil {
		return nil, err
	}
	if !network.External || network.ProviderNetworkID == "" {
		return nil, fmt.Errorf("router %q external network %q is not provider-backed and external", router.ID, network.ID)
	}
	if err := safeID(network.ProviderNetworkID); err != nil {
		return nil, fmt.Errorf("external network %q has an invalid provider network ID: %w", network.ID, err)
	}
	provider, err := getResource[*model.ProviderNetwork](ctx, renderer.store, model.KindProviderNetwork, network.ProviderNetworkID)
	if err != nil {
		return nil, err
	}
	segment, err := renderer.defaultProviderSegment(ctx, provider.ID)
	if err != nil {
		return nil, err
	}
	if err := segment.Validate(); err != nil {
		return nil, fmt.Errorf("external network %q provider segment %q is invalid: %w", network.ID, segment.ID, err)
	}
	if err := safePhysicalNetwork(segment.PhysicalNetwork); err != nil {
		return nil, err
	}
	subnet, err := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, router.ExternalSubnetID)
	if err != nil {
		return nil, err
	}
	if subnet.NetworkID != network.ID {
		return nil, fmt.Errorf("router %q external subnet %q does not belong to external network %q", router.ID, subnet.ID, network.ID)
	}
	prefix, err := netip.ParsePrefix(subnet.CIDR)
	if err != nil || !prefix.Addr().Is4() {
		return nil, fmt.Errorf("external subnet %q has an invalid IPv4 CIDR", subnet.ID)
	}
	prefix = prefix.Masked()
	externalIP, err := netip.ParseAddr(router.ExternalIPAddress)
	if err != nil || !externalIP.Is4() || !prefix.Contains(externalIP) {
		return nil, fmt.Errorf("router %q external IP is outside subnet %q", router.ID, subnet.ID)
	}
	gateway, err := subnetGateway(subnet)
	if err != nil {
		return nil, err
	}
	if externalIP == gateway {
		return nil, fmt.Errorf("router %q external IP must not equal subnet %q gateway", router.ID, subnet.ID)
	}
	return &routerExternal{network: network, subnet: subnet, prefix: prefix, gateway: gateway, externalIP: externalIP}, nil
}

func (renderer *Renderer) enabledGatewayChassis(ctx context.Context) ([]string, error) {
	resources, err := renderer.store.List(ctx, model.KindNode, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	chassis := make([]string, 0, len(resources))
	seen := make(map[string]bool, len(resources))
	for _, resource := range resources {
		node, ok := resource.(*model.Node)
		if !ok {
			return nil, fmt.Errorf("control store returned %T while listing nodes", resource)
		}
		if !node.Enabled || node.State == model.ResourceDeleting || !hasNodeRole(node.Roles, model.NodeRoleGateway) {
			continue
		}
		if err := safeID(node.ChassisID); err != nil {
			return nil, fmt.Errorf("gateway node %q has an invalid chassis ID: %w", node.ID, err)
		}
		if seen[node.ChassisID] {
			return nil, fmt.Errorf("gateway chassis ID %q is assigned to multiple nodes", node.ChassisID)
		}
		seen[node.ChassisID] = true
		chassis = append(chassis, node.ChassisID)
	}
	sort.Strings(chassis)
	if len(chassis) > 32768 {
		return nil, errors.New("OVN supports at most 32768 distinct gateway chassis priorities")
	}
	return chassis, nil
}

func hasNodeRole(roles []model.NodeRole, expected model.NodeRole) bool {
	for _, role := range roles {
		if role == expected {
			return true
		}
	}
	return false
}

func (renderer *Renderer) renderRouterGateway(ctx context.Context, router *model.Router, routerUUID, externalSwitchUUID string, external *routerExternal, chassis []string) error {
	routerPort := gatewayRouterPort(router.ID)
	switchPort := gatewaySwitchPort(router.ID)
	network := fmt.Sprintf("%s/%d", external.externalIP, external.prefix.Bits())
	args := renderer.routerGatewayArgs(router, routerUUID, externalSwitchUUID, external.network.ID, routerPort, switchPort, network, false)
	if _, err := renderer.client.run(ctx, args...); err != nil {
		// The --may-exist forms intentionally reject a changed LRP network or
		// an LSP that belongs to another switch. Only retry destructively after
		// both read-only ownership probes succeed; a transient DB failure must
		// never be mistaken for a topology change.
		routerPortOutput, routerPortLookupErr := renderer.client.run(ctx, "--", "--if-exists", "get", "Logical_Router_Port", routerPort, "name")
		switchPortOutput, switchPortLookupErr := renderer.client.run(ctx, "--", "--if-exists", "get", "Logical_Switch_Port", switchPort, "name")
		if routerPortLookupErr != nil || switchPortLookupErr != nil || (len(strings.TrimSpace(string(routerPortOutput))) == 0 && len(strings.TrimSpace(string(switchPortOutput))) == 0) {
			return wrapRender("router gateway", router.ID, err)
		}
		args = renderer.routerGatewayArgs(router, routerUUID, externalSwitchUUID, external.network.ID, routerPort, switchPort, network, true)
		if _, retryErr := renderer.client.run(ctx, args...); retryErr != nil {
			return wrapRender("move router gateway", router.ID, retryErr)
		}
	}
	if err := renderer.renderRouterDefaultRoute(ctx, router, routerUUID, external.gateway, routerPort); err != nil {
		return err
	}
	return renderer.syncGatewayChassis(ctx, router.ID, routerPort, chassis)
}

func (renderer *Renderer) routerGatewayArgs(router *model.Router, routerUUID, switchUUID, networkID, routerPort, switchPort, network string, move bool) []string {
	mac := deterministicMAC("router-gateway:" + router.ID)
	args := make([]string, 0, 40)
	if move {
		args = append(args,
			"--", "--if-exists", "lsp-del", switchPort,
			"--", "--if-exists", "lrp-del", routerPort,
		)
	}
	args = append(args,
		"--", "--may-exist", "lrp-add", routerUUID, routerPort, mac, network,
		"--", "set", "Logical_Router_Port", routerPort, stringAssignment("mac", mac), stringAssignment("networks", network),
	)
	args = append(args, metadataAssignments(router, map[string]string{"pvn-role": "external-gateway"})...)
	args = append(args,
		"--", "--may-exist", "lsp-add", switchUUID, switchPort,
		"--", "lsp-set-type", switchPort, "router",
		"--", "lsp-set-addresses", switchPort, "router",
		"--", "lsp-set-options", switchPort, "router-port="+routerPort, "nat-addresses=router",
		"--", "set", "Logical_Switch_Port", switchPort,
	)
	return append(args, metadataAssignments(router, map[string]string{"pvn-network": networkID, "pvn-role": "external-gateway"})...)
}

func (renderer *Renderer) renderRouterDefaultRoute(ctx context.Context, router *model.Router, routerUUID string, gateway netip.Addr, routerPort string) error {
	assignments := []string{
		stringAssignment("ip_prefix", "0.0.0.0/0"),
		stringAssignment("nexthop", gateway.String()),
		stringAssignment("output_port", routerPort),
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", "router-default-route"),
		mapAssignment("external_ids", "pvn-router", router.ID),
		mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(router.Revision, 10)),
	}
	if _, err := renderer.ensureOwnedAttachedRow(ctx, routerDefaultRouteOwnedRow(router.ID), assignments,
		"Logical_Router", routerUUID, "static_routes"); err != nil {
		return wrapRender("router default route", router.ID, err)
	}
	return nil
}

func (renderer *Renderer) syncGatewayChassis(ctx context.Context, routerID, routerPort string, desired []string) error {
	existing, err := renderer.gatewayChassis(ctx, routerPort)
	if err != nil {
		return wrapRender("read router gateway chassis", routerID, err)
	}
	desiredSet := make(map[string]bool, len(desired))
	args := make([]string, 0, len(desired)*7)
	for index, chassis := range desired {
		desiredSet[chassis] = true
		priority := 32767 - index
		args = append(args, "--", "--may-exist", "lrp-set-gateway-chassis", routerPort, chassis, strconv.Itoa(priority))
	}
	if len(args) > 0 {
		if _, err := renderer.client.run(ctx, args...); err != nil {
			return wrapRender("set router gateway chassis", routerID, err)
		}
	}
	for _, chassis := range existing {
		if desiredSet[chassis] {
			continue
		}
		if _, err := renderer.client.run(ctx, "lrp-del-gateway-chassis", routerPort, chassis); err != nil {
			// Another active manager can delete the same stale row after our read.
			current, retryErr := renderer.gatewayChassis(ctx, routerPort)
			if retryErr != nil || containsString(current, chassis) {
				return wrapRender("remove stale router gateway chassis", routerID, err)
			}
		}
	}
	return nil
}

func (renderer *Renderer) gatewayChassis(ctx context.Context, routerPort string) ([]string, error) {
	output, err := renderer.client.run(ctx, "lrp-get-gateway-chassis", routerPort)
	if err != nil {
		return nil, err
	}
	prefix := routerPort + "-"
	result := make([]string, 0)
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], prefix) {
			return nil, fmt.Errorf("unexpected gateway chassis output %q", line)
		}
		chassis := strings.TrimPrefix(fields[0], prefix)
		priority, parseErr := strconv.Atoi(fields[1])
		if err := safeID(chassis); err != nil || parseErr != nil || priority < 0 || priority > 32767 {
			return nil, fmt.Errorf("unexpected gateway chassis output %q", line)
		}
		if !seen[chassis] {
			seen[chassis] = true
			result = append(result, chassis)
		}
	}
	sort.Strings(result)
	return result, nil
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (renderer *Renderer) routerInterface(ctx context.Context, routerInterface *model.RouterInterface) error {
	if err := validateReferencedIDs(routerInterface.RouterID, routerInterface.SubnetID); err != nil {
		return err
	}
	router, err := getResource[*model.Router](ctx, renderer.store, model.KindRouter, routerInterface.RouterID)
	if err != nil {
		return err
	}
	subnet, err := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, routerInterface.SubnetID)
	if err != nil {
		return err
	}
	network, err := getResource[*model.Network](ctx, renderer.store, model.KindNetwork, subnet.NetworkID)
	if err != nil {
		return err
	}
	if network.External {
		return fmt.Errorf("router interface %q cannot use external subnet %q", routerInterface.ID, subnet.ID)
	}
	gateway, err := subnetGateway(subnet)
	if err != nil {
		return err
	}
	prefix, _ := netip.ParsePrefix(subnet.CIDR)
	routerPort := "pvn-lrp-" + compact(routerInterface.ID)
	switchPort := "pvn-rsp-" + compact(routerInterface.ID)
	mac := deterministicMAC("router:" + routerInterface.ID)
	portNetwork := fmt.Sprintf("%s/%d", gateway, prefix.Bits())
	routerUUID, err := renderer.requireOwnedRow(ctx, logicalRouterOwnedRow(router.ID))
	if err != nil {
		return wrapRender("router interface router", routerInterface.ID, err)
	}
	switchUUID, err := renderer.requireOwnedRow(ctx, logicalSwitchOwnedRow(subnet.NetworkID))
	if err != nil {
		return wrapRender("router interface network", routerInterface.ID, err)
	}
	args := renderer.routerInterfaceArgs(routerInterface, routerUUID, switchUUID, routerPort, switchPort, mac, portNetwork, false)
	if _, err = renderer.client.run(ctx, args...); err != nil {
		// The may-exist forms reject a changed subnet or logical switch. Probe
		// both deterministic PVN port names before replacing them atomically;
		// a lookup failure must never turn into a destructive move.
		routerPortOutput, routerPortLookupErr := renderer.client.run(ctx, "--", "--if-exists", "get", "Logical_Router_Port", routerPort, "name")
		switchPortOutput, switchPortLookupErr := renderer.client.run(ctx, "--", "--if-exists", "get", "Logical_Switch_Port", switchPort, "name")
		if routerPortLookupErr != nil || switchPortLookupErr != nil || (len(strings.TrimSpace(string(routerPortOutput))) == 0 && len(strings.TrimSpace(string(switchPortOutput))) == 0) {
			return wrapRender("router interface", routerInterface.ID, err)
		}
		args = renderer.routerInterfaceArgs(routerInterface, routerUUID, switchUUID, routerPort, switchPort, mac, portNetwork, true)
		if _, retryErr := renderer.client.run(ctx, args...); retryErr != nil {
			return wrapRender("move router interface", routerInterface.ID, retryErr)
		}
	}
	if err := renderer.reconcileRouterSNAT(ctx, router, routerUUID); err != nil {
		return err
	}
	return renderer.reconcileRouterStaticRoutes(ctx, router, routerUUID)
}

func (renderer *Renderer) routerInterfaceArgs(routerInterface *model.RouterInterface, routerUUID, switchUUID, routerPort, switchPort, mac, portNetwork string, move bool) []string {
	args := make([]string, 0, 36)
	if move {
		args = append(args,
			"--", "--if-exists", "lsp-del", switchPort,
			"--", "--if-exists", "lrp-del", routerPort,
		)
	}
	args = append(args,
		"--", "--may-exist", "lrp-add", routerUUID, routerPort, mac, portNetwork,
		"--", "set", "Logical_Router_Port", routerPort, stringAssignment("mac", mac), stringAssignment("networks", portNetwork),
		"--", "--may-exist", "lsp-add", switchUUID, switchPort,
		"--", "lsp-set-type", switchPort, "router",
		"--", "lsp-set-addresses", switchPort, "router",
		"--", "lsp-set-options", switchPort, "router-port="+routerPort,
		"--", "set", "Logical_Router_Port", routerPort,
	)
	args = append(args, metadataAssignments(routerInterface, nil)...)
	return args
}

func (renderer *Renderer) reconcileRouterSNAT(ctx context.Context, router *model.Router, routerUUID string) error {
	actual, err := renderer.findMany(ctx, "NAT",
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", "router-snat"),
		mapAssignment("external_ids", "pvn-router", router.ID),
	)
	if err != nil {
		return wrapRender("list router SNAT", router.ID, err)
	}
	desired := make(map[string]bool)
	if router.State != model.ResourceDeleting && router.EnableSNAT && router.ExternalNetworkID != "" {
		if _, err := renderer.validateRouterExternal(ctx, router); err != nil {
			return err
		}
		interfaces, err := renderer.store.List(ctx, model.KindRouterInterface, controlstore.ListOptions{})
		if err != nil {
			return err
		}
		for _, resource := range interfaces {
			routerInterface, ok := resource.(*model.RouterInterface)
			if !ok {
				return fmt.Errorf("control store returned %T while listing router interfaces", resource)
			}
			if routerInterface.RouterID != router.ID || routerInterface.State == model.ResourceDeleting {
				continue
			}
			subnet, err := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, routerInterface.SubnetID)
			if err != nil {
				return err
			}
			network, err := getResource[*model.Network](ctx, renderer.store, model.KindNetwork, subnet.NetworkID)
			if err != nil {
				return err
			}
			if network.External {
				return fmt.Errorf("router interface %q is not an internal subnet of router %q", routerInterface.ID, router.ID)
			}
			prefix, err := netip.ParsePrefix(subnet.CIDR)
			if err != nil || !prefix.Addr().Is4() {
				return fmt.Errorf("router interface %q subnet %q has an invalid IPv4 CIDR", routerInterface.ID, subnet.ID)
			}
			uuid := routerSNATUUID(router.ID, routerInterface.ID)
			desired[uuid] = true
			assignments := []string{
				stringAssignment("type", "snat"),
				stringAssignment("external_ip", router.ExternalIPAddress),
				stringAssignment("logical_ip", prefix.Masked().String()),
				mapAssignment("external_ids", "pvn-managed", "true"),
				mapAssignment("external_ids", "pvn-kind", "router-snat"),
				mapAssignment("external_ids", "pvn-router", router.ID),
				mapAssignment("external_ids", "pvn-router-interface", routerInterface.ID),
				mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(router.Revision, 10)),
				mapAssignment("external_ids", "pvn-interface-revision", strconv.FormatInt(routerInterface.Revision, 10)),
			}
			actual, err := renderer.ensureOwnedAttachedRow(ctx, routerSNATOwnedRow(router.ID, routerInterface.ID), assignments,
				"Logical_Router", routerUUID, "nat")
			if err != nil {
				return wrapRender("router SNAT", routerInterface.ID, err)
			}
			delete(desired, uuid)
			desired[actual] = true
		}
	}
	stale := make([]string, 0)
	for _, uuid := range actual {
		if !desired[uuid] {
			stale = append(stale, uuid)
		}
	}
	return renderer.removeRouterSNATRows(ctx, router.ID, routerUUID, stale)
}

func (renderer *Renderer) removeRouterSNATRows(ctx context.Context, routerID, routerUUID string, uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}
	args := make([]string, 0, len(uuids)*12)
	for _, uuid := range uuids {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "nat", uuid)
	}
	for _, uuid := range uuids {
		args = append(args, "--", "--if-exists", "destroy", "NAT", uuid)
	}
	_, err := renderer.client.run(ctx, args...)
	return wrapRender("remove stale router SNAT", routerID, err)
}

func (renderer *Renderer) reconcileRouterStaticRoutes(ctx context.Context, router *model.Router, routerUUID string) error {
	actual, err := renderer.findMany(ctx, "Logical_Router_Static_Route",
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", "router-static-route"),
		mapAssignment("external_ids", "pvn-router", router.ID),
	)
	if err != nil {
		return wrapRender("list router static routes", router.ID, err)
	}
	desired := make(map[string]bool, len(router.StaticRoutes))
	if router.State != model.ResourceDeleting {
		for _, route := range router.StaticRoutes {
			outputPort, err := renderer.staticRouteOutputPort(ctx, router, route)
			if err != nil {
				return err
			}
			row := routerStaticRouteOwnedRow(router.ID, route)
			desired[row.deterministicUUID] = true
			assignments := []string{
				stringAssignment("ip_prefix", route.Destination),
				stringAssignment("nexthop", route.NextHop),
				stringAssignment("output_port", outputPort),
				mapAssignment("external_ids", "pvn-managed", "true"),
				mapAssignment("external_ids", "pvn-kind", "router-static-route"),
				mapAssignment("external_ids", "pvn-router", router.ID),
				mapAssignment("external_ids", "pvn-route-key", routerStaticRouteKey(route)),
				mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(router.Revision, 10)),
			}
			resolved, err := renderer.ensureOwnedAttachedRow(ctx, row, assignments, "Logical_Router", routerUUID, "static_routes")
			if err != nil {
				return wrapRender("router static route", router.ID, err)
			}
			delete(desired, row.deterministicUUID)
			desired[resolved] = true
		}
	}
	stale := make([]string, 0)
	for _, uuid := range actual {
		if !desired[uuid] {
			stale = append(stale, uuid)
		}
	}
	return renderer.removeRouterStaticRouteRows(ctx, router.ID, routerUUID, stale)
}

func (renderer *Renderer) staticRouteOutputPort(ctx context.Context, router *model.Router, route model.StaticRoute) (string, error) {
	matches := make([]string, 0, 2)
	if router.ExternalSubnetID != "" {
		subnet, err := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, router.ExternalSubnetID)
		if err != nil {
			return "", err
		}
		if subnet.State != model.ResourceDeleting && model.ValidateIPv4NextHop(subnet, route.NextHop, router.ExternalIPAddress) == nil {
			matches = append(matches, gatewayRouterPort(router.ID))
		}
	}
	interfaces, err := renderer.store.List(ctx, model.KindRouterInterface, controlstore.ListOptions{RouterID: router.ID})
	if err != nil {
		return "", err
	}
	for _, resource := range interfaces {
		routerInterface, ok := resource.(*model.RouterInterface)
		if !ok {
			return "", fmt.Errorf("control store returned %T while listing router interfaces", resource)
		}
		if routerInterface.RouterID != router.ID || routerInterface.State == model.ResourceDeleting {
			continue
		}
		subnet, err := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, routerInterface.SubnetID)
		if err != nil {
			return "", err
		}
		gateway, err := model.EffectiveIPv4Gateway(subnet)
		if err == nil && subnet.State != model.ResourceDeleting && model.ValidateIPv4NextHop(subnet, route.NextHop, gateway.String()) == nil {
			matches = append(matches, "pvn-lrp-"+compact(routerInterface.ID))
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("router %q static route %s via %s has no on-link attachment", router.ID, route.Destination, route.NextHop)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("router %q static route %s via %s has ambiguous attachments %v", router.ID, route.Destination, route.NextHop, matches)
	}
	return matches[0], nil
}

func (renderer *Renderer) removeRouterStaticRouteRows(ctx context.Context, routerID, routerUUID string, uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}
	args := make([]string, 0, len(uuids)*12)
	for _, uuid := range uuids {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "static_routes", uuid)
	}
	for _, uuid := range uuids {
		args = append(args, "--", "--if-exists", "destroy", "Logical_Router_Static_Route", uuid)
	}
	_, err := renderer.client.run(ctx, args...)
	return wrapRender("remove stale router static routes", routerID, err)
}

func (renderer *Renderer) deleteRouterInterface(ctx context.Context, routerInterface *model.RouterInterface) error {
	routerUUID, err := renderer.lookupOwnedRow(ctx, logicalRouterOwnedRow(routerInterface.RouterID))
	if err != nil {
		return wrapRender("delete router interface", routerInterface.ID, err)
	}
	snatUUID, err := renderer.lookupOwnedRow(ctx, routerSNATOwnedRow(routerInterface.RouterID, routerInterface.ID))
	if err != nil {
		return wrapRender("delete router interface", routerInterface.ID, err)
	}
	args := []string{
		"--", "--if-exists", "lsp-del", "pvn-rsp-" + compact(routerInterface.ID),
		"--", "--if-exists", "lrp-del", "pvn-lrp-" + compact(routerInterface.ID),
	}
	if routerUUID != "" && snatUUID != "" {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "nat", snatUUID)
	}
	if snatUUID != "" {
		args = append(args, "--", "--if-exists", "destroy", "NAT", snatUUID)
	}
	if _, err := renderer.client.run(ctx, args...); err != nil {
		return wrapRender("delete router interface", routerInterface.ID, err)
	}
	router, err := getResource[*model.Router](ctx, renderer.store, model.KindRouter, routerInterface.RouterID)
	if errors.Is(err, controlstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if routerUUID == "" {
		return fmt.Errorf("router %q remains in desired state but its PVN-owned Logical_Router row is absent", router.ID)
	}
	return renderer.reconcileRouterSNAT(ctx, router, routerUUID)
}

func (renderer *Renderer) clearRouterExternal(ctx context.Context, router *model.Router, routerUUID string) error {
	snats, err := renderer.findMany(ctx, "NAT",
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", "router-snat"),
		mapAssignment("external_ids", "pvn-router", router.ID),
	)
	if err != nil {
		return wrapRender("list router SNAT", router.ID, err)
	}
	routeUUID, err := renderer.lookupOwnedRow(ctx, routerDefaultRouteOwnedRow(router.ID))
	if err != nil {
		return wrapRender("read router default route", router.ID, err)
	}
	args := []string{
		"--", "--if-exists", "lsp-del", gatewaySwitchPort(router.ID),
		"--", "--if-exists", "lrp-del", gatewayRouterPort(router.ID),
	}
	if routeUUID != "" {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "static_routes", routeUUID)
	}
	for _, uuid := range snats {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "nat", uuid)
	}
	if routeUUID != "" {
		args = append(args, "--", "--if-exists", "destroy", "Logical_Router_Static_Route", routeUUID)
	}
	for _, uuid := range snats {
		args = append(args, "--", "--if-exists", "destroy", "NAT", uuid)
	}
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("clear router external gateway", router.ID, err)
}

func (renderer *Renderer) deleteRouter(ctx context.Context, router *model.Router) error {
	snats, err := renderer.findMany(ctx, "NAT",
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", "router-snat"),
		mapAssignment("external_ids", "pvn-router", router.ID),
	)
	if err != nil {
		return wrapRender("list router SNAT", router.ID, err)
	}
	staticRoutes, err := renderer.findMany(ctx, "Logical_Router_Static_Route",
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", "router-static-route"),
		mapAssignment("external_ids", "pvn-router", router.ID),
	)
	if err != nil {
		return wrapRender("list router static routes", router.ID, err)
	}
	routerUUID, err := renderer.lookupOwnedRow(ctx, logicalRouterOwnedRow(router.ID))
	if err != nil {
		return wrapRender("read router", router.ID, err)
	}
	routeUUID, err := renderer.lookupOwnedRow(ctx, routerDefaultRouteOwnedRow(router.ID))
	if err != nil {
		return wrapRender("read router default route", router.ID, err)
	}
	args := []string{
		"--", "--if-exists", "lsp-del", gatewaySwitchPort(router.ID),
		"--", "--if-exists", "lrp-del", gatewayRouterPort(router.ID),
	}
	if routerUUID != "" && routeUUID != "" {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "static_routes", routeUUID)
	}
	for _, uuid := range staticRoutes {
		if routerUUID != "" {
			args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "static_routes", uuid)
		}
	}
	for _, uuid := range snats {
		if routerUUID != "" {
			args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "nat", uuid)
		}
	}
	if routerUUID != "" {
		args = append(args, "--", "--if-exists", "lr-del", routerUUID)
	}
	if routeUUID != "" {
		args = append(args, "--", "--if-exists", "destroy", "Logical_Router_Static_Route", routeUUID)
	}
	for _, uuid := range staticRoutes {
		args = append(args, "--", "--if-exists", "destroy", "Logical_Router_Static_Route", uuid)
	}
	for _, uuid := range snats {
		args = append(args, "--", "--if-exists", "destroy", "NAT", uuid)
	}
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("delete router", router.ID, err)
}

func (renderer *Renderer) floatingIP(ctx context.Context, floatingIP *model.FloatingIP) error {
	if err := validateReferencedIDs(floatingIP.ProviderNetworkID); err != nil {
		return err
	}
	row := floatingIPOwnedRow(floatingIP.ID)
	routers, err := renderer.store.List(ctx, model.KindRouter, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	if floatingIP.RouterID == "" || floatingIP.FixedIPAddress == "" {
		uuid, lookupErr := renderer.lookupOwnedRow(ctx, row)
		if lookupErr != nil || uuid == "" {
			return wrapRender("remove floating IP", floatingIP.ID, lookupErr)
		}
		args := make([]string, 0, len(routers)*7+6)
		for _, resource := range routers {
			router, ok := resource.(*model.Router)
			if !ok {
				return fmt.Errorf("control store returned %T while listing routers", resource)
			}
			routerUUID, lookupErr := renderer.lookupOwnedRow(ctx, logicalRouterOwnedRow(router.ID))
			if lookupErr != nil {
				return lookupErr
			}
			if routerUUID != "" {
				args = append(args, "--", "--if-exists", "remove", "Logical_Router", routerUUID, "nat", uuid)
			}
		}
		args = append(args, "--", "--if-exists", "destroy", "NAT", uuid)
		_, err := renderer.client.run(ctx, args...)
		return wrapRender("remove floating IP", floatingIP.ID, err)
	}
	if err := safeID(floatingIP.RouterID); err != nil {
		return fmt.Errorf("invalid floating-IP router ID: %w", err)
	}
	router, err := getResource[*model.Router](ctx, renderer.store, model.KindRouter, floatingIP.RouterID)
	if err != nil {
		return err
	}
	external, err := renderer.validateRouterExternal(ctx, router)
	if err != nil {
		return fmt.Errorf("floating IP %q router has no usable external gateway: %w", floatingIP.ID, err)
	}
	if external.network.ProviderNetworkID != floatingIP.ProviderNetworkID {
		return fmt.Errorf("floating IP %q provider network does not match router %q external network", floatingIP.ID, router.ID)
	}
	floatingAddress, err := netip.ParseAddr(floatingIP.Address)
	if err != nil || !external.prefix.Contains(floatingAddress) {
		return fmt.Errorf("floating IP %q address is outside router %q external subnet", floatingIP.ID, router.ID)
	}
	if floatingIP.PortID != "" {
		if err := safeID(floatingIP.PortID); err != nil {
			return fmt.Errorf("invalid floating-IP port ID: %w", err)
		}
		port, getErr := getResource[*model.Port](ctx, renderer.store, model.KindPort, floatingIP.PortID)
		if getErr != nil {
			return getErr
		}
		foundAddress := false
		for _, fixed := range port.FixedIPs {
			foundAddress = foundAddress || fixed.Address == floatingIP.FixedIPAddress
		}
		if !foundAddress {
			return fmt.Errorf("floating IP %q fixed address is not assigned to port %q", floatingIP.ID, port.ID)
		}
	}
	assignments := []string{
		stringAssignment("type", "dnat_and_snat"),
		stringAssignment("external_ip", floatingIP.Address),
		stringAssignment("logical_ip", floatingIP.FixedIPAddress),
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", floatingIP.ResourceKind().String()),
		mapAssignment("external_ids", "pvn-id", floatingIP.ID),
		mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(floatingIP.Revision, 10)),
	}
	existing, findErr := renderer.lookupOwnedRow(ctx, row)
	if findErr != nil {
		return findErr
	}
	routerRows := make(map[string]string, len(routers))
	for _, resource := range routers {
		candidate, ok := resource.(*model.Router)
		if !ok {
			return fmt.Errorf("control store returned %T while listing routers", resource)
		}
		actual, lookupErr := renderer.lookupOwnedRow(ctx, logicalRouterOwnedRow(candidate.ID))
		if lookupErr != nil {
			return lookupErr
		}
		if actual != "" {
			routerRows[candidate.ID] = actual
		}
	}
	targetRouterUUID := routerRows[router.ID]
	if targetRouterUUID == "" {
		return fmt.Errorf("router %q has no PVN-owned Logical_Router row", router.ID)
	}
	childUUID := existing
	args := []string{"--"}
	if existing == "" {
		childUUID = row.deterministicUUID
		args = append(args, "--id="+childUUID, "create", "NAT")
	} else {
		args = append(args, "set", "NAT", childUUID)
	}
	args = append(args, assignments...)
	for _, resource := range routers {
		candidate, ok := resource.(*model.Router)
		if !ok {
			return fmt.Errorf("control store returned %T while listing routers", resource)
		}
		if candidate.ID != router.ID {
			if candidateRouterUUID := routerRows[candidate.ID]; candidateRouterUUID != "" {
				args = append(args, "--", "--if-exists", "remove", "Logical_Router", candidateRouterUUID, "nat", childUUID)
			}
		}
	}
	args = append(args, "--", "add", "Logical_Router", targetRouterUUID, "nat", childUUID)
	_, err = renderer.client.run(ctx, args...)
	if err != nil && existing == "" {
		// Deterministic UUIDs turn active-active creates into a harmless race.
		if found, retryErr := renderer.lookupOwnedRow(ctx, row); retryErr == nil && found != "" {
			return renderer.floatingIP(ctx, floatingIP)
		}
	} else if err == nil && existing == "" {
		if _, lookupErr := renderer.requireOwnedRow(ctx, row); lookupErr != nil {
			return wrapRender("floating IP", floatingIP.ID, lookupErr)
		}
	}
	return wrapRender("floating IP", floatingIP.ID, err)
}

func (renderer *Renderer) providerSegment(ctx context.Context, segment *model.ProviderSegment) error {
	if err := safeID(segment.ProviderNetworkID); err != nil {
		return err
	}
	if err := safePhysicalNetwork(segment.PhysicalNetwork); err != nil {
		return err
	}
	provider, err := getResource[*model.ProviderNetwork](ctx, renderer.store, model.KindProviderNetwork, segment.ProviderNetworkID)
	if err != nil {
		return err
	}
	if provider.DefaultSegmentID != "" && provider.DefaultSegmentID != segment.ID {
		return nil
	}
	if provider.DefaultSegmentID == "" {
		segments, listErr := renderer.store.List(ctx, model.KindProviderSegment, controlstore.ListOptions{})
		if listErr != nil {
			return listErr
		}
		matching := 0
		for _, resource := range segments {
			candidate, ok := resource.(*model.ProviderSegment)
			if !ok {
				return fmt.Errorf("control store returned %T while listing provider segments", resource)
			}
			if candidate.ProviderNetworkID == segment.ProviderNetworkID {
				matching++
			}
		}
		if matching > 1 {
			return fmt.Errorf("provider network %q has multiple segments but no default segment", provider.ID)
		}
	}
	networks, err := renderer.store.List(ctx, model.KindNetwork, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	for _, resource := range networks {
		network, ok := resource.(*model.Network)
		if !ok {
			return fmt.Errorf("control store returned %T while listing networks", resource)
		}
		if network.ProviderNetworkID != segment.ProviderNetworkID {
			continue
		}
		switchUUID, err := renderer.requireOwnedRow(ctx, logicalSwitchOwnedRow(network.ID))
		if err != nil {
			return wrapRender("provider network logical switch", network.ID, err)
		}
		if err := renderer.renderProviderPort(ctx, segment, network, switchUUID); err != nil {
			return err
		}
	}
	return nil
}

func (renderer *Renderer) defaultProviderSegment(ctx context.Context, providerID string) (*model.ProviderSegment, error) {
	provider, err := getResource[*model.ProviderNetwork](ctx, renderer.store, model.KindProviderNetwork, providerID)
	if err != nil {
		return nil, err
	}
	if provider.DefaultSegmentID != "" {
		segment, err := getResource[*model.ProviderSegment](ctx, renderer.store, model.KindProviderSegment, provider.DefaultSegmentID)
		if err != nil {
			return nil, err
		}
		if segment.ProviderNetworkID != providerID {
			return nil, fmt.Errorf("default segment %q belongs to another provider network", segment.ID)
		}
		return segment, nil
	}
	resources, err := renderer.store.List(ctx, model.KindProviderSegment, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	var selected *model.ProviderSegment
	for _, resource := range resources {
		segment, ok := resource.(*model.ProviderSegment)
		if !ok {
			return nil, fmt.Errorf("control store returned %T while listing provider segments", resource)
		}
		if segment.ProviderNetworkID != providerID {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("provider network %q has multiple segments but no default segment", providerID)
		}
		selected = segment
	}
	if selected == nil {
		return nil, fmt.Errorf("provider network %q has no segment", providerID)
	}
	return selected, nil
}

func (renderer *Renderer) renderProviderPort(ctx context.Context, segment *model.ProviderSegment, network *model.Network, switchUUID string) error {
	port := "pvn-localnet-" + compact(network.ID)
	existing, err := renderer.ownedProviderPort(ctx, network.ID, port)
	if err != nil {
		return wrapRender("provider port ownership", network.ID, err)
	}
	target := port
	args := make([]string, 0, 32)
	if existing == "" {
		args = append(args, "--", "--may-exist", "lsp-add", switchUUID, port)
	} else {
		// Address the row by UUID after the ownership probe so a same-name row
		// cannot be substituted between the read and the update.
		target = existing
	}
	args = append(args,
		"--", "lsp-set-addresses", target, "unknown",
		"--", "lsp-set-type", target, "localnet",
		"--", "lsp-set-options", target, "network_name="+segment.PhysicalNetwork,
		"--", "set", "Logical_Switch_Port", target,
	)
	args = append(args, metadataAssignments(segment, map[string]string{"pvn-network": network.ID})...)
	if segment.NetworkType == model.ProviderVLAN {
		args = append(args, "--", "set", "Logical_Switch_Port", target, "tag="+strconv.Itoa(segment.VLANID))
	} else {
		args = append(args, "--", "clear", "Logical_Switch_Port", target, "tag")
	}
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("provider segment", segment.ID, err)
}

func (renderer *Renderer) removeProviderPort(ctx context.Context, network *model.Network) error {
	port := "pvn-localnet-" + compact(network.ID)
	existing, err := renderer.ownedProviderPort(ctx, network.ID, port)
	if err != nil {
		return wrapRender("provider port ownership", network.ID, err)
	}
	if existing == "" {
		return nil
	}
	_, err = renderer.client.run(ctx, "--", "--if-exists", "lsp-del", existing)
	return wrapRender("remove provider port", network.ID, err)
}

// ownedProviderPort returns the UUID of the deterministic localnet port only
// when the row has all ownership markers written by PVN. Provider changes are
// allowed to replace pvn-id (the segment ID), so network ownership is the
// stable identity across those updates.
func (renderer *Renderer) ownedProviderPort(ctx context.Context, networkID, port string) (string, error) {
	existing, err := renderer.findOne(ctx, "Logical_Switch_Port", stringAssignment("name", port))
	if err != nil || existing == "" {
		return existing, err
	}
	owned, err := renderer.findMany(ctx, "Logical_Switch_Port",
		stringAssignment("name", port),
		stringAssignment("type", "localnet"),
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", model.KindProviderSegment.String()),
		mapAssignment("external_ids", "pvn-network", networkID),
	)
	if err != nil {
		return "", err
	}
	if len(owned) != 1 || owned[0] != existing {
		return "", fmt.Errorf("logical switch port %q exists but is not owned by PVN network %q", port, networkID)
	}
	return existing, nil
}

func (renderer *Renderer) securityGroup(ctx context.Context, group *model.SecurityGroup) error {
	name := portGroup(group.ID)
	metadata := metadataAssignments(group, nil)
	assignments := append([]string{stringAssignment("name", name)}, metadata...)
	if _, err := renderer.ensureOwnedRow(ctx, portGroupOwnedRow(group.ID), assignments); err != nil {
		return wrapRender("security group", group.ID, err)
	}
	for _, exception := range []struct {
		owner     string
		direction string
		match     string
	}{
		{owner: group.ID + ":dhcpv4-client", direction: "from-lport", match: "ip4 && udp && udp.src == 68 && udp.dst == 67"},
		{owner: group.ID + ":dhcpv4-server", direction: "to-lport", match: "ip4 && udp && udp.src == 67 && udp.dst == 68"},
	} {
		if err := renderer.ensureACL(ctx, name, exception.owner, exception.direction, 3000, exception.match, "allow", group.Revision); err != nil {
			return err
		}
	}
	for _, direction := range []string{"to-lport", "from-lport"} {
		owner := group.ID + ":default-drop:" + direction
		if err := renderer.ensureACL(ctx, name, owner, direction, 1000, "ip4", "drop", group.Revision); err != nil {
			return err
		}
	}
	return nil
}

func (renderer *Renderer) securityGroupRule(ctx context.Context, rule *model.SecurityGroupRule) error {
	if err := validateReferencedIDs(rule.SecurityGroupID); err != nil {
		return err
	}
	group, err := getResource[*model.SecurityGroup](ctx, renderer.store, model.KindSecurityGroup, rule.SecurityGroupID)
	if err != nil {
		return err
	}
	if rule.RemoteGroupID != "" {
		if err := safeID(rule.RemoteGroupID); err != nil {
			return fmt.Errorf("invalid remote group ID: %w", err)
		}
		if _, getErr := getResource[*model.SecurityGroup](ctx, renderer.store, model.KindSecurityGroup, rule.RemoteGroupID); getErr != nil {
			return getErr
		}
	}
	spec, err := securityGroupRuleACLSpec(rule)
	if err != nil {
		return err
	}
	return renderer.ensureACL(ctx, portGroup(group.ID), rule.ID, spec.direction, spec.priority, spec.match, spec.action, rule.Revision)
}

type securityGroupACLSpec struct {
	direction string
	priority  int
	match     string
	action    string
}

func securityGroupRuleACLSpec(rule *model.SecurityGroupRule) (securityGroupACLSpec, error) {
	if rule == nil {
		return securityGroupACLSpec{}, errors.New("security group rule is nil")
	}
	direction := "to-lport"
	remoteField := "ip4.src"
	if rule.Direction == model.DirectionEgress {
		direction = "from-lport"
		remoteField = "ip4.dst"
	}
	match := []string{"ip4"}
	protocol := strings.ToLower(rule.Protocol)
	if protocol != "" {
		if protocol == "icmp" {
			protocol = "icmp4"
		}
		match = append(match, protocol)
	}
	if rule.RemoteCIDR != "" {
		match = append(match, remoteField+" == "+rule.RemoteCIDR)
	} else if rule.RemoteGroupID != "" {
		if err := safeID(rule.RemoteGroupID); err != nil {
			return securityGroupACLSpec{}, fmt.Errorf("invalid remote group ID: %w", err)
		}
		match = append(match, remoteField+" == $"+portGroup(rule.RemoteGroupID)+"_ip4")
	}
	if (protocol == "tcp" || protocol == "udp") && rule.PortRangeMin != 0 {
		if rule.PortRangeMax == 0 || rule.PortRangeMax == rule.PortRangeMin {
			match = append(match, protocol+".dst == "+strconv.Itoa(rule.PortRangeMin))
		} else {
			match = append(match,
				protocol+".dst >= "+strconv.Itoa(rule.PortRangeMin),
				protocol+".dst <= "+strconv.Itoa(rule.PortRangeMax),
			)
		}
	}
	action := "drop"
	priority := 2500
	if rule.Action == model.ActionAllow {
		action = "allow-related"
		priority = 2000
	}
	return securityGroupACLSpec{
		direction: direction, priority: priority,
		match: strings.Join(match, " && "), action: action,
	}, nil
}

func (renderer *Renderer) ensureACL(ctx context.Context, group, owner, direction string, priority int, match, action string, revision int64) error {
	row := aclOwnedRow(owner)
	existing, err := renderer.lookupOwnedRow(ctx, row)
	if err != nil {
		return err
	}
	assignments := []string{
		stringAssignment("direction", direction),
		"priority=" + strconv.Itoa(priority),
		stringAssignment("match", match),
		stringAssignment("action", action),
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-owner", owner),
		mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(revision, 10)),
	}
	groups, listErr := renderer.store.List(ctx, model.KindSecurityGroup, controlstore.ListOptions{})
	if listErr != nil {
		return listErr
	}
	appendMembership := func(args []string, aclUUID string) ([]string, error) {
		foundDesired := false
		for _, resource := range groups {
			candidate, ok := resource.(*model.SecurityGroup)
			if !ok {
				return nil, fmt.Errorf("control store returned %T while listing security groups", resource)
			}
			candidateName := portGroup(candidate.ID)
			groupUUID, lookupErr := renderer.lookupOwnedRow(ctx, portGroupOwnedRow(candidate.ID))
			if lookupErr != nil {
				return nil, lookupErr
			}
			if candidateName == group {
				foundDesired = true
				if groupUUID == "" {
					return nil, fmt.Errorf("target port group %q has no PVN-owned OVN row", group)
				}
				args = append(args, "--", "add", "Port_Group", groupUUID, "acls", aclUUID)
				continue
			}
			if groupUUID != "" {
				args = append(args, "--", "--if-exists", "remove", "Port_Group", groupUUID, "acls", aclUUID)
			}
		}
		if !foundDesired {
			return nil, fmt.Errorf("target port group %q is absent from desired state", group)
		}
		return args, nil
	}
	if existing == "" {
		args := append([]string{"--", "--id=" + row.deterministicUUID, "create", "ACL"}, assignments...)
		args, err = appendMembership(args, row.deterministicUUID)
		if err != nil {
			return err
		}
		_, err = renderer.client.run(ctx, args...)
		if err != nil {
			if found, findErr := renderer.lookupOwnedRow(ctx, row); findErr == nil && found != "" {
				return renderer.ensureACL(ctx, group, owner, direction, priority, match, action, revision)
			}
		} else {
			actual, findErr := renderer.requireOwnedRow(ctx, row)
			if findErr != nil {
				return wrapRender("security group ACL", owner, findErr)
			}
			if actual != row.deterministicUUID {
				args, membershipErr := appendMembership(append([]string{"set", "ACL", actual}, assignments...), actual)
				if membershipErr != nil {
					return membershipErr
				}
				_, err = renderer.client.run(ctx, args...)
			}
		}
	} else {
		args := append([]string{"set", "ACL", existing}, assignments...)
		args, err = appendMembership(args, existing)
		if err != nil {
			return err
		}
		_, err = renderer.client.run(ctx, args...)
	}
	return wrapRender("security group ACL", owner, err)
}

func (renderer *Renderer) portGroups(ctx context.Context, port *model.Port, portUUID string) error {
	groups, err := renderer.store.List(ctx, model.KindSecurityGroup, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	desired := make(map[string]bool, len(port.SecurityGroupIDs))
	for _, id := range port.SecurityGroupIDs {
		if err := safeID(id); err != nil {
			return fmt.Errorf("invalid security group ID: %w", err)
		}
		desired[id] = true
	}
	for _, resource := range groups {
		group, ok := resource.(*model.SecurityGroup)
		if !ok {
			return fmt.Errorf("control store returned %T while listing security groups", resource)
		}
		groupUUID, lookupErr := renderer.lookupOwnedRow(ctx, portGroupOwnedRow(group.ID))
		if lookupErr != nil {
			return lookupErr
		}
		command := "remove"
		if desired[group.ID] {
			command = "add"
			delete(desired, group.ID)
		}
		if groupUUID == "" {
			if command == "add" {
				return fmt.Errorf("port %q references security group %q whose PVN-owned OVN row is absent", port.ID, group.ID)
			}
			continue
		}
		args := []string{
			"--", "--id=@lsp", "get", "Logical_Switch_Port", portUUID,
			"--", command, "Port_Group", groupUUID, "ports", "@lsp",
		}
		if _, err := renderer.client.run(ctx, args...); err != nil {
			return wrapRender("security group membership", port.ID, err)
		}
	}
	if len(desired) != 0 {
		return fmt.Errorf("port %q references unknown security groups %v", port.ID, sortedKeys(desired))
	}
	return nil
}

func (renderer *Renderer) findOne(ctx context.Context, table, condition string) (string, error) {
	output, err := renderer.client.run(ctx, "--bare", "--columns=_uuid", "find", table, condition)
	if err != nil {
		return "", err
	}
	lines := strings.Fields(string(output))
	if len(lines) > 1 {
		return "", fmt.Errorf("OVN contains duplicate %s rows for %s", table, condition)
	}
	if len(lines) == 0 {
		return "", nil
	}
	if err := safeUUID(lines[0]); err != nil {
		return "", err
	}
	return lines[0], nil
}

func (renderer *Renderer) findUUID(ctx context.Context, table, uuid string) (string, error) {
	if err := safeUUID(uuid); err != nil {
		return "", err
	}
	output, err := renderer.client.run(ctx, "--bare", "--", "--if-exists", "get", table, uuid, "_uuid")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 1 {
		return "", fmt.Errorf("OVN returned multiple UUIDs while looking up %s row %q", table, uuid)
	}
	if err := safeUUID(fields[0]); err != nil {
		return "", err
	}
	if !strings.EqualFold(fields[0], uuid) {
		return "", fmt.Errorf("OVN returned UUID %q while looking up %s row %q", fields[0], table, uuid)
	}
	return fields[0], nil
}

func (renderer *Renderer) findMany(ctx context.Context, table string, conditions ...string) ([]string, error) {
	arguments := append([]string{"--bare", "--columns=_uuid", "find", table}, conditions...)
	output, err := renderer.client.run(ctx, arguments...)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, uuid := range strings.Fields(string(output)) {
		if err := safeUUID(uuid); err != nil {
			return nil, err
		}
		if !seen[uuid] {
			seen[uuid] = true
			result = append(result, uuid)
		}
	}
	sort.Strings(result)
	return result, nil
}

// ownedRow identifies one PVN-managed OVN row using values that survive an
// ovsdb-client restore. OVSDB restores preserve row data but allocate new row
// UUIDs, so the deterministic UUID is only the preferred UUID for a fresh
// insert; it is never sufficient proof of ownership on its own.
type ownedRow struct {
	table             string
	deterministicUUID string
	name              string
	identity          []string
}

func managedRow(table, uuid, name string, identity ...string) ownedRow {
	return ownedRow{
		table:             table,
		deterministicUUID: uuid,
		name:              name,
		identity: append([]string{
			mapAssignment("external_ids", "pvn-managed", "true"),
		}, identity...),
	}
}

func logicalSwitchOwnedRow(networkID string) ownedRow {
	return managedRow(
		"Logical_Switch",
		logicalSwitchUUID(networkID),
		logicalSwitch(networkID),
		mapAssignment("external_ids", "pvn-kind", model.KindNetwork.String()),
		mapAssignment("external_ids", "pvn-id", networkID),
	)
}

func logicalRouterOwnedRow(routerID string) ownedRow {
	return managedRow(
		"Logical_Router",
		logicalRouterUUID(routerID),
		logicalRouter(routerID),
		mapAssignment("external_ids", "pvn-kind", model.KindRouter.String()),
		mapAssignment("external_ids", "pvn-id", routerID),
	)
}

func dhcpOptionsOwnedRow(subnetID string) ownedRow {
	return managedRow(
		"DHCP_Options",
		deterministicUUID("dhcp-options:"+subnetID),
		"",
		mapAssignment("external_ids", "pvn-kind", model.KindSubnet.String()),
		mapAssignment("external_ids", "pvn-id", subnetID),
	)
}

func logicalSwitchPortOwnedRow(portID, lspName string) ownedRow {
	return managedRow(
		"Logical_Switch_Port",
		deterministicUUID("logical-switch-port:"+portID),
		lspName,
		mapAssignment("external_ids", "pvn-kind", model.KindPort.String()),
		mapAssignment("external_ids", "pvn-id", portID),
	)
}

func portGroupOwnedRow(groupID string) ownedRow {
	return managedRow(
		"Port_Group",
		deterministicUUID("port-group:"+groupID),
		portGroup(groupID),
		mapAssignment("external_ids", "pvn-kind", model.KindSecurityGroup.String()),
		mapAssignment("external_ids", "pvn-id", groupID),
	)
}

func aclOwnedRow(owner string) ownedRow {
	return managedRow(
		"ACL",
		deterministicUUID("acl:"+owner),
		"",
		mapAssignment("external_ids", "pvn-owner", owner),
	)
}

func routerDefaultRouteOwnedRow(routerID string) ownedRow {
	return managedRow(
		"Logical_Router_Static_Route",
		routerDefaultRouteUUID(routerID),
		"",
		mapAssignment("external_ids", "pvn-kind", "router-default-route"),
		mapAssignment("external_ids", "pvn-router", routerID),
	)
}

func routerStaticRouteOwnedRow(routerID string, route model.StaticRoute) ownedRow {
	key := routerStaticRouteKey(route)
	return managedRow(
		"Logical_Router_Static_Route",
		routerStaticRouteUUID(routerID, route),
		"",
		mapAssignment("external_ids", "pvn-kind", "router-static-route"),
		mapAssignment("external_ids", "pvn-router", routerID),
		mapAssignment("external_ids", "pvn-route-key", key),
	)
}

func routerSNATOwnedRow(routerID, routerInterfaceID string) ownedRow {
	return managedRow(
		"NAT",
		routerSNATUUID(routerID, routerInterfaceID),
		"",
		mapAssignment("external_ids", "pvn-kind", "router-snat"),
		mapAssignment("external_ids", "pvn-router", routerID),
		mapAssignment("external_ids", "pvn-router-interface", routerInterfaceID),
	)
}

func floatingIPOwnedRow(floatingIPID string) ownedRow {
	return managedRow(
		"NAT",
		deterministicUUID("floating-ip-nat:"+floatingIPID),
		"",
		mapAssignment("external_ids", "pvn-kind", model.KindFloatingIP.String()),
		mapAssignment("external_ids", "pvn-id", floatingIPID),
	)
}

// lookupOwnedRow returns the actual OVN UUID of exactly one matching PVN row.
// It deliberately probes the stable identity, the stable name (when present),
// and the preferred deterministic UUID independently. This rejects both an
// unowned name/UUID collision and a restored row plus a conflicting fresh row
// instead of silently updating or deleting the wrong object.
func (renderer *Renderer) lookupOwnedRow(ctx context.Context, row ownedRow) (string, error) {
	if row.table == "" || len(row.identity) == 0 {
		return "", errors.New("OVN owned-row table and identity are required")
	}
	if err := safeUUID(row.deterministicUUID); err != nil {
		return "", err
	}

	owned, err := renderer.findMany(ctx, row.table, row.identity...)
	if err != nil {
		return "", err
	}
	if len(owned) > 1 {
		return "", fmt.Errorf("OVN contains duplicate PVN-owned %s rows for %s", row.table, strings.Join(row.identity, ", "))
	}

	var named string
	if row.name != "" {
		named, err = renderer.findOne(ctx, row.table, stringAssignment("name", row.name))
		if err != nil {
			return "", err
		}
	}
	preferred, err := renderer.findUUID(ctx, row.table, row.deterministicUUID)
	if err != nil {
		return "", err
	}

	if len(owned) == 0 {
		if named != "" {
			return "", fmt.Errorf("OVN %s name %q is occupied by row %q that is not owned by the expected PVN resource", row.table, row.name, named)
		}
		if preferred != "" {
			return "", fmt.Errorf("OVN %s deterministic UUID %q is occupied by a row that is not owned by the expected PVN resource", row.table, preferred)
		}
		return "", nil
	}

	actual := owned[0]
	if row.name != "" && named != actual {
		if named == "" {
			return "", fmt.Errorf("PVN-owned %s row %q does not have expected name %q", row.table, actual, row.name)
		}
		return "", fmt.Errorf("PVN-owned %s row %q conflicts with expected-name row %q", row.table, actual, named)
	}
	if preferred != "" && preferred != actual {
		return "", fmt.Errorf("PVN-owned %s row %q conflicts with deterministic-UUID row %q", row.table, actual, preferred)
	}
	return actual, nil
}

func (renderer *Renderer) requireOwnedRow(ctx context.Context, row ownedRow) (string, error) {
	actual, err := renderer.lookupOwnedRow(ctx, row)
	if err != nil {
		return "", err
	}
	if actual == "" {
		return "", fmt.Errorf("PVN-owned %s row is absent for %s", row.table, strings.Join(row.identity, ", "))
	}
	return actual, nil
}

func (renderer *Renderer) ensureOwnedRow(ctx context.Context, row ownedRow, assignments []string) (string, error) {
	existing, err := renderer.lookupOwnedRow(ctx, row)
	if err != nil {
		return "", err
	}
	if existing != "" {
		args := append([]string{"set", row.table, existing}, assignments...)
		_, err = renderer.client.run(ctx, args...)
		return existing, err
	}

	args := append([]string{"--", "--id=" + row.deterministicUUID, "create", row.table}, assignments...)
	_, createErr := renderer.client.run(ctx, args...)
	actual, lookupErr := renderer.lookupOwnedRow(ctx, row)
	if lookupErr != nil {
		if createErr != nil {
			return "", createErr
		}
		return "", lookupErr
	}
	if actual == "" {
		if createErr != nil {
			return "", createErr
		}
		return "", fmt.Errorf("created %s row is not discoverable by its PVN ownership identity", row.table)
	}
	if createErr == nil {
		return actual, nil
	}

	// A second active manager can win the deterministic create. Once its row
	// is proven to be the unique PVN-owned row, converge its mutable fields.
	args = append([]string{"set", row.table, actual}, assignments...)
	_, err = renderer.client.run(ctx, args...)
	return actual, err
}

func (renderer *Renderer) ensureOwnedAttachedRow(ctx context.Context, row ownedRow, assignments []string, parentTable, parent, column string) (string, error) {
	existing, err := renderer.lookupOwnedRow(ctx, row)
	if err != nil {
		return "", err
	}
	args := []string{"--"}
	child := existing
	if existing == "" {
		child = row.deterministicUUID
		args = append(args, "--id="+child, "create", row.table)
	} else {
		args = append(args, "set", row.table, child)
	}
	args = append(args, assignments...)
	args = append(args, "--", "add", parentTable, parent, column, child)
	_, mutationErr := renderer.client.run(ctx, args...)
	if mutationErr == nil {
		if existing != "" {
			return existing, nil
		}
		actual, lookupErr := renderer.requireOwnedRow(ctx, row)
		if lookupErr != nil {
			return "", lookupErr
		}
		return actual, nil
	}
	if existing != "" {
		return "", mutationErr
	}

	// A concurrent insert is safe only after the stable identity resolves to
	// one row. Reattach using that row's actual (possibly restored) UUID.
	actual, lookupErr := renderer.lookupOwnedRow(ctx, row)
	if lookupErr != nil || actual == "" {
		return "", mutationErr
	}
	args = append([]string{"--", "set", row.table, actual}, assignments...)
	args = append(args, "--", "add", parentTable, parent, column, actual)
	_, err = renderer.client.run(ctx, args...)
	return actual, err
}

func metadataAssignments(resource model.Resource, extra map[string]string) []string {
	metadata := resource.GetMetadata()
	result := []string{
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", resource.ResourceKind().String()),
		mapAssignment("external_ids", "pvn-id", metadata.ID),
		mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(metadata.Revision, 10)),
	}
	for _, key := range sortedStringKeys(extra) {
		result = append(result, mapAssignment("external_ids", key, extra[key]))
	}
	return result
}

func logicalSwitch(id string) string { return "pvn-ls-" + compact(id) }
func logicalRouter(id string) string { return "pvn-lr-" + compact(id) }
func portGroup(id string) string     { return "pvn_pg_" + compact(id) }

func gatewayRouterPort(id string) string { return "pvn-gw-lrp-" + compact(id) }
func gatewaySwitchPort(id string) string { return "pvn-gw-lsp-" + compact(id) }

func logicalSwitchUUID(id string) string { return deterministicUUID("logical-switch:" + id) }
func logicalRouterUUID(id string) string { return deterministicUUID("logical-router:" + id) }

func routerDefaultRouteUUID(id string) string {
	return deterministicUUID("router-default-route:" + id)
}

func routerStaticRouteUUID(routerID string, route model.StaticRoute) string {
	return deterministicUUID("router-static-route:" + routerID + ":" + routerStaticRouteKey(route))
}

func routerStaticRouteKey(route model.StaticRoute) string {
	digest := sha256.Sum256([]byte(route.Destination + "\x00" + route.NextHop))
	return hex.EncodeToString(digest[:])
}

func routerSNATUUID(routerID, routerInterfaceID string) string {
	return deterministicUUID("router-snat:" + routerID + ":" + routerInterfaceID)
}

func compact(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func deterministicMAC(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return net.HardwareAddr([]byte{0x02, digest[0], digest[1], digest[2], digest[3], digest[4]}).String()
}

func subnetGateway(subnet *model.Subnet) (netip.Addr, error) {
	gateway, err := model.EffectiveIPv4Gateway(subnet)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("subnet %q has an invalid gateway: %w", subnet.ID, err)
	}
	return gateway, nil
}

func normalizedDNSDomain(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}

func safeID(value string) error {
	if value == "" || strings.HasPrefix(value, "-") || len(value) > 255 {
		return fmt.Errorf("unsafe resource identifier %q", value)
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return fmt.Errorf("unsafe resource identifier %q", value)
	}
	return nil
}

func safePhysicalNetwork(value string) error {
	if value == "" || len(value) > 63 {
		return fmt.Errorf("unsafe physical network name %q", value)
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return fmt.Errorf("unsafe physical network name %q", value)
	}
	return nil
}

func nilResource(resource model.Resource) bool {
	if resource == nil {
		return true
	}
	value := reflect.ValueOf(resource)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func validateReferencedIDs(values ...string) error {
	for _, value := range values {
		if err := safeID(value); err != nil {
			return fmt.Errorf("invalid referenced identifier: %w", err)
		}
	}
	return nil
}

func ovsdbString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func stringAssignment(column, value string) string {
	return column + "=" + ovsdbString(value)
}

func mapAssignment(column, key, value string) string {
	return column + ":" + key + "=" + ovsdbString(value)
}

func deterministicUUID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	bytes := digest[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	hexadecimal := hex.EncodeToString(bytes)
	return hexadecimal[0:8] + "-" + hexadecimal[8:12] + "-" + hexadecimal[12:16] + "-" + hexadecimal[16:20] + "-" + hexadecimal[20:32]
}

func getResource[T model.Resource](ctx context.Context, store controlstore.Store, kind model.Kind, id string) (T, error) {
	var zero T
	resource, err := store.Get(ctx, kind, id)
	if err != nil {
		return zero, err
	}
	typed, ok := resource.(T)
	if !ok || nilResource(typed) {
		return zero, fmt.Errorf("control store returned %T for %s %q", resource, kind, id)
	}
	return typed, nil
}

func safeUUID(value string) error {
	parts := strings.Split(value, "-")
	want := []int{8, 4, 4, 4, 12}
	if len(parts) != len(want) {
		return fmt.Errorf("invalid UUID %q", value)
	}
	for index, part := range parts {
		if len(part) != want[index] {
			return fmt.Errorf("invalid UUID %q", value)
		}
		for _, character := range part {
			if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
				return fmt.Errorf("invalid UUID %q", value)
			}
		}
	}
	return nil
}

func wrapRender(kind, id string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("render OVN %s %q: %w", kind, id, err)
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
