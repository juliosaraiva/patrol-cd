/**
 * Applications list filters (parity API-088, API-089): URL search params ⇄ filter state, the
 * predicate, per-option counts against the other active filters, and sorting.
 */
import type { Application } from '../../../api/types';
import { compareValues } from '../../../design';
import { appKey, appSources, clusterOf, healthOf, isAutoSyncEnabled, isDeleting, syncOf } from '../shared/app';

export type ViewMode = 'tiles' | 'list' | 'summary';
export type SortKey = 'name' | 'created' | 'synced';
export type AutoSyncFilter = 'Enabled' | 'Disabled';

export interface AppFilters {
  search: string;
  projects: string[];
  sync: string[];
  health: string[];
  operation: string[];
  labels: string[];
  annotations: string[];
  namespaces: string[];
  clusters: string[];
  repos: string[];
  targetRevisions: string[];
  autoSync: AutoSyncFilter[];
  favorites: boolean;
}

/** Filter dimensions whose options are counted. */
export type Dimension = 'projects' | 'sync' | 'health' | 'operation' | 'namespaces' | 'clusters' | 'repos' | 'targetRevisions' | 'autoSync';

export const EMPTY_FILTERS: AppFilters = {
  search: '',
  projects: [],
  sync: [],
  health: [],
  operation: [],
  labels: [],
  annotations: [],
  namespaces: [],
  clusters: [],
  repos: [],
  targetRevisions: [],
  autoSync: [],
  favorites: false,
};

/** URL parameter of each list filter (upstream names, so shared links keep working). */
const PARAM: Record<Exclude<keyof AppFilters, 'search' | 'favorites'>, string> = {
  projects: 'proj',
  sync: 'sync',
  health: 'health',
  operation: 'operation',
  labels: 'labels',
  annotations: 'annotations',
  namespaces: 'namespace',
  clusters: 'cluster',
  repos: 'repo',
  targetRevisions: 'targetRevision',
  autoSync: 'autoSync',
};

const LIST_KEYS = Object.keys(PARAM) as (keyof typeof PARAM)[];

/** Comma-separated values; each value is URI-encoded so labels and URLs may contain commas. */
function readList(params: URLSearchParams, key: string): string[] {
  const raw = params.get(key);
  if (!raw) return [];
  return raw
    .split(',')
    .map((v) => {
      try {
        return decodeURIComponent(v).trim();
      } catch {
        return v.trim();
      }
    })
    .filter(Boolean);
}

export function parseFilters(params: URLSearchParams): AppFilters {
  const filters: AppFilters = { ...EMPTY_FILTERS, search: params.get('search') ?? '', favorites: params.get('showFavorites') === 'true' };
  for (const key of LIST_KEYS) {
    const values = readList(params, PARAM[key]);
    if (key === 'autoSync') filters.autoSync = values.filter((v): v is AutoSyncFilter => v === 'Enabled' || v === 'Disabled');
    else filters[key] = values;
  }
  return filters;
}

/** Writes filters into `params` (other keys untouched); empty filters are removed. */
export function writeFilters(params: URLSearchParams, filters: AppFilters): URLSearchParams {
  const next = new URLSearchParams(params);
  if (filters.search) next.set('search', filters.search);
  else next.delete('search');
  if (filters.favorites) next.set('showFavorites', 'true');
  else next.delete('showFavorites');
  for (const key of LIST_KEYS) {
    const values = filters[key];
    if (values.length > 0) next.set(PARAM[key], values.map((v) => encodeURIComponent(v)).join(','));
    else next.delete(PARAM[key]);
  }
  return next;
}

export function hasActiveFilters(filters: AppFilters): boolean {
  return filters.search !== '' || filters.favorites || LIST_KEYS.some((k) => filters[k].length > 0);
}

const LABELS: Record<keyof typeof PARAM, string> = {
  projects: 'Project',
  sync: 'Sync',
  health: 'Health',
  operation: 'Operation',
  labels: 'Label',
  annotations: 'Annotation',
  namespaces: 'Namespace',
  clusters: 'Cluster',
  repos: 'Repository',
  targetRevisions: 'Target revision',
  autoSync: 'Auto-sync',
};

