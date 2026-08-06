import { useEffect, useMemo, useState } from 'react';
import { ApiError } from '../api/client';
import { useApi } from '../api/context';
import type {
  DefaultSecurityGroupBackfillPlan,
  DefaultSecurityGroupBackfillProject,
  DefaultSecurityGroupBackfillReport,
} from '../api/types';
import { ErrorState } from './ErrorState';
import { StatusPill } from './StatusPill';

const uuidPattern = /\b[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}\b/gi;
const unavailableStatuses = new Set([403, 404, 405, 501]);

export function backfillDisplayName(value: unknown, fallback: string): string {
  if (typeof value !== 'string') return fallback;
  const redacted = value.replace(uuidPattern, '').replace(/\s{2,}/g, ' ').trim();
  return redacted ? redacted.slice(0, 120) : fallback;
}

function endpointUnavailable(reason: unknown): boolean {
  return reason instanceof ApiError && unavailableStatuses.has(reason.status);
}

function blockedReason(project: DefaultSecurityGroupBackfillProject): string {
  switch (project.blocked_reason) {
  case 'default_name_collision':
    return 'An existing policy conflicts with the reserved default name.';
  case 'deterministic_group_malformed':
    return 'The reserved default security group does not match the baseline policy.';
  case 'deterministic_rule_malformed':
    return 'A reserved default security-group rule does not match the baseline policy.';
  case 'project_deleting':
    return 'The project is being deleted.';
  default:
    return 'The reserved default policy is not ready.';
  }
}

