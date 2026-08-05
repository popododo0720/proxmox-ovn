import { useEffect, useState } from 'react';
import { useApi } from '../api/context';
import type { HealthStatus, NodeStatus, Operation, Project } from '../api/types';
import { ErrorState } from '../components/ErrorState';
import { LoadingState } from '../components/LoadingState';
import { StatusPill } from '../components/StatusPill';

interface OverviewData {
  health: HealthStatus;
  projects: Project[];
  nodes: NodeStatus[];
  operations: Operation[];
}

export function OverviewPage() {
  const api = useApi();
  const [data, setData] = useState<OverviewData | null>(null);
  const [error, setError] = useState('');
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let current = true;
    setData(null);
    setError('');
    void Promise.all([api.health(), api.projects(), api.nodes(), api.operations(8)])
      .then(([health, projects, nodes, operations]) => {
        if (current) setData({ health, projects: projects.items, nodes: nodes.items, operations: operations.items });
      })
      .catch((reason: unknown) => {
        if (current) setError(reason instanceof Error ? reason.message : 'The PVN overview is unavailable');
      });
    return () => { current = false; };
  }, [api, reloadKey]);

  return (
    <section>
      <div className="page-heading overview-heading">
        <div>
          <span className="eyebrow">Network fabric</span>
          <h1>Overview</h1>
          <p>Cluster-wide OVN health, capacity, and recent control-plane activity.</p>
        </div>
        <button className="button button-secondary" onClick={() => setReloadKey((value) => value + 1)}>Refresh</button>
      </div>
      {!data && !error && <LoadingState label="Reading cluster state" />}
      {error && <ErrorState message={error} onRetry={() => setReloadKey((value) => value + 1)} />}
      {data && (
        <>
          <div className="hero-card">
            <div>
              <span className="eyebrow">PVN control plane</span>
              <h2>{data.health.cluster || 'Proxmox cluster'}</h2>
              <p>{data.health.version ? `PVN ${data.health.version}` : 'Local manager'} · Open Virtual Network</p>
            </div>
            <StatusPill value={data.health.status} />
          </div>
          <div className="metric-grid">
            <Metric label="Projects" value={data.projects.length} detail="Mapped PVE pools" />
            <Metric label="Nodes enabled" value={data.nodes.filter((node) => node.enabled !== false).length} detail={`of ${data.nodes.length} installed`} />
            <Metric label="Central nodes" value={data.nodes.filter((node) => node.roles?.includes('central')).length} detail="OVSDB placement" />
            <Metric label="Operations" value={data.operations.filter((operation) => /running|pending|queued/i.test(operation.status || '')).length} detail="Pending or running" />
          </div>
          <div className="overview-grid">
            <section className="panel-card">
              <div className="panel-heading">
                <div><span className="eyebrow">Components</span><h2>Control plane</h2></div>
              </div>
              <HealthRow label="PVN database" value={data.health.database} />
              <HealthRow label="OVN northbound" value={data.health.ovn_northbound} />
              <HealthRow label="OVN southbound" value={data.health.ovn_southbound} />
              <HealthRow label="Reconciler" value={data.health.reconciler} />
              <HealthRow label="All online PVE nodes" value={data.health.capacity?.ready ? 'ready' : 'degraded'} />
              {data.health.capacity?.reason && <p className="muted">{data.health.capacity.reason}</p>}
            </section>
            <section className="panel-card">
              <div className="panel-heading">
                <div><span className="eyebrow">Audit trail</span><h2>Recent operations</h2></div>
              </div>
              {data.operations.length ? data.operations.slice(0, 6).map((operation) => (
                <div className="activity-row" key={operation.id}>
                  <span className="activity-mark" aria-hidden="true" />
                  <div>
                    <strong>{operation.action || operation.kind || 'Operation'}</strong>
                    <p>{operation.target_kind || 'resource'} · {operation.target_id || 'control plane'}</p>
                  </div>
                  <StatusPill value={operation.status} />
                </div>
              )) : <p className="muted">No recent operations.</p>}
            </section>
          </div>
        </>
      )}
    </section>
  );
}

function Metric({ label, value, detail }: { label: string; value: number; detail: string }) {
  return <div className="metric-card"><span>{label}</span><strong>{value}</strong><p>{detail}</p></div>;
}

function HealthRow({ label, value }: { label: string; value: unknown }) {
  return <div className="health-row"><span>{label}</span><StatusPill value={value} /></div>;
}
