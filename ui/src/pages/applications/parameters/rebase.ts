/**
 * Three-way rebase of an application edit onto a newer version of the object, used when the
 * application changed on the server while it was being edited (409 conflict, or a change seen
 * through the watch stream).
 *
 *   base   — the version the edit started from (or was last rebased onto)
 *   mine   — base with the user's edits applied
 *   latest — the current version on the server
 *
 * Only what the editors can change is merged: metadata.labels, metadata.annotations,
 * metadata.finalizers and spec. Everything else (resourceVersion, status, server-managed
 * metadata) is taken from `latest`. For every field the user did not change, the server's value
 * wins; for every field the server did not change, the user's value wins; when both changed the
 * same field differently the user's value is kept and the path is reported as a conflict.
 * Records merge per key; lists of objects with unique `name`s (Helm and plugin parameters, env,
 * info items, replicas, …) merge per name; other lists merge per index when all three versions
 * have the same length and are otherwise replaced as a whole.
 */
import type { Application } from '../../../api/types';
import { cloneJSON, isMultiSource } from '../shared/app';
import { applySources, applySummary, dropUndefined, manifestYAML, parseManifest, sourceDrafts, summaryFormFrom, type SourceDraft, type SummaryForm } from './specEdit';

export interface RebaseReport {
  /** Paths changed on the server that the user did not edit: the server's values were taken. */
  serverChanges: string[];
  /** Paths both the user and the server changed differently: the user's values are kept. */
  conflicts: string[];
}

export interface RebaseResult extends RebaseReport {
  /** `latest` with the user's edits applied. */
  merged: Application;
}

type Rec = Record<string, unknown>;

function isRecord(v: unknown): v is Rec {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

/** Structural equality of JSON values; key order and undefined-valued keys are ignored. */
export function deepEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (Array.isArray(a) || Array.isArray(b)) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
    return a.every((v, i) => deepEqual(v, b[i]));
  }
  if (isRecord(a) && isRecord(b)) {
    const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
    for (const k of keys) if (!deepEqual(a[k], b[k])) return false;
    return true;
  }
  return false;
}

function childPath(path: string, key: string): string {
  return /^[A-Za-z_$][\w$]*$/.test(key) ? `${path}.${key}` : `${path}[${JSON.stringify(key)}]`;
}

/** Maps by `name` when every element of every list is an object with a unique string name. */
function byName(lists: readonly unknown[][]): Map<string, unknown>[] | null {
  const maps: Map<string, unknown>[] = [];
  for (const list of lists) {
    const map = new Map<string, unknown>();
    for (const item of list) {
      if (!isRecord(item) || typeof item.name !== 'string' || map.has(item.name)) return null;
      map.set(item.name, item);
    }
    maps.push(map);
  }
  return maps;
}

function orderedNames(...maps: Map<string, unknown>[]): string[] {
  const names: string[] = [];
  const seen = new Set<string>();
  for (const map of maps) {
    for (const name of map.keys()) {
      if (!seen.has(name)) {
        seen.add(name);
        names.push(name);
      }
    }
  }
  return names;
}

/** Leaf paths where `a` and `b` differ (a whole added or removed subtree is one path). */
export function diffPaths(a: unknown, b: unknown, path: string): string[] {
  if (deepEqual(a, b)) return [];
  if (isRecord(a) && isRecord(b)) {
    return [...new Set([...Object.keys(a), ...Object.keys(b)])].flatMap((k) => diffPaths(a[k], b[k], childPath(path, k)));
  }
  if (Array.isArray(a) && Array.isArray(b)) {
    const named = byName([a, b]);
    if (named) {
      const [ma, mb] = named as [Map<string, unknown>, Map<string, unknown>];
      return orderedNames(ma, mb).flatMap((n) => diffPaths(ma.get(n), mb.get(n), `${path}[name=${n}]`));
    }
    if (a.length === b.length) return a.flatMap((v, i) => diffPaths(v, b[i], `${path}[${i}]`));
  }
  return [path];
}

