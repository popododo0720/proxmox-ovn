package model

import "time"

type Project struct {
	Metadata
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	PoolID      string `json:"pool_id"`
}

func (*Project) ResourceKind() Kind     { return KindProject }
func (p *Project) ResourceName() string { return p.Name }
func (p *Project) Validate() error      { return validateProject(p) }

type Network struct {
	Metadata
	ProjectID         string `json:"project_id"`
	Name              string `json:"name"`
	Description       string `json:"description,omitempty"`
	MTU               int    `json:"mtu"`
	External          bool   `json:"external"`
	ProviderNetworkID string `json:"provider_network_id,omitempty"`
}

func (*Network) ResourceKind() Kind     { return KindNetwork }
func (n *Network) ResourceName() string { return n.Name }
func (n *Network) Validate() error      { return validateNetwork(n) }

type IPRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type Subnet struct {
	Metadata
	ProjectID       string    `json:"project_id"`
	NetworkID       string    `json:"network_id"`
	Name            string    `json:"name"`
	CIDR            string    `json:"cidr"`
	GatewayIP       string    `json:"gateway_ip,omitempty"`
	EnableDHCP      bool      `json:"enable_dhcp"`
	DNSNameservers  []string  `json:"dns_nameservers,omitempty"`
	AllocationPools []IPRange `json:"allocation_pools,omitempty"`
}

func (*Subnet) ResourceKind() Kind     { return KindSubnet }
func (s *Subnet) ResourceName() string { return s.Name }
func (s *Subnet) Validate() error      { return validateSubnet(s) }

type FixedIP struct {
	SubnetID string `json:"subnet_id"`
	Address  string `json:"address,omitempty"`
}

type PortBindingStatus string

const (
	PortUnbound      PortBindingStatus = "unbound"
	PortBinding      PortBindingStatus = "binding"
	PortBound        PortBindingStatus = "bound"
	PortDetaching    PortBindingStatus = "detaching"
	PortBindingError PortBindingStatus = "error"
)

type Port struct {
	Metadata
	ProjectID        string            `json:"project_id"`
	NetworkID        string            `json:"network_id"`
	Name             string            `json:"name"`
	MACAddress       string            `json:"mac_address"`
	FixedIPs         []FixedIP         `json:"fixed_ips,omitempty"`
	SecurityGroupIDs []string          `json:"security_group_ids,omitempty"`
	AdminStateUp     bool              `json:"admin_state_up"`
	BindingStatus    PortBindingStatus `json:"binding_status"`
	NodeID           string            `json:"node_id,omitempty"`
	VMID             int               `json:"vmid,omitempty"`
	NIC              string            `json:"nic,omitempty"`
	LSPName          string            `json:"lsp_name"`
	Generation       int64             `json:"generation"`
	RequestedChassis string            `json:"requested_chassis,omitempty"`
}

func (*Port) ResourceKind() Kind     { return KindPort }
func (p *Port) ResourceName() string { return p.Name }
func (p *Port) Validate() error      { return validatePort(p) }

type IPAllocationState string

const (
	IPReserved  IPAllocationState = "reserved"
	IPAllocated IPAllocationState = "allocated"
)

type IPAllocation struct {
	Metadata
	ProjectID string            `json:"project_id"`
	SubnetID  string            `json:"subnet_id"`
	PortID    string            `json:"port_id,omitempty"`
	Address   string            `json:"address"`
	State     IPAllocationState `json:"allocation_state"`
}

func (*IPAllocation) ResourceKind() Kind { return KindIPAllocation }
func (a *IPAllocation) Validate() error  { return validateIPAllocation(a) }

type Router struct {
	Metadata
	ProjectID         string `json:"project_id"`
	Name              string `json:"name"`
	Description       string `json:"description,omitempty"`
	ExternalNetworkID string `json:"external_network_id,omitempty"`
	ExternalSubnetID  string `json:"external_subnet_id,omitempty"`
	ExternalIPAddress string `json:"external_ip_address,omitempty"`
	EnableSNAT        bool   `json:"enable_snat"`
}

func (*Router) ResourceKind() Kind     { return KindRouter }
func (r *Router) ResourceName() string { return r.Name }
func (r *Router) Validate() error      { return validateRouter(r) }

type RouterInterface struct {
	Metadata
	ProjectID string `json:"project_id"`
	RouterID  string `json:"router_id"`
	SubnetID  string `json:"subnet_id"`
	PortID    string `json:"port_id,omitempty"`
}

func (*RouterInterface) ResourceKind() Kind { return KindRouterInterface }
func (r *RouterInterface) Validate() error  { return validateRouterInterface(r) }

type FloatingIPStatus string

const (
	FloatingIPDown   FloatingIPStatus = "down"
	FloatingIPActive FloatingIPStatus = "active"
	FloatingIPError  FloatingIPStatus = "error"
)

