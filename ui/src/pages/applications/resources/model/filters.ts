import type { ResourceItem } from './resources';

/** Resource filters; serialized to URL search params so filtered views can be shared. */
export interface ResourceFilters {
  /** Name filter; `*` and `?` are wildcards, otherwise a case-insensitive substring. */
  name: string;
  kinds: string[];
  /** "Synced" and/or "OutOfSync" (which also matches Unknown). */
  sync: string[];
  health: string[];
  namespaces: string[];
  /** Include orphaned resources (only offered when the project monitors them). */
  showOrphaned: boolean;
  /** Show sync hooks (on by default). */
  showHooks: boolean;
}

export const EMPTY_FILTERS: ResourceFilters = {
  name: '',
  kinds: [],
  sync: [],
  health: [],
  namespaces: [],
  showOrphaned: false,
  showHooks: true,
};

export const FILTER_PARAMS = {
  name: 'name',
  kind: 'kind',
  sync: 'sync',
  health: 'health',
  namespace: 'namespace',
  orphaned: 'orphaned',
  hooks: 'hooks',
} as const;

function list(params: URLSearchParams, key: string): string[] {
  return params
    .getAll(key)
    .flatMap((v) => v.split(','))
    .map((v) => v.trim())
    .filter(Boolean);
}

export function filtersFromParams(params: URLSearchParams): ResourceFilters {
  return {
    name: params.get(FILTER_PARAMS.name) ?? '',
    kinds: list(params, FILTER_PARAMS.kind),
    sync: list(params, FILTER_PARAMS.sync),
    health: list(params, FILTER_PARAMS.health),
    namespaces: list(params, FILTER_PARAMS.namespace),
    showOrphaned: params.get(FILTER_PARAMS.orphaned) === 'true',
    showHooks: params.get(FILTER_PARAMS.hooks) !== 'false',
  };
}

/** Writes `filters` into a copy of `params` (other params are kept). */
export function applyFiltersToParams(params: URLSearchParams, filters: ResourceFilters): URLSearchParams {
  const next = new URLSearchParams(params);
  const set = (key: string, value: string | null) => {
    if (value) next.set(key, value);
    else next.delete(key);
  };
  set(FILTER_PARAMS.name, filters.name.trim() || null);
  set(FILTER_PARAMS.kind, filters.kinds.join(',') || null);
  set(FILTER_PARAMS.sync, filters.sync.join(',') || null);
  set(FILTER_PARAMS.health, filters.health.join(',') || null);
  set(FILTER_PARAMS.namespace, filters.namespaces.join(',') || null);
  set(FILTER_PARAMS.orphaned, filters.showOrphaned ? 'true' : null);
  set(FILTER_PARAMS.hooks, filters.showHooks ? null : 'false');
  return next;
}

/** True when a narrowing filter (name, kind, sync, health, namespace) is active. */
export function hasNarrowingFilters(f: ResourceFilters): boolean {
  return f.name.trim() !== '' || f.kinds.length > 0 || f.sync.length > 0 || f.health.length > 0 || f.namespaces.length > 0;
}

/**
 * Glob match where `*` is any run of characters and `?` exactly one, over code points. The
 * pattern comes from a shareable `?name=` link, so this is the iterative two-pointer matcher
 * (backtracking only to the last `*`): O(name × pattern) worst case. A compiled RegExp of
 * `.*` runs backtracks polynomially and lets a crafted link freeze the page.
 * Both arguments must already be case-folded.
 */
export function globMatch(pattern: readonly string[], text: readonly string[]): boolean {
  let p = 0;
  let t = 0;
  let star = -1;
  let resume = 0;
  while (t < text.length) {
    const c = pattern[p];
    if (c === '*') {
      star = p;
      p += 1;
      resume = t;
    } else if (c !== undefined && (c === '?' || c === text[t])) {
      p += 1;
      t += 1;
    } else if (star >= 0) {
      p = star + 1;
      resume += 1;
      t = resume;
    } else {
      return false;
    }
  }
  while (pattern[p] === '*') p += 1;
  return p === pattern.length;
}

/** Compiles the name filter: wildcards anchor the whole name, plain text matches a substring. */
export function compileNameFilter(pattern: string): (name: string) => boolean {
  const trimmed = pattern.trim().toLowerCase();
  if (!trimmed) return () => true;
  if (trimmed.includes('*') || trimmed.includes('?')) {
    // Runs of `*` are equivalent to one.
    const glob = Array.from(trimmed.replace(/\*+/g, '*'));
    return (name) => globMatch(glob, Array.from(name.toLowerCase()));
  }
  return (name) => name.toLowerCase().includes(trimmed);
}

/**
 * Predicate for the narrowing filters. Hooks ignore the sync filter (they are shown whatever
 * their comparison state), and the hooks toggle hides them entirely.
 */
export function createMatcher(f: ResourceFilters): (item: ResourceItem) => boolean {
  const nameMatches = compileNameFilter(f.name);
  const kinds = new Set(f.kinds);
  const health = new Set(f.health);
  const namespaces = new Set(f.namespaces);
  const sync = new Set(f.sync);
  if (sync.has('OutOfSync')) sync.add('Unknown');
  return (item) => {
    if (!f.showHooks && item.hook) return false;
    if (!nameMatches(item.name)) return false;
    if (kinds.size > 0 && !kinds.has(item.kind)) return false;
    if (namespaces.size > 0 && !namespaces.has(item.namespace)) return false;
    if (health.size > 0 && !health.has(item.health?.status ?? '')) return false;
    if (sync.size > 0 && !item.hook && !sync.has(item.sync ?? '')) return false;
    return true;
  };
}

/** Distinct values offered by the filter menus, sorted. */
export function filterOptions(items: readonly ResourceItem[]): { kinds: string[]; namespaces: string[]; health: string[] } {
  const kinds = new Set<string>();
  const namespaces = new Set<string>();
  const health = new Set<string>();
  for (const item of items) {
    kinds.add(item.kind);
    if (item.namespace) namespaces.add(item.namespace);
    if (item.health?.status) health.add(item.health.status);
  }
  const sort = (s: Set<string>) => [...s].sort((a, b) => a.localeCompare(b));
  return { kinds: sort(kinds), namespaces: sort(namespaces), health: sort(health) };
}
