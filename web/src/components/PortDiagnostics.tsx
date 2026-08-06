import type { NodeStatus, Port } from '../api/types';
import {
  diagnosePort,
  nodeDisplayName,
  portDisplayName,
  vmDisplayName,
  type VMObservation,
} from '../diagnostics/port';
import { useResourceCatalog } from './ResourceCatalog';
import { StatusPill } from './StatusPill';

function findPortNode(port: Port, nodes: NodeStatus[]): NodeStatus | undefined {
  return nodes.find((node) =>
    (port.node_id && node.id === port.node_id) ||
    (port.requested_chassis && node.chassis_id === port.requested_chassis));
}

function DiagnosticWarnings({ warnings }: { warnings: string[] }) {
  if (!warnings.length) return null;
  return (
    <div className="diagnostic-warnings">
      {warnings.map((warning) => <span key={warning}>{warning}</span>)}
    </div>
  );
}

export function PortDiagnosticCell({ port }: { port: Port }) {
  const catalog = useResourceCatalog('/nodes', Boolean(port.node_id || port.requested_chassis));
  const node = findPortNode(port, catalog.items as unknown as NodeStatus[]);
  if ((port.node_id || port.requested_chassis) && catalog.loading) {
    return <span className="muted">Loading node diagnostics…</span>;
  }
  const diagnostic = diagnosePort(port, node);
  return (
    <div className={`port-diagnostic-cell diagnostic-${diagnostic.tone}`}>
      <div className="diagnostic-pills">
        <StatusPill value={port.state || 'unknown'} />
        <StatusPill value={diagnostic.chassis} />
      </div>
      <span>{diagnostic.reason}</span>
      <DiagnosticWarnings warnings={diagnostic.warnings} />
    </div>
  );
}

export function PortDiagnosticsCard({
  port,
  node,
  observation,
  nodeLoading = false,
  liveAvailable,
}: {
  port: Port;
  node?: NodeStatus;
  observation?: VMObservation;
  nodeLoading?: boolean;
  liveAvailable: boolean;
}) {
  const diagnostic = diagnosePort(port, node, observation);
  return (
    <div className="port-diagnostic-card" role="region" aria-label={`Diagnostics for ${portDisplayName(port)}`}>
      <div className="port-diagnostic-heading">
        <div>
          <span className="eyebrow">Selected port diagnostics</span>
          <h2>{portDisplayName(port)}</h2>
        </div>
        <StatusPill value={port.binding_status || 'unknown'} />
      </div>
      <div className="port-diagnostic-grid">
        <div><span>VM</span><strong>{vmDisplayName(port, observation)}</strong></div>
        <div><span>Node</span><strong>{nodeLoading ? 'Loading…' : nodeDisplayName(node)}</strong></div>
        <div><span>Control</span><StatusPill value={port.state || 'unknown'} /></div>
        <div><span>Chassis</span><StatusPill value={diagnostic.chassis} /></div>
      </div>
      <div className={`port-diagnostic-reason diagnostic-${diagnostic.tone}`}>
        <strong>Why this link is {port.binding_status === 'bound' ? 'up or degraded' : 'down'}</strong>
        <span>{diagnostic.reason}</span>
      </div>
      <DiagnosticWarnings warnings={diagnostic.warnings} />
      <p className="diagnostic-scope">
        {observation
          ? 'Live PVE VM/NIC state and the authenticated runtime resolver are included.'
          : liveAvailable
            ? 'Select Inspect to add live PVE VM/NIC and runtime resolver evidence.'
            : 'Open PVN inside Proxmox to add live VM/NIC evidence.'}
        {' '}Actual OVN Southbound chassis state is not queried by this view.
      </p>
    </div>
  );
}
