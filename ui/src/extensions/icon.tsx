import type { SVGProps } from 'react';

/**
 * Generic glyph for extension entries (nav items, view switcher, top-bar actions). Extensions
 * name Font Awesome classes, which Patrol does not ship, so every extension uses this mark.
 * Same grid and stroke as src/design/icons.
 */
export function IconExtension({ size = 16, ...rest }: Omit<SVGProps<SVGSVGElement>, 'children'> & { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      focusable="false"
      aria-hidden="true"
      {...rest}
    >
      <path d="M3 5.5h2.25a1.25 1.25 0 1 1 2.5 0H10v2.25a1.25 1.25 0 1 1 0 2.5V12.5H7.75a1.25 1.25 0 1 0-2.5 0H3V10.25a1.25 1.25 0 1 0 0-2.5Z" />
    </svg>
  );
}
