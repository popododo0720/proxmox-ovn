import { useMemo, useState, type ReactNode } from 'react';
import { useApi } from '../api/context';
import type { BaseResource } from '../api/types';
import { redactResourceIDs, uiErrorMessage } from '../diagnostics/display';
import { CreateDialog, type FormField } from './CreateDialog';
import { EmptyState } from './EmptyState';
import { ErrorState } from './ErrorState';
import { LoadingState } from './LoadingState';
import { ReferenceLabel } from './ReferenceLabel';
import { useResourceCatalog } from './ResourceCatalog';
import { ResourceDetailsDialog } from './ResourceDetailsDialog';
import type { ResourceReference } from './ResourceSelect';
import { StatusPill } from './StatusPill';

export interface Column<T> {
  key: keyof T | string;
  label: string;
  render?: (item: T) => ReactNode;
  className?: string;
  reference?: ResourceReference | ((item: T) => ResourceReference | undefined);
}

export interface ResourcePageProps<T extends BaseResource> {
  title: string;
  description: string;
  endpoint: string;
  columns: Column<T>[];
  createLabel?: string;
  createFields?: FormField[];
  editFields?: FormField[];
  allowDelete?: boolean;
  createResource?: (payload: Record<string, unknown>) => Promise<T>;
  updateResource?: (item: T, payload: Record<string, unknown>) => Promise<T>;
  deleteResource?: (item: T) => Promise<void>;
  compact?: boolean;
  emptyMessage?: string;
}

function readPath(value: unknown, path: string): unknown {
  return path.split('.').reduce<unknown>((current, key) => {
    if (!current || typeof current !== 'object') return undefined;
    return (current as Record<string, unknown>)[key];
  }, value);
}

export function formatValue(value: unknown): ReactNode {
  if (value === null || value === undefined || value === '') return <span className="muted">—</span>;
  if (typeof value === 'boolean') return value ? 'Yes' : 'No';
  if (Array.isArray(value)) {
    if (!value.length) return <span className="muted">—</span>;
    if (value.every((item) => typeof item !== 'object')) return redactResourceIDs(value.join(', '));
    return value.map((item) => {
      if (item && typeof item === 'object' && 'ip_address' in item) return String(item.ip_address);
      if (item && typeof item === 'object' && 'address' in item) return String(item.address);
      return redactResourceIDs(JSON.stringify(item));
    }).join(', ');
  }
  if (typeof value === 'object') return redactResourceIDs(JSON.stringify(value));
  return redactResourceIDs(String(value));
}

function resourceLabel(resource: BaseResource): string {
  for (const key of ['name', 'address', 'cidr']) {
    const value = resource[key];
    if (value !== null && value !== undefined && value !== '') return String(value);
  }
  return 'resource';
}

function editableResource(resource: BaseResource): Record<string, unknown> {
  const value = { ...resource };
  delete value.managed;
  delete value.read_only;
  return value;
}

