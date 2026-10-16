import type { ReactNode } from 'react';

/*
 * Status glyph families (see docs/ui/design-system.md, "Status"):
 *
 *  - Health:      solid silhouettes, every state a different outline shape
 *                 (circle, half-filled ring, triangle, square, dashed ring, diamond).
 *  - Sync:        comparison strokes (= / ≠ / dashed =) inside a bordered square box.
 *  - Operation:   bare process strokes (hourglass, play, stop, check, cross, !, slash, return)
 *                 inside a bordered tag together with the label.
 *  - Connection:  two endpoints and a link (solid / broken / dotted).
 *
 * All glyphs are aria-hidden: the visible (or visually hidden) text label carries the meaning,
 * the shape is the redundant non-color cue, and color is the third cue.
 */

const INNER = 'var(--color-surface)';

function Glyph({ children, size = 14 }: { children: ReactNode; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" aria-hidden="true" focusable="false" data-glyph="">
      {children}
    </svg>
  );
}

const stroke = { fill: 'none', stroke: 'currentColor', strokeWidth: 1.5, strokeLinecap: 'round', strokeLinejoin: 'round' } as const;
const innerStroke = { fill: 'none', stroke: INNER, strokeWidth: 1.75, strokeLinecap: 'round', strokeLinejoin: 'round' } as const;

export const HEALTH_GLYPHS = {
  Healthy: (
    <Glyph>
      <circle cx="8" cy="8" r="6.5" fill="currentColor" />
      <path d="m5 8.25 2 2 4-4.25" {...innerStroke} />
    </Glyph>
  ),
  Progressing: (
    <Glyph>
      <circle cx="8" cy="8" r="5.75" {...stroke} />
      <path d="M8 2.25a5.75 5.75 0 0 0 0 11.5Z" fill="currentColor" />
    </Glyph>
  ),
  Degraded: (
    <Glyph>
      <path d="M8 1.5 14.75 14H1.25Z" fill="currentColor" stroke="currentColor" strokeWidth={1} strokeLinejoin="round" />
      <path d="M8 6.25v3.25" {...innerStroke} />
      <circle cx="8" cy="11.9" r="0.95" fill={INNER} />
    </Glyph>
  ),
  Suspended: (
    <Glyph>
      <rect x="2" y="2" width="12" height="12" rx="2" fill="currentColor" />
      <path d="M6.25 5.5v5M9.75 5.5v5" {...innerStroke} />
    </Glyph>
  ),
  Missing: (
    <Glyph>
      <circle cx="8" cy="8" r="5.75" {...stroke} strokeDasharray="2.25 2" strokeLinecap="butt" />
    </Glyph>
  ),
  Unknown: (
    <Glyph>
      <path d="M8 1 15 8 8 15 1 8Z" fill="currentColor" />
      <path d="M6.6 6.5a1.45 1.45 0 1 1 2.05 1.3c-.42.2-.65.5-.65.95v.3" {...innerStroke} strokeWidth={1.5} />
      <circle cx="8" cy="11.1" r="0.85" fill={INNER} />
    </Glyph>
  ),
} as const;

export const SYNC_GLYPHS = {
  Synced: (
    <Glyph size={12}>
      <path d="M3.5 6h9M3.5 10h9" {...stroke} strokeWidth={1.75} />
    </Glyph>
  ),
  OutOfSync: (
    <Glyph size={12}>
      <path d="M3.5 6h9M3.5 10h9M10.25 3 5.75 13" {...stroke} strokeWidth={1.75} />
    </Glyph>
  ),
  Unknown: (
    <Glyph size={12}>
      <path d="M3.5 6h9M3.5 10h9" {...stroke} strokeWidth={1.75} strokeDasharray="1.5 2" strokeLinecap="butt" />
    </Glyph>
  ),
} as const;

export const OPERATION_GLYPHS = {
  Pending: (
    <Glyph size={12}>
      <path d="M4.5 2.5h7M4.5 13.5h7M5.5 2.5 8 8l-2.5 5.5M10.5 2.5 8 8l2.5 5.5" {...stroke} />
    </Glyph>
  ),
  Running: (
    <Glyph size={12}>
      <path d="M5 3.25v9.5L12.5 8Z" {...stroke} />
    </Glyph>
  ),
  Terminating: (
    <Glyph size={12}>
      <rect x="3.75" y="3.75" width="8.5" height="8.5" rx="1" {...stroke} />
    </Glyph>
  ),
  Succeeded: (
    <Glyph size={12}>
      <path d="m3 8.5 3.25 3.25L13 4.75" {...stroke} strokeWidth={1.75} />
    </Glyph>
  ),
  Failed: (
    <Glyph size={12}>
      <path d="m4 4 8 8M12 4l-8 8" {...stroke} strokeWidth={1.75} />
    </Glyph>
  ),
  Error: (
    <Glyph size={12}>
      <path d="M8 2.75v7" {...stroke} strokeWidth={1.75} />
      <circle cx="8" cy="12.75" r="1.1" fill="currentColor" />
    </Glyph>
  ),
  Cancelled: (
    <Glyph size={12}>
      <circle cx="8" cy="8" r="5.5" {...stroke} />
      <path d="m4.1 11.9 7.8-7.8" {...stroke} />
    </Glyph>
  ),
  Rejected: (
    <Glyph size={12}>
      <path d="M12.5 3.5v3.5a2 2 0 0 1-2 2h-7M6 6l-3 3 3 3" {...stroke} />
    </Glyph>
  ),
} as const;

export const CONNECTION_GLYPHS = {
  Successful: (
    <Glyph>
      <circle cx="3" cy="8" r="1.9" fill="currentColor" />
      <circle cx="13" cy="8" r="1.9" fill="currentColor" />
      <path d="M4.9 8h6.2" {...stroke} strokeWidth={1.75} />
    </Glyph>
  ),
  Failed: (
    <Glyph>
      <circle cx="3" cy="8" r="1.9" fill="currentColor" />
      <circle cx="13" cy="8" r="1.9" fill="currentColor" />
      <path d="M4.9 8h1.6M9.5 8h1.6M9 4.75 7 11.25" {...stroke} />
    </Glyph>
  ),
  Unknown: (
    <Glyph>
      <circle cx="3" cy="8" r="1.9" {...stroke} />
      <circle cx="13" cy="8" r="1.9" {...stroke} />
      <path d="M5.4 8h5.2" {...stroke} strokeDasharray="0.1 2.1" strokeWidth={1.75} />
    </Glyph>
  ),
} as const;
