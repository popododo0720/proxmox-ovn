import type { ResourceReference } from '../components/ResourceSelect';

export const projectReference: ResourceReference = {
  endpoint: '/projects',
  detailKeys: ['pool_id'],
  emptyLabel: 'No project mappings available',
};

export const providerNetworkReference: ResourceReference = {
  endpoint: '/provider-networks',
  emptyLabel: 'No provider networks available',
};

export const projectNetworkReference: ResourceReference = {
  endpoint: '/networks',
  matches: [{ formField: 'project_id' }],
  emptyLabel: 'No networks in this project',
};

export const projectSubnetReference: ResourceReference = {
  endpoint: '/subnets',
  detailKeys: ['cidr'],
  matches: [
    { formField: 'project_id' },
    { formField: 'network_id' },
  ],
  emptyLabel: 'No subnets on this network',
};

export const externalNetworkReference: ResourceReference = {
  endpoint: '/networks',
  where: { external: true },
  emptyLabel: 'No external networks available',
};

export const externalSubnetReference: ResourceReference = {
  endpoint: '/subnets',
  detailKeys: ['cidr'],
  matches: [{ formField: 'external_network_id', resourceField: 'network_id' }],
  emptyLabel: 'No subnets on this external network',
};

export const projectRouterReference: ResourceReference = {
  endpoint: '/routers',
  detailKeys: ['external_ip_address'],
  matches: [{ formField: 'project_id' }],
  emptyLabel: 'No routers in this project',
};

export const projectSecurityGroupReference: ResourceReference = {
  endpoint: '/security-groups',
  matches: [{ formField: 'project_id' }],
  emptyLabel: 'No security groups in this project',
};

export const projectPortReference: ResourceReference = {
  endpoint: '/ports',
  detailKeys: ['mac_address'],
  matches: [{ formField: 'project_id' }],
  emptyLabel: 'No ports in this project',
};

export const currentProviderSegmentReference: ResourceReference = {
  endpoint: '/provider-segments',
  detailKeys: ['network_type', 'physical_network', 'vlan_id'],
  matches: [{ formField: 'id', resourceField: 'provider_network_id' }],
  emptyLabel: 'Create a segment for this provider network first',
};

export const nodeReference: ResourceReference = {
  endpoint: '/nodes',
  labelKeys: ['name', 'management_address'],
};

const operationReferences: Record<string, ResourceReference> = {
  project: projectReference,
  network: projectNetworkReference,
  subnet: projectSubnetReference,
  port: projectPortReference,
  'ip-allocation': { endpoint: '/ip-allocations', labelKeys: ['address'], fallbackLabel: 'IP allocation' },
  router: projectRouterReference,
  'router-interface': { endpoint: '/router-interfaces', fallbackLabel: 'Router interface' },
  'floating-ip': { endpoint: '/floating-ips', labelKeys: ['address'], fallbackLabel: 'Floating IP' },
  'provider-network': providerNetworkReference,
  'provider-segment': currentProviderSegmentReference,
  'security-group': projectSecurityGroupReference,
  'security-group-rule': { endpoint: '/security-group-rules', fallbackLabel: 'Security group rule' },
  node: nodeReference,
};

export function operationTargetReference(kind: unknown): ResourceReference | undefined {
  return typeof kind === 'string' ? operationReferences[kind] : undefined;
}
