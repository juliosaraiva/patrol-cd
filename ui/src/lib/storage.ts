/**
 * Browser storage that never throws: private windows, blocked site data and quota errors all
 * degrade to "nothing stored". Use only for per-browser conveniences (theme, dismissed banners,
 * view preferences), never for state that must persist.
 */
type Area = 'local' | 'session';

function area(kind: Area): Storage | null {
  try {
    return kind === 'local' ? window.localStorage : window.sessionStorage;
  } catch {
    return null;
  }
}

export function readStorage(key: string, kind: Area = 'local'): string | null {
  try {
    return area(kind)?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

export function writeStorage(key: string, value: string, kind: Area = 'local'): void {
  try {
    area(kind)?.setItem(key, value);
  } catch {
    // Storage unavailable or full: the preference simply is not remembered.
  }
}

export function removeStorage(key: string, kind: Area = 'local'): void {
  try {
    area(kind)?.removeItem(key);
  } catch {
    // Ignore: see writeStorage.
  }
}

export function readJSON<T>(key: string, fallback: T, kind: Area = 'local'): T {
  const raw = readStorage(key, kind);
  if (raw === null) return fallback;
  try {
    return JSON.parse(raw) as T;
  } catch {
    return fallback;
  }
}

export function writeJSON(key: string, value: unknown, kind: Area = 'local'): void {
  writeStorage(key, JSON.stringify(value), kind);
}
