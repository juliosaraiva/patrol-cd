/**
 * Comparison of generated Applications for the Preview (ASET-126): desired (dry-run generate)
 * versus existing, as YAML line diffs. Only fields the ApplicationSet controls are compared, so
 * server-managed metadata and status never show up as changes.
 */
import { stringify } from 'yaml';
import type { Application } from '../../api/types';

export interface DiffLine {
  kind: 'same' | 'add' | 'del';
  text: string;
  /** 1-based line numbers in the old/new text. */
  oldNo?: number | undefined;
  newNo?: number | undefined;
}

/** Cells above which the LCS table is not built (the diff degrades to replace-all). */
const MAX_CELLS = 4_000_000;

export function lineDiff(a: string, b: string): DiffLine[] {
  const x = a === '' ? [] : a.split('\n');
  const y = b === '' ? [] : b.split('\n');
  const n = x.length;
  const m = y.length;
  if (n * m > MAX_CELLS) {
    return [
      ...x.map((text, i): DiffLine => ({ kind: 'del', text, oldNo: i + 1 })),
      ...y.map((text, j): DiffLine => ({ kind: 'add', text, newNo: j + 1 })),
    ];
  }
  // lcs[i][j] = LCS length of x[i..] and y[j..]
  const width = m + 1;
  const lcs = new Uint32Array((n + 1) * width);
  for (let i = n - 1; i >= 0; i -= 1) {
    for (let j = m - 1; j >= 0; j -= 1) {
      lcs[i * width + j] = x[i] === y[j] ? (lcs[(i + 1) * width + j + 1] ?? 0) + 1 : Math.max(lcs[(i + 1) * width + j] ?? 0, lcs[i * width + j + 1] ?? 0);
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      out.push({ kind: 'same', text: x[i] ?? '', oldNo: i + 1, newNo: j + 1 });
      i += 1;
      j += 1;
    } else if ((lcs[(i + 1) * width + j] ?? 0) >= (lcs[i * width + j + 1] ?? 0)) {
      out.push({ kind: 'del', text: x[i] ?? '', oldNo: i + 1 });
      i += 1;
    } else {
      out.push({ kind: 'add', text: y[j] ?? '', newNo: j + 1 });
      j += 1;
    }
  }
  for (; i < n; i += 1) out.push({ kind: 'del', text: x[i] ?? '', oldNo: i + 1 });
  for (; j < m; j += 1) out.push({ kind: 'add', text: y[j] ?? '', newNo: j + 1 });
  return out;
}

export type DiffSegment = { type: 'lines'; lines: DiffLine[] } | { type: 'skip'; count: number };

/** Changed lines with `context` unchanged lines around them; longer unchanged runs collapse. */
export function hunks(lines: readonly DiffLine[], context = 3): DiffSegment[] {
  const keep = lines.map(() => false);
  lines.forEach((l, idx) => {
    if (l.kind === 'same') return;
    for (let k = Math.max(0, idx - context); k <= Math.min(lines.length - 1, idx + context); k += 1) keep[k] = true;
  });
  const segments: DiffSegment[] = [];
  let skipped = 0;
  let current: DiffLine[] = [];
  lines.forEach((l, idx) => {
    if (keep[idx]) {
      if (skipped > 0) {
        segments.push({ type: 'skip', count: skipped });
        skipped = 0;
      }
      current.push(l);
    } else {
      if (current.length > 0) {
        segments.push({ type: 'lines', lines: current });
        current = [];
      }
      skipped += 1;
    }
  });
  if (current.length > 0) segments.push({ type: 'lines', lines: current });
  if (skipped > 0) segments.push({ type: 'skip', count: skipped });
  return segments;
}

/** Annotations written by controllers or tools, not by the ApplicationSet template. */
const IGNORED_ANNOTATION = /^(notified\.notifications\.|kubectl\.kubernetes\.io\/last-applied-configuration$|cd\.patrol\.example\/(refresh|hydrate|application-set-progressive-status)$)/;

function nonEmpty<T extends object>(value: T | undefined): T | undefined {
  return value && Object.keys(value).length > 0 ? value : undefined;
}

/** The part of an Application an ApplicationSet controls, in a stable shape. */
export function comparableApplication(app: Application): Record<string, unknown> {
  const annotations = Object.fromEntries(Object.entries(app.metadata.annotations ?? {}).filter(([k]) => !IGNORED_ANNOTATION.test(k)));
  const metadata: Record<string, unknown> = { name: app.metadata.name, namespace: app.metadata.namespace };
  const labels = nonEmpty(app.metadata.labels);
  if (labels) metadata.labels = labels;
  if (nonEmpty(annotations)) metadata.annotations = annotations;
  if (app.metadata.finalizers && app.metadata.finalizers.length > 0) metadata.finalizers = app.metadata.finalizers;
  return { metadata, spec: app.spec };
}

