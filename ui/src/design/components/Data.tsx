import { useEffect, useId, useRef, useState, type CSSProperties, type KeyboardEvent, type PointerEvent, type ReactNode } from 'react';
import { useNow } from '../hooks';
import { IconCheck, IconCopy } from '../icons';
import { Button, IconButton } from './Button';
import styles from './Data.module.css';
import { Tooltip } from './Tooltip';

/** Inline code: commands, manifests snippets, field paths. */
export function Code({ children, className }: { children: ReactNode; className?: string }) {
  return <code className={[styles.code, className].filter(Boolean).join(' ')}>{children}</code>;
}

/** Identifier in monospace (revision SHA, resource name, URL) without the code background. */
export function Mono({ children, className, title }: { children: ReactNode; className?: string; title?: string }) {
  return (
    <span className={[styles.mono, className].filter(Boolean).join(' ')} title={title}>
      {children}
    </span>
  );
}

export interface KeyValueItem {
  key: string;
  label: ReactNode;
  value: ReactNode;
}

export function KeyValueList({ items, layout = 'columns', className }: { items: readonly KeyValueItem[]; layout?: 'columns' | 'stacked'; className?: string }) {
  return (
    <dl className={[styles.kv, layout === 'stacked' ? styles.kvStacked : undefined, className].filter(Boolean).join(' ')}>
      {items.map((item) => (
        <div key={item.key} className={styles.kvRow}>
          <dt>{item.label}</dt>
          <dd>{item.value === undefined || item.value === null || item.value === '' ? '—' : item.value}</dd>
        </div>
      ))}
    </dl>
  );
}

async function writeClipboard(text: string): Promise<boolean> {
  try {
    if (!navigator.clipboard) return false;
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

export interface CopyButtonProps {
  value: string;
  /** Names what is copied, e.g. "Copy revision". */
  label?: string;
  size?: 'sm' | 'md';
  /** Show the label next to the icon. */
  showLabel?: boolean;
}

/** Copies `value`; the result is announced through a polite live region. */
export function CopyButton({ value, label = 'Copy', size = 'sm', showLabel = false }: CopyButtonProps) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const onClick = async () => {
    const ok = await writeClipboard(value);
    setState(ok ? 'copied' : 'failed');
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setState('idle'), 2000);
  };
  const icon = state === 'copied' ? <IconCheck /> : <IconCopy />;
  const message = state === 'copied' ? 'Copied to clipboard' : state === 'failed' ? 'Copy failed: clipboard unavailable' : '';
  return (
    <span className={styles.copy}>
      {showLabel ? (
        <Button size={size} variant="secondary" icon={icon} onClick={() => void onClick()}>
          {label}
        </Button>
      ) : (
        <IconButton aria-label={label} icon={icon} size={size} onClick={() => void onClick()} />
      )}
      <span className="sr-only" aria-live="polite">
        {message}
      </span>
    </span>
  );
}

export interface CodeBlockProps {
  code: string;
  title?: ReactNode;
  /** Accessible name for the scrollable region (defaults to the title or "Code"). */
  label?: string;
  language?: string;
  wrap?: boolean;
  maxHeight?: CSSProperties['maxHeight'];
  copyable?: boolean;
}

export function CodeBlock({ code, title, label, language, wrap = false, maxHeight = 480, copyable = true }: CodeBlockProps) {
  const name = label ?? (typeof title === 'string' ? title : 'Code');
  return (
    <figure className={styles.codeBlock}>
      {title || copyable || language ? (
        <figcaption className={styles.codeHeader}>
          <span>{title ?? language ?? ''}</span>
          {copyable ? <CopyButton value={code} label={`Copy ${typeof title === 'string' ? title : 'code'}`} /> : null}
        </figcaption>
      ) : null}
      {/* Scrollable region: focusable so keyboard users can scroll it. */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex */}
      <pre className={[styles.pre, wrap ? styles.wrap : undefined].join(' ')} style={{ maxHeight }} tabIndex={0} aria-label={name}>
        <code data-language={language}>{code}</code>
      </pre>
    </figure>
  );
}

const relativeFormat = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
const absoluteFormat = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'medium' });

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 365 * 24 * 3600],
  ['month', 30 * 24 * 3600],
  ['week', 7 * 24 * 3600],
  ['day', 24 * 3600],
  ['hour', 3600],
  ['minute', 60],
  ['second', 1],
];

