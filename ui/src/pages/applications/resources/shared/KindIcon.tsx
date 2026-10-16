import type { ReactNode } from 'react';
import { kindInitials, resolveKindIcon, type KindIconId } from '../model/kinds';
import styles from './KindIcon.module.css';

/*
 * Resource kind icons drawn for Patrol (16×16 grid, 1.5px strokes, currentColor), deliberately
 * avoiding the circle/triangle/square silhouettes of the health glyphs. Kinds without an icon
 * show their initials in a filled circle (API-127 fallback).
 */
const PATHS: Record<KindIconId, ReactNode> = {
  node: (
    <>
      <rect x="2" y="2.5" width="12" height="4.5" rx="1" />
      <rect x="2" y="9" width="12" height="4.5" rx="1" />
      <path d="M4.5 4.75h.01M4.5 11.25h.01" strokeWidth={2} />
    </>
  ),
  application: (
    <>
      <rect x="2" y="2" width="12" height="12" rx="1" />
      <path d="M2 5.5h12M5 9h6M5 11.5h4" />
    </>
  ),
  applicationset: (
    <>
      <rect x="4.5" y="1.75" width="9.75" height="9.75" rx="1" />
      <path d="M4.5 4.75h9.75M1.75 5v8.25a1 1 0 0 0 1 1H11" />
    </>
  ),
  pod: (
    <>
      <path d="M8 1.75 13.5 4.9v6.2L8 14.25 2.5 11.1V4.9Z" />
      <path d="M2.5 4.9 8 8l5.5-3.1M8 8v6.25" />
    </>
  ),
  deployment: (
    <>
      <rect x="1.75" y="5.25" width="9" height="9" rx="1" />
      <path d="M4.75 2.25h8a1 1 0 0 1 1 1v8" />
      <path d="M4.5 9.75h4M6.5 7.75v4" />
    </>
  ),
  replicaset: (
    <>
      <rect x="1.75" y="6.25" width="8" height="8" rx="1" />
      <path d="M4.25 3.75h8v8M6.75 1.25h8v8" />
    </>
  ),
  statefulset: (
    <>
      <rect x="2" y="2" width="12" height="12" rx="1" />
      <path d="M2 6h12M2 10h12M4.5 4h.01M4.5 8h.01M4.5 12h.01" />
    </>
  ),
  daemonset: (
    <>
      <rect x="1.75" y="1.75" width="5" height="5" rx="1" />
      <rect x="9.25" y="1.75" width="5" height="5" rx="1" />
      <rect x="1.75" y="9.25" width="5" height="5" rx="1" />
      <rect x="9.25" y="9.25" width="5" height="5" rx="1" />
      <path d="M4.25 4.25h.01M11.75 4.25h.01M4.25 11.75h.01M11.75 11.75h.01" strokeWidth={2} />
    </>
  ),
  job: (
    <>
      <rect x="1.75" y="2.5" width="12.5" height="11" rx="1" />
      <path d="m4.75 6 2 2-2 2M8.5 10.5h3" />
    </>
  ),
  cronjob: (
    <>
      <circle cx="8" cy="8.5" r="5.75" />
      <path d="M8 5.5v3l2 1.25M6 1.5h4" />
    </>
  ),
  service: (
    <>
      <circle cx="8" cy="3.75" r="2" />
      <circle cx="3.5" cy="12.25" r="2" />
      <circle cx="12.5" cy="12.25" r="2" />
      <path d="M7 5.5 4.5 10.5M9 5.5l2.5 5" />
    </>
  ),
  ingress: (
    <>
      <path d="M1.75 8h7.5M6.5 5.25 9.25 8 6.5 10.75" />
      <path d="M9.75 2.25h3.5a1 1 0 0 1 1 1v9.5a1 1 0 0 1-1 1h-3.5" />
    </>
  ),
  endpoints: (
    <>
      <path d="M1.75 8h12.5" />
      <circle cx="4" cy="8" r="1.75" />
      <circle cx="12" cy="8" r="1.75" />
      <path d="M8 4v8" />
    </>
  ),
  configmap: (
    <>
      <path d="M3.5 1.75h6l3 3v9.5h-9Z" />
      <path d="M9.5 1.75v3h3M5.5 8h5M5.5 10.75h5" />
    </>
  ),
  secret: (
    <>
      <rect x="3" y="7" width="10" height="7.25" rx="1" />
      <path d="M5.25 7V5a2.75 2.75 0 0 1 5.5 0v2M8 9.75v2" />
    </>
  ),
  pvc: (
    <>
      <ellipse cx="8" cy="3.75" rx="5" ry="2" />
      <path d="M3 3.75v8.5c0 1.1 2.24 2 5 2s5-.9 5-2v-8.5M6 9.25l1.5 1.5 2.5-3" />
    </>
  ),
  pv: (
    <>
      <ellipse cx="8" cy="3.75" rx="5" ry="2" />
      <path d="M3 3.75v8.5c0 1.1 2.24 2 5 2s5-.9 5-2v-8.5M3 8c0 1.1 2.24 2 5 2s5-.9 5-2" />
    </>
  ),
  storageclass: (
    <>
      <ellipse cx="8" cy="3.5" rx="5" ry="1.75" />
      <path d="M3 3.5v9c0 1 2.24 1.75 5 1.75s5-.75 5-1.75v-9M3 6.5c0 1 2.24 1.75 5 1.75s5-.75 5-1.75M3 9.5c0 1 2.24 1.75 5 1.75s5-.75 5-1.75" />
    </>
  ),
  serviceaccount: (
    <>
      <circle cx="6.5" cy="5" r="2.5" />
      <path d="M1.75 13.75c.5-2.5 2.4-3.75 4.75-3.75 1 0 1.9.25 2.6.7" />
      <rect x="10" y="9.5" width="4.5" height="4.75" rx="0.75" />
    </>
  ),
  role: <path d="M8 1.75 13.25 3.75v4c0 3.25-2.25 5.5-5.25 6.5-3-1-5.25-3.25-5.25-6.5v-4Z" />,
  rolebinding: (
    <>
      <path d="M6.5 1.75 11 3.5v3.5c0 2.75-1.9 4.75-4.5 5.6C3.9 11.75 2 9.75 2 7V3.5Z" />
      <path d="M10.5 11.5h3.75M12.5 9.75l1.75 1.75-1.75 1.75" />
    </>
  ),
  namespace: <rect x="1.75" y="1.75" width="12.5" height="12.5" rx="1" strokeDasharray="2.5 1.75" />,
  hpa: (
    <>
      <path d="M1.75 8h12.5M1.75 8 4 5.75M1.75 8 4 10.25M14.25 8 12 5.75M14.25 8 12 10.25" />
      <path d="M6.5 3.5v9M9.5 3.5v9" />
    </>
  ),
  networkpolicy: (
    <>
      <rect x="1.75" y="3" width="12.5" height="10" rx="1" />
      <path d="M1.75 6.33h12.5M1.75 9.66h12.5M6 3v3.33M10 6.33v3.33M6 9.66V13" />
    </>
  ),
  crd: (
    <path d="M6 2.5H5a1.5 1.5 0 0 0-1.5 1.5v2.25L2 8l1.5 1.75V12A1.5 1.5 0 0 0 5 13.5h1M10 2.5h1A1.5 1.5 0 0 1 12.5 4v2.25L14 8l-1.5 1.75V12a1.5 1.5 0 0 1-1.5 1.5h-1" />
  ),
  limitrange: (
    <>
      <path d="M2.25 11.5a5.75 5.75 0 0 1 11.5 0" />
      <path d="M8 11.5 10.75 7M1.75 13.75h12.5" />
    </>
  ),
  quota: (
    <>
      <circle cx="8" cy="8" r="6.25" />
      <path d="M8 1.75V8h6.25" />
    </>
  ),
  psp: (
    <>
      <path d="M8 1.75 13.25 3.75v4c0 3.25-2.25 5.5-5.25 6.5-3-1-5.25-3.25-5.25-6.5v-4Z" />
      <rect x="5.75" y="7.5" width="4.5" height="3.5" rx="0.5" />
      <path d="M6.75 7.5V6.5a1.25 1.25 0 0 1 2.5 0v1" />
    </>
  ),
  user: (
    <>
      <circle cx="8" cy="5.5" r="2.75" />
      <path d="M2.75 14c.6-2.6 2.7-4 5.25-4s4.65 1.4 5.25 4" />
    </>
  ),
  group: (
    <>
      <circle cx="5.75" cy="5.5" r="2.25" />
      <circle cx="11" cy="6" r="1.75" />
      <path d="M1.75 13.5c.5-2.25 2-3.5 4-3.5s3.5 1.25 4 3.5M10.5 9.75c1.75 0 3.25 1 3.75 3" />
    </>
  ),
  volume: (
    <>
      <rect x="2" y="3.5" width="12" height="9" rx="1" />
      <path d="M2 9.5h12M11.5 11h.01" />
    </>
  ),
};

