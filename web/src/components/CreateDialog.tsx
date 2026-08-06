import { useEffect, useId, useMemo, useRef, useState, type FormEvent } from 'react';
import { type ResourceAlias, uiErrorMessage } from '../diagnostics/display';
import { ResourceSelect, type ResourceReference } from './ResourceSelect';

export interface FormField {
  name: string;
  label: string;
  type?: 'text' | 'number' | 'checkbox' | 'select' | 'resource-select';
  required?: boolean;
  placeholder?: string;
  defaultValue?: string | string[] | number | boolean;
  options?: Array<{ label: string; value: string }>;
  reference?: ResourceReference;
  multiple?: boolean;
  help?: string;
}

const emptyFormValues: Record<string, unknown> = {};

function defaultFormValues(fields: FormField[], values: Record<string, unknown>): Record<string, unknown> {
  const result = { ...values };
  for (const field of fields) {
    if (result[field.name] === undefined && field.defaultValue !== undefined) {
      result[field.name] = field.defaultValue;
    }
  }
  return result;
}

function selectedResourceAliases(form: HTMLFormElement, fields: FormField[]): ResourceAlias[] {
  return fields.flatMap((field) => {
    if (field.type !== 'resource-select') return [];
    const select = form.elements.namedItem(field.name);
    if (!(select instanceof HTMLSelectElement)) return [];
    return Array.from(select.selectedOptions)
      .filter((option) => option.value)
      .map((option) => ({ id: option.value, name: option.textContent }));
  });
}

export function CreateDialog({
  title,
  fields,
  open,
  onClose,
  onSubmit,
  mode = 'create',
  values = emptyFormValues,
}: {
  title: string;
  fields: FormField[];
  open: boolean;
  onClose: () => void;
  onSubmit: (value: Record<string, unknown>) => Promise<void>;
  mode?: 'create' | 'edit';
  values?: Record<string, unknown>;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const fieldPrefix = `resource-${useId().replaceAll(':', '')}`;
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');
  const initialValues = useMemo(() => defaultFormValues(fields, values), [fields, values]);
  const [formValues, setFormValues] = useState<Record<string, unknown>>(initialValues);

  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    if (open && !element.open) {
      setError('');
      setFormValues(initialValues);
      element.showModal();
    }
    if (!open && element.open) element.close();
  }, [initialValues, open]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    setSubmitting(true);
    setError('');
    const form = new FormData(formElement);
    const resourceAliases = selectedResourceAliases(formElement, fields);
    const payload: Record<string, unknown> = {};
    for (const field of fields) {
      if (field.type === 'checkbox') {
        payload[field.name] = form.has(field.name);
        continue;
      }
      if (field.type === 'resource-select' && field.multiple) {
        const values = form.getAll(field.name).map((value) => String(value).trim()).filter(Boolean);
        if (values.length > 0 || field.required || mode === 'edit') payload[field.name] = values;
        continue;
      }
      const raw = String(form.get(field.name) ?? '').trim();
      if (!raw && !field.required && mode === 'create') continue;
      payload[field.name] = field.type === 'number' ? Number(raw) : raw;
    }
    try {
      await onSubmit(payload);
      formElement.reset();
      setFormValues(initialValues);
      onClose();
    } catch (reason) {
      const fallback = `The resource could not be ${mode === 'edit' ? 'updated' : 'created'}`;
      setError(uiErrorMessage(reason, fallback, [
        ...resourceAliases,
        { id: values.id, name: values.name || values.address || values.cidr },
      ]));
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
            <span className="eyebrow">{mode === 'edit' ? 'Edit resource' : 'New resource'}</span>
            <h2>{title}</h2>
          </div>
          <button type="button" className="icon-button" aria-label="Close" onClick={onClose} disabled={submitting}>×</button>
        </div>
        <div className="dialog-fields">
          {fields.map((field) => {
            const fieldID = `${fieldPrefix}-${field.name}`;
            const initialValue = initialValues[field.name];
            return field.type === 'checkbox' ? (
              <label className="checkbox-field" key={field.name}>
                <>
                  <input
                    name={field.name}
                    type="checkbox"
                    defaultChecked={Boolean(initialValue)}
                    onChange={(event) => setFormValues((values) => ({ ...values, [field.name]: event.target.checked }))}
                  />
                  <span>{field.label}</span>
                </>
                {field.help && <small>{field.help}</small>}
              </label>
            ) : (
              <div className="form-field" key={field.name}>
                <>
                  <label htmlFor={fieldID}>{field.label}{field.required && <em aria-hidden="true"> required</em>}</label>
                  {field.type === 'resource-select' && field.reference ? (
                    <ResourceSelect
                      id={fieldID}
                      name={field.name}
                      source={field.reference}
                      active={open}
                      required={field.required}
                      multiple={field.multiple}
                      defaultValue={Array.isArray(initialValue)
                        ? initialValue.map(String)
                        : initialValue === undefined ? undefined : String(initialValue)}
                      formValues={formValues}
                      onChange={(value) => setFormValues((values) => ({ ...values, [field.name]: value }))}
                    />
                  ) : field.type === 'select' ? (
                    <select
                      id={fieldID}
                      name={field.name}
                      defaultValue={String(initialValue ?? '')}
                      required={field.required}
                      onChange={(event) => setFormValues((values) => ({ ...values, [field.name]: event.target.value }))}
                    >
                      <option value="" disabled={field.required}>{field.required ? 'Select…' : 'None'}</option>
                      {field.options?.map((option) => <option value={option.value} key={option.value}>{option.label}</option>)}
                    </select>
                  ) : (
                    <input
                      id={fieldID}
                      name={field.name}
                      type={field.type || 'text'}
                      required={field.required}
                      placeholder={field.placeholder}
                      defaultValue={typeof initialValue === 'boolean' || Array.isArray(initialValue) ? undefined : String(initialValue ?? '')}
                      onChange={(event) => setFormValues((values) => ({ ...values, [field.name]: event.target.value }))}
                    />
                  )}
                </>
                {field.help && <small>{field.help}</small>}
              </div>
            );
          })}
        </div>
        {error && <p className="inline-error" role="alert">{error}</p>}
        <div className="dialog-actions">
          <button type="button" className="button button-secondary" onClick={onClose} disabled={submitting}>Cancel</button>
          <button type="submit" className="button button-primary" disabled={submitting}>
            {submitting ? (mode === 'edit' ? 'Saving…' : 'Creating…') : (mode === 'edit' ? 'Save changes' : 'Create')}
          </button>
        </div>
      </form>
    </dialog>
  );
}
