import type { BaseResource, Port, PortAttachInput, PortDetachInput } from '../api/types';
import type { PveNicUpdate, PveQemuStatus } from './bridge';

export type PortLifecycleStep =
  | 'checking-vm'
  | 'staging-nic'
  | 'requesting-binding'
  | 'waiting-for-binding'
  | 'enabling-nic'
  | 'disabling-nic'
  | 'requesting-detach'
  | 'waiting-for-detach'
  | 'deleting-nic'
  | 'rolling-back';

export interface VMPortTarget {
  nodeID: string;
  nodeName: string;
  vmid: number;
  nic: `net${number}`;
}

export interface PortLifecycleAPI {
  get<T extends BaseResource>(path: string, id: string): Promise<T>;
  attachPort(id: string, input: PortAttachInput, revision: number, idempotencyKey?: string): Promise<Port>;
  detachPort(id: string, input: PortDetachInput, revision: number, idempotencyKey?: string): Promise<Port>;
}

export interface PortLifecycleBridge {
  getQemuConfig(node: string, vmid: number): Promise<Record<string, unknown>>;
  getQemuStatus(node: string, vmid: number): Promise<PveQemuStatus>;
  setQemuNic(node: string, vmid: number, update: PveNicUpdate): Promise<unknown>;
  deleteQemuNic(node: string, vmid: number, digest: string, nic: `net${number}`): Promise<unknown>;
}

export interface PortLifecycleOptions {
  pollAttempts?: number;
  pollIntervalMs?: number;
  sleep?: (delayMs: number) => Promise<void>;
  onStep?: (step: PortLifecycleStep) => void;
}

export class PortLifecycleError extends Error {
  readonly step: PortLifecycleStep;
  readonly cause: unknown;
  readonly rollbackErrors: Error[];

  constructor(step: PortLifecycleStep, cause: unknown, rollbackErrors: Error[] = []) {
    const reason = cause instanceof Error ? cause.message : String(cause);
    const rollback = rollbackErrors.length ? ` Cleanup also failed: ${rollbackErrors.map((error) => error.message).join('; ')}` : '';
    super(`VM port operation failed while ${step.replaceAll('-', ' ')}: ${reason}.${rollback}`);
    this.name = 'PortLifecycleError';
    this.step = step;
    this.cause = cause;
    this.rollbackErrors = rollbackErrors;
  }
}

const defaultSleep = (delayMs: number) => new Promise<void>((resolve) => window.setTimeout(resolve, delayMs));

function revisionOf(port: Port): number {
  if (!Number.isSafeInteger(port.revision) || Number(port.revision) < 1) throw new Error('PVN port has no valid revision');
  return Number(port.revision);
}

function generationOf(port: Port): number {
  if (!Number.isSafeInteger(port.generation) || Number(port.generation) < 1) throw new Error('PVN port has no valid generation');
  return Number(port.generation);
}

function digestOf(config: Record<string, unknown>): string {
  const digest = config.digest;
  if (typeof digest !== 'string' || !/^[A-Fa-f0-9]{40,128}$/.test(digest)) {
    throw new Error('PVE returned a VM config without a valid digest');
  }
  return digest;
}

function normalizeMAC(value: string): string {
  return value.trim().toLowerCase();
}

function nicOptions(value: unknown): Record<string, string> | null {
  if (typeof value !== 'string') return null;
  const parts = value.split(',');
  const first = parts.shift()?.split('=', 2);
  if (!first || first.length !== 2) return null;
  const result: Record<string, string> = { model: first[0], mac: first[1] };
  for (const part of parts) {
    const separator = part.indexOf('=');
    if (separator > 0) result[part.slice(0, separator)] = part.slice(separator + 1);
  }
  return result;
}

export function isOwnedPVNNIC(value: unknown, macAddress: string, linkDown?: boolean): boolean {
  const options = nicOptions(value);
  if (!options || options.model !== 'virtio' || options.bridge !== 'br-int' || options.firewall !== '0') return false;
  if (normalizeMAC(options.mac) !== normalizeMAC(macAddress)) return false;
  if (linkDown === undefined) return true;
  return (options.link_down === '1') === linkDown;
}

function assertPortIdentity(port: Port): void {
  if (!port.id || !port.mac_address || !/^[A-Fa-f0-9]{2}(?::[A-Fa-f0-9]{2}){5}$/.test(port.mac_address)) {
    throw new Error('PVN port has no valid ID or MAC address');
  }
  revisionOf(port);
  generationOf(port);
}

function assertTarget(target: VMPortTarget): void {
  if (!target.nodeID || !/^[A-Za-z0-9._-]+$/.test(target.nodeName) || !Number.isSafeInteger(target.vmid) || target.vmid < 1 || !/^net[0-9]+$/.test(target.nic)) {
    throw new Error('Select a valid PVN node, VM ID, and netN slot');
  }
}

async function assertRunning(bridge: PortLifecycleBridge, target: VMPortTarget): Promise<void> {
  const status = await bridge.getQemuStatus(target.nodeName, target.vmid);
  if (status.status !== 'running' && status.qmpstatus !== 'running') {
    throw new Error('PVN v1 can attach or detach only a running QEMU VM');
  }
}