export interface KindIconProps {
  kind: string;
  /** Rendered size in px (square). */
  size?: number;
  /** Position when nested inside an SVG canvas. */
  x?: number;
  y?: number;
  className?: string;
}

/** Icon for a Kubernetes kind; decorative (the kind is always written next to it). */
export function KindIcon({ kind, size = 20, x, y, className }: KindIconProps) {
  const icon = resolveKindIcon(kind);
  if (icon) {
    return (
      <svg
        x={x}
        y={y}
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
        data-kind-icon={icon}
        className={[styles.icon, className].filter(Boolean).join(' ')}
      >
        {PATHS[icon]}
      </svg>
    );
  }
  const initials = kindInitials(kind);
  return (
    <svg
      x={x}
      y={y}
      width={size}
      height={size}
      viewBox="0 0 16 16"
      aria-hidden="true"
      focusable="false"
      data-kind-initials={initials}
      className={[styles.icon, className].filter(Boolean).join(' ')}
    >
      <circle cx="8" cy="8" r="7.5" className={styles.initialsCircle} />
      {initials ? (
        <text x="8" y="8" dy="0.35em" textAnchor="middle" fontSize={initials.length <= 2 ? 7 : 5} className={styles.initialsText}>
          {initials}
        </text>
      ) : null}
    </svg>
  );
}