export interface Chip {
  id: string;
  label: string;
}

export function filterChips(filters: AppFilters): Chip[] {
  const chips: Chip[] = [];
  if (filters.search) chips.push({ id: 'search', label: `Search: ${filters.search}` });
  if (filters.favorites) chips.push({ id: 'favorites', label: 'Favorites only' });
  for (const key of LIST_KEYS) {
    for (const value of filters[key]) chips.push({ id: `${key}\u0000${value}`, label: `${LABELS[key]}: ${value === 'OutOfSync' ? 'Out of sync' : value}` });
  }
  return chips;
}

export function removeChip(filters: AppFilters, id: string): AppFilters {
  if (id === 'search') return { ...filters, search: '' };
  if (id === 'favorites') return { ...filters, favorites: false };
  const [key, value] = id.split('\u0000') as [keyof typeof PARAM, string];
  if (!LIST_KEYS.includes(key)) return filters;
  if (key === 'autoSync') return { ...filters, autoSync: filters.autoSync.filter((v) => v !== value) };
  return { ...filters, [key]: filters[key].filter((v) => v !== value) };
}

/** Shell-style glob: `*` any run of characters, `?` one character; whole-string match. */
export function globToRegExp(glob: string): RegExp {
  const escaped = glob.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.');
  return new RegExp(`^${escaped}$`);
}

export function matchGlob(value: string, pattern: string): boolean {
  if (pattern === value) return true;
  if (!/[*?]/.test(pattern)) return false;
  return globToRegExp(pattern).test(value);
}

/**
 * One Kubernetes label selector expression: `key`, `!key`, `key=value`, `key==value`,
 * `key!=value`, `key in (a,b)`, `key notin (a,b)`. A comma-separated selector requires all parts.
 */
export function matchLabelSelector(labels: Readonly<Record<string, string>>, selector: string): boolean {
  const parts = splitSelector(selector);
  return parts.length > 0 && parts.every((part) => matchRequirement(labels, part));
}

function splitSelector(selector: string): string[] {
  const parts: string[] = [];
  let depth = 0;
  let current = '';
  for (const ch of selector) {
    if (ch === '(') depth += 1;
    if (ch === ')') depth = Math.max(0, depth - 1);
    if (ch === ',' && depth === 0) {
      parts.push(current.trim());
      current = '';
    } else current += ch;
  }
  parts.push(current.trim());
  return parts.filter(Boolean);
}

function matchRequirement(labels: Readonly<Record<string, string>>, req: string): boolean {
  const set = /^([^\s!=]+)\s+(in|notin)\s+\(([^)]*)\)$/.exec(req);
  if (set) {
    const [, key = '', op, list = ''] = set;
    const values = list.split(',').map((v) => v.trim());
    const has = Object.hasOwn(labels, key);
    const inSet = has && values.includes(labels[key] ?? '');
    return op === 'in' ? inSet : !inSet;
  }
  if (req.startsWith('!')) return !Object.hasOwn(labels, req.slice(1).trim());
  const neq = /^([^!=]+)!=(.*)$/.exec(req);
  if (neq) return labels[(neq[1] ?? '').trim()] !== (neq[2] ?? '').trim();
  const eq = /^([^!=]+)==?(.*)$/.exec(req);
  if (eq) return labels[(eq[1] ?? '').trim()] === (eq[2] ?? '').trim();
  return Object.hasOwn(labels, req.trim());
}

/** Operation state used by the filter: the operation phase, or "Deleting". */
export function operationFilterValue(app: Application): string | undefined {
  if (isDeleting(app)) return 'Deleting';
  return app.status?.operationState?.phase;
}

export function autoSyncValue(app: Application): AutoSyncFilter {
  return isAutoSyncEnabled(app) ? 'Enabled' : 'Disabled';
}

function searchMatches(app: Application, search: string): boolean {
  if (!search) return true;
  const q = search.toLowerCase();
  return (app.metadata.name ?? '').toLowerCase().includes(q) || (app.metadata.namespace ?? '').toLowerCase().includes(q);
}