function runtime(options: PortLifecycleOptions) {
  return {
    attempts: options.pollAttempts ?? 90,
    interval: options.pollIntervalMs ?? 500,
    sleep: options.sleep ?? defaultSleep,
    emit: options.onStep ?? (() => undefined),
  };
}

async function waitForPort(
  api: PortLifecycleAPI,
  portID: string,
  expected: 'bound' | 'unbound',
  options: PortLifecycleOptions,
): Promise<Port> {
  const run = runtime(options);
  for (let attempt = 0; attempt < run.attempts; attempt += 1) {
    const port = await api.get<Port>('/ports', portID);
    if (port.binding_status === expected) return port;
    if (port.state === 'error' || port.binding_status === 'error') {
      throw new Error(port.last_error || `PVN port entered the ${port.binding_status || port.state} state`);
    }
    const valid = expected === 'bound'
      ? port.binding_status === 'binding'
      : port.binding_status === 'detaching';
    if (!valid) throw new Error(`PVN port changed unexpectedly to ${port.binding_status || 'unknown'}`);
    await run.sleep(run.interval);
  }
  throw new Error(`Timed out waiting for PVN port to become ${expected}`);
}

async function waitForNIC(
  bridge: PortLifecycleBridge,
  target: VMPortTarget,
  predicate: (value: unknown) => boolean,
  options: PortLifecycleOptions,
): Promise<Record<string, unknown>> {
  const run = runtime(options);
  for (let attempt = 0; attempt < run.attempts; attempt += 1) {
    const config = await bridge.getQemuConfig(target.nodeName, target.vmid);
    if (predicate(config[target.nic])) return config;
    await run.sleep(run.interval);
  }
  throw new Error(`Timed out waiting for PVE ${target.nic} configuration`);
}

function sameAttachment(port: Port, target: VMPortTarget, generation: number): boolean {
  return port.node_id === target.nodeID && port.vmid === target.vmid && port.nic === target.nic &&
    port.generation === generation && (port.binding_status === 'binding' || port.binding_status === 'bound');
}

async function removeOwnedNIC(
  bridge: PortLifecycleBridge,
  port: Port,
  target: VMPortTarget,
  options: PortLifecycleOptions,
): Promise<void> {
  const config = await bridge.getQemuConfig(target.nodeName, target.vmid);
  const current = config[target.nic];
  if (current === undefined) return;
  if (!isOwnedPVNNIC(current, port.mac_address || '')) {
    throw new Error(`Refusing to delete ${target.nic} because it is not the selected PVN port`);
  }
  await bridge.deleteQemuNic(target.nodeName, target.vmid, digestOf(config), target.nic);
  await waitForNIC(bridge, target, (value) => value === undefined, options);
}

async function rollbackAttach(
  api: PortLifecycleAPI,
  bridge: PortLifecycleBridge,
  port: Port,
  target: VMPortTarget,
  managerAccepted: boolean,
  nicMayBeStaged: boolean,
  options: PortLifecycleOptions,
): Promise<Error[]> {
  const errors: Error[] = [];
  let safeToDelete = nicMayBeStaged && !managerAccepted;
  if (managerAccepted) {
    try {
      let current = await api.get<Port>('/ports', port.id);
      if (current.binding_status === 'binding' || current.binding_status === 'bound' || current.binding_status === 'error') {
        current = await api.detachPort(current.id, { generation: generationOf(current) }, revisionOf(current));
      }
      if (current.binding_status === 'detaching') current = await waitForPort(api, current.id, 'unbound', options);
      safeToDelete = nicMayBeStaged && current.binding_status === 'unbound';
    } catch (reason) {
      errors.push(reason instanceof Error ? reason : new Error(String(reason)));
    }
  }
  if (safeToDelete) {
    try {
      await removeOwnedNIC(bridge, port, target, options);
    } catch (reason) {
      errors.push(reason instanceof Error ? reason : new Error(String(reason)));
    }
  }
  return errors;
}

