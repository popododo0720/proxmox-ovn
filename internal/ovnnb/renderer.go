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
	switch value := resource.(type) {
	case *model.Network:
		return renderer.network(ctx, value)
	case *model.Subnet:
		return renderer.subnet(ctx, value)
	case *model.Port:
		return renderer.port(ctx, value)
	case *model.Router:
		return renderer.router(ctx, value)
	case *model.RouterInterface:
		return renderer.routerInterface(ctx, value)
	case *model.FloatingIP:
		return renderer.floatingIP(ctx, value)
	case *model.ProviderSegment:
		return renderer.providerSegment(ctx, value)
	case *model.SecurityGroup:
		return renderer.securityGroup(ctx, value)
	case *model.SecurityGroupRule:
		return renderer.securityGroupRule(ctx, value)
	case *model.Project, *model.ProviderNetwork, *model.IPAllocation, *model.Node, *model.Operation:
		return nil
	default:
		return fmt.Errorf("unsupported resource type %T", resource)
	}
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
	switch value := resource.(type) {
	case *model.Network:
		_, err := renderer.client.run(ctx, "--", "--if-exists", "ls-del", logicalSwitchUUID(value.ID))
		return wrapRender("delete network", value.ID, err)
	case *model.Subnet:
		if err := renderer.attachDHCPToPorts(ctx, value, ""); err != nil {
			return err
		}
		uuid := deterministicUUID("dhcp-options:" + value.ID)
		var err error
		_, err = renderer.client.run(ctx, "--", "--if-exists", "destroy", "DHCP_Options", uuid)
		return wrapRender("delete subnet", value.ID, err)
	case *model.Port:
		if err := safeID(value.LSPName); err != nil {
			return fmt.Errorf("invalid LSP name: %w", err)
		}
		_, err := renderer.client.run(ctx, "--", "--if-exists", "lsp-del", value.LSPName)
		return wrapRender("delete port", value.ID, err)
	case *model.Router:
		return renderer.deleteRouter(ctx, value)
	case *model.RouterInterface:
		return renderer.deleteRouterInterface(ctx, value)
	case *model.FloatingIP:
		return renderer.deleteFloatingIP(ctx, value)
	case *model.ProviderSegment:
		return renderer.deleteProviderSegment(ctx, value)
	case *model.SecurityGroup:
		uuid := deterministicUUID("port-group:" + value.ID)
		_, err := renderer.client.run(ctx, "--", "--if-exists", "destroy", "Port_Group", uuid)
		return wrapRender("delete security group", value.ID, err)
	case *model.SecurityGroupRule:
		return renderer.deleteACL(ctx, value.ID)
	case *model.Project, *model.ProviderNetwork, *model.IPAllocation, *model.Node, *model.Operation:
		return nil
	default:
		return fmt.Errorf("unsupported resource type %T", resource)
	}
}

func (renderer *Renderer) deleteFloatingIP(ctx context.Context, floatingIP *model.FloatingIP) error {
	uuid := deterministicUUID("floating-ip-nat:" + floatingIP.ID)
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
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(router.ID), "nat", uuid)
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
	uuid := deterministicUUID("acl:" + owner)
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
		args = append(args, "--", "--if-exists", "remove", "Port_Group", portGroup(group.ID), "acls", uuid)
	}
	args = append(args, "--", "--if-exists", "destroy", "ACL", uuid)
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("delete security group ACL", owner, err)
}

func (renderer *Renderer) network(ctx context.Context, network *model.Network) error {
	if err := safeID(network.ProjectID); err != nil {
		return fmt.Errorf("invalid project ID: %w", err)
	}
	name := logicalSwitch(network.ID)
	assignments := []string{stringAssignment("name", name)}
	assignments = append(assignments, metadataAssignments(network, map[string]string{"pvn-project": network.ProjectID})...)
	if err := renderer.ensureRow(ctx, "Logical_Switch", logicalSwitchUUID(network.ID), assignments); err != nil {
		return wrapRender("network", network.ID, err)
	}
	if network.ProviderNetworkID == "" {
		return renderer.removeProviderPort(ctx, network)
	}
	segment, err := renderer.defaultProviderSegment(ctx, network.ProviderNetworkID)
	if err != nil {
		return wrapRender("network provider segment", network.ID, err)
	}
	return renderer.renderProviderPort(ctx, segment, network)
}

