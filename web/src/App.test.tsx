import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import App from './App';
import type { ApiClient } from './api/client';
import { ApiProvider } from './api/context';

function client(): ApiClient {
  return {
    bootstrap: vi.fn().mockResolvedValue({
      user: 'root@pam',
      csrf_token: 'token',
      permissions: ['PVN.Audit'],
      cluster: 'lab',
    }),
    list: vi.fn().mockResolvedValue({ items: [] }),
  } as unknown as ApiClient;
}

function renderApp() {
  render(
    <ApiProvider client={client()}>
      <App />
    </ApiProvider>,
  );
}

afterEach(() => {
  window.history.replaceState({}, '', '/');
});

describe('App navigation modes', () => {
  it('keeps the PVN sidebar when the manager is opened directly', async () => {
    window.history.replaceState({}, '', '/#/projects');

    renderApp();

    expect(await screen.findByRole('heading', { name: 'Projects' })).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: 'PVN sections' })).toBeInTheDocument();
  });

  it('uses the loader route and removes duplicate navigation when embedded', async () => {
    window.history.replaceState({}, '', '/?embedded=1&route=projects');

    renderApp();

    const heading = await screen.findByRole('heading', { name: 'Projects' });
    expect(heading.closest('.app-shell')).toHaveClass('app-shell-embedded');
    expect(screen.queryByRole('navigation', { name: 'PVN sections' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Open navigation' })).not.toBeInTheDocument();
  });
});