function BackfillReport({ report }: { report: DefaultSecurityGroupBackfillReport }) {
  const failures = report.results.filter((result) => result.status === 'failed');
  return (
    <div className={report.failed ? 'backfill-report backfill-report-warning' : 'backfill-report'} role="status">
      <strong>{report.dry_run ? 'Dry-run complete' : 'Backfill complete'}</strong>
      <span>
        {report.dry_run ? `${report.planned} planned` : `${report.migrated} migrated`}
        {` · ${report.skipped} skipped · ${report.failed} failed`}
      </span>
      {failures.length > 0 && (
        <div className="backfill-failures">
          <strong>Needs attention</strong>
          {failures.map((result) => (
            <span key={`${result.project_id}-${result.port_id}`}>
              {backfillDisplayName(result.port_name, 'Unavailable port')} · {backfillDisplayName(result.project_name, 'Unavailable project')}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

export function DefaultSecurityGroupBackfillPanel({ onApplied }: { onApplied?: () => void }) {
  const api = useApi();
  const [plan, setPlan] = useState<DefaultSecurityGroupBackfillPlan>();
  const [preview, setPreview] = useState<DefaultSecurityGroupBackfillReport>();
  const [report, setReport] = useState<DefaultSecurityGroupBackfillReport>();
  const [confirmation, setConfirmation] = useState('');
  const [busy, setBusy] = useState<'refresh' | 'dry-run' | 'apply' | ''>('');
  const [error, setError] = useState('');
  const [hidden, setHidden] = useState(false);

  useEffect(() => {
    let current = true;
    void api.defaultSecurityGroupBackfillPlan()
      .then((value) => { if (current) setPlan(value); })
      .catch((reason: unknown) => {
        if (!current) return;
        if (endpointUnavailable(reason)) setHidden(true);
        else setError('The administrator backfill plan could not be loaded.');
      });
    return () => { current = false; };
  }, [api]);

  const candidates = useMemo(() => (plan?.projects || []).flatMap((project) =>
    (project.legacy_ports || []).map((port) => ({ project, port }))), [plan]);
  const blockedProjects = useMemo(() => (plan?.projects || []).filter((project) =>
    !project.default_ready && project.legacy_ports.length > 0), [plan]);
  const expectedCluster = preview?.cluster || plan?.cluster || '';
  const previewAllowsApply = Boolean(
    preview?.dry_run && preview.plan_token && preview.plan_token === plan?.plan_token
      && preview.cluster === plan?.cluster && preview.planned > preview.failed && plan?.can_apply,
  );
  const exactConfirmation = Boolean(expectedCluster && confirmation === expectedCluster);

  async function refresh() {
    setBusy('refresh');
    setError('');
    try {
      const value = await api.defaultSecurityGroupBackfillPlan();
      setPlan(value);
      setPreview(undefined);
      setReport(undefined);
      setConfirmation('');
    } catch (reason) {
      if (endpointUnavailable(reason)) setHidden(true);
      else setError('The administrator backfill plan could not be refreshed.');
    } finally {
      setBusy('');
    }
  }

  async function dryRun() {
    if (!plan) return;
    const planToken = plan.plan_token;
    setBusy('dry-run');
    setError('');
    setPreview(undefined);
    setReport(undefined);
    setConfirmation('');
    try {
      const value = await api.applyDefaultSecurityGroupBackfill({ plan_token: planToken });
      if (!value.dry_run) throw new Error('unexpected non-dry-run response');
      setPreview(value);
    } catch (reason) {
      if (endpointUnavailable(reason)) setHidden(true);
      else if (reason instanceof ApiError && reason.code === 'backfill_plan_stale') {
        await recoverStalePlan();
      }
      else setError('The backfill dry-run could not be completed. No ports were changed.');
    } finally {
      setBusy('');
    }
  }

  async function apply() {
    if (!previewAllowsApply || !exactConfirmation) return;
    setBusy('apply');
    setError('');
    try {
      const value = await api.applyDefaultSecurityGroupBackfill({
        dry_run: false,
        confirm: confirmation,
        plan_token: preview!.plan_token,
      });
      if (value.dry_run) throw new Error('unexpected dry-run response');
      setReport(value);
      setPreview(undefined);
      setConfirmation('');
      onApplied?.();
      try {
        setPlan(await api.defaultSecurityGroupBackfillPlan());
      } catch (reason) {
        if (endpointUnavailable(reason)) setHidden(true);
        else setError('The backfill finished, but its refreshed plan could not be loaded.');
      }
    } catch (reason) {
      if (endpointUnavailable(reason)) setHidden(true);
      else if (reason instanceof ApiError && reason.code === 'backfill_plan_stale') {
        await recoverStalePlan();
      }
      else {
        setPreview(undefined);
        setConfirmation('');
        setError('The backfill could not be applied. Run a new dry-run before trying again.');
      }
    } finally {
      setBusy('');
    }
  }

  async function recoverStalePlan() {
    setPreview(undefined);
    setReport(undefined);
    setConfirmation('');
    try {
      setPlan(await api.defaultSecurityGroupBackfillPlan());
      setError('The backfill plan changed. Review the refreshed ports and run a new dry-run before applying.');
    } catch (reason) {
      if (endpointUnavailable(reason)) setHidden(true);
      else setError('The backfill plan changed and could not be refreshed. Refresh it before running a new dry-run.');
    }
  }

  if (hidden || (!plan && !error)) return null;
  if (!plan) {
    return (
      <section className="resource-section compact-section">
        <ErrorState title="Backfill plan unavailable" message={error} onRetry={() => void refresh()} />
      </section>
    );
  }

  return (
    <section className="resource-section compact-section backfill-section" aria-labelledby="default-sg-backfill-title">
      <div className="page-heading">
        <div>
          <span className="eyebrow">Administrator maintenance</span>
          <h1 id="default-sg-backfill-title">Legacy security policy backfill</h1>
          <p>Move ports created before project defaults from unrestricted policy to their reserved default security group.</p>
        </div>
        <div className="heading-actions">
          <button className="button button-secondary" disabled={Boolean(busy)} onClick={() => void refresh()}>
            {busy === 'refresh' ? 'Refreshing…' : 'Refresh'}
          </button>
          <button className="button button-secondary" disabled={Boolean(busy) || plan.total_legacy_ports < 1} onClick={() => void dryRun()}>
            {busy === 'dry-run' ? 'Checking…' : 'Dry-run'}
          </button>
        </div>
      </div>

      <div className="panel-card backfill-panel">
        <div className="backfill-summary">
          <div><strong>{plan.total_legacy_ports}</strong><span>legacy unrestricted ports</span></div>
          <div><strong>{plan.total_attached_ports}</strong><span>attached VM ports</span></div>
          <StatusPill value={plan.total_legacy_ports === 0 ? 'complete' : plan.can_apply ? 'ready' : 'blocked'} />
        </div>

        {plan.total_attached_ports > 0 && (
          <div className="backfill-traffic-warning" role="alert">
            <strong>Attached traffic will change immediately</strong>
            <span>{plan.total_attached_ports} attached VM port{plan.total_attached_ports === 1 ? '' : 's'} will move from unrestricted traffic to the project default policy. Active connectivity may change.</span>
          </div>
        )}

        {blockedProjects.length > 0 && (
          <div className="backfill-blockers">
            {blockedProjects.map((project) => (
              <span key={project.project_id}>
                <strong>{backfillDisplayName(project.project_name, 'Unavailable project')}</strong>
                {blockedReason(project)}
              </span>
            ))}
          </div>
        )}

        {candidates.length > 0 ? (
          <div className="backfill-candidates" role="list" aria-label="Legacy unrestricted ports">
            {candidates.map(({ project, port }) => (
              <div className="backfill-candidate" role="listitem" key={`${project.project_id}-${port.port_id}`}>
                <div>
                  <strong>{backfillDisplayName(port.port_name, 'Unavailable port')}</strong>
                  <span>{backfillDisplayName(project.project_name, 'Unavailable project')}</span>
                </div>
                {port.attached ? (
                  <div className="backfill-attachment">
                    <StatusPill value="attached" />
                    <span>
                      {port.vmid ? `VM ${port.vmid}` : 'Unavailable VM'} · {backfillDisplayName(port.nic, 'Unavailable NIC')} on {backfillDisplayName(port.node_name, 'Unavailable node')}
                    </span>
                  </div>
                ) : <span className="muted">Not attached</span>}
              </div>
            ))}
          </div>
        ) : <div className="backfill-empty">No legacy unrestricted ports remain.</div>}

        {preview && <BackfillReport report={preview} />}
        {report && <BackfillReport report={report} />}
        {error && <p className="inline-error" role="alert">{error}</p>}

        {previewAllowsApply && (
          <div className="backfill-confirmation">
            <label className="form-field">
              <span>Type <strong>{expectedCluster}</strong> to confirm this traffic-policy change</span>
              <input value={confirmation} onChange={(event) => setConfirmation(event.target.value)} autoComplete="off" />
            </label>
            <button
              className="button button-danger"
              disabled={Boolean(busy) || !previewAllowsApply || !exactConfirmation}
              onClick={() => void apply()}
            >
              {busy === 'apply' ? 'Applying…' : 'Apply backfill'}
            </button>
          </div>
        )}
      </div>
    </section>
  );
}
