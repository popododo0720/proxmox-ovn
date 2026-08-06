import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import { ResourceSelect } from './ResourceSelect';

describe('ResourceSelect', () => {
  it('shows human-readable project-scoped choices while submitting the exact ID', async () => {
    const networkID = '9e21e0b5-a40f-4bf8-9fe1-cfcdadbc0f7a';
    const list = vi.fn().mockResolvedValue({
      items: [
        { id: networkID, name: 'application', project_id: 'project-a' },
        { id: 'network-bbbbbbbb', name: 'database', project_id: 'project-b' },
      ],
    });
    const onChange = vi.fn();

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <ResourceSelect
          id="network"
          name="network_id"
          active
          required
          source={{ endpoint: '/networks', matches: [{ formField: 'project_id' }] }}
          formValues={{ project_id: 'project-a' }}
          onChange={onChange}
        />
      </ApiProvider>,
    );

    const option = await screen.findByRole('option', { name: 'application' });
    expect(screen.queryByRole('option', { name: /database/ })).not.toBeInTheDocument();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: networkID } });

    expect(option).toHaveValue(networkID);
    expect(screen.getByRole('combobox')).not.toHaveTextContent(networkID);
    expect(onChange).toHaveBeenCalledWith(networkID);
    expect(list).toHaveBeenCalledWith('/networks');
  });

  it('reports load failures and retries the same source', async () => {
    const operationID = 'acbd18db-4cc2-4854-978d-8472f72f8d1b';
    const list = vi.fn()
      .mockRejectedValueOnce(new Error(`manager unavailable: ${operationID}`))
      .mockResolvedValueOnce({ items: [{ id: 'project-1', name: 'tenant-a' }] });

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <ResourceSelect
          id="project"
          name="project_id"
          active
          source={{ endpoint: '/projects' }}
        />
      </ApiProvider>,
    );

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('manager unavailable: [resource]');
    expect(alert).not.toHaveTextContent(operationID);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));

    await screen.findByRole('option', { name: 'tenant-a' });
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  });

  it('preserves an exact edit value while its human-readable option loads', async () => {
    let resolveList: ((value: { items: Array<{ id: string; name: string }> }) => void) | undefined;
    const list = vi.fn().mockReturnValue(new Promise((resolve) => { resolveList = resolve; }));

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <ResourceSelect
          id="provider"
          name="provider_network_id"
          active
          source={{ endpoint: '/provider-networks' }}
          defaultValue="provider-exact-id"
        />
      </ApiProvider>,
    );

    expect(screen.getByRole('combobox')).toHaveValue('provider-exact-id');
    expect(screen.getByRole('option', { name: 'Current value unavailable' })).toBeInTheDocument();
    expect(screen.queryByText('provider-exact-id')).not.toBeInTheDocument();
    resolveList?.({ items: [{ id: 'provider-exact-id', name: 'public' }] });

    await screen.findByRole('option', { name: 'public' });
    expect(screen.getByRole('combobox')).toHaveValue('provider-exact-id');
  });

  it('coalesces identical endpoint loads across multiple selectors', async () => {
    const list = vi.fn().mockResolvedValue({
      items: [{ id: 'project-1', name: 'Tenant A' }],
    });

    render(
      <ApiProvider client={{ list } as unknown as ApiClient}>
        <ResourceSelect id="project-a" name="project_a" active source={{ endpoint: '/projects' }} />
        <ResourceSelect id="project-b" name="project_b" active source={{ endpoint: '/projects' }} />
      </ApiProvider>,
    );

    expect(await screen.findAllByRole('option', { name: 'Tenant A' })).toHaveLength(2);
    expect(list).toHaveBeenCalledTimes(1);
    expect(list).toHaveBeenCalledWith('/projects');
  });
});
