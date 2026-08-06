import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import { ResourcePage } from './ResourcePage';

describe('ResourcePage resource identity', () => {
  it('shows no raw UUID in the table but preserves and copies it in Details', async () => {
    const resourceID = '9e21e0b5-a40f-4bf8-9fe1-cfcdadbc0f7a';
    const projectID = 'acbd18db-4cc2-4854-978d-8472f72f8d1b';
    const writeText = vi.fn().mockResolvedValue(undefined);
    const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const list = vi.fn(async (endpoint: string) => endpoint === '/projects'
      ? { items: [{ id: projectID, name: 'Tenant A' }] }
      : { items: [{ id: resourceID, name: 'application', project_id: projectID }] });

    try {
      render(
        <ApiProvider client={{ list } as unknown as ApiClient}>
          <ResourcePage
            title="Networks"
            description="Tenant networks"
            endpoint="/networks"
            columns={[
              { key: 'name', label: 'Network' },
              { key: 'project_id', label: 'Project', reference: { endpoint: '/projects' } },
            ]}
          />
        </ApiProvider>,
      );

      const table = await screen.findByRole('table');
      expect(await within(table).findByText('Tenant A')).toBeInTheDocument();
      expect(table).not.toHaveTextContent(resourceID);
      expect(table).not.toHaveTextContent(projectID);

      fireEvent.click(within(table).getByRole('button', { name: 'Details' }));
      const dialog = screen.getByRole('dialog');
      expect(dialog).toHaveTextContent(resourceID);
      expect(dialog).toHaveTextContent(projectID);
      fireEvent.click(within(dialog).getByRole('button', { name: `Copy resource ID ${projectID}` }));
      await waitFor(() => expect(writeText).toHaveBeenCalledWith(projectID));
    } finally {
      if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
      else Reflect.deleteProperty(navigator, 'clipboard');
    }
  });

  it('shows cached human references in the table and keeps full IDs in Details', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/projects') return { items: [{ id: 'project-12345678', name: 'Tenant A' }] };
      return {
        items: [
          { id: 'network-11111111', name: 'application', project_id: 'project-12345678' },
          { id: 'network-22222222', name: 'database', project_id: 'project-12345678' },
          {
            id: 'network-33333333',
            name: 'orphan',
            project_id: 'project-deleted',
            security_group_ids: ['security-group-11111111', 'security-group-22222222'],
            fixed_ips: [{ subnet_id: 'subnet-11111111', address: '10.0.0.8' }],
          },
        ],
      };
    });

    try {
      render(
        <ApiProvider client={{ list } as unknown as ApiClient}>
          <ResourcePage
            title="Networks"
            description="Tenant networks"
            endpoint="/networks"
            columns={[
              { key: 'name', label: 'Network' },
              { key: 'project_id', label: 'Project', reference: { endpoint: '/projects' } },
            ]}
          />
        </ApiProvider>,
      );

      expect(await screen.findAllByText('Tenant A')).toHaveLength(2);
      expect(screen.getByText('Unavailable')).toBeInTheDocument();
      expect(screen.queryByRole('columnheader', { name: 'ID' })).not.toBeInTheDocument();
      const table = screen.getByRole('table');
      expect(within(table).queryByText('network-11111111')).not.toBeInTheDocument();
      expect(within(table).queryByText('project-12345678')).not.toBeInTheDocument();
      expect(within(table).queryByText('project-deleted')).not.toBeInTheDocument();
      expect(list.mock.calls.filter(([endpoint]) => endpoint === '/projects')).toHaveLength(1);

      const orphanRow = screen.getByText('orphan').closest('tr');
      expect(orphanRow).not.toBeNull();
      fireEvent.click(within(orphanRow!).getByRole('button', { name: 'Details' }));

      const dialog = screen.getByRole('dialog');
      expect(within(dialog).getByText('network-33333333')).toBeInTheDocument();
      expect(within(dialog).getByText('project-deleted')).toBeInTheDocument();
      expect(within(dialog).getByText('security-group-11111111')).toBeInTheDocument();
      expect(within(dialog).getByText('security-group-22222222')).toBeInTheDocument();
      expect(within(dialog).getByText('subnet-11111111')).toBeInTheDocument();
      fireEvent.click(within(dialog).getByRole('button', { name: 'Copy resource ID project-deleted' }));
      await waitFor(() => expect(writeText).toHaveBeenCalledWith('project-deleted'));
      expect(within(dialog).getByTitle('Copied')).toBeInTheDocument();
      fireEvent.click(within(dialog).getByRole('button', { name: 'Copy resource ID security-group-11111111' }));
      await waitFor(() => expect(writeText).toHaveBeenLastCalledWith('security-group-11111111'));
    } finally {
      if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
      else Reflect.deleteProperty(navigator, 'clipboard');
    }
  });

  it('keeps a forbidden reference neutral until Details is opened', async () => {
    const list = vi.fn(async (endpoint: string) => {
      if (endpoint === '/projects') throw new Error('forbidden');
      return { items: [{ id: 'network-private', name: 'private', project_id: 'project-forbidden' }] };
    });

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <ResourcePage
          title="Networks"
          description="Tenant networks"
          endpoint="/networks"
          columns={[
            { key: 'name', label: 'Network' },
            { key: 'project_id', label: 'Project', reference: { endpoint: '/projects' } },
          ]}
        />
      </ApiProvider>,
    );

    const row = (await screen.findByText('private')).closest('tr');
    expect(row).not.toBeNull();
    expect(await within(row!).findByText('Unavailable')).toBeInTheDocument();
    expect(within(row!).queryByText('project-forbidden')).not.toBeInTheDocument();

    fireEvent.click(within(row!).getByRole('button', { name: 'Details' }));
    expect(within(screen.getByRole('dialog')).getByText('project-forbidden')).toBeInTheDocument();
  });

  it('maps a failed action ID to the resource name and hides unrelated UUIDs', async () => {
    const resourceID = '9e21e0b5-a40f-4bf8-9fe1-cfcdadbc0f7a';
    const operationID = 'acbd18db-4cc2-4854-978d-8472f72f8d1b';
    const list = vi.fn().mockResolvedValue({ items: [{ id: resourceID, name: 'application', revision: 2 }] });
    const remove = vi.fn().mockRejectedValue(new Error(`delete ${resourceID} failed in ${operationID}`));
    vi.spyOn(window, 'confirm').mockReturnValue(true);

    render(
      <ApiProvider client={{ list, remove } as unknown as ApiClient}>
        <ResourcePage
          title="Networks"
          description="Tenant networks"
          endpoint="/networks"
          columns={[{ key: 'name', label: 'Network' }]}
          allowDelete
        />
      </ApiProvider>,
    );

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('delete application failed in [resource]');
    expect(alert).not.toHaveTextContent(resourceID);
    expect(alert).not.toHaveTextContent(operationID);
  });

  it('edits a safe field with the complete resource and optimistic revision', async () => {
    const item = {
      id: 'provider-1',
      name: 'public',
      description: 'old description',
      shared: true,
      revision: 7,
      state: 'ready',
    };
    const list = vi.fn().mockResolvedValue({ items: [item] });
    const update = vi.fn().mockResolvedValue({ ...item, description: 'new description', revision: 8 });

    render(
      <ApiProvider client={{ list, update } as unknown as ApiClient}>
        <ResourcePage
          title="Provider networks"
          description="External networks"
          endpoint="/provider-networks"
          columns={[{ key: 'name', label: 'Network' }]}
          editFields={[{ name: 'description', label: 'Description' }]}
        />
      </ApiProvider>,
    );

    fireEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    const description = screen.getByRole('textbox', { name: 'Description' });
    expect(description).toHaveValue('old description');
    fireEvent.change(description, { target: { value: 'new description' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(
      '/provider-networks',
      'provider-1',
      { ...item, description: 'new description' },
      7,
    ));
  });
});
