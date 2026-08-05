import { describe, expect, it, vi } from 'vitest';
import { ApiClient, ApiError } from './client';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('ApiClient', () => {
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
