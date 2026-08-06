import type { Operation } from '../api/types';
import { redactResourceIDs } from './display';

const maxSummaryLength = 300;

export function operationErrorSummary(
  operation: Pick<Operation, 'error' | 'target_id'>,
  targetName?: unknown,
): string {
  let summary = operation.error?.trim() || '';
  if (!summary) return '';

  summary = redactResourceIDs(summary, [{ id: operation.target_id, name: targetName }]);

  return summary.length > maxSummaryLength
    ? `${summary.slice(0, maxSummaryLength - 1)}…`
    : summary;
}
