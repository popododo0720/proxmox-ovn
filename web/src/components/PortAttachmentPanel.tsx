import { useEffect, useMemo, useState } from 'react';
import { ApiError } from '../api/client';
import { useApi } from '../api/context';
import type { NodeStatus, Port } from '../api/types';
import { observeVM, type RuntimeDiagnostic } from '../diagnostics/port';
import { pveBridge } from '../pve/bridge';
import {
  attachVMPort,
  detachVMPort,
  type PortLifecycleBridge,
  type PortLifecycleStep,
  type VMPortTarget,
} from '../pve/portLifecycle';
import { ErrorState } from './ErrorState';
import { PortDiagnosticsCard } from './PortDiagnostics';
import { useResourceCatalog } from './ResourceCatalog';
import { StatusPill } from './StatusPill';

const stepLabels: Record<PortLifecycleStep, string> = {
  'checking-vm': 'Checking VM',
  'staging-nic': 'Creating link-down NIC',
  'requesting-binding': 'Requesting OVN binding',
  'waiting-for-binding': 'Waiting for OVN binding',
  'enabling-nic': 'Enabling VM NIC',
  'disabling-nic': 'Disabling VM NIC',
  'requesting-detach': 'Requesting detach',
  'waiting-for-detach': 'Waiting for OVN cleanup',
  'deleting-nic': 'Deleting VM NIC',
  'rolling-back': 'Rolling back safely',
};

export function firstFreeNIC(config: Record<string, unknown>, limit = 32): `net${number}` | null {
  for (let index = 0; index < limit; index += 1) {
    const nic = `net${index}` as `net${number}`;
    if (config[nic] === undefined) return nic;
  }
  return null;
}

function validNIC(value: string): value is `net${number}` {
  return /^net[0-9]+$/.test(value);
}

type PortAttachmentBridge = PortLifecycleBridge & { readonly available: boolean };

