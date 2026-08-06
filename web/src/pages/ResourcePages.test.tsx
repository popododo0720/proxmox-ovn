import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import type { FloatingIP } from '../api/types';
import {
  floatingIPDisplayStatus,
  OperationsPage,
  PortsPage,
  SecurityGroupsPage,
} from './ResourcePages';

describe('floatingIPDisplayStatus', () => {
  it('shows control-plane transitions before the realized floating status', () => {
    const base: FloatingIP = { id: 'fip-1', status: 'down' };

    expect(floatingIPDisplayStatus({ ...base, state: 'pending' })).toBe('pending');
    expect(floatingIPDisplayStatus({ ...base, state: 'deleting', status: 'active' })).toBe('deleting');
    expect(floatingIPDisplayStatus({ ...base, state: 'error' })).toBe('error');
  });

  it('shows active and down after successful reconciliation', () => {
    expect(floatingIPDisplayStatus({ id: 'fip-active', state: 'ready', status: 'active' })).toBe('active');
    expect(floatingIPDisplayStatus({ id: 'fip-reserved', state: 'ready', status: 'down' })).toBe('down');
  });
});

describe('resource reference UX', () => {
  it('resolves polymorphic operation targets and handles an unknown kind neutrally', async () => {
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/operations') {
        return { items: [
          { id: 'operation-known', action: 'update', target_kind: 'network', target_id: 'network-aaaaaaaa', status: 'ready' },
          { id: 'operation-unknown', action: 'inspect', target_kind: 'future-kind', target_id: 'future-aaaaaaaa', status: 'ready' },
        ] };
      }
      if (endpoint === '/networks') return { items: [{ id: 'network-aaaaaaaa', name: 'application' }] };
      return { items: [] };
    });

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <OperationsPage />
      </ApiProvider>,
    );

    const knownRow = (await screen.findByText('update')).closest('tr');
    const unknownRow = screen.getByText('inspect').closest('tr');
    expect(knownRow).not.toBeNull();
    expect(unknownRow).not.toBeNull();
    expect(await within(knownRow!).findByText('application')).toBeInTheDocument();
    expect(within(knownRow!).queryByText('network-aaaaaaaa')).not.toBeInTheDocument();
    expect(within(unknownRow!).getByText('Unavailable')).toBeInTheDocument();
    expect(within(unknownRow!).queryByText('future-aaaaaaaa')).not.toBeInTheDocument();
    expect(list.mock.calls.filter(([endpoint]) => endpoint === '/networks')).toHaveLength(1);

    fireEvent.click(within(unknownRow!).getByRole('button', { name: 'Details' }));
    expect(within(screen.getByRole('dialog')).getByText('future-aaaaaaaa')).toBeInTheDocument();
  });

  it('shares port and node endpoint loads across tables, references, and attachment selects', async () => {
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/ports') return { items: [{
        id: 'port-aaaaaaaa',
        name: 'web-01',
        network_id: 'network-aaaaaaaa',
        node_id: 'node-aaaaaaaa',
        vmid: 100,
        nic: 'net0',
        requested_chassis: 'chassis-a',
        binding_status: 'bound',
        admin_state_up: true,
        security_group_ids: [],
        state: 'ready',
        revision: 3,
        applied_revision: 3,
      }] };
      if (endpoint === '/nodes') return { items: [{ id: 'node-aaaaaaaa', name: 'pve-a', chassis_id: 'chassis-a', enabled: true, state: 'ready' }] };
      if (endpoint === '/networks') return { items: [{ id: 'network-aaaaaaaa', name: 'application' }] };
      return { items: [] };
    });

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <PortsPage />
      </ApiProvider>,
    );

    const table = await screen.findByRole('table');
    expect(await within(table).findByText('application')).toBeInTheDocument();
    expect(await within(table).findByText('pve-a')).toBeInTheDocument();
    expect(await within(table).findByText('matched')).toBeInTheDocument();
    expect(within(table).getByText('The local agent confirmed the OVN binding.')).toBeInTheDocument();
    expect(within(table).getByText('No security groups; traffic is unrestricted by PVN policy.')).toBeInTheDocument();
    expect(table).not.toHaveTextContent('port-aaaaaaaa');
    expect(table).not.toHaveTextContent('node-aaaaaaaa');
    expect(table).not.toHaveTextContent('chassis-a');
    expect(list.mock.calls.filter(([endpoint]) => endpoint === '/ports')).toHaveLength(1);
    expect(list.mock.calls.filter(([endpoint]) => endpoint === '/nodes')).toHaveLength(1);
    expect(list.mock.calls.filter(([endpoint]) => endpoint === '/networks')).toHaveLength(1);
  });
});

describe('security group statefulness', () => {
  it('explains stateful-only behavior and does not offer a writable Stateful field', async () => {
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/security-groups') return { items: [{
        id: 'security-group-aaaaaaaa',
        name: 'web',
        project_id: 'project-aaaaaaaa',
        stateful: true,
      }] };
      if (endpoint === '/projects') return { items: [{ id: 'project-aaaaaaaa', name: 'Tenant A' }] };
      return { items: [] };
    });

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <SecurityGroupsPage />
      </ApiProvider>,
    );

    expect(screen.getByText(/Stateful-only OVN port-group policies/)).toBeInTheDocument();
    const table = await screen.findByRole('table');
    await within(table).findByText('web');
    fireEvent.click(screen.getByRole('button', { name: '+ Security group' }));
    expect(screen.queryByRole('checkbox', { name: 'Stateful' })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));

    fireEvent.click(screen.getByRole('button', { name: 'Edit' }));
    expect(screen.queryByRole('checkbox', { name: 'Stateful' })).not.toBeInTheDocument();
  });
});
