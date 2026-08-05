import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { useApi } from '../api/context';
import type { BaseResource } from '../api/types';
import { CreateDialog, type FormField } from './CreateDialog';
import { EmptyState } from './EmptyState';
import { ErrorState } from './ErrorState';
import { LoadingState } from './LoadingState';
import { StatusPill } from './StatusPill';

export interface Column<T> {
  key: keyof T | string;
  label: string;
  render?: (item: T) => ReactNode;
  className?: string;
}

export interface ResourcePageProps<T extends BaseResource> {
  title: string;
  description: string;
  endpoint: string;
  columns: Column<T>[];
  createLabel?: string;
  createFields?: FormField[];
  allowDelete?: boolean;
  createResource?: (payload: Record<string, unknown>) => Promise<T>;
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
    if (value.every((item) => typeof item !== 'object')) return value.join(', ');
    return value.map((item) => {
      if (item && typeof item === 'object' && 'ip_address' in item) return String(item.ip_address);
      if (item && typeof item === 'object' && 'address' in item) return String(item.address);
      return JSON.stringify(item);
    }).join(', ');
  }
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

export function ResourcePage<T extends BaseResource>({
  title,
  description,
  endpoint,
  columns,
  createLabel,
  createFields,
  allowDelete = false,
  createResource,
  deleteResource,
  compact = false,
  emptyMessage = 'Create the first resource when the cluster is ready.',
}: ResourcePageProps<T>) {
  const api = useApi();
  const [items, setItems] = useState<T[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [query, setQuery] = useState('');
  const [reloadKey, setReloadKey] = useState(0);
  const [createOpen, setCreateOpen] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const result = await api.list<T>(endpoint);
      setItems(result.items);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Unexpected API response');
    } finally {
      setLoading(false);
    }
  }, [api, endpoint]);

  useEffect(() => {
    void load();
  }, [load, reloadKey]);

  const visibleItems = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return items;
    return items.filter((item) => JSON.stringify(item).toLowerCase().includes(needle));
  }, [items, query]);

  async function remove(item: T) {
    const label = item.name || item.id;
    if (!window.confirm(`Delete ${label}? This request is reconciled through OVN.`)) return;
    setDeleting(item.id);
    try {
      if (deleteResource) await deleteResource(item);
      else await api.remove(endpoint, item.id, item.revision);
      await load();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Delete failed');
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
          <button className="button button-secondary" onClick={() => setReloadKey((value) => value + 1)} disabled={loading}>
            Refresh
          </button>
          {createFields && (
            <button className="button button-primary" onClick={() => setCreateOpen(true)}>
              + {createLabel || title.replace(/s$/, '')}
            </button>
          )}
        </div>
      </div>

      {loading ? <LoadingState label={`Loading ${title.toLowerCase()}`} /> : error ? (
        <ErrorState message={error} onRetry={() => setReloadKey((value) => value + 1)} />
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
                  {allowDelete && <th className="actions-column"><span className="sr-only">Actions</span></th>}
                </tr>
              </thead>
              <tbody>
                {visibleItems.map((item) => (
                  <tr key={item.id}>
                    {columns.map((column) => {
                      const value = readPath(item, String(column.key));
                      return (
                        <td className={column.className} key={String(column.key)}>
                          {column.render ? column.render(item) : /^(status|state)$/.test(String(column.key)) ? <StatusPill value={value} /> : formatValue(value)}
                        </td>
                      );
                    })}
                    {allowDelete && (
                      <td className="actions-column">
                        <button className="table-action danger" disabled={deleting === item.id} onClick={() => void remove(item)}>
                          {deleting === item.id ? 'Deleting…' : 'Delete'}
                        </button>
                      </td>
                    )}
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
            await load();
          }}
        />
      )}
    </section>
  );
}
