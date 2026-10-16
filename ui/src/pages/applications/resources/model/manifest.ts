import { parseDocument, stringify } from 'yaml';

/*
 * Manifest helpers: the API returns objects as JSON text (live manifest, diff states); the UI
 * shows and edits YAML. Edits are applied as an RFC 7386 JSON merge patch computed against
 * the object the user started from, so only changed fields are sent.
 */

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** Parses JSON text from the API; '' or invalid text yields null. */
export function parseJSONManifest(text: string | undefined | null): Record<string, unknown> | null {
  if (!text) return null;
  try {
    const value: unknown = JSON.parse(text);
    return isObject(value) ? value : null;
  } catch {
    return null;
  }
}

/** Copy without metadata.managedFields (noise for people reading or editing manifests). */
export function withoutManagedFields(obj: Record<string, unknown>): Record<string, unknown> {
  const metadata = obj.metadata;
  if (!isObject(metadata) || !('managedFields' in metadata)) return obj;
  const { managedFields: _managed, ...rest } = metadata;
  return { ...obj, metadata: rest };
}

export function toYAML(value: unknown): string {
  if (value === null || value === undefined) return '';
  return stringify(value, { lineWidth: 0, aliasDuplicateObjects: false });
}

/** YAML text of a JSON manifest from the API ('' when absent). */
export function manifestYAML(json: string | undefined | null, options: { hideManagedFields?: boolean } = {}): string {
  const obj = parseJSONManifest(json);
  if (!obj) return '';
  return toYAML(options.hideManagedFields === false ? obj : withoutManagedFields(obj));
}

export type ParsedYAML = { ok: true; value: unknown } | { ok: false; message: string; line: number | undefined };

/** Parses edited YAML; reports the first error with its line. */
export function parseYAML(text: string): ParsedYAML {
  const doc = parseDocument(text, { uniqueKeys: true });
  const error = doc.errors[0];
  if (error) {
    // Pretty errors end the first line with " at line L, column C:" followed by a code frame.
    const first = (error.message.split('\n')[0] ?? error.message).replace(/ at line \d+, column \d+:?$/, '');
    return { ok: false, message: first, line: error.linePos?.[0]?.line };
  }
  return { ok: true, value: doc.toJS({ maxAliasCount: 100 }) as unknown };
}

function deepEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (Array.isArray(a) && Array.isArray(b)) return a.length === b.length && a.every((v, i) => deepEqual(v, b[i]));
  if (isObject(a) && isObject(b)) {
    const ka = Object.keys(a);
    const kb = Object.keys(b);
    return ka.length === kb.length && ka.every((k) => Object.hasOwn(b, k) && deepEqual(a[k], b[k]));
  }
  return false;
}

/**
 * RFC 7386 merge patch turning `original` into `edited`: removed keys become null, nested
 * objects recurse, everything else (arrays included) is replaced. Returns undefined when the
 * two are equal.
 */
export function createMergePatch(original: unknown, edited: unknown): unknown {
  if (deepEqual(original, edited)) return undefined;
  if (!isObject(original) || !isObject(edited)) return edited ?? null;
  const patch: Record<string, unknown> = {};
  for (const key of Object.keys(original)) {
    if (!Object.hasOwn(edited, key)) patch[key] = null;
  }
  for (const [key, value] of Object.entries(edited)) {
    const sub = Object.hasOwn(original, key) ? createMergePatch(original[key], value) : value;
    if (sub !== undefined) patch[key] = sub;
  }
  return patch;
}

/** The server replaces Secret values with this mask before returning manifests. */
export const SECRET_MASK = '++++++++';

/** True for core Secrets, whose values the server masks in every manifest it returns. */
export function isMaskedKind(kind: string, group: string): boolean {
  return kind === 'Secret' && group === '';
}

export interface ContainerNames {
  containers: string[];
  initContainers: string[];
}

/** Container names from a Pod spec or a workload's pod template. */
export function containerNames(obj: Record<string, unknown> | null): ContainerNames {
  const result: ContainerNames = { containers: [], initContainers: [] };
  if (!obj) return result;
  let spec: Record<string, unknown> | undefined;
  // Pod: spec; workloads: spec.template.spec; CronJob: spec.jobTemplate.spec.template.spec.
  for (const path of [[], ['template', 'spec'], ['jobTemplate', 'spec', 'template', 'spec']]) {
    let node: unknown = obj.spec;
    for (const key of path) node = isObject(node) ? node[key] : undefined;
    if (isObject(node) && Array.isArray(node.containers)) {
      spec = node;
      break;
    }
  }
  if (!spec) return result;
  const names = (list: unknown) =>
    Array.isArray(list) ? list.map((c) => (isObject(c) && typeof c.name === 'string' ? c.name : '')).filter(Boolean) : [];
  result.containers = names(spec.containers);
  result.initContainers = names(spec.initContainers);
  return result;
}

/** metadata.labels / metadata.annotations as string maps. */
export function metadataMap(obj: Record<string, unknown> | null, field: 'labels' | 'annotations'): Record<string, string> {
  const metadata = obj?.metadata;
  if (!isObject(metadata)) return {};
  const map = metadata[field];
  if (!isObject(map)) return {};
  const result: Record<string, string> = {};
  for (const [k, v] of Object.entries(map)) result[k] = typeof v === 'string' ? v : JSON.stringify(v);
  return result;
}
