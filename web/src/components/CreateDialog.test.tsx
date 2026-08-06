import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import { CreateDialog, type FormField } from './CreateDialog';

describe('CreateDialog resource references', () => {
  it('submits exact single and multiple resource IDs', async () => {
    const list = vi.fn(async (endpoint: string) => ({
      items: endpoint === '/projects'
        ? [{ id: 'project-1', name: 'tenant-a' }]
        : [
          { id: 'sg-1', name: 'web', project_id: 'project-1' },
          { id: 'sg-2', name: 'ssh', project_id: 'project-1' },
        ],
    }));
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    const fields: FormField[] = [
      {
        name: 'project_id',
        label: 'Project',
        type: 'resource-select',
        required: true,
        reference: { endpoint: '/projects' },
      },
      {
        name: 'security_group_ids',
        label: 'Security groups',
        type: 'resource-select',
        multiple: true,
        reference: { endpoint: '/security-groups', matches: [{ formField: 'project_id' }] },
      },
    ];

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <CreateDialog title="Tenant port" fields={fields} open onClose={() => undefined} onSubmit={onSubmit} />
      </ApiProvider>,
    );

    await screen.findByRole('option', { name: 'tenant-a' });
    fireEvent.change(screen.getByRole('combobox', { name: 'Project' }), { target: { value: 'project-1' } });
    await screen.findByRole('option', { name: 'web' });

    const securityGroups = screen.getByRole('listbox', { name: 'Security groups' }) as HTMLSelectElement;
    for (const option of Array.from(securityGroups.options)) {
      option.selected = option.value === 'sg-1' || option.value === 'sg-2';
    }
    fireEvent.change(securityGroups);
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith({
      project_id: 'project-1',
      security_group_ids: ['sg-1', 'sg-2'],
    }));
  });

  it('omits an empty optional multi-select so the server can apply its default', async () => {
    const list = vi.fn(async () => ({ items: [] }));
    const onSubmit = vi.fn().mockResolvedValue(undefined);

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <CreateDialog
          title="Tenant port"
          fields={[{
            name: 'security_group_ids',
            label: 'Security groups',
            type: 'resource-select',
            multiple: true,
            reference: { endpoint: '/security-groups' },
          }]}
          open
          onClose={() => undefined}
          onSubmit={onSubmit}
        />
      </ApiProvider>,
    );

    await screen.findByRole('listbox', { name: 'Security groups' });
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith({}));
  });
});
