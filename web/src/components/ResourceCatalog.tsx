import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type PropsWithChildren,
} from 'react';
import type { ApiClient } from '../api/client';
import type { BaseResource } from '../api/types';

interface CatalogEntry {
  items?: BaseResource[];
  error?: string;
  promise?: Promise<BaseResource[]>;
}

interface ResourceCatalogValue {
  revision: number;
  entry: (endpoint: string) => CatalogEntry | undefined;
  load: (endpoint: string) => Promise<BaseResource[]>;
  reload: (endpoint: string) => Promise<BaseResource[]>;
  invalidate: (endpoint: string) => void;
}

const ResourceCatalogContext = createContext<ResourceCatalogValue | null>(null);

function errorMessage(reason: unknown): string {
  return reason instanceof Error ? reason.message : 'Resource names are unavailable';
}

export function ResourceCatalogProvider({ client, children }: PropsWithChildren<{ client: ApiClient }>) {
  const cache = useMemo(() => new Map<string, CatalogEntry>(), [client]);
  const [revision, setRevision] = useState(0);
  const changed = useCallback(() => setRevision((value) => value + 1), []);

  const load = useCallback((endpoint: string): Promise<BaseResource[]> => {
    const current = cache.get(endpoint);
    if (current?.items) return Promise.resolve(current.items);
    if (current?.promise) return current.promise;
    if (current?.error) return Promise.reject(new Error(current.error));

    const promise = client.list<BaseResource>(endpoint)
      .then((result) => {
        cache.set(endpoint, { items: result.items });
        changed();
        return result.items;
      })
      .catch((reason: unknown) => {
        const message = errorMessage(reason);
        cache.set(endpoint, { error: message });
        changed();
        throw reason;
      });
    cache.set(endpoint, { promise });
    changed();
    return promise;
  }, [cache, changed, client]);

  const invalidate = useCallback((endpoint: string) => {
    if (cache.delete(endpoint)) changed();
  }, [cache, changed]);

  const reload = useCallback((endpoint: string) => {
    cache.delete(endpoint);
    changed();
    return load(endpoint);
  }, [cache, changed, load]);

  const value = useMemo<ResourceCatalogValue>(() => ({
    revision,
    entry: (endpoint) => cache.get(endpoint),
    load,
    reload,
    invalidate,
  }), [cache, invalidate, load, reload, revision]);

  return <ResourceCatalogContext.Provider value={value}>{children}</ResourceCatalogContext.Provider>;
}

export function useResourceCatalog(endpoint: string, active = true) {
  const catalog = useContext(ResourceCatalogContext);
  if (!catalog) throw new Error('ResourceCatalogProvider is required');
  const entry = catalog.entry(endpoint);

  useEffect(() => {
    if (!active || entry) return;
    void catalog.load(endpoint).catch(() => undefined);
  }, [active, catalog, endpoint, entry]);

  return {
    items: entry?.items ?? [],
    loading: active && (!entry || Boolean(entry.promise)),
    error: entry?.error ?? '',
    retry: () => catalog.reload(endpoint),
    invalidate: () => catalog.invalidate(endpoint),
  };
}
