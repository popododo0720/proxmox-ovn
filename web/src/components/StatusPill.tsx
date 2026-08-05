export function StatusPill({ value }: { value: unknown }) {
  const label = String(value ?? 'unknown');
  const tone = /^(active|online|ready|succeeded|complete|completed|bound|leader|voter)$/i.test(label)
    ? 'good'
    : /^(error|failed|offline|degraded|blocked)$/i.test(label)
      ? 'bad'
      : /^(creating|updating|deleting|running|pending|queued)$/i.test(label)
        ? 'busy'
        : 'neutral';
  return <span className={`status-pill status-${tone}`}>{label}</span>;
}
