import { describe, expect, it } from 'vitest';
import { normalizeQemuVMs, readPveBridgeBootstrap } from './bridge';

const nonce = 'a'.repeat(64);

describe('readPveBridgeBootstrap', () => {
  it('accepts a nonce only when the declared PVE origin matches the referrer', () => {
    const search = `?pveBridgeNonce=${nonce}&pveOrigin=${encodeURIComponent('https://pve.example:8006')}`;
    expect(readPveBridgeBootstrap({ search } as Location, 'https://pve.example:8006/')).toEqual({
      nonce,
      origin: 'https://pve.example:8006',
    });
  });

  it('rejects an origin mismatch, non-HTTPS origins, and weak nonces', () => {
    const validSearch = `?pveBridgeNonce=${nonce}&pveOrigin=${encodeURIComponent('https://pve.example:8006')}`;
    expect(readPveBridgeBootstrap({ search: validSearch } as Location, 'https://other.example:8006/')).toBeNull();
    expect(readPveBridgeBootstrap({ search: `?pveBridgeNonce=${nonce}&pveOrigin=http://pve.example:8006` } as Location, 'http://pve.example:8006/')).toBeNull();
    expect(readPveBridgeBootstrap({ search: '?pveBridgeNonce=short&pveOrigin=https://pve.example:8006' } as Location, 'https://pve.example:8006/')).toBeNull();
  });
});

describe('normalizeQemuVMs', () => {
  it('keeps only safe QEMU identity fields used for human labels', () => {
    expect(normalizeQemuVMs([
      { type: 'qemu', vmid: 100, name: ' frontend ', node: 'prox1', id: 'qemu/100' },
      { type: 'qemu', vmid: 101, node: 'prox2' },
      { type: 'lxc', vmid: 102, name: 'container', node: 'prox3' },
      { type: 'qemu', vmid: 103, name: 'unsafe', node: '../prox1' },
    ])).toEqual([
      { vmid: 100, name: 'frontend', node: 'prox1' },
      { vmid: 101, node: 'prox2' },
    ]);
  });
});
