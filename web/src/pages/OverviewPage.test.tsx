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
    expect(screen.queryByText('unknown')).not.toBeInTheDocument();
  });
});
