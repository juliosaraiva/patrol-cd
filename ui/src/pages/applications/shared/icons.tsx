import type { ReactNode, SVGProps } from 'react';

/** Area-local icons (16×16 grid, 1.5px strokes, currentColor) missing from the shared set. */
interface Props extends Omit<SVGProps<SVGSVGElement>, 'children'> {
  size?: number;
}

function Svg({ size = 16, children, ...rest }: Props & { children: ReactNode }) {
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
      aria-hidden="true"
      focusable="false"
      {...rest}
    >
      {children}
    </svg>
  );
}

const STAR = 'M8 1.75 9.9 5.6l4.25.62-3.08 3 .73 4.23L8 11.45 4.2 13.45l.73-4.23-3.08-3 4.25-.62Z';

export function IconStar(props: Props) {
  return (
    <Svg {...props}>
      <path d={STAR} />
    </Svg>
  );
}

export function IconStarFilled(props: Props) {
  return (
    <Svg {...props}>
      <path d={STAR} fill="currentColor" />
    </Svg>
  );
}

export function IconSync(props: Props) {
  return (
    <Svg {...props}>
      <path d="M2.75 6.5a5.25 5.25 0 0 1 9.4-2.4M13.25 9.5a5.25 5.25 0 0 1-9.4 2.4" />
      <path d="M12.5 1.75v2.6H9.9M3.5 14.25v-2.6h2.6" />
    </Svg>
  );
}

export function IconHistory(props: Props) {
  return (
    <Svg {...props}>
      <path d="M2.5 8a5.5 5.5 0 1 0 1.6-3.9" />
      <path d="M2.25 2.75v2.5h2.5M8 5v3.25l2 1.25" />
    </Svg>
  );
}

export function IconTrash(props: Props) {
  return (
    <Svg {...props}>
      <path d="M2.75 4.25h10.5M6.25 4.25V2.75h3.5v1.5M4 4.25l.6 9h6.8l.6-9M6.75 7v4M9.25 7v4" />
    </Svg>
  );
}

export function IconDiff(props: Props) {
  return (
    <Svg {...props}>
      <path d="M4.5 2.5v6M1.5 5.5h6M9 11.5h6M11.5 2.75 4.5 13.25" />
    </Svg>
  );
}

export function IconDetails(props: Props) {
  return (
    <Svg {...props}>
      <path d="M2.75 2.75h10.5v10.5H2.75zM5.25 5.75h5.5M5.25 8h5.5M5.25 10.25h3" />
    </Svg>
  );
}

export function IconStop(props: Props) {
  return (
    <Svg {...props}>
      <rect x="3.5" y="3.5" width="9" height="9" rx="1" />
    </Svg>
  );
}

export function IconLink(props: Props) {
  return (
    <Svg {...props}>
      <path d="M6.75 9.25a2.75 2.75 0 0 0 3.9 0l2.1-2.1a2.75 2.75 0 0 0-3.9-3.9l-.6.6M9.25 6.75a2.75 2.75 0 0 0-3.9 0l-2.1 2.1a2.75 2.75 0 0 0 3.9 3.9l.6-.6" />
    </Svg>
  );
}
