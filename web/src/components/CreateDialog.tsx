import { useEffect, useRef, useState, type FormEvent } from 'react';

export interface FormField {
  name: string;
  label: string;
  type?: 'text' | 'number' | 'checkbox' | 'select';
  required?: boolean;
  placeholder?: string;
  defaultValue?: string | number | boolean;
  options?: Array<{ label: string; value: string }>;
  help?: string;
}

export function CreateDialog({
  title,
  fields,
  open,
  onClose,
  onSubmit,
}: {
  title: string;
  fields: FormField[];
  open: boolean;
  onClose: () => void;
  onSubmit: (value: Record<string, unknown>) => Promise<void>;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    if (open && !element.open) element.showModal();
    if (!open && element.open) element.close();
  }, [open]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    setSubmitting(true);
    setError('');
    const form = new FormData(formElement);
    const payload: Record<string, unknown> = {};
    for (const field of fields) {
      if (field.type === 'checkbox') {
        payload[field.name] = form.has(field.name);
        continue;
      }
      const raw = String(form.get(field.name) ?? '').trim();
      if (!raw && !field.required) continue;
      payload[field.name] = field.type === 'number' ? Number(raw) : raw;
    }
    try {
      await onSubmit(payload);
      formElement.reset();
      onClose();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'The resource could not be created');
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <dialog
      className="resource-dialog"
      ref={dialog}
      onCancel={(event) => {
        event.preventDefault();
        if (!submitting) onClose();
      }}
      onClose={() => {
        if (open && !submitting) onClose();
      }}
    >
      <form onSubmit={submit}>
        <div className="dialog-heading">
          <div>
            <span className="eyebrow">New resource</span>
            <h2>{title}</h2>
          </div>
          <button type="button" className="icon-button" aria-label="Close" onClick={onClose} disabled={submitting}>×</button>
        </div>
        <div className="dialog-fields">
          {fields.map((field) => (
            <label className={field.type === 'checkbox' ? 'checkbox-field' : 'form-field'} key={field.name}>
              {field.type === 'checkbox' ? (
                <>
                  <input
                    name={field.name}
                    type="checkbox"
                    defaultChecked={Boolean(field.defaultValue)}
                  />
                  <span>{field.label}</span>
                </>
              ) : (
                <>
                  <span>{field.label}{field.required && <em> required</em>}</span>
                  {field.type === 'select' ? (
                    <select name={field.name} defaultValue={String(field.defaultValue ?? '')} required={field.required}>
                      <option value="" disabled>Select…</option>
                      {field.options?.map((option) => <option value={option.value} key={option.value}>{option.label}</option>)}
                    </select>
                  ) : (
                    <input
                      name={field.name}
                      type={field.type || 'text'}
                      required={field.required}
                      placeholder={field.placeholder}
                      defaultValue={typeof field.defaultValue === 'boolean' ? undefined : field.defaultValue}
                    />
                  )}
                </>
              )}
              {field.help && <small>{field.help}</small>}
            </label>
          ))}
        </div>
        {error && <p className="inline-error" role="alert">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="button button-secondary" onClick={onClose} disabled={submitting}>Cancel</button>
          <button type="submit" className="button button-primary" disabled={submitting}>
            {submitting ? 'Creating…' : 'Create'}
          </button>
        </div>
      </form>
    </dialog>
  );
}
