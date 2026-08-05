import { describe, expect, it } from 'vitest';
import { readPveBridgeBootstrap } from './bridge';

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
