import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import type { PortLifecycleBridge } from '../pve/portLifecycle';
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

  it('combines human VM, node, binding, and policy evidence during live inspection', async () => {
    const list = vi.fn(async (endpoint: string) => endpoint === '/ports'
      ? { items: [{
        id: 'port-aaaaaaaa',
        name: 'web-01',
        mac_address: '02:00:00:00:00:08',
        security_group_ids: [],
        admin_state_up: true,
        node_id: 'node-bbbbbbbb',
        vmid: 100,
        nic: 'net0',
        requested_chassis: 'chassis-a',
        binding_status: 'bound',
        state: 'ready',
        revision: 3,
        applied_revision: 3,
      }] }
      : { items: [{
        id: 'node-bbbbbbbb',
        name: 'pve-a',
        chassis_id: 'chassis-a',
        enabled: true,
        state: 'ready',
      }] });
    const resolveRuntimePort = vi.fn().mockResolvedValue({
      port_id: 'port-aaaaaaaa',
      lsp_name: 'lsp-hidden',
      mac_address: '02:00:00:00:00:08',
      generation: 3,
      requested_chassis: 'chassis-a',
      status: 'bound',
    });
    const bridge: PortLifecycleBridge & { readonly available: boolean } = {
      available: true,
      getQemuConfig: vi.fn().mockResolvedValue({
        name: 'frontend',
        net0: 'virtio=02:00:00:00:00:08,bridge=br-int,firewall=0,link_down=1',
      }),
      getQemuStatus: vi.fn().mockResolvedValue({ status: 'running' }),
      setQemuNic: vi.fn().mockResolvedValue(undefined),
      deleteQemuNic: vi.fn().mockResolvedValue(undefined),
    };

    render(
      <ApiProvider client={{ list, resolveRuntimePort } as unknown as ApiClient}>
        <PortAttachmentPanel bridge={bridge} />
      </ApiProvider>,
    );

    const inspect = await screen.findByRole('button', { name: 'Inspect' });
    await waitFor(() => expect(inspect).toBeEnabled());
    fireEvent.click(inspect);

    expect(await screen.findByText('frontend (VM 100)')).toBeInTheDocument();
    expect(screen.getByText('OVN reports bound, but the VM NIC remains link-down.')).toBeInTheDocument();
    expect(screen.getByText('Legacy unrestricted port: no security group is attached. Migrate it with the default security-group backfill.')).toBeInTheDocument();
    expect(resolveRuntimePort).toHaveBeenCalledWith('pve-a', 100, 'net0');
    expect(screen.queryByText('port-aaaaaaaa')).not.toBeInTheDocument();
    expect(screen.queryByText('node-bbbbbbbb')).not.toBeInTheDocument();
    expect(screen.queryByText('chassis-a')).not.toBeInTheDocument();
  });
});
