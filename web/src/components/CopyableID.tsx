import { useEffect, useRef, useState } from 'react';

async function copyText(value: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value);
    return;
  }
  const input = document.createElement('textarea');
  input.value = value;
  input.setAttribute('readonly', '');
  input.style.position = 'fixed';
  input.style.opacity = '0';
  document.body.appendChild(input);
  input.select();
  const copied = document.execCommand('copy');
  input.remove();
  if (!copied) throw new Error('Copy is not supported by this browser');
}

export function CopyableID({ value }: { value: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'error'>('idle');
  const resetTimer = useRef<number | undefined>(undefined);

  useEffect(() => () => window.clearTimeout(resetTimer.current), []);

  async function copy() {
    window.clearTimeout(resetTimer.current);
    try {
      await copyText(value);
      setState('copied');
    } catch {
      setState('error');
    }
    resetTimer.current = window.setTimeout(() => setState('idle'), 1800);
  }

  const label = state === 'copied' ? 'Copied' : state === 'error' ? 'Copy failed' : 'Copy';
  return (
    <span className="copyable-id">
      <code title={value}>{value}</code>
      <button type="button" onClick={() => void copy()} aria-label={`Copy resource ID ${value}`} title={label}>
        {label}
      </button>
    </span>
  );
}
