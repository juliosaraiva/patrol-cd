// @vitest-environment node
import { readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const here = path.dirname(fileURLToPath(import.meta.url));
const tokensCss = readFileSync(path.join(here, 'tokens.css'), 'utf8');

function block(selector: string): Record<string, string> {
  const start = tokensCss.indexOf(`${selector} {`);
  if (start === -1) throw new Error(`selector ${selector} not found`);
  const end = tokensCss.indexOf('}', start);
  const vars: Record<string, string> = {};
  for (const m of tokensCss.slice(start, end).matchAll(/(--[\w-]+):\s*([^;]+);/g)) vars[m[1] ?? ''] = (m[2] ?? '').trim();
  return vars;
}

function luminance(hex: string): number {
  const n = hex.replace('#', '');
  const [r, g, b] = [0, 2, 4].map((i) => {
    const v = parseInt(n.slice(i, i + 2), 16) / 255;
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * (r ?? 0) + 0.7152 * (g ?? 0) + 0.0722 * (b ?? 0);
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return ((hi ?? 0) + 0.05) / ((lo ?? 0) + 0.05);
}

const light = block(':root');
const dark = { ...light, ...block(":root[data-theme='dark']") };

// [foreground, background, minimum ratio]
const TEXT_PAIRS: [string, string, number][] = [
  ['--color-text', '--color-bg', 4.5],
  ['--color-text', '--color-surface-subtle', 4.5],
  ['--color-text', '--color-surface-selected', 4.5],
  ['--color-text-muted', '--color-bg', 4.5],
  ['--color-text-muted', '--color-surface-subtle', 4.5],
  ['--color-text-placeholder', '--color-surface', 4.5],
  ['--color-accent', '--color-bg', 4.5],
  ['--color-accent', '--color-surface-subtle', 4.5],
  ['--color-on-accent', '--color-accent', 4.5],
  ['--color-on-accent', '--color-accent-hover', 4.5],
  ['--color-on-danger', '--color-danger', 4.5],
  ['--color-danger', '--color-surface', 4.5],
  ['--color-warning', '--color-surface', 4.5],
  ['--color-success', '--color-surface', 4.5],
  ['--color-info', '--color-surface', 4.5],
  ['--color-text-inverse', '--color-text', 4.5],
  // Non-text UI boundaries (WCAG 1.4.11) and status glyphs.
  ['--color-border-strong', '--color-surface', 3],
  ['--color-suspended', '--color-surface', 3],
  ['--color-unknown', '--color-surface', 3],
];

describe.each([
  ['light', light],
  ['dark', dark],
])('%s theme', (_name, theme) => {
  it.each(TEXT_PAIRS)('%s on %s meets %d:1', (fg, bg, min) => {
    const f = theme[fg];
    const b = theme[bg];
    expect(f, fg).toMatch(/^#[0-9a-f]{6}$/i);
    expect(b, bg).toMatch(/^#[0-9a-f]{6}$/i);
    expect(contrast(f ?? '', b ?? '')).toBeGreaterThanOrEqual(min);
  });
});

describe('design constraints', () => {
  it('keeps the dark palette identical for the OS preference and the explicit override', () => {
    const mediaStart = tokensCss.indexOf("@media (prefers-color-scheme: dark)");
    const media = tokensCss.slice(mediaStart, tokensCss.indexOf("}\n}", mediaStart));
    const fromMedia = Object.fromEntries(
      [...media.matchAll(/(--[\w-]+):\s*([^;]+);/g)].map((m): [string, string] => [m[1] ?? '', (m[2] ?? '').trim()]),
    );
    expect(fromMedia).toEqual(block(":root[data-theme='dark']"));
  });

  it('uses pure white / near-black page backgrounds', () => {
    expect(light['--color-bg']).toBe('#ffffff');
    expect(luminance(dark['--color-bg'] ?? '#ffffff')).toBeLessThan(0.01);
  });

  it('caps radii at 4px and uses no gradients or backdrop blur anywhere in src', () => {
    expect(light['--radius-md']).toBe('4px');
    const srcRoot = path.resolve(here, '..');
    const cssFiles: string[] = [];
    const walk = (dir: string) => {
      for (const entry of readdirSync(dir)) {
        const full = path.join(dir, entry);
        if (statSync(full).isDirectory()) walk(full);
        else if (full.endsWith('.css')) cssFiles.push(full);
      }
    };
    walk(srcRoot);
    for (const file of cssFiles) {
      const css = readFileSync(file, 'utf8');
      expect(css, file).not.toMatch(/gradient\(/);
      expect(css, file).not.toMatch(/backdrop-filter/);
      for (const m of css.matchAll(/border-radius:\s*(\d+)px/g)) expect(Number(m[1]), file).toBeLessThanOrEqual(4);
      expect(css, file).not.toMatch(/border-radius:\s*(50%|9999px|999px)/);
    }
  });

  it('disables motion under prefers-reduced-motion', () => {
    expect(tokensCss).toMatch(/prefers-reduced-motion: reduce[\s\S]*--duration-fast: 0ms/);
  });
});
