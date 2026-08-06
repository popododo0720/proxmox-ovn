import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import { OverviewPage } from './OverviewPage';

describe('OverviewPage operational health', () => {
  it('renders the component statuses returned by the authenticated health API', async () => {
    const client = {
      health: vi.fn().mockResolvedValue({
        status: 'degraded', cluster: 'lab', version: 'test',
        database: 'ready', ovn_northbound: 'degraded',
        ovn_southbound: 'ready', reconciler: 'unavailable',
        default_security_policy: 'degraded',
        capacity: { ready: true },
      }),
      projects: vi.fn().mockResolvedValue({ items: [] }),
      nodes: vi.fn().mockResolvedValue({ items: [] }),
      operations: vi.fn().mockResolvedValue({ items: [] }),
    } as unknown as ApiClient;

    render(
      <ApiProvider client={client}>
        <OverviewPage />
      </ApiProvider>,
    );

    expect(await screen.findByRole('heading', { name: 'lab' })).toBeInTheDocument();
    expect(screen.getByText('PVN database').parentElement).toHaveTextContent('ready');
    expect(screen.getByText('OVN northbound').parentElement).toHaveTextContent('degraded');
    expect(screen.getByText('OVN southbound').parentElement).toHaveTextContent('ready');
    expect(screen.getByText('Reconciler').parentElement).toHaveTextContent('unavailable');
    expect(screen.getByText('Default security policy').parentElement).toHaveTextContent('degraded');
    expect(screen.queryByText('unknown')).not.toBeInTheDocument();
  });

  it('resolves recent operation targets without exposing their UUIDs', async () => {
    const list = vi.fn().mockResolvedValue({
      items: [{ id: 'network-aaaaaaaa', name: 'application' }],
    });
    const client = {
      health: vi.fn().mockResolvedValue({
        status: 'ready', cluster: 'lab', version: 'test',
        database: 'ready', ovn_northbound: 'ready',
        ovn_southbound: 'ready', reconciler: 'ready',
        default_security_policy: 'ready',
        capacity: { ready: true },
      }),
      projects: vi.fn().mockResolvedValue({ items: [] }),
      nodes: vi.fn().mockResolvedValue({ items: [] }),
      operations: vi.fn().mockResolvedValue({
        items: [{
          id: 'operation-11111111',
          action: 'update',
          target_kind: 'network',
          target_id: 'network-aaaaaaaa',
          status: 'ready',
        }],
      }),
      list,
    } as unknown as ApiClient;

    render(
      <ApiProvider client={client}>
        <OverviewPage />
      </ApiProvider>,
    );

    expect(await screen.findByText('application')).toBeInTheDocument();
    expect(screen.queryByText('network-aaaaaaaa')).not.toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(1);
    expect(list).toHaveBeenCalledWith('/networks');
  });

  it('maps capacity node IDs to names and redacts unknown UUIDs', async () => {
    const nodeID = '9e21e0b5-a40f-4bf8-9fe1-cfcdadbc0f7a';
    const reporterID = 'acbd18db-4cc2-4854-978d-8472f72f8d1b';
    const client = {
      health: vi.fn().mockResolvedValue({
        status: 'degraded', cluster: 'lab', version: 'test',
        database: 'ready', ovn_northbound: 'ready',
        ovn_southbound: 'ready', reconciler: 'ready',
        default_security_policy: 'ready',
        capacity: { ready: false, reason: `node ${nodeID} was not reported by ${reporterID}` },
      }),
      projects: vi.fn().mockResolvedValue({ items: [] }),
      nodes: vi.fn().mockResolvedValue({ items: [{ id: nodeID, name: 'prox3', enabled: true }] }),
      operations: vi.fn().mockResolvedValue({ items: [] }),
    } as unknown as ApiClient;

    render(
      <ApiProvider client={client}>
        <OverviewPage />
      </ApiProvider>,
    );

    const reason = await screen.findByText('node prox3 was not reported by [resource]');
    expect(reason).not.toHaveTextContent(nodeID);
    expect(reason).not.toHaveTextContent(reporterID);
  });
});
