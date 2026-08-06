import type { ResourceReference } from './ResourceSelect';
import { resourceOptionLabel } from './ResourceSelect';
import { useResourceCatalog } from './ResourceCatalog';

export function ReferenceLabel({
  value,
  source,
}: {
  value: unknown;
  source: ResourceReference;
}) {
  const ids = Array.isArray(value)
    ? value.map(String).filter(Boolean)
    : value === null || value === undefined || value === '' ? [] : [String(value)];
  const { items, loading } = useResourceCatalog(source.endpoint, ids.length > 0);

  if (ids.length === 0) return <span className="muted">—</span>;
  return (
    <span className="reference-label">
      {ids.map((id, index) => {
        const resource = items.find((item) => item.id === id);
        const label = resource
          ? resourceOptionLabel(resource, source)
          : loading ? 'Loading…' : 'Unavailable';
        return <span className={!resource ? 'muted' : undefined} key={`${id}-${index}`}>{label}</span>;
      })}
    </span>
  );
}
