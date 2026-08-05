import { describe, expect, it } from 'vitest';
import { firstFreeNIC } from './PortAttachmentPanel';

describe('firstFreeNIC', () => {
  it('chooses the first unused PVE NIC index', () => {
    expect(firstFreeNIC({ net0: 'configured', net2: 'configured' })).toBe('net1');
    expect(firstFreeNIC({ net0: 'configured', net1: 'configured' }, 2)).toBeNull();
  });
});
