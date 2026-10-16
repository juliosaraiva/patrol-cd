import { useCallback, useState } from 'react';
import { readJSON, writeJSON } from '../../../../lib/storage';

/*
 * Per-browser view preferences of the resource views (zoom, diff layout, log options). Stored
 * through lib/storage, which never throws; losing them only resets the defaults.
 */

export interface ResourceViewPrefs {
  zoom: number;
  groupSimilar: boolean;
  /** Diff: 2 lines of context instead of the full manifest. */
  compactDiff: boolean;
  /** Diff: unified (inline) instead of side by side. */
  inlineDiff: boolean;
  logWrap: boolean;
  logTimestamps: boolean;
  logFollow: boolean;
}

export const DEFAULT_PREFS: ResourceViewPrefs = {
  zoom: 1,
  groupSimilar: true,
  compactDiff: true,
  inlineDiff: false,
  logWrap: false,
  logTimestamps: false,
  logFollow: true,
};

const STORAGE_KEY = 'patrol.appResources.prefs';

export function readPrefs(): ResourceViewPrefs {
  const stored = readJSON<Partial<ResourceViewPrefs> | null>(STORAGE_KEY, null);
  if (!stored || typeof stored !== 'object') return DEFAULT_PREFS;
  const merged: ResourceViewPrefs = { ...DEFAULT_PREFS };
  for (const key of Object.keys(DEFAULT_PREFS) as (keyof ResourceViewPrefs)[]) {
    const value = stored[key];
    if (typeof value === typeof DEFAULT_PREFS[key]) (merged as unknown as Record<string, unknown>)[key] = value;
  }
  return merged;
}

/** One preference with persistence. */
export function usePref<K extends keyof ResourceViewPrefs>(key: K): [ResourceViewPrefs[K], (value: ResourceViewPrefs[K]) => void] {
  const [value, setValue] = useState<ResourceViewPrefs[K]>(() => readPrefs()[key]);
  const update = useCallback(
    (next: ResourceViewPrefs[K]) => {
      setValue(next);
      writeJSON(STORAGE_KEY, { ...readPrefs(), [key]: next });
    },
    [key],
  );
  return [value, update];
}
