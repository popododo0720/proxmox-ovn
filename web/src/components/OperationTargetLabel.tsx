import type { BaseResource, Operation, SecurityGroupRule } from '../api/types';
import { humanLabel } from '../diagnostics/display';
import {
  operationTargetReference,
  projectRouterReference,
  projectSecurityGroupReference,
  projectSubnetReference,
} from '../resources/references';
import { ReferenceLabel } from './ReferenceLabel';
import { useResourceCatalog } from './ResourceCatalog';
import { resourceOptionLabel, type ResourceReference } from './ResourceSelect';

function text(value: unknown): string {
  return humanLabel(value, '');
}

export function securityGroupRuleSummary(rule: SecurityGroupRule): string {
  const direction = text(rule.direction) || 'rule';
  const protocol = text(rule.protocol) || text(rule.ethertype) || 'any protocol';
  const minimum = typeof rule.port_range_min === 'number' ? rule.port_range_min : undefined;
  const maximum = typeof rule.port_range_max === 'number' ? rule.port_range_max : undefined;
  const port = minimum === undefined && maximum === undefined
    ? ''
    : minimum === maximum || maximum === undefined
      ? `port ${minimum}`
      : minimum === undefined
        ? `port ${maximum}`
        : `ports ${minimum}–${maximum}`;
  const remote = text(rule.remote_cidr);
  return [direction, protocol, port, remote].filter(Boolean).join(' · ');
}

function RouterInterfaceTarget({ target }: { target: BaseResource }) {
  if (!target.router_id && !target.subnet_id) return <>Router interface</>;
  return (
    <span className="operation-target-label">
      <ReferenceLabel value={target.router_id} source={projectRouterReference} />
      <span>→</span>
      <ReferenceLabel value={target.subnet_id} source={projectSubnetReference} />
    </span>
  );
}

function SecurityGroupRuleTarget({ target }: { target: SecurityGroupRule }) {
  return (
    <span className="operation-target-label">
      <ReferenceLabel value={target.security_group_id} source={projectSecurityGroupReference} />
      <span>·</span>
      <span>{securityGroupRuleSummary(target)}</span>
      {target.remote_group_id && (
        <>
          <span>· from</span>
          <ReferenceLabel value={target.remote_group_id} source={projectSecurityGroupReference} />
        </>
      )}
    </span>
  );
}

function ResolvedOperationTarget({
  operation,
  source,
}: {
  operation: Operation;
  source: ResourceReference;
}) {
  const targetID = typeof operation.target_id === 'string' ? operation.target_id : '';
  const catalog = useResourceCatalog(source.endpoint, Boolean(targetID));
  if (!targetID) return <span className="muted">—</span>;
  const target = catalog.items.find((item) => item.id === targetID);
  if (!target) return <span className="muted">{catalog.loading ? 'Loading…' : 'Unavailable'}</span>;

  if (operation.target_kind === 'router-interface') return <RouterInterfaceTarget target={target} />;
  if (operation.target_kind === 'security-group-rule') {
    return <SecurityGroupRuleTarget target={target as SecurityGroupRule} />;
  }
  return <>{resourceOptionLabel(target, source)}</>;
}

export function OperationTargetLabel({ operation }: { operation: Operation }) {
  const source = operationTargetReference(operation.target_kind);
  return source
    ? <ResolvedOperationTarget operation={operation} source={source} />
    : <span className="muted">Unavailable</span>;
}