function merge3(o: unknown, m: unknown, l: unknown, path: string, report: RebaseReport): unknown {
  if (deepEqual(m, l)) return m;
  if (deepEqual(m, o)) {
    report.serverChanges.push(...diffPaths(o, l, path));
    return l;
  }
  if (deepEqual(l, o)) return m;
  // Both sides changed this value, differently.
  if (isRecord(m) && isRecord(l) && (o === undefined || isRecord(o))) {
    const base: Rec = o ?? {};
    const out: Rec = {};
    for (const k of [...new Set([...Object.keys(m), ...Object.keys(l), ...Object.keys(base)])]) {
      const v = merge3(base[k], m[k], l[k], childPath(path, k), report);
      if (v !== undefined) out[k] = v;
    }
    return out;
  }
  if (Array.isArray(o) && Array.isArray(m) && Array.isArray(l)) {
    const named = byName([o, m, l]);
    if (named) {
      const [mo, mm, ml] = named as [Map<string, unknown>, Map<string, unknown>, Map<string, unknown>];
      return orderedNames(mm, ml)
        .map((n) => merge3(mo.get(n), mm.get(n), ml.get(n), `${path}[name=${n}]`, report))
        .filter((v) => v !== undefined);
    }
    if (o.length === m.length && m.length === l.length) return o.map((v, i) => merge3(v, m[i], l[i], `${path}[${i}]`, report));
  }
  report.conflicts.push(path);
  return m;
}

const EDITABLE_METADATA = ['labels', 'annotations', 'finalizers'] as const;

/** Paths of the editable fields (labels, annotations, finalizers, spec) that differ. */
export function editableChanges(base: Application, latest: Application): string[] {
  return [
    ...EDITABLE_METADATA.flatMap((k) => diffPaths(base.metadata[k], latest.metadata[k], `metadata.${k}`)),
    ...diffPaths(base.spec, latest.spec, 'spec'),
  ];
}

/** Applies the user's edits (`mine` relative to `base`) onto `latest`. */
export function rebaseApplication(base: Application, mine: Application, latest: Application): RebaseResult {
  const report: RebaseReport = { serverChanges: [], conflicts: [] };
  const merged = cloneJSON(latest);
  for (const key of EDITABLE_METADATA) {
    const value = merge3(base.metadata[key], mine.metadata[key], latest.metadata[key], `metadata.${key}`, report);
    if (value === undefined) delete merged.metadata[key];
    else (merged.metadata as Rec)[key] = value;
  }
  merged.spec = merge3(base.spec, mine.spec, latest.spec, 'spec', report) as Application['spec'];
  return { merged: dropUndefined(merged), ...report };
}

function reportOf(result: RebaseResult): RebaseReport {
  return { serverChanges: result.serverChanges, conflicts: result.conflicts };
}

/** The summary form rebased onto `latest`. */
export function rebaseSummary(base: Application, form: SummaryForm, latest: Application): { draft: SummaryForm; report: RebaseReport } {
  const result = rebaseApplication(base, applySummary(base, form), latest);
  return { draft: summaryFormFrom(result.merged), report: reportOf(result) };
}

/** The source drafts rebased onto `latest`. The drafts must be valid (their YAML parses). */
export function rebaseSources(
  base: Application,
  draft: { drafts: readonly SourceDraft[]; multi: boolean },
  latest: Application,
): { draft: { drafts: SourceDraft[]; multi: boolean }; report: RebaseReport } {
  const result = rebaseApplication(base, applySources(base, draft.drafts, draft.multi), latest);
  return { draft: { drafts: sourceDrafts(result.merged), multi: isMultiSource(result.merged) }, report: reportOf(result) };
}

/** The manifest text rebased onto `latest`, or null when the text is not a valid manifest. */
export function rebaseManifest(base: Application, text: string, latest: Application): { draft: string; report: RebaseReport } | null {
  const parsed = parseManifest(text, base);
  if (!parsed.ok) return null;
  const result = rebaseApplication(base, parsed.app, latest);
  return { draft: manifestYAML(result.merged), report: reportOf(result) };
}