export function toYAML(value: unknown): string {
  return stringify(value, { sortMapEntries: true, lineWidth: 0 }).trimEnd();
}

export type ChangeKind = 'added' | 'removed' | 'modified' | 'unchanged';

/**
 * One Application of the preview. Only the comparable shapes are kept: the YAML and the line
 * diff are computed on demand (changeYAML, changeLines) for the rows a user actually looks at, so
 * a preview of thousands of Applications stays cheap.
 */
export interface ApplicationChange {
  key: string;
  name: string;
  namespace: string;
  change: ChangeKind;
  before: Record<string, unknown> | null;
  after: Record<string, unknown> | null;
}

/** JSON with object keys sorted, for equality of comparable shapes (undefined fields dropped). */
export function canonicalJSON(value: unknown): string {
  return JSON.stringify(value, (_key, v: unknown) =>
    v && typeof v === 'object' && !Array.isArray(v) ? Object.fromEntries(Object.entries(v as Record<string, unknown>).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) : v,
  );
}

/**
 * Pairs Applications by namespace/name (a missing namespace is the ApplicationSet's) and
 * classifies them. Sorted: added, modified, removed, unchanged; by name within each group.
 */
export function diffApplications(existing: readonly Application[], desired: readonly Application[], defaultNamespace: string): ApplicationChange[] {
  // An empty namespace means the ApplicationSet's namespace, like a missing one.
  const nsOf = (a: Application) => (a.metadata.namespace === undefined || a.metadata.namespace === '' ? defaultNamespace : a.metadata.namespace);
  const keyOf = (a: Application) => `${nsOf(a)}/${a.metadata.name ?? ''}`;
  const withNs = (a: Application): Application => ({ ...a, metadata: { ...a.metadata, namespace: nsOf(a) } });
  const before = new Map(existing.map((a) => [keyOf(a), withNs(a)]));
  const after = new Map(desired.map((a) => [keyOf(a), withNs(a)]));
  const keys = [...new Set([...after.keys(), ...before.keys()])];
  const order: Record<ChangeKind, number> = { added: 0, modified: 1, removed: 2, unchanged: 3 };
  return keys
    .map((key): ApplicationChange => {
      const old = before.get(key);
      const next = after.get(key);
      const a = old ? comparableApplication(old) : null;
      const b = next ? comparableApplication(next) : null;
      const change: ChangeKind = !a ? 'added' : !b ? 'removed' : canonicalJSON(a) === canonicalJSON(b) ? 'unchanged' : 'modified';
      const sep = key.indexOf('/');
      return { key, namespace: key.slice(0, sep), name: key.slice(sep + 1), change, before: a, after: b };
    })
    .sort((p, q) => order[p.change] - order[q.change] || p.name.localeCompare(q.name));
}

const yamlCache = new WeakMap<ApplicationChange, { before: string; after: string }>();
const linesCache = new WeakMap<ApplicationChange, DiffLine[]>();

/** YAML of both sides ('' for a missing side), computed once per change. */
export function changeYAML(change: ApplicationChange): { before: string; after: string } {
  let yaml = yamlCache.get(change);
  if (!yaml) {
    yaml = { before: change.before ? toYAML(change.before) : '', after: change.after ? toYAML(change.after) : '' };
    yamlCache.set(change, yaml);
  }
  return yaml;
}

/** Line diff of a change, computed once and only when asked for. */
export function changeLines(change: ApplicationChange): DiffLine[] {
  let lines = linesCache.get(change);
  if (!lines) {
    const { before, after } = changeYAML(change);
    lines = lineDiff(before, after);
    linesCache.set(change, lines);
  }
  return lines;
}

export function lineStats(lines: readonly DiffLine[]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const l of lines) {
    if (l.kind === 'add') added += 1;
    else if (l.kind === 'del') removed += 1;
  }
  return { added, removed };
}

export function countChanges(changes: readonly ApplicationChange[]): Record<ChangeKind, number> {
  const counts: Record<ChangeKind, number> = { added: 0, modified: 0, removed: 0, unchanged: 0 };
  for (const c of changes) counts[c.change] += 1;
  return counts;
}