export async function attachVMPort(
  api: PortLifecycleAPI,
  bridge: PortLifecycleBridge,
  port: Port,
  target: VMPortTarget,
  options: PortLifecycleOptions = {},
): Promise<Port> {
  let step: PortLifecycleStep = 'checking-vm';
  let managerAccepted = false;
  let nicMayBeStaged = false;
  const emit = (next: PortLifecycleStep) => { step = next; options.onStep?.(next); };
  try {
    assertPortIdentity(port);
    assertTarget(target);
    if (port.binding_status !== 'unbound') throw new Error('Only an unbound PVN port can be attached');
    emit('checking-vm');
    await assertRunning(bridge, target);
    let config = await bridge.getQemuConfig(target.nodeName, target.vmid);
    if (config[target.nic] !== undefined) throw new Error(`${target.nic} already exists on VM ${target.vmid}`);

    emit('staging-nic');
    nicMayBeStaged = true;
    await bridge.setQemuNic(target.nodeName, target.vmid, {
      digest: digestOf(config), nic: target.nic, macAddress: port.mac_address!, linkDown: true,
    });
    await waitForNIC(bridge, target, (value) => isOwnedPVNNIC(value, port.mac_address!, true), options);

    emit('requesting-binding');
    const nextGeneration = generationOf(port) + 1;
    try {
      port = await api.attachPort(port.id, {
        node_id: target.nodeID, vmid: target.vmid, nic: target.nic, generation: generationOf(port),
      }, revisionOf(port), crypto.randomUUID());
      managerAccepted = true;
    } catch (reason) {
      const current = await api.get<Port>('/ports', port.id);
      if (!sameAttachment(current, target, nextGeneration)) throw reason;
      port = current;
      managerAccepted = true;
    }

    emit('waiting-for-binding');
    if (port.binding_status !== 'bound') port = await waitForPort(api, port.id, 'bound', options);
    emit('enabling-nic');
    config = await bridge.getQemuConfig(target.nodeName, target.vmid);
    if (!isOwnedPVNNIC(config[target.nic], port.mac_address || '')) throw new Error(`${target.nic} no longer matches the PVN port`);
    await bridge.setQemuNic(target.nodeName, target.vmid, {
      digest: digestOf(config), nic: target.nic, macAddress: port.mac_address!, linkDown: false,
    });
    await waitForNIC(bridge, target, (value) => isOwnedPVNNIC(value, port.mac_address!, false), options);
    return port;
  } catch (reason) {
    options.onStep?.('rolling-back');
    let ambiguousManagerState = false;
    if (!managerAccepted) {
      try {
        const current = await api.get<Port>('/ports', port.id);
        managerAccepted = sameAttachment(current, target, generationOf(port) + 1);
      } catch {
        ambiguousManagerState = true;
      }
    }
    const rollbackErrors = ambiguousManagerState
      ? [new Error('PVN manager state is unknown; the staged NIC was left link-down')]
      : await rollbackAttach(api, bridge, port, target, managerAccepted, nicMayBeStaged, options);
    throw new PortLifecycleError(step, reason, rollbackErrors);
  }
}

export async function detachVMPort(
  api: PortLifecycleAPI,
  bridge: PortLifecycleBridge,
  port: Port,
  target: VMPortTarget,
  options: PortLifecycleOptions = {},
): Promise<Port> {
  let step: PortLifecycleStep = 'checking-vm';
  let detachAccepted = false;
  let nicMayBeDisabled = false;
  const emit = (next: PortLifecycleStep) => { step = next; options.onStep?.(next); };
  try {
    assertPortIdentity(port);
    assertTarget(target);
    if (port.node_id !== target.nodeID || port.vmid !== target.vmid || port.nic !== target.nic) {
      throw new Error('The selected VM target does not match this PVN port attachment');
    }
    if (!['binding', 'bound', 'error'].includes(port.binding_status || '')) {
      throw new Error('Only an attached PVN port can be detached');
    }
    emit('checking-vm');
    await assertRunning(bridge, target);
    let config = await bridge.getQemuConfig(target.nodeName, target.vmid);
    if (!isOwnedPVNNIC(config[target.nic], port.mac_address || '')) {
      throw new Error(`Refusing to change ${target.nic} because it is not the selected PVN port`);
    }

    emit('disabling-nic');
    nicMayBeDisabled = true;
    await bridge.setQemuNic(target.nodeName, target.vmid, {
      digest: digestOf(config), nic: target.nic, macAddress: port.mac_address!, linkDown: true,
    });
    await waitForNIC(bridge, target, (value) => isOwnedPVNNIC(value, port.mac_address!, true), options);

    emit('requesting-detach');
    const originalGeneration = generationOf(port);
    try {
      port = await api.detachPort(port.id, { generation: originalGeneration }, revisionOf(port), crypto.randomUUID());
      detachAccepted = true;
    } catch (reason) {
      const current = await api.get<Port>('/ports', port.id);
      if (current.generation !== originalGeneration || (current.binding_status !== 'detaching' && current.binding_status !== 'unbound')) throw reason;
      port = current;
      detachAccepted = true;
    }

    emit('waiting-for-detach');
    if (port.binding_status !== 'unbound') port = await waitForPort(api, port.id, 'unbound', options);
    emit('deleting-nic');
    await removeOwnedNIC(bridge, port, target, options);
    return port;
  } catch (reason) {
    const rollbackErrors: Error[] = [];
    if (!detachAccepted && nicMayBeDisabled) {
      options.onStep?.('rolling-back');
      try {
        const config = await bridge.getQemuConfig(target.nodeName, target.vmid);
        if (isOwnedPVNNIC(config[target.nic], port.mac_address || '')) {
          await bridge.setQemuNic(target.nodeName, target.vmid, {
            digest: digestOf(config), nic: target.nic, macAddress: port.mac_address!, linkDown: false,
          });
          await waitForNIC(bridge, target, (value) => isOwnedPVNNIC(value, port.mac_address!, false), options);
        }
      } catch (rollbackReason) {
        rollbackErrors.push(rollbackReason instanceof Error ? rollbackReason : new Error(String(rollbackReason)));
      }
    }
    throw new PortLifecycleError(step, reason, rollbackErrors);
  }
}
