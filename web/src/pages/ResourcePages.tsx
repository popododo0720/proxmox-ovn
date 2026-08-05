import { useState } from 'react';
import { useApi } from '../api/context';
import type {
  FloatingIP,
  Network,
  NodeStatus,
  Operation,
  Port,
  PortProvisionInput,
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
import type { FormField } from '../components/CreateDialog';
import { ResourcePage, formatValue, type Column } from '../components/ResourcePage';
import type { ResourceReference } from '../components/ResourceSelect';
import { StatusPill } from '../components/StatusPill';

const projectReference: ResourceReference = {
  endpoint: '/projects',
  detailKeys: ['pool_id'],
  emptyLabel: 'No project mappings available',
};

const providerNetworkReference: ResourceReference = {
  endpoint: '/provider-networks',
  emptyLabel: 'No provider networks available',
};

const projectNetworkReference: ResourceReference = {
  endpoint: '/networks',
  matches: [{ formField: 'project_id' }],
  emptyLabel: 'No networks in this project',
};

const projectSubnetReference: ResourceReference = {
  endpoint: '/subnets',
  detailKeys: ['cidr'],
  matches: [
    { formField: 'project_id' },
    { formField: 'network_id' },
  ],
  emptyLabel: 'No subnets on this network',
};

const externalNetworkReference: ResourceReference = {
  endpoint: '/networks',
  where: { external: true },
  emptyLabel: 'No external networks available',
};

const externalSubnetReference: ResourceReference = {
  endpoint: '/subnets',
  detailKeys: ['cidr'],
  matches: [{ formField: 'external_network_id', resourceField: 'network_id' }],
  emptyLabel: 'No subnets on this external network',
};

const projectRouterReference: ResourceReference = {
  endpoint: '/routers',
  detailKeys: ['external_ip_address'],
  matches: [{ formField: 'project_id' }],
  emptyLabel: 'No routers in this project',
};

const projectSecurityGroupReference: ResourceReference = {
  endpoint: '/security-groups',
  matches: [{ formField: 'project_id' }],
  emptyLabel: 'No security groups in this project',
};

const projectPortReference: ResourceReference = {
  endpoint: '/ports',
  detailKeys: ['mac_address'],
  matches: [{ formField: 'project_id' }],
  emptyLabel: 'No ports in this project',
};

const projectField: FormField = {
  name: 'project_id',
  label: 'Project',
  type: 'resource-select',
  reference: projectReference,
  required: true,
};

const projectColumns: Column<Project>[] = [
  { key: 'name', label: 'Project', render: (item) => <strong>{item.name || item.id}</strong> },
  { key: 'pool_id', label: 'PVE pool' },
  { key: 'state', label: 'State' },
  { key: 'id', label: 'ID', className: 'mono-cell' },
];

const networkFields = [
  { name: 'name', label: 'Name', required: true, placeholder: 'application' },
  projectField,
  { name: 'mtu', label: 'Guest MTU', type: 'number' as const, defaultValue: 1400 },
  { name: 'external', label: 'Provider-backed external network', type: 'checkbox' as const },
  { name: 'provider_network_id', label: 'Provider network', type: 'resource-select' as const, reference: providerNetworkReference, help: 'Required only for an external network.' },
  { name: 'description', label: 'Description' },
];

export function ProjectsPage() {
  return <ResourcePage<Project>
    title="Projects"
    description="Existing PVE pools mapped into isolated PVN project boundaries."
    endpoint="/projects"
    columns={projectColumns}
    createLabel="Project mapping"
    createFields={[
      { name: 'name', label: 'Name', required: true, placeholder: 'tenant-a' },
      { name: 'pool_id', label: 'Existing PVE pool ID', required: true, placeholder: 'tenant-a' },
      { name: 'description', label: 'Description' },
    ]}
    allowDelete
    emptyMessage="Create a PVE pool first, then map its ID here."
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
        projectField,
        { name: 'network_id', label: 'Network', type: 'resource-select', reference: projectNetworkReference, required: true },
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
        projectField,
        { name: 'external_network_id', label: 'External network', type: 'resource-select', reference: externalNetworkReference },
        { name: 'external_subnet_id', label: 'External subnet', type: 'resource-select', reference: externalSubnetReference },
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
        projectField,
        { name: 'router_id', label: 'Router', type: 'resource-select', reference: projectRouterReference, required: true },
        { name: 'subnet_id', label: 'Subnet', type: 'resource-select', reference: { ...projectSubnetReference, matches: [{ formField: 'project_id' }] }, required: true },
      ]}
      allowDelete
      compact
    />
  </div>;
}

