export function LoadingState({ label = 'Loading PVN data' }: { label?: string }) {
  return (
    <div className="state-card" role="status">
      <span className="spinner" aria-hidden="true" />
      <div>
        <strong>{label}</strong>
        <p>Synchronizing with the local PVN manager.</p>
      </div>
    </div>
  );
}