type FloatingIP struct {
	Metadata
	ProjectID         string           `json:"project_id"`
	ProviderNetworkID string           `json:"provider_network_id"`
	Address           string           `json:"address"`
	PortID            string           `json:"port_id,omitempty"`
	FixedIPAddress    string           `json:"fixed_ip_address,omitempty"`
	RouterID          string           `json:"router_id,omitempty"`
	FloatingStatus    FloatingIPStatus `json:"status"`
}

func (*FloatingIP) ResourceKind() Kind { return KindFloatingIP }
func (f *FloatingIP) Validate() error  { return validateFloatingIP(f) }

type ProviderNetwork struct {
	Metadata
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	Shared           bool   `json:"shared"`
	DefaultSegmentID string `json:"default_segment_id,omitempty"`
}

func (*ProviderNetwork) ResourceKind() Kind     { return KindProviderNetwork }
func (p *ProviderNetwork) ResourceName() string { return p.Name }
func (p *ProviderNetwork) Validate() error      { return validateProviderNetwork(p) }

type ProviderNetworkType string

const (
	ProviderFlat ProviderNetworkType = "flat"
	ProviderVLAN ProviderNetworkType = "vlan"
)

type ProviderSegment struct {
	Metadata
	ProviderNetworkID string              `json:"provider_network_id"`
	Name              string              `json:"name"`
	PhysicalNetwork   string              `json:"physical_network"`
	NetworkType       ProviderNetworkType `json:"network_type"`
	VLANID            int                 `json:"vlan_id,omitempty"`
}

func (*ProviderSegment) ResourceKind() Kind     { return KindProviderSegment }
func (p *ProviderSegment) ResourceName() string { return p.Name }
func (p *ProviderSegment) Validate() error      { return validateProviderSegment(p) }

type SecurityGroup struct {
	Metadata
	ProjectID   string `json:"project_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Stateful    bool   `json:"stateful"`
}

func (*SecurityGroup) ResourceKind() Kind     { return KindSecurityGroup }
func (s *SecurityGroup) ResourceName() string { return s.Name }
func (s *SecurityGroup) Validate() error      { return validateSecurityGroup(s) }

type RuleDirection string
type EtherType string
type RuleAction string

const (
	DirectionIngress RuleDirection = "ingress"
	DirectionEgress  RuleDirection = "egress"
	EtherTypeIPv4    EtherType     = "IPv4"
	ActionAllow      RuleAction    = "allow"
	ActionDrop       RuleAction    = "drop"
)

type SecurityGroupRule struct {
	Metadata
	ProjectID       string        `json:"project_id"`
	SecurityGroupID string        `json:"security_group_id"`
	Direction       RuleDirection `json:"direction"`
	EtherType       EtherType     `json:"ethertype"`
	Protocol        string        `json:"protocol,omitempty"`
	PortRangeMin    int           `json:"port_range_min,omitempty"`
	PortRangeMax    int           `json:"port_range_max,omitempty"`
	RemoteCIDR      string        `json:"remote_cidr,omitempty"`
	RemoteGroupID   string        `json:"remote_group_id,omitempty"`
	Action          RuleAction    `json:"action"`
	Description     string        `json:"description,omitempty"`
}

func (*SecurityGroupRule) ResourceKind() Kind { return KindSecurityGroupRule }
func (r *SecurityGroupRule) Validate() error  { return validateSecurityGroupRule(r) }

type NodeRole string

const (
	NodeRoleCompute NodeRole = "compute"
	NodeRoleGateway NodeRole = "gateway"
	NodeRoleCentral NodeRole = "central"
)

type Node struct {
	Metadata
	Name              string     `json:"name"`
	ChassisID         string     `json:"chassis_id"`
	ManagementAddress string     `json:"management_address,omitempty"`
	Roles             []NodeRole `json:"roles"`
	Enabled           bool       `json:"enabled"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty"`
}

func (*Node) ResourceKind() Kind     { return KindNode }
func (n *Node) ResourceName() string { return n.Name }
func (n *Node) Validate() error      { return validateNode(n) }

type OperationStatus string

const (
	OperationQueued    OperationStatus = "queued"
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
)

type Operation struct {
	Metadata
	Action          string          `json:"action"`
	TargetKind      Kind            `json:"target_kind"`
	TargetID        string          `json:"target_id"`
	TargetRevision  int64           `json:"target_revision"`
	OperationStatus OperationStatus `json:"status"`
	IdempotencyKey  string          `json:"idempotency_key,omitempty"`
	Error           string          `json:"error,omitempty"`
	LeaseOwner      string          `json:"lease_owner,omitempty"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	CompletedAt     *time.Time      `json:"completed_at,omitempty"`
}

func (*Operation) ResourceKind() Kind { return KindOperation }
func (o *Operation) Validate() error  { return validateOperation(o) }
