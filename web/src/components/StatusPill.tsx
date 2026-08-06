export function StatusPill({ value }: { value: unknown }) {
  const label = String(value ?? 'unknown');
  const tone = /^(ok|active|online|ready|succeeded|complete|completed|bound|leader|voter|matched|healthy)$/i.test(label)
    ? 'good'
    : /^(error|failed|offline|degraded|blocked|unavailable|disabled|mismatch|missing)$/i.test(label)
      ? 'bad'
      : /^(creating|updating|deleting|running|pending|queued|binding|detaching)$/i.test(label)
        ? 'busy'
        : 'neutral';
  return <span className={`status-pill status-${tone}`}>{label}</span>;
}
