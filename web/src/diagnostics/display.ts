export interface ResourceAlias {
  id: unknown;
  name?: unknown;
}

const uuidPattern = /[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}/gi;
const maxErrorLength = 300;

function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

export function redactResourceIDs(value: unknown, aliases: ResourceAlias[] = []): string {
  let result = text(value);
  const replacements = aliases
    .map((alias) => ({ id: text(alias.id), name: text(alias.name) }))
    .filter((alias) => alias.id.length > 3)
    .sort((left, right) => right.id.length - left.id.length);

  for (const alias of replacements) {
    const replacement = alias.name && alias.name !== alias.id ? alias.name : '[resource]';
    result = result.split(alias.id).join(replacement);
  }
  return result.replace(uuidPattern, '[resource]');
}

export function humanLabel(value: unknown, fallback: string): string {
  const label = redactResourceIDs(value);
  const humanContent = label
    .replaceAll('[resource]', '')
    .replace(/[\s._:/-]+/g, '');
  return humanContent ? label : fallback;
}

export function humanResourceLabel(
  resource: Record<string, unknown>,
  fallback: string,
  keys: string[] = ['name', 'address', 'cidr', 'management_address'],
): string {
  for (const key of keys) {
    const label = humanLabel(resource[key], '');
    if (label) return label;
  }
  return fallback;
}

export function uiErrorMessage(
  reason: unknown,
  fallback: string,
  aliases: ResourceAlias[] = [],
): string {
  const message = reason instanceof Error ? reason.message : fallback;
  const safe = redactResourceIDs(message, aliases) || fallback;
  return safe.length > maxErrorLength
    ? `${safe.slice(0, maxErrorLength - 1)}…`
    : safe;
}
