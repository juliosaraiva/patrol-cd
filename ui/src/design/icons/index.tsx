import type { ReactNode, SVGProps } from 'react';

/*
 * Inline SVG icons authored for Patrol CD (16×16 grid, 1.5px strokes, currentColor). Decorative
 * by default (aria-hidden); pass `title` when an icon is the only content conveying meaning.
 * Status glyphs (health, sync, operation, connection) live in ../status/glyphs.tsx.
 */

export interface IconProps extends Omit<SVGProps<SVGSVGElement>, 'children'> {
  size?: number;
  title?: string;
}

function createIcon(displayName: string, children: ReactNode) {
  function Icon({ size = 16, title, ...rest }: IconProps) {
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
        {...(title ? { role: 'img', 'aria-label': title } : { 'aria-hidden': true })}
        {...rest}
      >
        {title ? <title>{title}</title> : null}
        {children}
      </svg>
    );
  }
  Icon.displayName = displayName;
  return Icon;
}

export const IconApplications = createIcon(
  'IconApplications',
  <>
    <rect x="2" y="2" width="5" height="5" rx="1" />
    <rect x="9" y="2" width="5" height="5" rx="1" />
    <rect x="2" y="9" width="5" height="5" rx="1" />
    <rect x="9" y="9" width="5" height="5" rx="1" />
  </>,
);

export const IconApplicationSets = createIcon(
  'IconApplicationSets',
  <>
    <rect x="5" y="2" width="9" height="9" rx="1" />
    <path d="M3 5v8a1 1 0 0 0 1 1h8" />
  </>,
);

export const IconSettings = createIcon(
  'IconSettings',
  <>
    <path d="M2 4h7M12 4h2M2 8h2M7 8h7M2 12h9M14 12h0" />
    <circle cx="10.5" cy="4" r="1.5" />
    <circle cx="5.5" cy="8" r="1.5" />
    <circle cx="12.5" cy="12" r="1.5" />
  </>,
);

export const IconUser = createIcon(
  'IconUser',
  <>
    <circle cx="8" cy="5.5" r="2.75" />
    <path d="M2.75 14c.6-2.6 2.7-4 5.25-4s4.65 1.4 5.25 4" />
  </>,
);

export const IconHelp = createIcon(
  'IconHelp',
  <>
    <circle cx="8" cy="8" r="6.25" />
    <path d="M6.25 6.25a1.8 1.8 0 1 1 2.5 1.65c-.5.2-.75.6-.75 1.1v.5" />
    <path d="M8 11.5v.01" strokeWidth={2} />
  </>,
);

export const IconSearch = createIcon(
  'IconSearch',
  <>
    <circle cx="7" cy="7" r="4.25" />
    <path d="m10.25 10.25 3.5 3.5" />
  </>,
);

export const IconMenu = createIcon('IconMenu', <path d="M2.5 4h11M2.5 8h11M2.5 12h11" />);
export const IconClose = createIcon('IconClose', <path d="m4 4 8 8M12 4l-8 8" />);
export const IconChevronDown = createIcon('IconChevronDown', <path d="m4 6 4 4 4-4" />);
export const IconChevronUp = createIcon('IconChevronUp', <path d="m4 10 4-4 4 4" />);
export const IconChevronLeft = createIcon('IconChevronLeft', <path d="M10 4 6 8l4 4" />);
export const IconChevronRight = createIcon('IconChevronRight', <path d="m6 4 4 4-4 4" />);
export const IconPlus = createIcon('IconPlus', <path d="M8 3v10M3 8h10" />);
export const IconCheck = createIcon('IconCheck', <path d="m3 8.5 3.25 3.25L13 5" />);
export const IconMinus = createIcon('IconMinus', <path d="M3.5 8h9" />);

export const IconCopy = createIcon(
  'IconCopy',
  <>
    <rect x="5.5" y="5.5" width="8" height="8" rx="1" />
    <path d="M10.5 3.5V3a1 1 0 0 0-1-1H3a1 1 0 0 0-1 1v6.5a1 1 0 0 0 1 1h.5" />
  </>,
);

export const IconExternal = createIcon(
  'IconExternal',
  <>
    <path d="M9 2.5h4.5V7" />
    <path d="m13.5 2.5-6 6" />
    <path d="M11.5 9.5v3a1 1 0 0 1-1 1h-7a1 1 0 0 1-1-1v-7a1 1 0 0 1 1-1h3" />
  </>,
);

