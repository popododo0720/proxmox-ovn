export type ResourceID = string;
export type LifecycleState =
  | 'active'
  | 'ready'
  | 'pending'
  | 'creating'
  | 'updating'
  | 'deleting'
  | 'disabled'
  | 'degraded'
  | 'error'
  | 'unknown';

export interface BaseResource {
  id: ResourceID;
  name?: string;
  status?: LifecycleState | string;
  state?: LifecycleState | string;
  revision?: number;
  applied_revision?: number;
  last_error?: string;
  created_at?: string;
  updated_at?: string;
  [key: string]: unknown;
}

export interface SessionInfo {
  user: string;
  csrf_token: string;
  permissions: string[] | Record<string, unknown>;
  cluster?: string;
  expires_at?: string;
}

export type OperationalHealth = 'ready' | 'degraded' | 'unavailable';

export interface HealthStatus {
  status: string;
  version?: string;
  cluster?: string;
  database: OperationalHealth;
  ovn_northbound: OperationalHealth;
  ovn_southbound: OperationalHealth;
  reconciler: OperationalHealth;
  capacity?: {
    ready: boolean;
    reason?: string;
    reporter?: string;
    online_nodes?: string[];
    missing_nodes?: string[];
    stale_nodes?: string[];
  };
  [key: string]: unknown;
}

export interface Project extends BaseResource {
  pool_id?: string;
  description?: string;
  network_count?: number;
}

export interface Network extends BaseResource {
  project_id?: string;
  description?: string;
  mtu?: number;
  external?: boolean;
  provider_network_id?: string;
}

export interface Subnet extends BaseResource {
  project_id?: string;
  network_id?: string;
  cidr?: string;
  gateway_ip?: string;
  enable_dhcp?: boolean;
  allocation_pools?: Array<{ start: string; end: string }>;
}

export interface Router extends BaseResource {
  project_id?: string;
  external_network_id?: string;
  external_subnet_id?: string;
  external_ip_address?: string;
  enable_snat?: boolean;
}

export interface RouterInterface extends BaseResource {
  project_id?: string;
  router_id?: string;
  subnet_id?: string;
  port_id?: string;
}

export interface Port extends BaseResource {
  project_id?: string;
  network_id?: string;
  mac_address?: string;
  fixed_ips?: Array<{ subnet_id?: string; address?: string }>;
  node_id?: string;
  vmid?: number;
  nic?: string;
  binding_status?: string;
  lsp_name?: string;
  generation?: number;
  requested_chassis?: string;
}

export interface PortAttachInput {
  node_id: string;
  vmid: number;
  nic: `net${number}`;
  generation: number;
}

export interface PortDetachInput {
  generation: number;
}

export interface PortProvisionInput {
  project_id: string;
  network_id: string;
  subnet_id?: string;
  name?: string;
  mac_address?: string;
  fixed_ip_address?: string;
  security_group_ids?: string[];
}

export interface FloatingIP extends BaseResource {
  project_id?: string;
  provider_network_id?: string;
  address?: string;
  fixed_ip_address?: string;
  port_id?: string;
  router_id?: string;
}

export interface SecurityGroup extends BaseResource {
  project_id?: string;
  description?: string;
  stateful?: boolean;
}

export interface SecurityGroupRule extends BaseResource {
  project_id?: string;
  security_group_id?: string;
  direction?: 'ingress' | 'egress' | string;
  ethertype?: 'IPv4' | string;
  protocol?: 'tcp' | 'udp' | 'icmp' | string;
  port_range_min?: number;
  port_range_max?: number;
  remote_cidr?: string;
  remote_group_id?: string;
  action?: 'allow' | 'drop' | string;
  description?: string;
}

export interface ProviderNetwork extends BaseResource {
  description?: string;
  shared?: boolean;
  default_segment_id?: string;
}

export interface ProviderSegment extends BaseResource {
  provider_network_id?: string;
  network_type?: 'flat' | 'vlan' | string;
  physical_network?: string;
  vlan_id?: number;
}

export interface NodeStatus extends BaseResource {
  chassis_id?: string;
  management_address?: string;
  enabled?: boolean;
  roles?: string[];
  central_role?: 'standalone' | 'voter' | 'non-voter' | 'none' | string;
  gateway_priority?: number;
  ovn_controller?: string;
  last_seen_at?: string;
}

export interface Operation extends BaseResource {
  kind?: string;
  action?: string;
  target_kind?: string;
  target_id?: string;
  target_revision?: number;
  idempotency_key?: string;
  error?: string;
  started_at?: string;
  completed_at?: string;
}

export interface ListResult<T> {
  items: T[];
  next_cursor?: string;
  total?: number;
}

export interface ApiErrorBody {
  code?: string;
  message?: string;
  error?: string | { message?: string; code?: string; details?: unknown };
  details?: unknown;
}
