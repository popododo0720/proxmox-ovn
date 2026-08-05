import { describe, expect, it } from 'vitest';
import type { FloatingIP } from '../api/types';
import { floatingIPDisplayStatus } from './ResourcePages';

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
