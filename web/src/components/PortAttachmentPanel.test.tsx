import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import { firstFreeNIC, PortAttachmentPanel } from './PortAttachmentPanel';

describe('firstFreeNIC', () => {
  it('chooses the first unused PVE NIC index', () => {
    expect(firstFreeNIC({ net0: 'configured', net2: 'configured' })).toBe('net1');
    expect(firstFreeNIC({ net0: 'configured', net1: 'configured' }, 2)).toBeNull();
  });

  it('uses human port and node labels while retaining exact option values', async () => {
    const list = vi.fn(async (endpoint: string) => endpoint === '/ports'
      ? { items: [{ id: 'port-aaaaaaaa', mac_address: '02:00:00:00:00:08', binding_status: 'unbound' }] }
      : { items: [{ id: 'node-bbbbbbbb', management_address: '192.0.2.10', enabled: true }] });

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <PortAttachmentPanel />
      </ApiProvider>,
    );

    const portOption = await screen.findByRole('option', { name: '02:00:00:00:00:08 · 02:00:00:00:00:08 · unbound' });
    const nodeOption = await screen.findByRole('option', { name: '192.0.2.10' });
    expect(portOption).toHaveValue('port-aaaaaaaa');
    expect(nodeOption).toHaveValue('node-bbbbbbbb');
    expect(screen.queryByText('port-aaaaaaaa')).not.toBeInTheDocument();
    expect(screen.queryByText('node-bbbbbbbb')).not.toBeInTheDocument();
    expect(list.mock.calls.filter(([endpoint]) => endpoint === '/ports')).toHaveLength(1);
    expect(list.mock.calls.filter(([endpoint]) => endpoint === '/nodes')).toHaveLength(1);
  });
});
