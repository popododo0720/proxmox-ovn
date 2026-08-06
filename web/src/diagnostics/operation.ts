import type { Operation } from '../api/types';

const uuidPattern = /[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}/gi;
const maxSummaryLength = 300;

export function operationErrorSummary(operation: Pick<Operation, 'error' | 'target_id'>): string {
  let summary = operation.error?.trim() || '';
  if (!summary) return '';

  const targetID = operation.target_id?.trim();
  if (targetID) summary = summary.split(targetID).join('[resource]');
  summary = summary.replace(uuidPattern, '[resource]');

  return summary.length > maxSummaryLength
    ? `${summary.slice(0, maxSummaryLength - 1)}…`
    : summary;
}
