/** Duration parsing for sync windows (Go time.ParseDuration syntax) and token lifetimes. */

const GO_UNITS: Record<string, number> = {
  ns: 1e-6,
  us: 1e-3,
  µs: 1e-3,
  μs: 1e-3,
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
};

/**
 * Parses a Go duration ("1h", "1h30m", "90m", "1.5h", "300ms") into milliseconds. Returns null
 * for anything time.ParseDuration would reject. A bare "0" is accepted, as in Go.
 */
export function parseGoDuration(input: string): number | null {
  let s = input.trim();
  if (s === '') return null;
  let sign = 1;
  if (s.startsWith('-') || s.startsWith('+')) {
    if (s.startsWith('-')) sign = -1;
    s = s.slice(1);
  }
  if (s === '0') return 0;
  if (s === '') return null;
  const part = /^(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/;
  let total = 0;
  while (s.length > 0) {
    const match = part.exec(s);
    if (!match) return null;
    const [whole, value = '', unit = ''] = match;
    const factor = GO_UNITS[unit];
    if (factor === undefined) return null;
    total += Number(value) * factor;
    s = s.slice(whole.length);
  }
  return sign * total;
}

const TOKEN_UNITS: Record<string, number> = { s: 1, m: 60, h: 3600, d: 86_400, w: 604_800 };

/**
 * Parses a token lifetime ("30m", "12h", "7d", "1w", "1d12h") into seconds. An empty value means
 * "never expires" (0). Returns null when the value is not understood.
 */
export function parseTokenLifetime(input: string): number | null {
  const s = input.trim().toLowerCase();
  if (s === '') return 0;
  if (/^\d+$/.test(s)) return Number(s);
  const part = /^(\d+)([smhdw])/;
  let rest = s;
  let total = 0;
  while (rest.length > 0) {
    const match = part.exec(rest);
    if (!match) return null;
    const [whole, value = '0', unit = 's'] = match;
    total += Number(value) * (TOKEN_UNITS[unit] ?? 0);
    rest = rest.slice(whole.length);
  }
  return total;
}

/** "1h 30m", "2d", "45s": compact human form of a duration in milliseconds. */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0s';
  const parts: string[] = [];
  let rest = Math.round(ms / 1000);
  for (const [unit, size] of [
    ['d', 86_400],
    ['h', 3600],
    ['m', 60],
    ['s', 1],
  ] as const) {
    const n = Math.floor(rest / size);
    if (n > 0) parts.push(`${n}${unit}`);
    rest -= n * size;
  }
  return parts.join(' ');
}
