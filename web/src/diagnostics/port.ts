import type { NodeStatus, Port } from '../api/types';
import { redactResourceIDs } from './display';

export type ChassisDiagnostic = 'matched' | 'mismatch' | 'missing' | 'not requested' | 'unavailable';
export type RuntimeDiagnostic = 'matched' | 'mismatch' | 'not-found' | 'ambiguous' | 'not-bindable' | 'error';
export type DiagnosticTone = 'good' | 'warning' | 'bad' | 'neutral';

export interface VMObservation {
  name?: string;
  powerState?: string;
  nicPresent: boolean;
  linkDown?: boolean;
  bridge?: string;
  macMatches?: boolean;
  runtime?: RuntimeDiagnostic;
  runtimeReason?: string;
}

export interface PortDiagnostic {
  chassis: ChassisDiagnostic;
  reason: string;
  tone: DiagnosticTone;
  warnings: string[];
}

function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

function nicOption(value: string, key: string): string {
  const part = value.split(',').find((entry) => entry.startsWith(`${key}=`));
  return part ? part.slice(key.length + 1) : '';
}

function nicMAC(value: string): string {
  return value.split(',', 1)[0]?.split('=', 2)[1]?.trim().toLowerCase() || '';
}

function sameMAC(left: string, right: string): boolean {
  return Boolean(left && right && left.trim().toLowerCase() === right.trim().toLowerCase());
}

export function observeVM(
  port: Port,
  config: Record<string, unknown>,
  status: Record<string, unknown>,
  runtime?: Pick<VMObservation, 'runtime' | 'runtimeReason'>,
): VMObservation {
  const nicValue = port.nic ? config[port.nic] : undefined;
  const nic = text(nicValue);
  const linkDown = nic ? nicOption(nic, 'link_down') : '';
  return {
    name: text(config.name) || text(status.name) || undefined,
    powerState: text(status.status) || text(status.qmpstatus) || undefined,
    nicPresent: nicValue !== undefined,
    linkDown: linkDown ? linkDown === '1' : undefined,
    bridge: nic ? nicOption(nic, 'bridge') || undefined : undefined,
    macMatches: nic && port.mac_address ? sameMAC(nicMAC(nic), port.mac_address) : undefined,
    ...runtime,
  };
}

export function portDisplayName(port: Port): string {
  return redactResourceIDs(port.name || port.mac_address || 'Unnamed port');
}

export function nodeDisplayName(node: NodeStatus | undefined): string {
  return redactResourceIDs(node?.name || node?.management_address || 'Unavailable node');
}

export function vmDisplayName(port: Port, observation?: VMObservation): string {
  if (!port.vmid) return 'Not attached';
  return observation?.name ? `${redactResourceIDs(observation.name)} (VM ${port.vmid})` : `VM ${port.vmid}`;
}

export function chassisDiagnostic(port: Port, node: NodeStatus | undefined): ChassisDiagnostic {
  if (!port.node_id && !port.requested_chassis) return 'not requested';
  if (!node) return 'unavailable';
  if (!port.requested_chassis || !node.chassis_id) return 'missing';
  return port.requested_chassis === node.chassis_id ? 'matched' : 'mismatch';
}

function safeReason(message: string, port: Port, node: NodeStatus | undefined): string {
  let result = message.trim();
  const sensitive = [port.id, port.node_id, port.requested_chassis, port.lsp_name, node?.id, node?.chassis_id]
    .filter((value): value is string => Boolean(value && value.length > 3))
    .sort((left, right) => right.length - left.length);
  for (const value of sensitive) result = result.split(value).join('[resource ID]');
  return redactResourceIDs(result).slice(0, 300);
}

function errorReason(port: Port, node: NodeStatus | undefined, fallback: string): string {
  return safeReason(port.last_error || node?.last_error || fallback, port, node);
}

function runtimeReason(port: Port, node: NodeStatus | undefined, observation: VMObservation): string | undefined {
  switch (observation.runtime) {
  case 'mismatch':
    return 'Runtime lookup resolved a different PVN port for this VM NIC.';
  case 'not-found':
    return 'The manager cannot resolve this VM NIC to a PVN port.';
  case 'ambiguous':
    return 'More than one PVN port matches this VM NIC.';
  case 'not-bindable':
    return safeReason(observation.runtimeReason || 'The manager reports that this VM NIC is not bindable.', port, node);
  case 'error':
    return safeReason(observation.runtimeReason || 'Runtime port verification failed.', port, node);
  default:
    return undefined;
  }
}