export function ResourcePage<T extends BaseResource>({
  title,
  description,
  endpoint,
  columns,
  createLabel,
  createFields,
  editFields,
  allowDelete = false,
  createResource,
  updateResource,
  deleteResource,
  compact = false,
  emptyMessage = 'Create the first resource when the cluster is ready.',
}: ResourcePageProps<T>) {
  const api = useApi();
  const catalog = useResourceCatalog(endpoint);
  const items = catalog.items as unknown as T[];
  const [actionError, setActionError] = useState('');
  const [query, setQuery] = useState('');
  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<T | null>(null);
  const [viewing, setViewing] = useState<T | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const error = actionError || catalog.error;

  const visibleItems = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return items;
    return items.filter((item) => JSON.stringify(item).toLowerCase().includes(needle));
  }, [items, query]);

  async function remove(item: T) {
    const label = resourceLabel(item);
    if (!window.confirm(`Delete ${label}? This request is reconciled through OVN.`)) return;
    setDeleting(item.id);
    setActionError('');
    try {
      if (deleteResource) await deleteResource(item);
      else await api.remove(endpoint, item.id, item.revision);
      catalog.invalidate();
    } catch (reason) {
      setActionError(uiErrorMessage(reason, 'Delete failed', [{ id: item.id, name: label }]));
    } finally {
      setDeleting(null);
    }
  }

  return (
    <section className={`resource-section${compact ? ' compact-section' : ''}`}>
      <div className="page-heading">
        <div>
          <span className="eyebrow">PVN control plane</span>
          <h1>{title}</h1>
          <p>{description}</p>
        </div>
        <div className="heading-actions">
          <button
            className="button button-secondary"
            onClick={() => {
              setActionError('');
              void catalog.retry().catch((reason: unknown) => {
                setActionError(uiErrorMessage(reason, 'Refresh failed'));
              });
            }}
            disabled={catalog.loading}
          >
            Refresh
          </button>
          {createFields && (
            <button className="button button-primary" onClick={() => setCreateOpen(true)}>
              + {createLabel || title.replace(/s$/, '')}
            </button>
          )}
        </div>
      </div>

      {catalog.loading ? <LoadingState label={`Loading ${title.toLowerCase()}`} /> : error ? (
        <ErrorState
          message={error}
          onRetry={() => {
            setActionError('');
            void catalog.retry().catch(() => undefined);
          }}
        />
      ) : items.length === 0 ? (
        <EmptyState title={`No ${title.toLowerCase()} yet`} message={emptyMessage} />
      ) : (
        <div className="table-card">
          <div className="table-toolbar">
            <label className="search-field">
              <span aria-hidden="true">⌕</span>
              <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder={`Filter ${title.toLowerCase()}`} />
            </label>
            <span>{visibleItems.length} of {items.length}</span>
          </div>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  {columns.map((column) => <th className={column.className} key={String(column.key)}>{column.label}</th>)}
                  <th className="actions-column"><span className="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                {visibleItems.map((item) => (
                  <tr key={item.id}>
                    {columns.map((column) => {
                      const value = readPath(item, String(column.key));
                      const reference = typeof column.reference === 'function'
                        ? column.reference(item)
                        : column.reference;
                      return (
                        <td className={column.className} key={String(column.key)}>
                          {column.render
                            ? column.render(item)
                            : column.reference !== undefined
                              ? reference
                                ? <ReferenceLabel value={value} source={reference} />
                                : <span className="muted">Unavailable</span>
                              : /^(status|state)$/.test(String(column.key))
                                ? <StatusPill value={value} />
                                : formatValue(value)}
                        </td>
                      );
                    })}
                    <td className="actions-column">
                      <span className="table-actions">
                        <button className="table-action" disabled={deleting === item.id} onClick={() => setViewing(item)}>Details</button>
                        {editFields && item.read_only !== true && <button className="table-action" disabled={deleting === item.id} onClick={() => setEditing(item)}>Edit</button>}
                        {allowDelete && item.read_only !== true && (
                          <button className="table-action danger" disabled={deleting === item.id} onClick={() => void remove(item)}>
                            {deleting === item.id ? 'Deleting…' : 'Delete'}
                          </button>
                        )}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {visibleItems.length === 0 && <EmptyState title="No matches" message="Try a different filter." />}
        </div>
      )}

      {createFields && (
        <CreateDialog
          title={createLabel || title.replace(/s$/, '')}
          fields={createFields}
          open={createOpen}
          onClose={() => setCreateOpen(false)}
          onSubmit={async (payload) => {
            if (createResource) await createResource(payload);
            else await api.create<T>(endpoint, payload);
            catalog.invalidate();
          }}
        />
      )}
      {editFields && editing && (
        <CreateDialog
          key={`${editing.id}-${editing.revision || 0}`}
          title={`Edit ${resourceLabel(editing)}`}
          fields={editFields}
          values={editing}
          mode="edit"
          open
          onClose={() => setEditing(null)}
          onSubmit={async (payload) => {
            if (updateResource) await updateResource(editing, payload);
            else await api.update<T>(endpoint, editing.id, { ...editableResource(editing), ...payload }, editing.revision);
            catalog.invalidate();
          }}
        />
      )}
      {viewing && <ResourceDetailsDialog resource={viewing} onClose={() => setViewing(null)} />}
    </section>
  );
}
