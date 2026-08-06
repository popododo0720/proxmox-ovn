import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { ApiError, type ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import {
  FloatingIPsPage,
  NetworksPage,
  OperationsPage,
  PortsPage,
  SecurityGroupsPage,
} from './ResourcePages';

const ids = {
  project: '11111111-1111-4111-8111-111111111111',
  network: '22222222-2222-4222-8222-222222222222',
  subnet: '33333333-3333-4333-8333-333333333333',
  port: '44444444-4444-4444-8444-444444444444',
  provider: '55555555-5555-4555-8555-555555555555',
  router: '66666666-6666-4666-8666-666666666666',
  securityGroup: '77777777-7777-4777-8777-777777777777',
  remoteGroup: '88888888-8888-4888-8888-888888888888',
  routerInterface: '99999999-9999-4999-8999-999999999999',
  rule: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
  floatingIP: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
};

function expectNoResourceIDs(element: HTMLElement) {
  for (const id of Object.values(ids)) expect(element).not.toHaveTextContent(id);
}

describe('human relationship labels', () => {
  it('maps subnet project and network columns and keeps exact selector/detail IDs', async () => {
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/projects') return { items: [{ id: ids.project, name: 'Tenant A' }] };
      if (endpoint === '/networks') return { items: [{ id: ids.network, name: 'application', project_id: ids.project }] };
      if (endpoint === '/subnets') return { items: [{
        id: ids.subnet,
        name: 'application-v4',
        project_id: ids.project,
        network_id: ids.network,
        cidr: '10.42.1.0/24',
        state: 'ready',
      }] };
      return { items: [] };
    });

    render(<ApiProvider client={{ list } as unknown as ApiClient}><NetworksPage /></ApiProvider>);

    const tables = await screen.findAllByRole('table');
    const row = within(tables[1]).getByText('application-v4').closest('tr');
    expect(row).not.toBeNull();
    expect(await within(row!).findByText('Tenant A')).toBeInTheDocument();
    expect(await within(row!).findByText('application')).toBeInTheDocument();
    expectNoResourceIDs(row!);

    fireEvent.click(within(row!).getByRole('button', { name: 'Details' }));
    const details = screen.getByRole('dialog');
    expect(details).toHaveTextContent(ids.subnet);
    expect(details).toHaveTextContent(ids.project);
    expect(details).toHaveTextContent(ids.network);
    fireEvent.click(within(details).getByText('Close', { selector: 'button' }));

    fireEvent.click(screen.getByRole('button', { name: '+ Subnet' }));
    const create = screen.getByRole('dialog');
    const projectOption = within(create).getByRole('option', { name: 'Tenant A' });
    expect(projectOption).toHaveValue(ids.project);
    expect(create).not.toHaveTextContent(ids.project);
    fireEvent.change(within(create).getByRole('combobox', { name: 'Project' }), { target: { value: ids.project } });
    const networkOption = within(create).getByRole('option', { name: 'application' });
    expect(networkOption).toHaveValue(ids.network);
    expect(create).not.toHaveTextContent(ids.network);
  });

  it('maps port project, network, subnet, and security-group relationships', async () => {
    const defaultSecurityGroupBackfillPlan = vi.fn().mockRejectedValue(new ApiError('not authorized', 403));
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/ports') return { items: [{
        id: ids.port,
        name: 'frontend',
        project_id: ids.project,
        network_id: ids.network,
        mac_address: '02:00:00:00:00:11',
        fixed_ips: [{ subnet_id: ids.subnet, address: '10.42.1.11' }],
        security_group_ids: [ids.securityGroup],
        state: 'ready',
      }] };
      if (endpoint === '/projects') return { items: [{ id: ids.project, name: 'Tenant A' }] };
      if (endpoint === '/networks') return { items: [{ id: ids.network, name: 'application', project_id: ids.project }] };
      if (endpoint === '/subnets') return { items: [{ id: ids.subnet, name: 'application-v4', cidr: '10.42.1.0/24' }] };
      if (endpoint === '/security-groups') return { items: [{ id: ids.securityGroup, name: 'web', description: 'Web policy' }] };
      return { items: [] };
    });

    render(
      <ApiProvider client={{ list, defaultSecurityGroupBackfillPlan } as unknown as ApiClient}>
        <PortsPage vmBridge={{ available: false, listQemuVMs: vi.fn() }} />
      </ApiProvider>,
    );

    const row = (await screen.findByText('frontend')).closest('tr');
    expect(row).not.toBeNull();
    expect(await within(row!).findByText('Tenant A')).toBeInTheDocument();
    expect(await within(row!).findByText('application')).toBeInTheDocument();
    expect(await within(row!).findByText('application-v4 · 10.42.1.0/24')).toBeInTheDocument();
    expect(await within(row!).findByText('web · Web policy')).toBeInTheDocument();
    expectNoResourceIDs(row!);

    fireEvent.click(within(row!).getByRole('button', { name: 'Details' }));
    const details = screen.getByRole('dialog');
    for (const id of [ids.port, ids.project, ids.network, ids.subnet, ids.securityGroup]) {
      expect(details).toHaveTextContent(id);
    }
  });

  it('maps every floating-IP relationship while retaining raw IDs in Details', async () => {
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/floating-ips') return { items: [{
        id: ids.floatingIP,
        address: '192.0.2.42',
        provider_network_id: ids.provider,
        project_id: ids.project,
        port_id: ids.port,
        router_id: ids.router,
        fixed_ip_address: '10.42.1.11',
        state: 'ready',
        status: 'active',
      }] };
      if (endpoint === '/provider-networks') return { items: [{ id: ids.provider, name: 'public' }] };
      if (endpoint === '/projects') return { items: [{ id: ids.project, name: 'Tenant A' }] };
      if (endpoint === '/ports') return { items: [{ id: ids.port, name: 'frontend', mac_address: '02:00:00:00:00:11' }] };
      if (endpoint === '/routers') return { items: [{ id: ids.router, name: 'edge', external_ip_address: '192.0.2.1' }] };
      return { items: [] };
    });

    render(<ApiProvider client={{ list } as unknown as ApiClient}><FloatingIPsPage /></ApiProvider>);

    const row = (await screen.findByText('192.0.2.42')).closest('tr');
    expect(row).not.toBeNull();
    for (const label of ['public', 'Tenant A', 'frontend · 02:00:00:00:00:11', 'edge · 192.0.2.1']) {
      expect(await within(row!).findByText(label)).toBeInTheDocument();
    }
    expectNoResourceIDs(row!);

    fireEvent.click(within(row!).getByRole('button', { name: 'Details' }));
    const details = screen.getByRole('dialog');
    for (const id of [ids.floatingIP, ids.provider, ids.project, ids.port, ids.router]) {
      expect(details).toHaveTextContent(id);
    }
  });

  it('maps security-rule project, policy, and remote-policy relationships', async () => {
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/security-groups') return { items: [
        { id: ids.securityGroup, name: 'web', description: 'Web policy', project_id: ids.project },
        { id: ids.remoteGroup, name: 'database', description: 'Database policy', project_id: ids.project },
      ] };
      if (endpoint === '/security-group-rules') return { items: [{
        id: ids.rule,
        project_id: ids.project,
        security_group_id: ids.securityGroup,
        remote_group_id: ids.remoteGroup,
        direction: 'ingress',
        ethertype: 'IPv4',
        protocol: 'tcp',
        port_range_min: 5432,
        port_range_max: 5432,
        action: 'allow',
        state: 'ready',
      }] };
      if (endpoint === '/projects') return { items: [{ id: ids.project, name: 'Tenant A' }] };
      return { items: [] };
    });

    render(<ApiProvider client={{ list } as unknown as ApiClient}><SecurityGroupsPage /></ApiProvider>);

    const tables = await screen.findAllByRole('table');
    const row = within(tables[1]).getAllByText('5432', { selector: 'td' })[0].closest('tr');
    expect(row).not.toBeNull();
    for (const label of ['web · Web policy', 'Tenant A', 'database · Database policy']) {
      expect(await within(row!).findByText(label)).toBeInTheDocument();
    }
    expectNoResourceIDs(row!);

    fireEvent.click(within(row!).getByRole('button', { name: 'Details' }));
    const details = screen.getByRole('dialog');
    for (const id of [ids.rule, ids.project, ids.securityGroup, ids.remoteGroup]) {
      expect(details).toHaveTextContent(id);
    }
  });

  it('distinguishes router-interface and security-rule operation targets by composite names', async () => {
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/operations') return { items: [
        { id: 'operation-router', action: 'attach', target_kind: 'router-interface', target_id: ids.routerInterface, status: 'succeeded' },
        { id: 'operation-rule', action: 'create', target_kind: 'security-group-rule', target_id: ids.rule, status: 'succeeded' },
      ] };
      if (endpoint === '/router-interfaces') return { items: [{ id: ids.routerInterface, router_id: ids.router, subnet_id: ids.subnet }] };
      if (endpoint === '/routers') return { items: [{ id: ids.router, name: 'edge', external_ip_address: '192.0.2.1' }] };
      if (endpoint === '/subnets') return { items: [{ id: ids.subnet, name: 'application-v4', cidr: '10.42.1.0/24' }] };
      if (endpoint === '/security-group-rules') return { items: [{
        id: ids.rule,
        security_group_id: ids.securityGroup,
        remote_group_id: ids.remoteGroup,
        direction: 'ingress',
        protocol: 'tcp',
        port_range_min: 443,
        port_range_max: 443,
      }] };
      if (endpoint === '/security-groups') return { items: [
        { id: ids.securityGroup, name: 'web', description: 'Web policy' },
        { id: ids.remoteGroup, name: 'client', description: 'Client policy' },
      ] };
      return { items: [] };
    });

    render(<ApiProvider client={{ list } as unknown as ApiClient}><OperationsPage /></ApiProvider>);

    const routerRow = (await screen.findByText('attach')).closest('tr');
    const ruleRow = screen.getByText('create').closest('tr');
    expect(routerRow).not.toBeNull();
    expect(ruleRow).not.toBeNull();
    expect(await within(routerRow!).findByText('edge · 192.0.2.1')).toBeInTheDocument();
    expect(await within(routerRow!).findByText('application-v4 · 10.42.1.0/24')).toBeInTheDocument();
    expect(await within(ruleRow!).findByText('web · Web policy')).toBeInTheDocument();
    expect(within(ruleRow!).getByText('ingress · tcp · port 443')).toBeInTheDocument();
    expect(await within(ruleRow!).findByText('client · Client policy')).toBeInTheDocument();
    expectNoResourceIDs(routerRow!);
    expectNoResourceIDs(ruleRow!);

    fireEvent.click(within(routerRow!).getByRole('button', { name: 'Details' }));
    expect(screen.getByRole('dialog')).toHaveTextContent(ids.routerInterface);
  });
});
