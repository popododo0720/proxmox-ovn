import { describe, expect, it } from 'vitest';
import type { NodeStatus, Port } from '../api/types';
import { diagnosePort, observeVM, vmDisplayName } from './port';

const port: Port = {
  id: '11111111-1111-4111-8111-111111111111',
  name: 'web-01',
  mac_address: '02:00:00:00:00:01',
  security_group_ids: ['security-group-id'],
  admin_state_up: true,
  node_id: '22222222-2222-4222-8222-222222222222',
  requested_chassis: 'chassis-a',
  vmid: 100,
  nic: 'net0',
  binding_status: 'bound',
  state: 'ready',
  revision: 3,
  applied_revision: 3,
};

const node: NodeStatus = {
  id: '22222222-2222-4222-8222-222222222222',
  name: 'pve-a',
  chassis_id: 'chassis-a',
  enabled: true,
  state: 'ready',
};

describe('port diagnostics', () => {
  it('explains a healthy control-plane binding without exposing identifiers', () => {
    expect(diagnosePort(port, node)).toMatchObject({
      chassis: 'matched',
      reason: 'The local agent confirmed the OVN binding.',
      tone: 'good',
      warnings: [],
    });
  });

  it('marks an empty security-group list as legacy unrestricted and directs backfill', () => {
    const diagnostic = diagnosePort({ ...port, security_group_ids: undefined }, node);
    expect(diagnostic.warnings).toEqual([
      'Legacy unrestricted port: no security group is attached. Migrate it with the default security-group backfill.',
    ]);
  });

  it('distinguishes binding progress from a bound NIC left link-down', () => {
    const binding = diagnosePort(
      { ...port, binding_status: 'binding' },
      node,
      { nicPresent: true, linkDown: true, bridge: 'br-int', macMatches: true, runtime: 'matched', powerState: 'running' },
    );
    expect(binding.reason).toMatch(/fail-closed/);
    expect(binding.tone).toBe('warning');

    const stuck = diagnosePort(
      port,
      node,
      { nicPresent: true, linkDown: true, bridge: 'br-int', macMatches: true, runtime: 'matched', powerState: 'running' },
    );
    expect(stuck.reason).toBe('OVN reports bound, but the VM NIC remains link-down.');
    expect(stuck.tone).toBe('bad');
  });

  it('detects chassis mismatch and redacts raw IDs from errors', () => {
    const mismatch = diagnosePort({ ...port, requested_chassis: 'chassis-b' }, node);
    expect(mismatch).toMatchObject({ chassis: 'mismatch', tone: 'bad' });
    expect(mismatch.reason).not.toContain('chassis-a');
    expect(mismatch.reason).not.toContain('chassis-b');

    const failed = diagnosePort({
      ...port,
      state: 'error',
      last_error: `failed to reconcile ${port.id} on ${node.id}`,
    }, node);
    expect(failed.reason).toBe('failed to reconcile [resource ID] on [resource ID]');
  });

  it('extracts a human VM name and live NIC evidence', () => {
    const observation = observeVM(port, {
      name: 'frontend',
      net0: 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=0',
    }, { status: 'running' }, { runtime: 'matched' });

    expect(observation).toMatchObject({
      name: 'frontend',
      powerState: 'running',
      nicPresent: true,
      linkDown: false,
      bridge: 'br-int',
      macMatches: true,
      runtime: 'matched',
    });
    expect(vmDisplayName(port, observation)).toBe('frontend (VM 100)');
  });

  it('turns runtime ambiguity into an actionable reason', () => {
    const diagnostic = diagnosePort(port, node, { nicPresent: true, runtime: 'ambiguous' });
    expect(diagnostic.reason).toBe('More than one PVN port matches this VM NIC.');
    expect(diagnostic.tone).toBe('bad');
  });
});
