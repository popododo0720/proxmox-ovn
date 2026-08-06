import { useEffect, useRef, type ReactNode } from 'react';
import type { BaseResource } from '../api/types';
import { CopyableID } from './CopyableID';

function fieldLabel(value: string): string {
  return value.replaceAll('_', ' ').replace(/\b\w/g, (letter) => letter.toUpperCase());
}

function isIDField(field: string): boolean {
  return field === 'id' || field.endsWith('_id') || field.endsWith('_ids');
}

function detailValue(value: unknown, field: string, path: string): ReactNode {
  if (value === null || value === undefined || value === '') return <span className="muted">—</span>;
  if (typeof value === 'string') {
    return isIDField(field) ? <CopyableID value={value} /> : <span>{value}</span>;
  }
  if (typeof value === 'number' || typeof value === 'boolean') return <span>{String(value)}</span>;
  if (Array.isArray(value)) {
    if (value.length === 0) return <span className="muted">—</span>;
    return (
      <div className="detail-list">
        {value.map((item, index) => (
          <div key={`${path}-${index}`}>{detailValue(item, field.endsWith('_ids') ? 'id' : field, `${path}-${index}`)}</div>
        ))}
      </div>
    );
  }
  if (typeof value === 'object') {
    return (
      <dl className="nested-details">
        {Object.entries(value as Record<string, unknown>).map(([key, item]) => (
          <div key={`${path}-${key}`}>
            <dt>{fieldLabel(key)}</dt>
            <dd>{detailValue(item, key, `${path}-${key}`)}</dd>
          </div>
        ))}
      </dl>
    );
  }
  return <span>{String(value)}</span>;
}

function resourceTitle(resource: BaseResource): string {
  for (const key of ['name', 'address', 'cidr']) {
    const value = resource[key];
    if (value !== null && value !== undefined && value !== '') return String(value);
  }
  return 'Resource';
}

export function ResourceDetailsDialog({ resource, onClose }: { resource: BaseResource; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const element = dialog.current;
    if (element && !element.open) element.showModal();
  }, []);

  return (
    <dialog
      className="resource-dialog resource-details-dialog"
      ref={dialog}
      onCancel={(event) => { event.preventDefault(); onClose(); }}
      onClose={onClose}
    >
      <div className="resource-details-content">
        <div className="dialog-heading">
          <div>
            <span className="eyebrow">Resource details</span>
            <h2>{resourceTitle(resource)}</h2>
          </div>
          <button type="button" className="icon-button" aria-label="Close" onClick={onClose}>×</button>
        </div>
        <dl className="resource-details">
          {Object.entries(resource).map(([key, value]) => (
            <div key={key}>
              <dt>{fieldLabel(key)}</dt>
              <dd>{detailValue(value, key, key)}</dd>
            </div>
          ))}
        </dl>
        <div className="dialog-actions">
          <button type="button" className="button button-secondary" onClick={onClose}>Close</button>
        </div>
      </div>
    </dialog>
  );
}
