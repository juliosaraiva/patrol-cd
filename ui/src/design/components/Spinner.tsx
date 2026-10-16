import styles from './Spinner.module.css';

export interface SpinnerProps {
  size?: 'sm' | 'md' | 'lg';
  /** Announced (and shown with `showLabel`). Pass "" inside a control that already has a name. */
  label?: string;
  showLabel?: boolean;
}

const SIZES = { sm: 12, md: 16, lg: 24 } as const;

export function Spinner({ size = 'md', label = 'Loading', showLabel = false }: SpinnerProps) {
  const px = SIZES[size];
  const svg = (
    <svg className={styles.ring} width={px} height={px} viewBox="0 0 16 16" aria-hidden="true" focusable="false">
      <circle className={styles.track} cx="8" cy="8" r="6.5" fill="none" stroke="currentColor" strokeWidth="2" />
      <path d="M8 1.5A6.5 6.5 0 0 1 14.5 8" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    </svg>
  );
  if (label === '') return <span className={styles.spinner}>{svg}</span>;
  return (
    <span className={styles.spinner} role="status">
      {svg}
      <span className={showLabel ? undefined : 'sr-only'}>{label}</span>
    </span>
  );
}