export const IconLogout = createIcon(
  'IconLogout',
  <>
    <path d="M6 2.5H3.5a1 1 0 0 0-1 1v9a1 1 0 0 0 1 1H6" />
    <path d="M10.5 11 13.5 8l-3-3M13.5 8H6.5" />
  </>,
);

export const IconSun = createIcon(
  'IconSun',
  <>
    <circle cx="8" cy="8" r="2.75" />
    <path d="M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.4 3.4l1.06 1.06M11.54 11.54l1.06 1.06M3.4 12.6l1.06-1.06M11.54 4.46l1.06-1.06" />
  </>,
);

export const IconMoon = createIcon('IconMoon', <path d="M13.5 9.5A5.75 5.75 0 0 1 6.5 2.5a5.75 5.75 0 1 0 7 7Z" />);

export const IconMonitor = createIcon(
  'IconMonitor',
  <>
    <rect x="1.75" y="2.5" width="12.5" height="8.5" rx="1" />
    <path d="M5.5 14h5M8 11v3" />
  </>,
);

export const IconInfo = createIcon(
  'IconInfo',
  <>
    <circle cx="8" cy="8" r="6.25" />
    <path d="M8 7.25V11" />
    <path d="M8 5v.01" strokeWidth={2} />
  </>,
);

export const IconWarning = createIcon(
  'IconWarning',
  <>
    <path d="M7.13 2.5a1 1 0 0 1 1.74 0l5.5 9.75a1 1 0 0 1-.87 1.5h-11a1 1 0 0 1-.87-1.5Z" />
    <path d="M8 6.25v3" />
    <path d="M8 11.5v.01" strokeWidth={2} />
  </>,
);

export const IconError = createIcon(
  'IconError',
  <>
    <path d="M5.4 1.75h5.2l3.65 3.65v5.2l-3.65 3.65H5.4L1.75 10.6V5.4Z" />
    <path d="M8 4.75v3.75" />
    <path d="M8 11v.01" strokeWidth={2} />
  </>,
);

export const IconSortAscending = createIcon('IconSortAscending', <path d="M8 13V3M4.5 6.5 8 3l3.5 3.5" />);
export const IconSortDescending = createIcon('IconSortDescending', <path d="M8 3v10M4.5 9.5 8 13l3.5-3.5" />);
export const IconSortNone = createIcon('IconSortNone', <path d="M5 6l3-3 3 3M5 10l3 3 3-3" />);

export const IconKeyboard = createIcon(
  'IconKeyboard',
  <>
    <rect x="1.75" y="3.75" width="12.5" height="8.5" rx="1" />
    <path d="M4.5 6.5h.01M7 6.5h.01M9.5 6.5h.01M12 6.5h.01M5 9.5h6" />
  </>,
);

export const IconRefresh = createIcon(
  'IconRefresh',
  <>
    <path d="M13.25 7.5A5.25 5.25 0 0 0 3.6 4.6L2.5 6" />
    <path d="M2.5 2.75V6h3.25" />
    <path d="M2.75 8.5a5.25 5.25 0 0 0 9.65 2.9L13.5 10" />
    <path d="M13.5 13.25V10h-3.25" />
  </>,
);

export const IconBook = createIcon(
  'IconBook',
  <>
    <path d="M2.5 3a1 1 0 0 1 1-1H7a1 1 0 0 1 1 1v11a1.5 1.5 0 0 0-1.5-1.5h-4Z" />
    <path d="M13.5 3a1 1 0 0 0-1-1H9a1 1 0 0 0-1 1v11a1.5 1.5 0 0 1 1.5-1.5h4Z" />
  </>,
);

export const IconFilter = createIcon('IconFilter', <path d="M2 3h12l-4.5 5.25V13l-3-1.5V8.25Z" />);

export const IconMore = createIcon(
  'IconMore',
  <>
    <path d="M3.5 8h.01M8 8h.01M12.5 8h.01" strokeWidth={2.5} />
  </>,
);

export const IconDrag = createIcon('IconDrag', <path d="M6 4h.01M10 4h.01M6 8h.01M10 8h.01M6 12h.01M10 12h.01" strokeWidth={2} />);

/** Patrol CD mark: a "P" inside a 4px-radius square (not a reused third-party logo). */
export function PatrolMark({ size = 20 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 20 20" aria-hidden="true" focusable="false">
      <rect width="20" height="20" rx="4" fill="var(--color-accent)" />
      <path
        d="M6 15V5h5a3.25 3.25 0 0 1 0 6.5H8"
        fill="none"
        stroke="var(--color-on-accent)"
        strokeWidth={2}
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  );
}
