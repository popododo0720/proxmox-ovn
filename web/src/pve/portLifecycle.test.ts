import { describe, expect, it, vi } from 'vitest';
import type { Port } from '../api/types';
import type { PveNicUpdate } from './bridge';
import { attachVMPort, detachVMPort, isOwnedPVNNIC, type PortLifecycleAPI, type PortLifecycleBridge, type VMPortTarget } from './portLifecycle';

const target: VMPortTarget = { nodeID: 'node-1', nodeName: 'pve-a', vmid: 100, nic: 'net1' };

function port(overrides: Partial<Port> = {}): Port {
  return {
    id: 'port-1', revision: 1, generation: 1, state: 'ready', applied_revision: 1,
    name: 'vm100-net1', mac_address: '02:00:00:00:00:01', binding_status: 'unbound',
    ...overrides,
  };
}

class FakeBridge implements PortLifecycleBridge {
  config: Record<string, unknown> = { digest: 'a'.repeat(40) };
  status = 'running';
  updates: PveNicUpdate[] = [];
  deletes: string[] = [];

  async getQemuConfig() { return { ...this.config }; }
  async getQemuStatus() { return { status: this.status }; }
  async setQemuNic(_node: string, _vmid: number, update: PveNicUpdate) {
    this.updates.push(update);
    this.config[update.nic] = `virtio=${update.macAddress},bridge=br-int,firewall=0,link_down=${update.linkDown ? 1 : 0}`;
    this.config.digest = String.fromCharCode(97 + this.updates.length).repeat(40);
  }
  async deleteQemuNic(_node: string, _vmid: number, _digest: string, nic: `net${number}`) {
    this.deletes.push(nic);
    delete this.config[nic];
    this.config.digest = 'f'.repeat(40);
  }
}

function lifecycleAPI(initial: Port) {
  let current = initial;
  const api: PortLifecycleAPI = {
    get: vi.fn(async () => current) as PortLifecycleAPI['get'],
    attachPort: vi.fn(async (_id, input) => {
      current = port({
        ...current, revision: Number(current.revision) + 1, generation: input.generation + 1,
        node_id: input.node_id, vmid: input.vmid, nic: input.nic, binding_status: 'binding',
      });
      return current;
    }),
    detachPort: vi.fn(async () => {
      current = port({ ...current, revision: Number(current.revision) + 1, binding_status: 'detaching' });
      return current;
    }),
  };
  return {
    api,
    setCurrent(value: Port) { current = value; },
    current() { return current; },
  };
}

const instant = { pollAttempts: 3, pollIntervalMs: 0, sleep: async () => undefined };

describe('VM port lifecycle', () => {
  it('stages link-down, waits for OVN binding, and only then enables the NIC', async () => {
    const bridge = new FakeBridge();
    const state = lifecycleAPI(port());
    const steps: string[] = [];
    (state.api.get as ReturnType<typeof vi.fn>).mockImplementation(async () => {
      const current = state.current();
      if (current.binding_status === 'binding') {
        state.setCurrent(port({ ...current, revision: 3, applied_revision: 3, binding_status: 'bound' }));
      }
      return state.current();
    });

    const result = await attachVMPort(state.api, bridge, state.current(), target, { ...instant, onStep: (step) => steps.push(step) });

    expect(result.binding_status).toBe('bound');
    expect(bridge.updates.map((update) => update.linkDown)).toEqual([true, false]);
    expect(bridge.deletes).toEqual([]);
    expect(steps).toEqual(['checking-vm', 'staging-nic', 'requesting-binding', 'waiting-for-binding', 'enabling-nic']);
  });

  it('removes only its staged NIC when the manager rejects attach', async () => {
    const bridge = new FakeBridge();
    const original = port();
    const state = lifecycleAPI(original);
    state.api.attachPort = vi.fn(async () => { throw new Error('forbidden'); });

    await expect(attachVMPort(state.api, bridge, original, target, instant)).rejects.toThrow('forbidden');
    expect(bridge.updates.map((update) => update.linkDown)).toEqual([true]);
    expect(bridge.deletes).toEqual(['net1']);
  });

  it('never removes a pre-existing NIC when attach validation fails', async () => {
    const bridge = new FakeBridge();
    bridge.config.net1 = 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=0';
    const state = lifecycleAPI(port());

    await expect(attachVMPort(state.api, bridge, state.current(), target, instant)).rejects.toThrow('already exists');
    expect(bridge.updates).toEqual([]);
    expect(bridge.deletes).toEqual([]);
  });

  it('disables and unbinds before deleting an attached NIC', async () => {
    const attached = port({
      revision: 4, generation: 2, node_id: target.nodeID, vmid: target.vmid, nic: target.nic, binding_status: 'bound',
    });
    const bridge = new FakeBridge();
    bridge.config.net1 = 'virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=0';
    const state = lifecycleAPI(attached);
    (state.api.get as ReturnType<typeof vi.fn>).mockImplementation(async () => {
      const current = state.current();
      if (current.binding_status === 'detaching') state.setCurrent(port({ ...current, revision: 6, binding_status: 'unbound' }));
      return state.current();
    });

    const result = await detachVMPort(state.api, bridge, attached, target, instant);

    expect(result.binding_status).toBe('unbound');
    expect(bridge.updates.map((update) => update.linkDown)).toEqual([true]);
    expect(bridge.deletes).toEqual(['net1']);
  });

  it('rejects stopped VMs without changing their configuration', async () => {
    const bridge = new FakeBridge();
    bridge.status = 'stopped';
    const state = lifecycleAPI(port());

    await expect(attachVMPort(state.api, bridge, state.current(), target, instant)).rejects.toThrow('running QEMU VM');
    expect(bridge.updates).toEqual([]);
    expect(bridge.deletes).toEqual([]);
  });

  it('recognizes only the exact PVN-owned virtio NIC', () => {
    expect(isOwnedPVNNIC('virtio=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=1', '02:00:00:00:00:01', true)).toBe(true);
    expect(isOwnedPVNNIC('virtio=02:00:00:00:00:01,bridge=vmbr0,firewall=0,link_down=1', '02:00:00:00:00:01')).toBe(false);
    expect(isOwnedPVNNIC('e1000=02:00:00:00:00:01,bridge=br-int,firewall=0,link_down=1', '02:00:00:00:00:01')).toBe(false);
  });
});