/** Values of one dimension for an app (an app can match several repositories). */
export function dimensionValues(app: Application, dimension: Dimension): string[] {
  switch (dimension) {
    case 'projects':
      return [app.spec.project];
    case 'sync':
      return [syncOf(app)];
    case 'health':
      return [healthOf(app)];
    case 'operation': {
      const value = operationFilterValue(app);
      return value ? [value] : [];
    }
    case 'namespaces':
      return [app.spec.destination.namespace ?? ''];
    case 'clusters': {
      const d = app.spec.destination;
      return [d.name, d.server, d.name && d.server ? `${d.name} (${d.server})` : undefined].filter((v): v is string => Boolean(v));
    }
    case 'repos':
      return appSources(app).map((s) => s.repoURL);
    case 'targetRevisions':
      return appSources(app).map((s) => s.targetRevision ?? 'HEAD');
    case 'autoSync':
      return [autoSyncValue(app)];
  }
}

/** Glob dimensions match patterns; the others compare exactly. */
const GLOB: ReadonlySet<Dimension> = new Set<Dimension>(['namespaces', 'clusters', 'repos', 'targetRevisions']);

function matchesDimension(app: Application, dimension: Dimension, selected: readonly string[]): boolean {
  if (selected.length === 0) return true;
  const values = dimensionValues(app, dimension);
  if (GLOB.has(dimension)) return selected.some((pattern) => values.some((v) => matchGlob(v, pattern)));
  return selected.some((s) => values.includes(s));
}

const DIMENSIONS: readonly Dimension[] = ['projects', 'sync', 'health', 'operation', 'namespaces', 'clusters', 'repos', 'targetRevisions', 'autoSync'];

function matches(app: Application, filters: AppFilters, favorites: ReadonlySet<string>, skip?: Dimension): boolean {
  if (filters.favorites && !favorites.has(appKey(app))) return false;
  if (!searchMatches(app, filters.search)) return false;
  const labels = app.metadata.labels ?? {};
  if (!filters.labels.every((selector) => matchLabelSelector(labels, selector))) return false;
  const annotations = app.metadata.annotations ?? {};
  if (!filters.annotations.every((selector) => matchLabelSelector(annotations, selector))) return false;
  return DIMENSIONS.every((d) => d === skip || matchesDimension(app, d, filters[d]));
}

export function applyFilters(apps: readonly Application[], filters: AppFilters, favorites: ReadonlySet<string>): Application[] {
  return apps.filter((app) => matches(app, filters, favorites));
}

/**
 * Option counts for one dimension, computed against every other active filter (so selecting a
 * value does not zero the counts of its siblings).
 */
export function dimensionCounts(
  apps: readonly Application[],
  filters: AppFilters,
  favorites: ReadonlySet<string>,
  dimension: Dimension,
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const app of apps) {
    if (!matches(app, filters, favorites, dimension)) continue;
    const values = dimension === 'clusters' ? [clusterOf(app)] : dimensionValues(app, dimension);
    for (const value of new Set(values)) counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  return counts;
}

function syncedAt(app: Application): number {
  const finished = app.status?.operationState?.finishedAt;
  return finished ? Date.parse(finished) : 0;
}

/** Name: natural order; created and last synced: newest first. */
export function sortApps(apps: readonly Application[], sort: SortKey): Application[] {
  const byName = (a: Application, b: Application) => compareValues(a.metadata.name, b.metadata.name) || compareValues(a.metadata.namespace, b.metadata.namespace);
  const list = [...apps];
  switch (sort) {
    case 'created':
      return list.sort((a, b) => compareValues(b.metadata.creationTimestamp, a.metadata.creationTimestamp) || byName(a, b));
    case 'synced':
      return list.sort((a, b) => syncedAt(b) - syncedAt(a) || byName(a, b));
    case 'name':
      return list.sort(byName);
  }
}

export function parseView(value: string | null): ViewMode {
  return value === 'list' || value === 'summary' ? value : 'tiles';
}

export function parseSort(value: string | null): SortKey {
  return value === 'created' || value === 'synced' ? value : 'name';
}