func (renderer *Renderer) subnet(ctx context.Context, subnet *model.Subnet) error {
	if err := validateReferencedIDs(subnet.ProjectID, subnet.NetworkID); err != nil {
		return err
	}
	if !subnet.EnableDHCP {
		return renderer.clearSubnetDHCP(ctx, subnet)
	}
	network, err := getResource[*model.Network](ctx, renderer.store, model.KindNetwork, subnet.NetworkID)
	if err != nil {
		return err
	}
	if network.ProjectID != subnet.ProjectID {
		return fmt.Errorf("subnet %q and network %q belong to different projects", subnet.ID, network.ID)
	}
	gateway, err := subnetGateway(subnet)
	if err != nil {
		return err
	}
	uuid := deterministicUUID("dhcp-options:" + subnet.ID)
	existing, err := renderer.findUUID(ctx, "DHCP_Options", uuid)
	if err != nil {
		return err
	}
	if existing == "" {
		_, createErr := renderer.client.run(ctx, "--", "--id="+uuid, "create", "DHCP_Options",
			stringAssignment("cidr", subnet.CIDR),
			mapAssignment("external_ids", "pvn-managed", "true"),
			mapAssignment("external_ids", "pvn-kind", subnet.ResourceKind().String()),
			mapAssignment("external_ids", "pvn-id", subnet.ID),
			mapAssignment("external_ids", "pvn-project", subnet.ProjectID),
			mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(subnet.Revision, 10)),
		)
		if createErr != nil {
			// Another active manager may have won the deterministic-UUID race.
			found, findErr := renderer.findUUID(ctx, "DHCP_Options", uuid)
			if findErr != nil || found == "" {
				return wrapRender("subnet DHCP", subnet.ID, createErr)
			}
		}
	}
	if _, err := renderer.client.run(ctx, "set", "DHCP_Options", uuid,
		stringAssignment("cidr", subnet.CIDR),
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", subnet.ResourceKind().String()),
		mapAssignment("external_ids", "pvn-id", subnet.ID),
		mapAssignment("external_ids", "pvn-project", subnet.ProjectID),
		mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(subnet.Revision, 10)),
	); err != nil {
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
	args := append([]string{"dhcp-options-set-options", uuid}, options...)
	if _, err := renderer.client.run(ctx, args...); err != nil {
		return wrapRender("subnet DHCP options", subnet.ID, err)
	}
	return renderer.attachDHCPToPorts(ctx, subnet, uuid)
}

func (renderer *Renderer) clearSubnetDHCP(ctx context.Context, subnet *model.Subnet) error {
	if err := renderer.attachDHCPToPorts(ctx, subnet, ""); err != nil {
		return err
	}
	uuid := deterministicUUID("dhcp-options:" + subnet.ID)
	var err error
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
		var arguments []string
		if uuid == "" {
			arguments = []string{"clear", "Logical_Switch_Port", port.LSPName, "dhcpv4_options"}
		} else {
			arguments = []string{"lsp-set-dhcpv4-options", port.LSPName, uuid}
		}
		if _, err := renderer.client.run(ctx, arguments...); err != nil {
			return wrapRender("attach subnet DHCP to port", port.ID, err)
		}
	}
	return nil
}