export function PortsPage() {
  const api = useApi();
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
      createLabel="Tenant port"
      createFields={[
        { name: 'name', label: 'Name', placeholder: 'web-01' },
        projectField,
        { name: 'network_id', label: 'Tenant network', type: 'resource-select', reference: projectNetworkReference, required: true },
        { name: 'subnet_id', label: 'Subnet', type: 'resource-select', reference: projectSubnetReference, help: 'Optional. PVN allocates the next free address when set.' },
        { name: 'fixed_ip_address', label: 'Requested fixed IPv4', help: 'Requires a subnet ID.' },
        { name: 'mac_address', label: 'Requested MAC', help: 'Leave blank for a stable PVN-generated MAC.' },
        { name: 'security_group_ids', label: 'Security groups', type: 'resource-select', reference: projectSecurityGroupReference, multiple: true, help: 'Optional. Select every policy that should apply to this port.' },
      ]}
      createResource={(payload) => {
        const securityGroupIDs = Array.isArray(payload.security_group_ids)
          ? payload.security_group_ids.map(String).map((value) => value.trim()).filter(Boolean)
          : String(payload.security_group_ids ?? '').split(',').map((value) => value.trim()).filter(Boolean);
        const input = { ...payload } as unknown as PortProvisionInput;
        delete (input as unknown as Record<string, unknown>).security_group_ids;
        if (securityGroupIDs.length > 0) input.security_group_ids = securityGroupIDs;
        return api.provisionPort(input);
      }}
      allowDelete
      deleteResource={(port) => api.deprovisionPort(port.id, port.revision || 0)}
      emptyMessage="Provision a tenant port, then attach it to a VM NIC below."
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
      projectField,
      { name: 'provider_network_id', label: 'Provider network', type: 'resource-select', reference: providerNetworkReference, required: true },
      { name: 'address', label: 'Floating IPv4 address', required: true, placeholder: '203.0.113.42' },
      { name: 'router_id', label: 'Router', type: 'resource-select', reference: projectRouterReference, help: 'Required when associating this address with a port.' },
      { name: 'port_id', label: 'Destination port', type: 'resource-select', reference: projectPortReference },
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
        projectField,
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
        projectField,
        { name: 'security_group_id', label: 'Security group', type: 'resource-select', reference: projectSecurityGroupReference, required: true },
        { name: 'direction', label: 'Direction', type: 'select', required: true, defaultValue: 'ingress', options: [{ label: 'Ingress', value: 'ingress' }, { label: 'Egress', value: 'egress' }] },
        { name: 'ethertype', label: 'Ether type', type: 'select', required: true, defaultValue: 'IPv4', options: [{ label: 'IPv4', value: 'IPv4' }] },
        { name: 'protocol', label: 'Protocol', type: 'select', options: [{ label: 'TCP', value: 'tcp' }, { label: 'UDP', value: 'udp' }, { label: 'ICMP', value: 'icmp' }] },
        { name: 'port_range_min', label: 'Port range start', type: 'number' },
        { name: 'port_range_max', label: 'Port range end', type: 'number' },
        { name: 'remote_cidr', label: 'Remote IPv4 CIDR', placeholder: '0.0.0.0/0' },
        { name: 'remote_group_id', label: 'Remote security group', type: 'resource-select', reference: projectSecurityGroupReference },
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
        { name: 'provider_network_id', label: 'Provider network', type: 'resource-select', reference: providerNetworkReference, required: true },
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
