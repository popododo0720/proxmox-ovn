import { describe, expect, it, vi } from 'vitest';
import { ApiClient, ApiError } from './client';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('ApiClient', () => {
  it('calls the default fetch with the browser window as its receiver', async () => {
    const fetcher = vi.fn(function (this: Window) {
      if (this !== window) throw new TypeError('Illegal invocation');
      return Promise.resolve(jsonResponse({ data: [] }));
    });
    vi.stubGlobal('fetch', fetcher);
    const client = new ApiClient('/api/v1');

    try {
      await expect(client.networks()).resolves.toEqual({ items: [], total: 0 });
      expect(fetcher.mock.contexts[0]).toBe(window);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('unwraps data lists from the v1 API envelope', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: [{ id: 'net-1', name: 'app' }] }));
    const client = new ApiClient('/api/v1', fetcher as unknown as typeof fetch);

    await expect(client.networks()).resolves.toEqual({
      items: [{ id: 'net-1', name: 'app' }],
      total: 1,
    });
    expect(fetcher).toHaveBeenCalledOnce();
    expect(String(fetcher.mock.calls[0][0])).toBe('http://localhost:3000/api/v1/networks');
  });

  it('bootstraps the PVE session and sends CSRF, idempotency, and revision headers', async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ data: { user: 'root@pam', csrf_token: 'csrf', permissions: ['SDN.Allocate'] } }))
      .mockResolvedValueOnce(jsonResponse({ data: { id: 'net-1', revision: 1 } }))
      .mockResolvedValueOnce(jsonResponse({ data: { id: 'net-1', revision: 2 } }));
    const client = new ApiClient('/api/v1', fetcher as unknown as typeof fetch);

    await client.bootstrap();
    await client.create('/networks', { name: 'app' }, 'operation-1');
    await client.update('/networks', 'net-1', { name: 'new' }, 1);

    const createRequest = fetcher.mock.calls[1][1] as RequestInit;
    const createHeaders = new Headers(createRequest.headers);
    expect(createHeaders.get('X-PVN-CSRF-Token')).toBe('csrf');
    expect(createHeaders.get('Idempotency-Key')).toBe('operation-1');
    expect(createRequest.credentials).toBe('include');
    const updateHeaders = new Headers((fetcher.mock.calls[2][1] as RequestInit).headers);
    expect(updateHeaders.get('If-Match')).toBe('"1"');
    expect(updateHeaders.get('Idempotency-Key')).toBeTruthy();
  });

  it('uses revision-guarded port lifecycle actions', async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ data: { id: 'port-1', revision: 3, generation: 2, binding_status: 'binding' } }))
      .mockResolvedValueOnce(jsonResponse({ data: { id: 'port-1', revision: 4, generation: 2, binding_status: 'detaching' } }));
    const client = new ApiClient('/api/v1', fetcher as unknown as typeof fetch);
    client.setCSRFToken('csrf-value');

    await client.attachPort('port-1', { node_id: 'pve-a', vmid: 100, nic: 'net1', generation: 1 }, 2, 'attach-key');
    await client.detachPort('port-1', { generation: 2 }, 3, 'detach-key');

    const attachURL = fetcher.mock.calls[0][0] as URL;
    const attach = fetcher.mock.calls[0][1] as RequestInit;
    expect(attachURL.pathname).toBe('/api/v1/ports/port-1/attach');
    expect(attach.method).toBe('POST');
    expect(new Headers(attach.headers).get('If-Match')).toBe('"2"');
    expect(new Headers(attach.headers).get('Idempotency-Key')).toBe('attach-key');
    expect(JSON.parse(String(attach.body))).toEqual({ node_id: 'pve-a', vmid: 100, nic: 'net1', generation: 1 });

    const detachURL = fetcher.mock.calls[1][0] as URL;
    const detach = fetcher.mock.calls[1][1] as RequestInit;
    expect(detachURL.pathname).toBe('/api/v1/ports/port-1/detach');
    expect(new Headers(detach.headers).get('If-Match')).toBe('"3"');
    expect(new Headers(detach.headers).get('Idempotency-Key')).toBe('detach-key');
    expect(JSON.parse(String(detach.body))).toEqual({ generation: 2 });
  });

  it('uses dedicated idempotent port provision and deprovision actions', async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ data: { id: 'port-1', revision: 1 } }, 201))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    const client = new ApiClient('/api/v1', fetcher as unknown as typeof fetch);
    client.setCSRFToken('csrf-value');

    await client.provisionPort({ project_id: 'project-1', network_id: 'network-1', subnet_id: 'subnet-1' }, 'provision-key');
    await client.deprovisionPort('port-1', 1, 'deprovision-key');

    const provisionURL = fetcher.mock.calls[0][0] as URL;
    const provision = fetcher.mock.calls[0][1] as RequestInit;
    expect(provisionURL.pathname).toBe('/api/v1/ports/provision');
    expect(provision.method).toBe('POST');
    expect(new Headers(provision.headers).get('Idempotency-Key')).toBe('provision-key');
    expect(JSON.parse(String(provision.body))).toEqual({ project_id: 'project-1', network_id: 'network-1', subnet_id: 'subnet-1' });

    const deprovisionURL = fetcher.mock.calls[1][0] as URL;
    const deprovision = fetcher.mock.calls[1][1] as RequestInit;
    expect(deprovisionURL.pathname).toBe('/api/v1/ports/port-1/deprovision');
    expect(deprovision.method).toBe('DELETE');
    expect(new Headers(deprovision.headers).get('If-Match')).toBe('"1"');
    expect(new Headers(deprovision.headers).get('Idempotency-Key')).toBe('deprovision-key');
  });

  it('resolves one runtime VM NIC with encoded lookup parameters', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: {
      port_id: 'port-1',
      lsp_name: 'pvn-port-1',
      mac_address: '02:00:00:00:00:01',
      generation: 3,
      requested_chassis: 'chassis-a',
      status: 'bound',
    } }));
    const client = new ApiClient('/api/v1', fetcher as unknown as typeof fetch);

    await expect(client.resolveRuntimePort('pve-a', 100, 'net0')).resolves.toMatchObject({
      port_id: 'port-1',
      status: 'bound',
    });

    const url = fetcher.mock.calls[0][0] as URL;
    expect(url.pathname).toBe('/api/v1/runtime/ports/resolve');
    expect(Object.fromEntries(url.searchParams)).toEqual({ node: 'pve-a', vmid: '100', nic: 'net0' });
    expect((fetcher.mock.calls[0][1] as RequestInit).method).toBe('GET');
  });

  it('plans, dry-runs, and applies the default security-group backfill', async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ data: { cluster: 'pve-lab', projects: [], total_legacy_ports: 0 } }))
      .mockResolvedValueOnce(jsonResponse({ data: { cluster: 'pve-lab', dry_run: true, planned: 2, results: [] } }))
      .mockResolvedValueOnce(jsonResponse({ data: { cluster: 'pve-lab', dry_run: false, migrated: 2, results: [] } }));
    const client = new ApiClient('/api/v1', fetcher as unknown as typeof fetch);
    client.setCSRFToken('csrf-value');

    await client.defaultSecurityGroupBackfillPlan();
    await client.applyDefaultSecurityGroupBackfill();
    await client.applyDefaultSecurityGroupBackfill({ dry_run: false, confirm: 'pve-lab', plan_token: 'v1.opaque' });

    const planURL = fetcher.mock.calls[0][0] as URL;
    const plan = fetcher.mock.calls[0][1] as RequestInit;
    expect(planURL.pathname).toBe('/api/v1/admin/default-security-group-backfill/plan');
    expect(plan.method).toBe('GET');

    const dryRunURL = fetcher.mock.calls[1][0] as URL;
    const dryRun = fetcher.mock.calls[1][1] as RequestInit;
    expect(dryRunURL.pathname).toBe('/api/v1/admin/default-security-group-backfill/apply');
    expect(dryRun.method).toBe('POST');
    expect(JSON.parse(String(dryRun.body))).toEqual({});
    expect(new Headers(dryRun.headers).get('X-PVN-CSRF-Token')).toBe('csrf-value');

    const apply = fetcher.mock.calls[2][1] as RequestInit;
    expect(JSON.parse(String(apply.body))).toEqual({ dry_run: false, confirm: 'pve-lab', plan_token: 'v1.opaque' });
    expect(new Headers(apply.headers).get('X-PVN-CSRF-Token')).toBe('csrf-value');
  });

  it('surfaces structured API errors', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({
      error: { code: 'revision_conflict', message: 'resource changed', details: { current: 3 } },
    }, 409));
    const client = new ApiClient('/api/v1', fetcher as unknown as typeof fetch);

    const error = await client.networks().catch((reason: unknown) => reason);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 409, code: 'revision_conflict', message: 'resource changed' });
  });
});
