import type {
  ApiErrorBody,
  BaseResource,
  FloatingIP,
  HealthStatus,
  ListResult,
  Network,
  NodeStatus,
  Operation,
  Port,
  Project,
  ProviderNetwork,
  ProviderSegment,
  ResourceID,
  Router,
  SecurityGroup,
  SessionInfo,
  Subnet,
} from './types';

type Fetcher = typeof fetch;
type QueryValue = string | number | boolean | undefined;

interface RequestOptions extends Omit<RequestInit, 'body'> {
  body?: unknown;
  query?: Record<string, QueryValue>;
  idempotencyKey?: string;
  revision?: number;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly details?: unknown;

  constructor(message: string, status: number, code?: string, details?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

function unwrap<T>(value: unknown): T {
  if (value && typeof value === 'object' && 'data' in value) {
    return (value as { data: T }).data;
  }
  return value as T;
}

function normalizeList<T>(value: unknown): ListResult<T> {
  const payload = unwrap<unknown>(value);
  if (Array.isArray(payload)) return { items: payload as T[], total: payload.length };
  if (payload && typeof payload === 'object') {
    const result = payload as Partial<ListResult<T>> & { results?: T[] };
    const items = Array.isArray(result.items)
      ? result.items
      : Array.isArray(result.results)
        ? result.results
        : [];
    return { ...result, items };
  }
  return { items: [] };
}

function errorMessage(body: ApiErrorBody | undefined, fallback: string): string {
  if (!body) return fallback;
  if (typeof body.error === 'string') return body.error;
  if (body.error && typeof body.error.message === 'string') return body.error.message;
  return body.message || fallback;
}

export class ApiClient {
  readonly baseURL: string;
  private readonly fetcher: Fetcher;
  private csrfToken = '';

  constructor(baseURL = '/api/v1', fetcher: Fetcher = fetch) {
    this.baseURL = baseURL.replace(/\/$/, '');
    this.fetcher = fetcher;
  }

  setCSRFToken(token: string): void {
    this.csrfToken = token;
  }

  private async request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const url = new URL(`${this.baseURL}${path}`, window.location.origin);
    for (const [key, value] of Object.entries(options.query ?? {})) {
      if (value !== undefined) url.searchParams.set(key, String(value));
    }

    const method = (options.method || 'GET').toUpperCase();
    const headers = new Headers(options.headers);
    headers.set('Accept', 'application/json');
    if (options.body !== undefined) headers.set('Content-Type', 'application/json');
    if (!['GET', 'HEAD', 'OPTIONS'].includes(method) && this.csrfToken) {
      headers.set('CSRFPreventionToken', this.csrfToken);
    }
    if (options.idempotencyKey) headers.set('Idempotency-Key', options.idempotencyKey);
    if (options.revision !== undefined) headers.set('If-Match', `"${options.revision}"`);

    const response = await this.fetcher(url, {
      ...options,
      method,
      headers,
      credentials: 'include',
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
    });

    const text = response.status === 204 ? '' : await response.text();
    let decoded: unknown;
    if (text) {
      try {
        decoded = JSON.parse(text);
      } catch {
        decoded = undefined;
      }
    }
    if (!response.ok) {
      const body = decoded as ApiErrorBody | undefined;
      const nestedError = body?.error && typeof body.error === 'object' ? body.error : undefined;
      throw new ApiError(
        errorMessage(body, response.statusText || `Request failed (${response.status})`),
        response.status,
        body?.code || nestedError?.code,
        body?.details || nestedError?.details,
      );
    }
    return unwrap<T>(decoded);
  }

  async bootstrap(): Promise<SessionInfo> {
    let raw: Partial<SessionInfo> & {
      csrfToken?: string;
      username?: string;
      authenticated?: boolean;
    };
    try {
      raw = await this.request<typeof raw>('/session');
    } catch (error) {
      if (!(error instanceof ApiError) || error.status !== 404) throw error;
      raw = await this.request<typeof raw>('/bootstrap');
    }
    if (raw.authenticated === false) throw new ApiError('PVE session is not authenticated', 401);
    const session: SessionInfo = {
      user: raw.user || raw.username || '',
      csrf_token: raw.csrf_token || raw.csrfToken || '',
      permissions: raw.permissions || [],
      cluster: raw.cluster,
      expires_at: raw.expires_at,
    };
    if (!session.user) throw new ApiError('PVN returned an invalid session response', 502);
    this.setCSRFToken(session.csrf_token);
    return session;
  }

  list<T extends BaseResource>(path: string, query?: RequestOptions['query']): Promise<ListResult<T>> {
    return this.request<unknown>(path, { query }).then(normalizeList<T>);
  }

  get<T extends BaseResource>(path: string, id: ResourceID): Promise<T> {
    return this.request<T>(`${path}/${encodeURIComponent(id)}`);
  }

  create<T extends BaseResource>(path: string, input: unknown, idempotencyKey: string = crypto.randomUUID()): Promise<T> {
    return this.request<T>(path, { method: 'POST', body: input, idempotencyKey });
  }

  update<T extends BaseResource>(path: string, id: ResourceID, input: unknown, revision?: number, idempotencyKey: string = crypto.randomUUID()): Promise<T> {
    return this.request<T>(`${path}/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: input,
      revision,
      idempotencyKey,
    });
  }

  remove(path: string, id: ResourceID, revision?: number, idempotencyKey: string = crypto.randomUUID()): Promise<void> {
    return this.request<void>(`${path}/${encodeURIComponent(id)}`, { method: 'DELETE', revision, idempotencyKey });
  }

  projects = () => this.list<Project>('/projects');
  networks = () => this.list<Network>('/networks');
  subnets = () => this.list<Subnet>('/subnets');
  routers = () => this.list<Router>('/routers');
  ports = () => this.list<Port>('/ports');
  floatingIPs = () => this.list<FloatingIP>('/floating-ips');
  securityGroups = () => this.list<SecurityGroup>('/security-groups');
  providerNetworks = () => this.list<ProviderNetwork>('/provider-networks');
  providerSegments = () => this.list<ProviderSegment>('/provider-segments');
  nodes = () => this.list<NodeStatus>('/nodes');
  operations = (limit = 100) => this.list<Operation>('/operations', { limit });
  health = () => this.request<HealthStatus>('/health');
}

export const apiClient = new ApiClient();