export function formatRelative(date: Date, now: number): string {
  const seconds = Math.round((date.getTime() - now) / 1000);
  const abs = Math.abs(seconds);
  if (abs < 10) return 'just now';
  for (const [unit, size] of UNITS) {
    if (abs >= size || unit === 'second') return relativeFormat.format(Math.round(seconds / size), unit);
  }
  return relativeFormat.format(seconds, 'second');
}

export function formatAbsolute(date: Date): string {
  return `${absoluteFormat.format(date)} (${date.toISOString().replace('.000Z', 'Z')})`;
}

/** Relative time ("3 minutes ago") with the absolute local + UTC time on hover/focus. */
export function Timestamp({ value, fallback = '—' }: { value: string | Date | null | undefined; fallback?: string }) {
  const now = useNow();
  const date = value instanceof Date ? value : value ? new Date(value) : null;
  if (!date || Number.isNaN(date.getTime())) return <span>{fallback}</span>;
  const absolute = formatAbsolute(date);
  return (
    <Tooltip content={absolute} describe={false}>
      {/* Focusable so keyboard users can reveal the absolute time tooltip. */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex */}
      <time className={styles.time} dateTime={date.toISOString()} tabIndex={0}>
        {formatRelative(date, now)}
        <span className="sr-only"> ({absolute})</span>
      </time>
    </Tooltip>
  );
}

export interface SplitProps {
  start: ReactNode;
  end: ReactNode;
  /** Accessible name of the resize handle. */
  label: string;
  /** Start pane size in percent. */
  defaultSize?: number;
  minSize?: number;
  maxSize?: number;
  className?: string;
}

/**
 * Two resizable panes. The separator is focusable: ArrowLeft/ArrowRight resize by 5%,
 * Home/End jump to the limits. Panes stack below 768px.
 */
export function Split({ start, end, label, defaultSize = 50, minSize = 20, maxSize = 80, className }: SplitProps) {
  const [size, setSize] = useState(defaultSize);
  const containerRef = useRef<HTMLDivElement>(null);
  const startId = useId();
  const clamp = (n: number) => Math.min(maxSize, Math.max(minSize, n));

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const delta = { ArrowLeft: -5, ArrowRight: 5 }[e.key as 'ArrowLeft' | 'ArrowRight'];
    if (delta !== undefined) {
      e.preventDefault();
      setSize((s) => clamp(s + delta));
    } else if (e.key === 'Home') {
      e.preventDefault();
      setSize(minSize);
    } else if (e.key === 'End') {
      e.preventDefault();
      setSize(maxSize);
    }
  };

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    const container = containerRef.current;
    if (!container) return;
    e.currentTarget.setPointerCapture(e.pointerId);
    const rect = container.getBoundingClientRect();
    const move = (ev: globalThis.PointerEvent) => {
      if (rect.width > 0) setSize(clamp(((ev.clientX - rect.left) / rect.width) * 100));
    };
    const up = () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', up);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  };

  return (
    <div ref={containerRef} className={[styles.split, className].filter(Boolean).join(' ')}>
      <div id={startId} className={styles.pane} style={{ flexBasis: `${size}%` }}>
        {start}
      </div>
      {/* APG window splitter: a focusable separator is a widget (jsx-a11y treats it as static). */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label={label}
        aria-controls={startId}
        aria-valuenow={Math.round(size)}
        aria-valuemin={minSize}
        aria-valuemax={maxSize}
        // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- focusable splitter (APG)
        tabIndex={0}
        className={styles.separator}
        onKeyDown={onKeyDown}
        onPointerDown={onPointerDown}
      />
      <div className={styles.pane} style={{ flex: 1 }}>
        {end}
      </div>
    </div>
  );
}

/** Page title row: h1, optional description and actions. */
export function PageHeader({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <header className={styles.pageHeader}>
      <div className={styles.pageTitles}>
        <h1 className={styles.pageTitle}>{title}</h1>
        {description ? <p className={styles.pageDescription}>{description}</p> : null}
      </div>
      {actions ? <div className={styles.pageActions}>{actions}</div> : null}
    </header>
  );
}
