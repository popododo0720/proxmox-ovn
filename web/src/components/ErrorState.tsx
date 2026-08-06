import { redactResourceIDs } from '../diagnostics/display';

export function ErrorState({
  title = 'Could not load data',
  message,
  onRetry,
}: {
  title?: string;
  message: string;
  onRetry?: () => void;
}) {
  return (
    <div className="state-card state-error" role="alert">
      <span className="state-icon" aria-hidden="true">!</span>
      <div>
        <strong>{title}</strong>
        <p>{redactResourceIDs(message)}</p>
        {onRetry && <button className="button button-secondary" onClick={onRetry}>Try again</button>}
      </div>
    </div>
  );
}
