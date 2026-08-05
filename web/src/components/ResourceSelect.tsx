import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useApi } from '../api/context';
import type { BaseResource } from '../api/types';

export interface ResourceMatch {
  formField: string;
  resourceField?: string;
  required?: boolean;
}

export interface ResourceReference {
  endpoint: string;
  labelKeys?: string[];
  detailKeys?: string[];
  where?: Record<string, unknown>;
  matches?: ResourceMatch[];
  emptyLabel?: string;
}

interface ResourceSelectProps {
  id: string;
  name: string;
  source: ResourceReference;
  active: boolean;
  required?: boolean;
  multiple?: boolean;
  defaultValue?: string | string[];
  formValues?: Record<string, unknown>;
  onChange?: (value: string | string[]) => void;
}

function selectionValue(value: string | string[] | undefined, multiple: boolean): string | string[] {
  if (multiple) return Array.isArray(value) ? value : value ? [value] : [];
  return Array.isArray(value) ? (value[0] || '') : (value || '');
}

function readPath(value: unknown, path: string): unknown {
  return path.split('.').reduce<unknown>((current, key) => {
    if (!current || typeof current !== 'object') return undefined;
    return (current as Record<string, unknown>)[key];
  }, value);
}

function firstText(resource: BaseResource, keys: string[]): string {
  for (const key of keys) {
    const value = readPath(resource, key);
    if (value !== null && value !== undefined && value !== '') return String(value);
  }
  return '';
}

export function resourceOptionLabel(resource: BaseResource, source: ResourceReference): string {
  const primary = firstText(resource, source.labelKeys || ['name', 'address', 'cidr']);
  const details = (source.detailKeys || [])
    .map((key) => firstText(resource, [key]))
    .filter((value, index, values) => value && value !== primary && values.indexOf(value) === index);
  return [primary, ...details, resource.id]
    .filter((value, index, values) => value && values.indexOf(value) === index)
    .join(' · ');
}

function matchesSource(resource: BaseResource, source: ResourceReference, formValues: Record<string, unknown>): boolean {
  if (source.where && !Object.entries(source.where).every(([key, expected]) => readPath(resource, key) === expected)) {
    return false;
  }
  return (source.matches || []).every((match) => {
    const selected = formValues[match.formField];
    if (selected === null || selected === undefined || selected === '') return true;
    return String(readPath(resource, match.resourceField || match.formField) ?? '') === String(selected);
  });
}

export function ResourceSelect({
  id,
  name,
  source,
  active,
  required = false,
  multiple = false,
  defaultValue,
  formValues = {},
  onChange,
}: ResourceSelectProps) {
  const api = useApi();
  const [items, setItems] = useState<BaseResource[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [reloadKey, setReloadKey] = useState(0);
  const [selection, setSelection] = useState<string | string[]>(() => selectionValue(defaultValue, multiple));
  const wasActive = useRef(false);

  const missingDependency = (source.matches || []).find((match) => {
    if (match.required === false) return false;
    const value = formValues[match.formField];
    return value === null || value === undefined || value === '';
  });
  const dependencyKey = (source.matches || [])
    .map((match) => String(formValues[match.formField] ?? ''))
    .join('\u0000');
  const previousDependency = useRef(dependencyKey);

  useEffect(() => {
    if (active && !wasActive.current) setSelection(selectionValue(defaultValue, multiple));
    wasActive.current = active;
  }, [active, defaultValue, multiple]);

  useEffect(() => {
    if (!active) {
      previousDependency.current = dependencyKey;
      return;
    }
    if (previousDependency.current === dependencyKey) return;
    previousDependency.current = dependencyKey;
    const cleared = selectionValue(undefined, multiple);
    setSelection(cleared);
    onChange?.(cleared);
  }, [active, dependencyKey, multiple, onChange]);

  const load = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const result = await api.list<BaseResource>(source.endpoint);
      setItems(result.items);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not load choices');
    } finally {
      setLoading(false);
    }
  }, [api, source.endpoint]);

  useEffect(() => {
    if (!active || missingDependency) return;
    let current = true;
    void load().catch(() => {
      if (current) setError('Could not load choices');
    });
    return () => { current = false; };
  }, [active, load, missingDependency?.formField, reloadKey]);

  const options = useMemo(
    () => items.filter((item) => matchesSource(item, source, formValues)),
    [formValues, items, source],
  );
  const selectedIDs = Array.isArray(selection) ? selection : selection ? [selection] : [];
  const unavailableSelectedIDs = selectedIDs.filter((id) => !options.some((item) => item.id === id));

  const placeholder = missingDependency
    ? `Select ${missingDependency.formField.replace(/_id$/, '').replaceAll('_', ' ')} first…`
    : loading
      ? 'Loading choices…'
      : error
        ? 'Choices unavailable'
        : options.length === 0
          ? (source.emptyLabel || 'No resources available')
          : 'Select…';

  return (
    <>
      <select
        id={id}
        name={name}
        required={required}
        multiple={multiple}
        size={multiple ? Math.min(Math.max(options.length, 2), 5) : undefined}
        value={selection}
        disabled={Boolean(missingDependency) || (multiple && (loading || Boolean(error) || options.length === 0))}
        aria-busy={loading || undefined}
        onChange={(event) => {
          const value = multiple
            ? Array.from(event.currentTarget.selectedOptions, (option) => option.value)
            : event.currentTarget.value;
          setSelection(value);
          onChange?.(value);
        }}
      >
        {!multiple && <option value="">{placeholder}</option>}
        {multiple && options.length === 0 && <option value="" disabled>{placeholder}</option>}
        {unavailableSelectedIDs.map((value) => (
          <option value={value} key={`current-${value}`}>{value} · current value unavailable</option>
        ))}
        {options.map((item) => (
          <option value={item.id} key={item.id}>{resourceOptionLabel(item, source)}</option>
        ))}
      </select>
      {multiple && options.length > 0 && <small>Use Ctrl/Cmd to select more than one.</small>}
      {error && (
        <span className="reference-error" role="alert">
          {error}
          <button type="button" onClick={() => setReloadKey((value) => value + 1)}>Retry</button>
        </span>
      )}
    </>
  );
}
