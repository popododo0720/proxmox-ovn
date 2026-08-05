import { useState } from 'react';
import type {
  FloatingIP,
  Network,
  NodeStatus,
  Operation,
  Port,
  Project,
  ProviderNetwork,
  ProviderSegment,
  Router,
  RouterInterface,
  SecurityGroup,
  SecurityGroupRule,
  Subnet,
} from '../api/types';
import { PortAttachmentPanel } from '../components/PortAttachmentPanel';
import { ResourcePage, formatValue, type Column } from '../components/ResourcePage';
import { StatusPill } from '../components/StatusPill';

const projectColumns: Column<Project>[] = [
  { key: 'name', label: 'Project', render: (item) => <strong>{item.name || item.id}</strong> },
  { key: 'pool_id', label: 'PVE pool' },
  { key: 'state', label: 'State' },
  { key: 'id', label: 'ID', className: 'mono-cell' },
];

const networkFields = [
  { name: 'name', label: 'Name', required: true, placeholder: 'application' },
  { name: 'project_id', label: 'Project ID', required: true },
  { name: 'mtu', label: 'Guest MTU', type: 'number' as const, defaultValue: 1400 },
  { name: 'description', label: 'Description' },
];

export function ProjectsPage() {
  return <ResourcePage<Project>
    title="Projects"
    description="PVE pools synchronized into isolated PVN project boundaries."
    endpoint="/projects"
    columns={projectColumns}
    emptyMessage="Create a PVE pool, then let PVN synchronize it as a project."
  />;
}

export function NetworksPage() {
  return <div className="stacked-pages">
    <ResourcePage<Network>
      title="Networks"
      description="Tenant logical switches backed by Geneve overlays."
      endpoint="/networks"
      columns={[
        { key: 'name', label: 'Network', render: (item) => <strong>{item.name || item.id}</strong> },
        { key: 'project_id', label: 'Project', className: 'mono-cell' },
        { key: 'external', label: 'External' },
        { key: 'mtu', label: 'MTU' },
        { key: 'provider_network_id', label: 'Provider network', className: 'mono-cell' },
        { key: 'state', label: 'State' },
      ]}
      createLabel="Network"
      createFields={networkFields}
      allowDelete
    />
    <ResourcePage<Subnet>
      title="Subnets"
      description="IPv4 address pools, gateways, and OVN-native DHCP options."
      endpoint="/subnets"
      columns={[
        { key: 'name', label: 'Subnet', render: (item) => <strong>{item.name || item.id}</strong> },
        { key: 'network_id', label: 'Network', className: 'mono-cell' },
        { key: 'cidr', label: 'CIDR', className: 'mono-cell' },
        { key: 'gateway_ip', label: 'Gateway', className: 'mono-cell' },
        { key: 'enable_dhcp', label: 'DHCP' },
        { key: 'state', label: 'State' },
      ]}
      createLabel="Subnet"
      createFields={[
        { name: 'name', label: 'Name', required: true, placeholder: 'application-v4' },
        { name: 'project_id', label: 'Project ID', required: true },
        { name: 'network_id', label: 'Network ID', required: true },
        { name: 'cidr', label: 'IPv4 CIDR', required: true, placeholder: '10.42.0.0/24' },
        { name: 'gateway_ip', label: 'Gateway IP', placeholder: '10.42.0.1' },
        { name: 'enable_dhcp', label: 'Enable OVN DHCP', type: 'checkbox', defaultValue: true },
      ]}
      allowDelete
      compact
    />
  </div>;
}

export function RoutersPage() {
  return <div className="stacked-pages">
    <ResourcePage<Router>
      title="Routers"
      description="Distributed east-west routing with optional centralized north-south SNAT."
      endpoint="/routers"
      columns={[
        { key: 'name', label: 'Router', render: (item) => <strong>{item.name || item.id}</strong> },
        { key: 'project_id', label: 'Project', className: 'mono-cell' },
        { key: 'external_network_id', label: 'External network', className: 'mono-cell' },
        { key: 'external_subnet_id', label: 'External subnet', className: 'mono-cell' },
        { key: 'external_ip_address', label: 'Gateway IP', className: 'mono-cell' },
        { key: 'enable_snat', label: 'SNAT' },
        { key: 'state', label: 'State' },
      ]}
      createLabel="Router"
      createFields={[
        { name: 'name', label: 'Name', required: true, placeholder: 'edge' },
        { name: 'project_id', label: 'Project ID', required: true },
        { name: 'external_network_id', label: 'External network ID' },
        { name: 'external_subnet_id', label: 'External subnet ID' },
        { name: 'external_ip_address', label: 'Router external IPv4', placeholder: '203.0.113.10' },
        { name: 'enable_snat', label: 'Enable SNAT', type: 'checkbox', defaultValue: true },
      ]}
      allowDelete
    />
    <ResourcePage<RouterInterface>
      title="Router interfaces"
      description="Subnet attachments rendered as logical router ports."
      endpoint="/router-interfaces"
      columns={[
        { key: 'router_id', label: 'Router', className: 'mono-cell' },
        { key: 'subnet_id', label: 'Subnet', className: 'mono-cell' },
        { key: 'port_id', label: 'Logical port', className: 'mono-cell' },
        { key: 'project_id', label: 'Project', className: 'mono-cell' },
        { key: 'state', label: 'State' },
      ]}
      createLabel="Router interface"
      createFields={[
        { name: 'project_id', label: 'Project ID', required: true },
        { name: 'router_id', label: 'Router ID', required: true },
        { name: 'subnet_id', label: 'Subnet ID', required: true },
      ]}
      allowDelete
      compact
    />
  </div>;
}