export function PortAttachmentPanel({
  onChanged,
  bridge = pveBridge,
}: {
  onChanged?: () => void;
  bridge?: PortAttachmentBridge;
}) {
  const api = useApi();
  const portCatalog = useResourceCatalog('/ports');
  const nodeCatalog = useResourceCatalog('/nodes');
  const ports = portCatalog.items as unknown as Port[];
  const nodes = nodeCatalog.items as unknown as NodeStatus[];
  const [portID, setPortID] = useState('');
  const [nodeID, setNodeID] = useState('');
  const [vmid, setVMID] = useState('');
  const [nic, setNIC] = useState('net0');
  const [vmConfig, setVMConfig] = useState<Record<string, unknown> | null>(null);
  const [vmStatus, setVMStatus] = useState('');
  const [runtimeEvidence, setRuntimeEvidence] = useState<{ runtime: RuntimeDiagnostic; runtimeReason?: string }>();
  const [reading, setReading] = useState(false);
  const [busyStep, setBusyStep] = useState<PortLifecycleStep | null>(null);
  const [error, setError] = useState('');
  const [message, setMessage] = useState('');

  function refresh() {
    setError('');
    void Promise.allSettled([portCatalog.retry(), nodeCatalog.retry()]);
  }

  useEffect(() => {
    setPortID((current) => current && ports.some((port) => port.id === current) ? current : (ports[0]?.id || ''));
  }, [ports]);

  const selectedPort = useMemo(() => ports.find((port) => port.id === portID), [ports, portID]);
  const selectedNode = useMemo(() => nodes.find((node) => node.id === nodeID), [nodes, nodeID]);
  const attachedNode = useMemo(() => nodes.find((node) => node.id === selectedPort?.node_id), [nodes, selectedPort?.node_id]);
  const attached = Boolean(selectedPort && selectedPort.binding_status !== 'unbound');

  const vmObservation = useMemo(
    () => selectedPort && vmConfig
      ? observeVM(selectedPort, vmConfig, { status: vmStatus }, runtimeEvidence)
      : undefined,
    [runtimeEvidence, selectedPort, vmConfig, vmStatus],
  );

  function clearInspection() {
    setVMConfig(null);
    setVMStatus('');
    setRuntimeEvidence(undefined);
  }

  useEffect(() => {
    if (!selectedPort || selectedPort.binding_status === 'unbound') return;
    setNodeID(selectedPort.node_id || '');
    setVMID(selectedPort.vmid ? String(selectedPort.vmid) : '');
    setNIC(selectedPort.nic || 'net0');
  }, [selectedPort]);

  useEffect(() => {
    clearInspection();
  }, [selectedPort?.id]);

  const nics = useMemo(() => {
    if (!vmConfig) return [];
    return Object.entries(vmConfig).filter(([key]) => /^net\d+$/.test(key)).sort(([left], [right]) => left.localeCompare(right, undefined, { numeric: true }));
  }, [vmConfig]);

  function target(): VMPortTarget {
    if (!selectedNode || !validNIC(nic)) throw new Error('Select a valid node and netN slot');
    return { nodeID: selectedNode.id, nodeName: selectedNode.name || '', vmid: Number(vmid), nic };
  }

  async function inspectVM() {
    if (!selectedPort) return;
    setError('');
    setMessage('');
    clearInspection();
    setReading(true);
    try {
      const selection = target();
      const runtimeRequest = attached
        ? api.resolveRuntimePort(selection.nodeName, selection.vmid, selection.nic)
          .then((resolution) => ({ runtime: resolution.port_id === selectedPort.id ? 'matched' : 'mismatch' } as const))
          .catch((reason: unknown) => {
            const code = reason instanceof ApiError ? reason.code : undefined;
            const runtime: RuntimeDiagnostic = code === 'port_not_found'
              ? 'not-found'
              : code === 'ambiguous_port'
                ? 'ambiguous'
                : code === 'port_not_bindable'
                  ? 'not-bindable'
                  : 'error';
            return {
              runtime,
              runtimeReason: reason instanceof Error ? reason.message : 'Runtime port verification failed',
            };
          })
        : Promise.resolve(undefined);
      const [config, status, runtime] = await Promise.all([
        bridge.getQemuConfig(selection.nodeName, selection.vmid),
        bridge.getQemuStatus(selection.nodeName, selection.vmid),
        runtimeRequest,
      ]);
      setVMConfig(config);
      setVMStatus(String(status.status || status.qmpstatus || 'unknown'));
      setRuntimeEvidence(runtime);
      if (!attached) {
        const available = firstFreeNIC(config);
        if (available) setNIC(available);
      }
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not read the VM configuration');
    } finally {
      setReading(false);
    }
  }

  async function run(action: 'attach' | 'detach') {
    if (!selectedPort) return;
    setError('');
    setMessage('');
    setBusyStep('checking-vm');
    try {
      const selection = target();
      const result = action === 'attach'
        ? await attachVMPort(api, bridge, selectedPort, selection, { onStep: setBusyStep })
        : await detachVMPort(api, bridge, selectedPort, selection, { onStep: setBusyStep });
      setMessage(action === 'attach'
        ? `${result.name || result.mac_address || 'Port'} is bound and the VM NIC is enabled.`
        : `${result.name || result.mac_address || 'Port'} is unbound and the VM NIC was removed.`);
      clearInspection();
      portCatalog.invalidate();
      onChanged?.();
    } catch (reason) {
      portCatalog.invalidate();
      setError(reason instanceof Error ? reason.message : `Could not ${action} the VM port`);
      onChanged?.();
    } finally {
      setBusyStep(null);
    }
  }

  const loading = portCatalog.loading || nodeCatalog.loading;
  const catalogError = portCatalog.error || nodeCatalog.error;
  const actionable = selectedPort && selectedNode && vmid && validNIC(nic) && bridge.available && !loading && !reading && !busyStep;
  const canAttach = Boolean(actionable && selectedPort?.binding_status === 'unbound' && selectedPort.admin_state_up !== false && selectedNode?.enabled !== false);
  const canDetach = Boolean(actionable && ['binding', 'bound', 'error'].includes(selectedPort?.binding_status || ''));

  return (
    <section className="resource-section compact-section">
      <div className="page-heading">
        <div>
          <span className="eyebrow">Fail-closed hotplug</span>
          <h1>Attach a VM NIC</h1>
          <p>PVN keeps the NIC link-down until OVN confirms the logical port binding.</p>
        </div>
        <div className="heading-actions">
          <StatusPill value={bridge.available ? 'connected' : 'unavailable'} />
          <button className="button button-secondary" disabled={loading || Boolean(busyStep)} onClick={refresh}>Refresh</button>
        </div>
      </div>

      <div className="attachment-workflow">
        <label className="form-field attachment-port-field">
          <span>PVN port</span>
          <select value={portID} onChange={(event) => { setPortID(event.target.value); clearInspection(); setMessage(''); }} disabled={loading || Boolean(busyStep)}>
            {!ports.length && <option value="">No ports available</option>}
            {ports.map((port) => <option value={port.id} key={port.id}>{port.name || port.mac_address || 'Unnamed port'} · {port.mac_address || 'no MAC'} · {port.binding_status || 'unknown'}</option>)}
          </select>
        </label>
        <label className="form-field">
          <span>PVE node</span>
          <select value={nodeID} onChange={(event) => { setNodeID(event.target.value); clearInspection(); }} disabled={attached || Boolean(busyStep)}>
            <option value="" disabled>Select node…</option>
            {nodes.map((node) => <option value={node.id} key={node.id}>{node.name || node.management_address || 'Unavailable node'}{node.enabled === false ? ' (disabled)' : ''}</option>)}
          </select>
        </label>
        <label className="form-field">
          <span>VM ID</span>
          <input type="number" min="1" value={vmid} onChange={(event) => { setVMID(event.target.value); clearInspection(); }} disabled={attached || Boolean(busyStep)} placeholder="100" />
        </label>
        <label className="form-field">
          <span>NIC slot</span>
          <input value={nic} onChange={(event) => { setNIC(event.target.value); clearInspection(); }} disabled={attached || Boolean(busyStep)} placeholder="net0" />
        </label>
        <div className="attachment-actions">
          <button className="button button-secondary" disabled={!actionable} onClick={() => void inspectVM()}>{reading ? 'Reading…' : 'Inspect'}</button>
          {selectedPort?.binding_status === 'unbound' ? (
            <button className="button button-primary" disabled={!canAttach} onClick={() => void run('attach')}>{busyStep ? stepLabels[busyStep] : 'Attach'}</button>
          ) : (
            <button className="button button-danger" disabled={!canDetach} onClick={() => void run('detach')}>{busyStep ? stepLabels[busyStep] : 'Detach'}</button>
          )}
        </div>
      </div>

      {selectedPort && (
        <PortDiagnosticsCard
          port={selectedPort}
          node={attachedNode}
          observation={vmObservation}
          nodeLoading={nodeCatalog.loading}
          liveAvailable={bridge.available}
        />
      )}
      {!bridge.available && <ErrorState title="Open PVN inside Proxmox" message="VM configuration writes are accepted only through the authenticated PVN Datacenter menu." />}
      {(error || catalogError) && <ErrorState title="VM port operation failed" message={error || catalogError} onRetry={refresh} />}
      {message && <div className="inline-success" role="status">{message}</div>}
      {busyStep && <div className="workflow-progress" role="status"><span className="spinner" /><strong>{stepLabels[busyStep]}</strong><span>The VM NIC remains fail-closed during this step.</span></div>}
      {vmConfig && <div className="nic-list">
        <div className="nic-list-heading"><strong>{nics.length} configured NICs</strong><span>VM status: {vmStatus}</span></div>
        {nics.length ? nics.map(([key, value]) => <div className="nic-row" key={key}><code>{key}</code><span>{String(value)}</span></div>) : <p className="muted">This VM has no configured NICs.</p>}
      </div>}
    </section>
  );
}
