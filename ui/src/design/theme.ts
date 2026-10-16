import { useSyncExternalStore } from 'react';
import { readStorage, removeStorage, writeStorage } from '../lib/storage';

/** "system" follows prefers-color-scheme; "light"/"dark" are explicit user overrides. */
export type ThemePreference = 'system' | 'light' | 'dark';
export type ResolvedTheme = 'light' | 'dark';

export const THEME_STORAGE_KEY = 'patrol.theme';

const listeners = new Set<() => void>();
let preference: ThemePreference = 'system';

function parse(value: string | null): ThemePreference {
  return value === 'light' || value === 'dark' ? value : 'system';
}

function apply(pref: ThemePreference) {
  const root = document.documentElement;
  if (pref === 'system') delete root.dataset.theme;
  else root.dataset.theme = pref;
}

/** Applies the stored preference to <html>; call once before the first render. */
export function initTheme(): ThemePreference {
  preference = parse(readStorage(THEME_STORAGE_KEY));
  apply(preference);
  return preference;
}

export function getThemePreference(): ThemePreference {
  return preference;
}

export function setThemePreference(next: ThemePreference): void {
  preference = next;
  if (next === 'system') removeStorage(THEME_STORAGE_KEY);
  else writeStorage(THEME_STORAGE_KEY, next);
  apply(next);
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function systemPrefersDark(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia('(prefers-color-scheme: dark)').matches;
}

function subscribeSystem(listener: () => void): () => void {
  if (typeof window.matchMedia !== 'function') return () => undefined;
  const query = window.matchMedia('(prefers-color-scheme: dark)');
  query.addEventListener('change', listener);
  return () => query.removeEventListener('change', listener);
}

/** Current preference plus the theme actually in effect. */
export function useTheme(): { preference: ThemePreference; resolved: ResolvedTheme; setPreference: (p: ThemePreference) => void } {
  const pref = useSyncExternalStore(subscribe, getThemePreference, getThemePreference);
  const systemDark = useSyncExternalStore(subscribeSystem, systemPrefersDark, () => false);
  const resolved: ResolvedTheme = pref === 'system' ? (systemDark ? 'dark' : 'light') : pref;
  return { preference: pref, resolved, setPreference: setThemePreference };
}