func (renderer *Renderer) port(ctx context.Context, port *model.Port) error {
	if err := validateReferencedIDs(port.ProjectID, port.NetworkID); err != nil {
		return err
	}
	network, err := getResource[*model.Network](ctx, renderer.store, model.KindNetwork, port.NetworkID)
	if err != nil {
		return err
	}
	if network.ProjectID != port.ProjectID {
		return fmt.Errorf("port %q and network %q belong to different projects", port.ID, network.ID)
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
		if subnet.ProjectID != port.ProjectID || subnet.NetworkID != port.NetworkID {
			return fmt.Errorf("fixed-IP subnet %q does not belong to port %q's project and network", subnet.ID, port.ID)
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
	enabledState := "disabled"
	if port.AdminStateUp && port.BindingStatus != model.PortUnbound && port.BindingStatus != model.PortDetaching && port.BindingStatus != model.PortBindingError {
		enabledState = "enabled"
	}
	arguments := []string{
		"--", "--may-exist", "lsp-add", logicalSwitchUUID(network.ID), port.LSPName,
		"--", "lsp-set-addresses", port.LSPName, strings.Join(addresses, " "),
		"--", "lsp-set-port-security", port.LSPName, strings.Join(addresses, " "),
		"--", "lsp-set-enabled", port.LSPName, enabledState,
		"--", "set", "Logical_Switch_Port", port.LSPName,
	}
	arguments = append(arguments, metadataAssignments(port, map[string]string{"pvn-project": port.ProjectID, "pvn-network": port.NetworkID})...)
	if _, err := renderer.client.run(ctx, arguments...); err != nil {
		return wrapRender("port", port.ID, err)
	}
	optionArgs := []string{"lsp-set-options", port.LSPName}
	if port.RequestedChassis != "" {
		if err := safeID(port.RequestedChassis); err != nil {
			return fmt.Errorf("invalid requested chassis: %w", err)
		}
		optionArgs = append(optionArgs, "requested-chassis="+port.RequestedChassis)
	}
	if _, err := renderer.client.run(ctx, optionArgs...); err != nil {
		return wrapRender("port chassis request", port.ID, err)
	}
	if err := renderer.portDHCP(ctx, port); err != nil {
		return err
	}
	return renderer.portGroups(ctx, port)
}

func (renderer *Renderer) portDHCP(ctx context.Context, port *model.Port) error {
	var selected string
	for _, fixed := range port.FixedIPs {
		subnet, err := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, fixed.SubnetID)
		if err != nil {
			return err
		}
		if !subnet.EnableDHCP {
			continue
		}
		uuid := deterministicUUID("dhcp-options:" + fixed.SubnetID)
		found, err := renderer.findUUID(ctx, "DHCP_Options", uuid)
		if err != nil {
			return err
		}
		if found != "" {
			selected = uuid
			break
		}
	}
	if selected == "" {
		_, err := renderer.client.run(ctx, "clear", "Logical_Switch_Port", port.LSPName, "dhcpv4_options")
		return wrapRender("clear port DHCP", port.ID, err)
	}
	_, err := renderer.client.run(ctx, "lsp-set-dhcpv4-options", port.LSPName, selected)
	return wrapRender("port DHCP", port.ID, err)
}

func (renderer *Renderer) router(ctx context.Context, router *model.Router) error {
	if err := safeID(router.ProjectID); err != nil {
		return fmt.Errorf("invalid project ID: %w", err)
	}
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
	assignments = append(assignments, metadataAssignments(router, map[string]string{"pvn-project": router.ProjectID})...)
	if err := renderer.ensureRow(ctx, "Logical_Router", logicalRouterUUID(router.ID), assignments); err != nil {
		return wrapRender("router", router.ID, err)
	}
	if external == nil {
		return renderer.clearRouterExternal(ctx, router)
	}
	if err := renderer.renderRouterGateway(ctx, router, external, gatewayChassis); err != nil {
		return err
	}
	return renderer.reconcileRouterSNAT(ctx, router)
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
	if network.ProjectID != router.ProjectID && !provider.Shared {
		return nil, fmt.Errorf("router %q cannot use non-shared external network %q from another project", router.ID, network.ID)
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
	if subnet.NetworkID != network.ID || subnet.ProjectID != network.ProjectID {
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

func (renderer *Renderer) renderRouterGateway(ctx context.Context, router *model.Router, external *routerExternal, chassis []string) error {
	routerPort := gatewayRouterPort(router.ID)
	switchPort := gatewaySwitchPort(router.ID)
	network := fmt.Sprintf("%s/%d", external.externalIP, external.prefix.Bits())
	args := renderer.routerGatewayArgs(router, external.network.ID, routerPort, switchPort, network, false)
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
		args = renderer.routerGatewayArgs(router, external.network.ID, routerPort, switchPort, network, true)
		if _, retryErr := renderer.client.run(ctx, args...); retryErr != nil {
			return wrapRender("move router gateway", router.ID, retryErr)
		}
	}
	if err := renderer.renderRouterDefaultRoute(ctx, router, external.gateway, routerPort); err != nil {
		return err
	}
	return renderer.syncGatewayChassis(ctx, router.ID, routerPort, chassis)
}

func (renderer *Renderer) routerGatewayArgs(router *model.Router, networkID, routerPort, switchPort, network string, move bool) []string {
	mac := deterministicMAC("router-gateway:" + router.ID)
	args := make([]string, 0, 40)
	if move {
		args = append(args,
			"--", "--if-exists", "lsp-del", switchPort,
			"--", "--if-exists", "lrp-del", routerPort,
		)
	}
	args = append(args,
		"--", "--may-exist", "lrp-add", logicalRouterUUID(router.ID), routerPort, mac, network,
		"--", "set", "Logical_Router_Port", routerPort, stringAssignment("mac", mac), stringAssignment("networks", network),
	)
	args = append(args, metadataAssignments(router, map[string]string{"pvn-project": router.ProjectID, "pvn-role": "external-gateway"})...)
	args = append(args,
		"--", "--may-exist", "lsp-add", logicalSwitchUUID(networkID), switchPort,
		"--", "lsp-set-type", switchPort, "router",
		"--", "lsp-set-addresses", switchPort, "router",
		"--", "lsp-set-options", switchPort, "router-port="+routerPort, "nat-addresses=router",
		"--", "set", "Logical_Switch_Port", switchPort,
	)
	return append(args, metadataAssignments(router, map[string]string{"pvn-network": networkID, "pvn-project": router.ProjectID, "pvn-role": "external-gateway"})...)
}

func (renderer *Renderer) renderRouterDefaultRoute(ctx context.Context, router *model.Router, gateway netip.Addr, routerPort string) error {
	uuid := routerDefaultRouteUUID(router.ID)
	assignments := []string{
		stringAssignment("ip_prefix", "0.0.0.0/0"),
		stringAssignment("nexthop", gateway.String()),
		stringAssignment("output_port", routerPort),
		mapAssignment("external_ids", "pvn-managed", "true"),
		mapAssignment("external_ids", "pvn-kind", "router-default-route"),
		mapAssignment("external_ids", "pvn-router", router.ID),
		mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(router.Revision, 10)),
	}
	if err := renderer.ensureAttachedRow(ctx, "Logical_Router_Static_Route", uuid, assignments,
		"Logical_Router", logicalRouterUUID(router.ID), "static_routes"); err != nil {
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
	if err := validateReferencedIDs(routerInterface.ProjectID, routerInterface.RouterID, routerInterface.SubnetID); err != nil {
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
	if router.ProjectID != routerInterface.ProjectID || subnet.ProjectID != routerInterface.ProjectID {
		return fmt.Errorf("router interface %q crosses project boundaries", routerInterface.ID)
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
	args := renderer.routerInterfaceArgs(routerInterface, router.ID, subnet.NetworkID, routerPort, switchPort, mac, portNetwork, false)
	if _, err = renderer.client.run(ctx, args...); err != nil {
		// The may-exist forms reject a changed subnet or logical switch. Probe
		// both deterministic PVN port names before replacing them atomically;
		// a lookup failure must never turn into a destructive move.
		routerPortOutput, routerPortLookupErr := renderer.client.run(ctx, "--", "--if-exists", "get", "Logical_Router_Port", routerPort, "name")
		switchPortOutput, switchPortLookupErr := renderer.client.run(ctx, "--", "--if-exists", "get", "Logical_Switch_Port", switchPort, "name")
		if routerPortLookupErr != nil || switchPortLookupErr != nil || (len(strings.TrimSpace(string(routerPortOutput))) == 0 && len(strings.TrimSpace(string(switchPortOutput))) == 0) {
			return wrapRender("router interface", routerInterface.ID, err)
		}
		args = renderer.routerInterfaceArgs(routerInterface, router.ID, subnet.NetworkID, routerPort, switchPort, mac, portNetwork, true)
		if _, retryErr := renderer.client.run(ctx, args...); retryErr != nil {
			return wrapRender("move router interface", routerInterface.ID, retryErr)
		}
	}
	return renderer.reconcileRouterSNAT(ctx, router)
}

func (renderer *Renderer) routerInterfaceArgs(routerInterface *model.RouterInterface, routerID, networkID, routerPort, switchPort, mac, portNetwork string, move bool) []string {
	args := make([]string, 0, 36)
	if move {
		args = append(args,
			"--", "--if-exists", "lsp-del", switchPort,
			"--", "--if-exists", "lrp-del", routerPort,
		)
	}
	args = append(args,
		"--", "--may-exist", "lrp-add", logicalRouterUUID(routerID), routerPort, mac, portNetwork,
		"--", "set", "Logical_Router_Port", routerPort, stringAssignment("mac", mac), stringAssignment("networks", portNetwork),
		"--", "--may-exist", "lsp-add", logicalSwitchUUID(networkID), switchPort,
		"--", "lsp-set-type", switchPort, "router",
		"--", "lsp-set-addresses", switchPort, "router",
		"--", "lsp-set-options", switchPort, "router-port="+routerPort,
		"--", "set", "Logical_Router_Port", routerPort,
	)
	args = append(args, metadataAssignments(routerInterface, map[string]string{"pvn-project": routerInterface.ProjectID})...)
	return args
}

func (renderer *Renderer) reconcileRouterSNAT(ctx context.Context, router *model.Router) error {
	actual, err := renderer.findMany(ctx, "NAT",
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
			if routerInterface.ProjectID != router.ProjectID {
				return fmt.Errorf("router interface %q crosses router %q project boundary", routerInterface.ID, router.ID)
			}
			subnet, err := getResource[*model.Subnet](ctx, renderer.store, model.KindSubnet, routerInterface.SubnetID)
			if err != nil {
				return err
			}
			network, err := getResource[*model.Network](ctx, renderer.store, model.KindNetwork, subnet.NetworkID)
			if err != nil {
				return err
			}
			if subnet.ProjectID != router.ProjectID || network.ProjectID != router.ProjectID || network.External {
				return fmt.Errorf("router interface %q is not an internal subnet of router %q's project", routerInterface.ID, router.ID)
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
			if err := renderer.ensureAttachedRow(ctx, "NAT", uuid, assignments,
				"Logical_Router", logicalRouterUUID(router.ID), "nat"); err != nil {
				return wrapRender("router SNAT", routerInterface.ID, err)
			}
		}
	}
	stale := make([]string, 0)
	for _, uuid := range actual {
		if !desired[uuid] {
			stale = append(stale, uuid)
		}
	}
	return renderer.removeRouterSNATRows(ctx, router.ID, stale)
}

func (renderer *Renderer) removeRouterSNATRows(ctx context.Context, routerID string, uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}
	args := make([]string, 0, len(uuids)*12)
	for _, uuid := range uuids {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(routerID), "nat", uuid)
	}
	for _, uuid := range uuids {
		args = append(args, "--", "--if-exists", "destroy", "NAT", uuid)
	}
	_, err := renderer.client.run(ctx, args...)
	return wrapRender("remove stale router SNAT", routerID, err)
}

func (renderer *Renderer) deleteRouterInterface(ctx context.Context, routerInterface *model.RouterInterface) error {
	uuid := routerSNATUUID(routerInterface.RouterID, routerInterface.ID)
	args := []string{
		"--", "--if-exists", "lsp-del", "pvn-rsp-" + compact(routerInterface.ID),
		"--", "--if-exists", "lrp-del", "pvn-lrp-" + compact(routerInterface.ID),
		"--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(routerInterface.RouterID), "nat", uuid,
		"--", "--if-exists", "destroy", "NAT", uuid,
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
	return renderer.reconcileRouterSNAT(ctx, router)
}

func (renderer *Renderer) clearRouterExternal(ctx context.Context, router *model.Router) error {
	snats, err := renderer.findMany(ctx, "NAT",
		mapAssignment("external_ids", "pvn-kind", "router-snat"),
		mapAssignment("external_ids", "pvn-router", router.ID),
	)
	if err != nil {
		return wrapRender("list router SNAT", router.ID, err)
	}
	args := []string{
		"--", "--if-exists", "lsp-del", gatewaySwitchPort(router.ID),
		"--", "--if-exists", "lrp-del", gatewayRouterPort(router.ID),
		"--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(router.ID), "static_routes", routerDefaultRouteUUID(router.ID),
	}
	for _, uuid := range snats {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(router.ID), "nat", uuid)
	}
	args = append(args, "--", "--if-exists", "destroy", "Logical_Router_Static_Route", routerDefaultRouteUUID(router.ID))
	for _, uuid := range snats {
		args = append(args, "--", "--if-exists", "destroy", "NAT", uuid)
	}
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("clear router external gateway", router.ID, err)
}

func (renderer *Renderer) deleteRouter(ctx context.Context, router *model.Router) error {
	snats, err := renderer.findMany(ctx, "NAT",
		mapAssignment("external_ids", "pvn-kind", "router-snat"),
		mapAssignment("external_ids", "pvn-router", router.ID),
	)
	if err != nil {
		return wrapRender("list router SNAT", router.ID, err)
	}
	args := []string{
		"--", "--if-exists", "lsp-del", gatewaySwitchPort(router.ID),
		"--", "--if-exists", "lrp-del", gatewayRouterPort(router.ID),
		"--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(router.ID), "static_routes", routerDefaultRouteUUID(router.ID),
	}
	for _, uuid := range snats {
		args = append(args, "--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(router.ID), "nat", uuid)
	}
	args = append(args,
		"--", "--if-exists", "lr-del", logicalRouterUUID(router.ID),
		"--", "--if-exists", "destroy", "Logical_Router_Static_Route", routerDefaultRouteUUID(router.ID),
	)
	for _, uuid := range snats {
		args = append(args, "--", "--if-exists", "destroy", "NAT", uuid)
	}
	_, err = renderer.client.run(ctx, args...)
	return wrapRender("delete router", router.ID, err)
}

func (renderer *Renderer) floatingIP(ctx context.Context, floatingIP *model.FloatingIP) error {
	if err := validateReferencedIDs(floatingIP.ProjectID, floatingIP.ProviderNetworkID); err != nil {
		return err
	}
	uuid := deterministicUUID("floating-ip-nat:" + floatingIP.ID)
	routers, err := renderer.store.List(ctx, model.KindRouter, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	if floatingIP.RouterID == "" || floatingIP.FixedIPAddress == "" {
		args := make([]string, 0, len(routers)*7+6)
		for _, resource := range routers {
			router, ok := resource.(*model.Router)
			if !ok {
				return fmt.Errorf("control store returned %T while listing routers", resource)
			}
			args = append(args, "--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(router.ID), "nat", uuid)
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
	if router.ProjectID != floatingIP.ProjectID {
		return fmt.Errorf("floating IP %q and router %q belong to different projects", floatingIP.ID, router.ID)
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
		if port.ProjectID != floatingIP.ProjectID {
			return fmt.Errorf("floating IP %q and port %q belong to different projects", floatingIP.ID, port.ID)
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
		mapAssignment("external_ids", "pvn-project", floatingIP.ProjectID),
		mapAssignment("external_ids", "pvn-revision", strconv.FormatInt(floatingIP.Revision, 10)),
	}
	existing, findErr := renderer.findUUID(ctx, "NAT", uuid)
	if findErr != nil {
		return findErr
	}
	args := []string{"--"}
	if existing == "" {
		args = append(args, "--id="+uuid, "create", "NAT")
	} else {
		args = append(args, "set", "NAT", uuid)
	}
	args = append(args, assignments...)
	for _, resource := range routers {
		candidate, ok := resource.(*model.Router)
		if !ok {
			return fmt.Errorf("control store returned %T while listing routers", resource)
		}
		if candidate.ID != router.ID {
			args = append(args, "--", "--if-exists", "remove", "Logical_Router", logicalRouterUUID(candidate.ID), "nat", uuid)
		}
	}
	args = append(args, "--", "add", "Logical_Router", logicalRouterUUID(router.ID), "nat", uuid)
	_, err = renderer.client.run(ctx, args...)
	if err != nil && existing == "" {
		// Deterministic UUIDs turn active-active creates into a harmless race.
		if found, retryErr := renderer.findUUID(ctx, "NAT", uuid); retryErr == nil && found != "" {
			return renderer.floatingIP(ctx, floatingIP)
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
		if err := renderer.renderProviderPort(ctx, segment, network); err != nil {
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

func (renderer *Renderer) renderProviderPort(ctx context.Context, segment *model.ProviderSegment, network *model.Network) error {
	port := "pvn-localnet-" + compact(network.ID)
	existing, err := renderer.ownedProviderPort(ctx, network.ID, port)
	if err != nil {
		return wrapRender("provider port ownership", network.ID, err)
	}
	target := port
	args := make([]string, 0, 32)
	if existing == "" {
		args = append(args, "--", "--may-exist", "lsp-add", logicalSwitchUUID(network.ID), port)
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
	if err := safeID(group.ProjectID); err != nil {
		return fmt.Errorf("invalid project ID: %w", err)
	}
	name := portGroup(group.ID)
	uuid := deterministicUUID("port-group:" + group.ID)
	existing, err := renderer.findUUID(ctx, "Port_Group", uuid)
	if err != nil {
		return err
	}
	metadata := metadataAssignments(group, map[string]string{"pvn-project": group.ProjectID})
	if existing == "" {
		args := append([]string{"--", "--id=" + uuid, "create", "Port_Group", stringAssignment("name", name)}, metadata...)
		if _, createErr := renderer.client.run(ctx, args...); createErr != nil {
			found, findErr := renderer.findUUID(ctx, "Port_Group", uuid)
			if findErr != nil || found == "" {
				return wrapRender("security group", group.ID, createErr)
			}
			uuid = found
		}
	}
	args := append([]string{"set", "Port_Group", uuid, stringAssignment("name", name)}, metadata...)
	if _, err := renderer.client.run(ctx, args...); err != nil {
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
	if err := validateReferencedIDs(rule.ProjectID, rule.SecurityGroupID); err != nil {
		return err
	}
	group, err := getResource[*model.SecurityGroup](ctx, renderer.store, model.KindSecurityGroup, rule.SecurityGroupID)
	if err != nil {
		return err
	}
	if group.ProjectID != rule.ProjectID {
		return fmt.Errorf("security group rule %q crosses project boundaries", rule.ID)
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
			return fmt.Errorf("invalid remote group ID: %w", err)
		}
		remote, getErr := getResource[*model.SecurityGroup](ctx, renderer.store, model.KindSecurityGroup, rule.RemoteGroupID)
		if getErr != nil {
			return getErr
		}
		if remote.ProjectID != rule.ProjectID {
			return fmt.Errorf("security group rule %q references a remote group in another project", rule.ID)
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
	if rule.Action == model.ActionAllow {
		action = "allow-related"
	}
	return renderer.ensureACL(ctx, portGroup(group.ID), rule.ID, direction, 2000, strings.Join(match, " && "), action, rule.Revision)
}

func (renderer *Renderer) ensureACL(ctx context.Context, group, owner, direction string, priority int, match, action string, revision int64) error {
	uuid := deterministicUUID("acl:" + owner)
	existing, err := renderer.findUUID(ctx, "ACL", uuid)
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
			if candidateName == group {
				foundDesired = true
				continue
			}
			args = append(args, "--", "--if-exists", "remove", "Port_Group", candidateName, "acls", aclUUID)
		}
		if !foundDesired {
			return nil, fmt.Errorf("target port group %q is absent from desired state", group)
		}
		return append(args, "--", "add", "Port_Group", group, "acls", aclUUID), nil
	}
	if existing == "" {
		args := append([]string{"--", "--id=" + uuid, "create", "ACL"}, assignments...)
		args, err = appendMembership(args, uuid)
		if err != nil {
			return err
		}
		_, err = renderer.client.run(ctx, args...)
		if err != nil {
			if found, findErr := renderer.findUUID(ctx, "ACL", uuid); findErr == nil && found != "" {
				return renderer.ensureACL(ctx, group, owner, direction, priority, match, action, revision)
			}
		}
	} else {
		args := append([]string{"set", "ACL", uuid}, assignments...)
		args, err = appendMembership(args, uuid)
		if err != nil {
			return err
		}
		_, err = renderer.client.run(ctx, args...)
	}
	return wrapRender("security group ACL", owner, err)
}

func (renderer *Renderer) portGroups(ctx context.Context, port *model.Port) error {
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
		command := "remove"
		if desired[group.ID] {
			if group.ProjectID != port.ProjectID {
				return fmt.Errorf("port %q references security group %q from another project", port.ID, group.ID)
			}
			command = "add"
			delete(desired, group.ID)
		}
		args := []string{
			"--", "--id=@lsp", "get", "Logical_Switch_Port", port.LSPName,
			"--", command, "Port_Group", portGroup(group.ID), "ports", "@lsp",
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

func (renderer *Renderer) ensureRow(ctx context.Context, table, uuid string, assignments []string) error {
	existing, err := renderer.findUUID(ctx, table, uuid)
	if err != nil {
		return err
	}
	if existing == "" {
		args := append([]string{"--", "--id=" + uuid, "create", table}, assignments...)
		if _, createErr := renderer.client.run(ctx, args...); createErr == nil {
			return nil
		} else {
			// All managers derive the same UUID, so a concurrent winner is safe.
			found, findErr := renderer.findUUID(ctx, table, uuid)
			if findErr != nil || found == "" {
				return createErr
			}
		}
	}
	args := append([]string{"set", table, uuid}, assignments...)
	_, err = renderer.client.run(ctx, args...)
	return err
}

func (renderer *Renderer) ensureAttachedRow(ctx context.Context, table, uuid string, assignments []string, parentTable, parent, column string) error {
	existing, err := renderer.findUUID(ctx, table, uuid)
	if err != nil {
		return err
	}
	args := []string{"--"}
	if existing == "" {
		args = append(args, "--id="+uuid, "create", table)
	} else {
		args = append(args, "set", table, uuid)
	}
	args = append(args, assignments...)
	args = append(args, "--", "add", parentTable, parent, column, uuid)
	if _, err := renderer.client.run(ctx, args...); err == nil {
		return nil
	} else if existing != "" {
		return err
	} else {
		// A second manager can win the deterministic UUID insert while this
		// transaction is in flight. Retry only after confirming that exact row.
		found, findErr := renderer.findUUID(ctx, table, uuid)
		if findErr != nil || found == "" {
			return err
		}
	}
	args = append([]string{"--", "set", table, uuid}, assignments...)
	args = append(args, "--", "add", parentTable, parent, column, uuid)
	_, err = renderer.client.run(ctx, args...)
	return err
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
