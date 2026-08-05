import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import { ResourceSelect } from './ResourceSelect';

describe('ResourceSelect', () => {
  it('shows human-readable project-scoped choices while submitting the exact ID', async () => {
    const list = vi.fn().mockResolvedValue({
      items: [
        { id: 'network-aaaaaaaa', name: 'application', project_id: 'project-a' },
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

    const option = await screen.findByRole('option', { name: 'application · network-aaaaaaaa' });
    expect(screen.queryByRole('option', { name: /database/ })).not.toBeInTheDocument();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'network-aaaaaaaa' } });

    expect(option).toHaveValue('network-aaaaaaaa');
    expect(onChange).toHaveBeenCalledWith('network-aaaaaaaa');
    expect(list).toHaveBeenCalledWith('/networks');
  });

  it('reports load failures and retries the same source', async () => {
    const list = vi.fn()
      .mockRejectedValueOnce(new Error('manager unavailable'))
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

    expect(await screen.findByRole('alert')).toHaveTextContent('manager unavailable');
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));

    await screen.findByRole('option', { name: 'tenant-a · project-1' });
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
    expect(screen.getByRole('option', { name: 'provider-exact-id · current value unavailable' })).toBeInTheDocument();
    resolveList?.({ items: [{ id: 'provider-exact-id', name: 'public' }] });

    await screen.findByRole('option', { name: 'public · provider-exact-id' });
    expect(screen.getByRole('combobox')).toHaveValue('provider-exact-id');
  });
});