export function diagnosePort(port: Port, node?: NodeStatus, observation?: VMObservation): PortDiagnostic {
  const chassis = chassisDiagnostic(port, node);
  const binding = String(port.binding_status || 'unknown').toLowerCase();
  const state = String(port.state || 'unknown').toLowerCase();
  const warnings = port.security_group_ids?.length
    ? []
    : ['Legacy unrestricted port: no security group is attached. Migrate it with the default security-group backfill.'];

  const result = (reason: string, tone: DiagnosticTone): PortDiagnostic => ({ chassis, reason, tone, warnings });
  if (port.admin_state_up === false) return result('The port is administratively disabled.', 'bad');
  if (port.node_id && !node) return result('The selected node is unavailable.', 'bad');
  if (node?.enabled === false) return result(`${nodeDisplayName(node)} is disabled.`, 'bad');
  if (chassis === 'mismatch') return result('Requested chassis does not match the selected node.', 'bad');
  if ((binding === 'binding' || binding === 'bound') && chassis === 'missing') {
    return result('The selected node or port has no complete chassis identity.', 'bad');
  }
  if (state === 'error' || binding === 'error' || String(node?.state || '').toLowerCase() === 'error') {
    return result(errorReason(port, node, 'Control-plane or binding reconciliation failed.'), 'bad');
  }
  if (
    typeof port.revision === 'number' &&
    typeof port.applied_revision === 'number' &&
    port.applied_revision < port.revision
  ) {
    return result('Waiting for OVN to apply the current port revision.', 'warning');
  }
  if (state === 'pending') return result('Control-plane realization is pending.', 'warning');
  if (state === 'deleting') return result('The port is being removed from OVN.', 'warning');

  if (observation) {
    const runtimeFailure = runtimeReason(port, node, observation);
    if (runtimeFailure) return result(runtimeFailure, 'bad');
  }

  switch (binding) {
  case 'unbound':
    return port.vmid
      ? result('A VM assignment exists, but the port is not bound.', 'bad')
      : result('The port is ready but not attached to a VM.', 'neutral');
  case 'binding':
    if (observation?.powerState && observation.powerState !== 'running') {
      return result(`The VM is ${observation.powerState}; a running TAP is required for binding.`, 'bad');
    }
    if (observation && !observation.nicPresent) return result(`The assigned VM NIC ${port.nic || ''} is missing.`, 'bad');
    if (observation?.bridge && observation.bridge !== 'br-int') return result('The assigned VM NIC is not connected to br-int.', 'bad');
    if (observation?.macMatches === false) return result('The assigned VM NIC MAC does not match this PVN port.', 'bad');
    if (observation?.linkDown === false) return result('The VM NIC is up before OVN binding completed; review fail-closed state.', 'bad');
    if (observation?.linkDown === true) return result('The VM NIC remains fail-closed while OVN binding completes.', 'warning');
    return result('Waiting for the local agent and OVN to confirm the binding.', 'warning');
  case 'bound':
    if (observation?.powerState && observation.powerState !== 'running') {
      return result(`The VM is ${observation.powerState}; no live TAP is available.`, 'warning');
    }
    if (observation && !observation.nicPresent) return result(`OVN reports bound, but VM NIC ${port.nic || ''} is missing.`, 'bad');
    if (observation?.bridge && observation.bridge !== 'br-int') return result('OVN reports bound, but the VM NIC is not connected to br-int.', 'bad');
    if (observation?.macMatches === false) return result('OVN reports bound, but the VM NIC MAC does not match.', 'bad');
    if (observation?.linkDown === true) return result('OVN reports bound, but the VM NIC remains link-down.', 'bad');
    return result('The local agent confirmed the OVN binding.', 'good');
  case 'detaching':
    return result('Waiting for the local agent to clear the binding.', 'warning');
  default:
    return result(`Unknown binding status: ${safeReason(binding, port, node)}.`, 'bad');
  }
}
