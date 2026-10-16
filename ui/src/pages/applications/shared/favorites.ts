/** Favorite applications, per browser (localStorage; a convenience, never authoritative). */
import { useCallback, useSyncExternalStore } from 'react';
import { readJSON, writeJSON } from '../../../lib/storage';

export const FAVORITES_KEY = 'patrol.apps.favorites';

const listeners = new Set<() => void>();
let cache: { raw: string; set: ReadonlySet<string> } | null = null;

function snapshot(): ReadonlySet<string> {
  const value = readJSON<unknown>(FAVORITES_KEY, []);
  const list = Array.isArray(value) ? value.filter((v): v is string => typeof v === 'string') : [];
  const raw = list.join('\n');
  if (cache?.raw !== raw) cache = { raw, set: new Set(list) };
  return cache.set;
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  const onStorage = (e: StorageEvent) => {
    if (e.key === null || e.key === FAVORITES_KEY) listener();
  };
  window.addEventListener('storage', onStorage);
  return () => {
    listeners.delete(listener);
    window.removeEventListener('storage', onStorage);
  };
}

export function toggleFavorite(key: string): void {
  const next = new Set(snapshot());
  if (next.has(key)) next.delete(key);
  else next.add(key);
  writeJSON(FAVORITES_KEY, [...next].sort());
  listeners.forEach((l) => l());
}

export function useFavorites(): { favorites: ReadonlySet<string>; toggle: (key: string) => void } {
  const favorites = useSyncExternalStore(subscribe, snapshot, snapshot);
  const toggle = useCallback((key: string) => toggleFavorite(key), []);
  return { favorites, toggle };
}
