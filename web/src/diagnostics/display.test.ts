import { describe, expect, it } from 'vitest';
import { redactResourceIDs, uiErrorMessage } from './display';

describe('human-readable display text', () => {
  const resourceID = '9e21e0b5-a40f-4bf8-9fe1-cfcdadbc0f7a';

  it('maps a known resource ID to its name before redacting other UUIDs', () => {
    const otherID = 'acbd18db-4cc2-4854-978d-8472f72f8d1b';
    expect(redactResourceIDs(
      `network ${resourceID} depends on ${otherID}`,
      [{ id: resourceID, name: 'application' }],
    )).toBe('network application depends on [resource]');
  });

  it('keeps raw IDs out of bounded UI errors', () => {
    expect(uiErrorMessage(new Error(`could not update ${resourceID}`), 'Update failed'))
      .toBe('could not update [resource]');
  });
});