export function PortsPage() {
  const [tableKey, setTableKey] = useState(0);

  return <div className="stacked-pages">
    <ResourcePage<Port>
      key={tableKey}
      title="Ports & VM attachments"
      description="Logical switch ports, fixed IP allocations, and chassis bindings."
      endpoint="/ports"
      columns={[
        { key: 'name', label: 'Port', render: (item) => <strong>{item.name || item.id}</strong> },
        { key: 'network_id', label: 'Network', className: 'mono-cell' },
        { key: 'mac_address', label: 'MAC address', className: 'mono-cell' },
        { key: 'fixed_ips', label: 'Fixed IPs' },
        { key: 'vmid', label: 'Attachment', render: (item) => formatValue(item.vmid ? `${item.node_id}/${item.vmid}/${item.nic || '?'}` : undefined) },
        { key: 'binding_status', label: 'Binding', render: (item) => <StatusPill value={item.binding_status || item.state} /> },
      ]}
      emptyMessage="Ports are created when a VM NIC is attached to a PVN network."
    />
    <PortAttachmentPanel onChanged={() => setTableKey((value) => value + 1)} />
  </div>;
}

export function FloatingIPsPage() {
  return <ResourcePage<FloatingIP>
    title="Floating IPs"
    description="North-south addresses translated at the active gateway chassis."
    endpoint="/floating-ips"
    columns={[
      { key: 'address', label: 'Floating IP', className: 'mono-cell', render: (item) => <strong>{item.address || item.name || item.id}</strong> },
      { key: 'fixed_ip_address', label: 'Fixed IP', className: 'mono-cell' },
      { key: 'port_id', label: 'Port', className: 'mono-cell' },
      { key: 'router_id', label: 'Router', className: 'mono-cell' },
      { key: 'project_id', label: 'Project', className: 'mono-cell' },
      { key: 'status', label: 'State' },
    ]}
    createLabel="Floating IP"
    createFields={[
      { name: 'project_id', label: 'Project ID', required: true },
      { name: 'provider_network_id', label: 'Provider network ID', required: true },
      { name: 'address', label: 'Floating IPv4 address', required: true, placeholder: '203.0.113.42' },
      { name: 'port_id', label: 'Destination port ID' },
      { name: 'fixed_ip_address', label: 'Destination fixed IP' },
    ]}
    allowDelete
  />;
}

export function SecurityGroupsPage() {
  return <div className="stacked-pages">
    <ResourcePage<SecurityGroup>
      title="Security groups"
      description="Stateful OVN port-group policies for tenant ingress and egress."
      endpoint="/security-groups"
      columns={[
        { key: 'name', label: 'Security group', render: (item) => <strong>{item.name || item.id}</strong> },
        { key: 'project_id', label: 'Project', className: 'mono-cell' },
        { key: 'description', label: 'Description' },
        { key: 'stateful', label: 'Stateful' },
        { key: 'state', label: 'State' },
      ]}
      createLabel="Security group"
      createFields={[
        { name: 'name', label: 'Name', required: true, placeholder: 'web-servers' },
        { name: 'project_id', label: 'Project ID', required: true },
        { name: 'description', label: 'Description' },
        { name: 'stateful', label: 'Stateful', type: 'checkbox', defaultValue: true },
      ]}
      allowDelete
    />
    <ResourcePage<SecurityGroupRule>
      title="Security group rules"
      description="Ingress and egress matches compiled into OVN ACLs."
      endpoint="/security-group-rules"
      columns={[
        { key: 'security_group_id', label: 'Security group', className: 'mono-cell' },
        { key: 'direction', label: 'Direction' },
        { key: 'protocol', label: 'Protocol' },
        { key: 'port_range_min', label: 'Port from' },
        { key: 'port_range_max', label: 'Port to' },
        { key: 'remote_cidr', label: 'Remote CIDR', className: 'mono-cell' },
        { key: 'action', label: 'Action' },
        { key: 'state', label: 'State' },
      ]}
      createLabel="Security group rule"
      createFields={[
        { name: 'project_id', label: 'Project ID', required: true },
        { name: 'security_group_id', label: 'Security group ID', required: true },
        { name: 'direction', label: 'Direction', type: 'select', required: true, defaultValue: 'ingress', options: [{ label: 'Ingress', value: 'ingress' }, { label: 'Egress', value: 'egress' }] },
        { name: 'ethertype', label: 'Ether type', type: 'select', required: true, defaultValue: 'IPv4', options: [{ label: 'IPv4', value: 'IPv4' }] },
        { name: 'protocol', label: 'Protocol', type: 'select', options: [{ label: 'TCP', value: 'tcp' }, { label: 'UDP', value: 'udp' }, { label: 'ICMP', value: 'icmp' }] },
        { name: 'port_range_min', label: 'Port range start', type: 'number' },
        { name: 'port_range_max', label: 'Port range end', type: 'number' },
        { name: 'remote_cidr', label: 'Remote IPv4 CIDR', placeholder: '0.0.0.0/0' },
        { name: 'remote_group_id', label: 'Remote security group ID' },
        { name: 'action', label: 'Action', type: 'select', required: true, defaultValue: 'allow', options: [{ label: 'Allow', value: 'allow' }, { label: 'Drop', value: 'drop' }] },
        { name: 'description', label: 'Description' },
      ]}
      allowDelete
      compact
    />
  </div>;
}

