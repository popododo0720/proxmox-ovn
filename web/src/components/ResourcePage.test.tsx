import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiProvider } from '../api/context';
import { ResourcePage } from './ResourcePage';

describe('ResourcePage resource IDs', () => {
  it('adds the full resource ID to every table and copies it', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const list = vi.fn().mockResolvedValue({ items: [{ id: 'network-12345678', name: 'application' }] });

    try {
      render(
        <ApiProvider client={{ list } as unknown as ApiClient}>
          <ResourcePage
            title="Networks"
            description="Tenant networks"
            endpoint="/networks"
            columns={[{ key: 'name', label: 'Network' }]}
          />
        </ApiProvider>,
      );

      expect(await screen.findByRole('columnheader', { name: 'ID' })).toBeInTheDocument();
      expect(screen.getByText('network-12345678')).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'Copy resource ID network-12345678' }));
      await waitFor(() => expect(writeText).toHaveBeenCalledWith('network-12345678'));
      expect(screen.getByTitle('Copied')).toBeInTheDocument();
    } finally {
      if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
      else Reflect.deleteProperty(navigator, 'clipboard');
    }
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