export function ProviderNetworksPage() {
  return <div className="stacked-pages">
    <ResourcePage<ProviderNetwork>
      title="Provider networks"
      description="Shared external network containers exposed to tenant routers."
      endpoint="/provider-networks"
      columns={[
        { key: 'name', label: 'Network', render: (item) => <strong>{item.name || item.id}</strong> },
        { key: 'shared', label: 'Shared' },
        { key: 'default_segment_id', label: 'Default segment', className: 'mono-cell' },
        { key: 'description', label: 'Description' },
        { key: 'state', label: 'State' },
      ]}
      createLabel="Provider network"
      createFields={[
        { name: 'name', label: 'Name', required: true, placeholder: 'public' },
        { name: 'description', label: 'Description' },
        { name: 'shared', label: 'Shared between projects', type: 'checkbox', defaultValue: true },
      ]}
      allowDelete
    />
    <ResourcePage<ProviderSegment>
      title="Provider segments"
      description="Flat or VLAN bridge mappings consumed by gateway chassis."
      endpoint="/provider-segments"
      columns={[
        { key: 'name', label: 'Segment', render: (item) => <strong>{item.name || item.id}</strong> },
        { key: 'provider_network_id', label: 'Provider network', className: 'mono-cell' },
        { key: 'network_type', label: 'Type' },
        { key: 'physical_network', label: 'Bridge mapping' },
        { key: 'vlan_id', label: 'VLAN' },
        { key: 'state', label: 'State' },
      ]}
      createLabel="Provider segment"
      createFields={[
        { name: 'name', label: 'Name', required: true, placeholder: 'public-vlan' },
        { name: 'provider_network_id', label: 'Provider network ID', required: true },
        { name: 'network_type', label: 'Type', type: 'select', required: true, defaultValue: 'vlan', options: [{ label: 'VLAN', value: 'vlan' }, { label: 'Flat', value: 'flat' }] },
        { name: 'physical_network', label: 'OVS bridge mapping', required: true, placeholder: 'provider' },
        { name: 'vlan_id', label: 'VLAN ID', type: 'number' },
      ]}
      allowDelete
      compact
    />
  </div>;
}

export function NodesPage() {
  return <ResourcePage<NodeStatus>
    title="Nodes, central & gateways"
    description="PVN installation health, OVSDB membership, and north-south gateway placement."
    endpoint="/nodes"
    columns={[
      { key: 'name', label: 'Node', render: (item) => <strong>{item.name || item.id}</strong> },
      { key: 'management_address', label: 'Management IP', className: 'mono-cell' },
      { key: 'chassis_id', label: 'Chassis ID', className: 'mono-cell' },
      { key: 'roles', label: 'Roles' },
      { key: 'enabled', label: 'Enabled', render: (item) => <StatusPill value={item.enabled ? 'enabled' : 'disabled'} /> },
      { key: 'state', label: 'Control state' },
      { key: 'last_seen_at', label: 'Last seen' },
    ]}
    emptyMessage="Install the PVN node package on every online PVE node."
  />;
}

export function OperationsPage() {
  return <ResourcePage<Operation>
    title="Operations"
    description="Asynchronous reconciler work and the cluster-wide audit trail."
    endpoint="/operations"
    columns={[
      { key: 'action', label: 'Action', render: (item) => <strong>{item.action || item.kind || item.id}</strong> },
      { key: 'target_kind', label: 'Resource' },
      { key: 'target_id', label: 'Resource ID', className: 'mono-cell' },
      { key: 'target_revision', label: 'Revision' },
      { key: 'status', label: 'State' },
      { key: 'started_at', label: 'Started' },
      { key: 'completed_at', label: 'Completed' },
      { key: 'error', label: 'Error', className: 'error-cell' },
    ]}
    emptyMessage="PVN operations will appear here when resources are reconciled."
  />;
}
