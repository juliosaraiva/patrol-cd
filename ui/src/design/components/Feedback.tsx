import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from 'react';
import { IconCheck, IconClose, IconError, IconInfo, IconWarning } from '../icons';
import { IconButton } from './Button';
import styles from './Feedback.module.css';

export type Tone = 'info' | 'warning' | 'error' | 'success';

const TONE_ICON: Record<Tone, ReactNode> = {
  info: <IconInfo />,
  warning: <IconWarning />,
  error: <IconError />,
  success: <IconCheck />,
};

const TONE_LABEL: Record<Tone, string> = { info: 'Information', warning: 'Warning', error: 'Error', success: 'Success' };

export interface BannerProps {
  tone?: Tone;
  title?: ReactNode;
  children?: ReactNode;
  /** Buttons or links shown at the end. */
  actions?: ReactNode;
  onDismiss?: () => void;
  dismissLabel?: string;
  /**
   * Announce the banner when it appears (role=status, or role=alert for errors). Leave off for
   * banners present on page load.
   */
  live?: boolean;
  className?: string;
}

/** Inline message with a tone icon and visually hidden tone name (not color alone). */
export function Banner({ tone = 'info', title, children, actions, onDismiss, dismissLabel = 'Dismiss', live = false, className }: BannerProps) {
  const role = live ? (tone === 'error' ? 'alert' : 'status') : undefined;
  return (
    <div role={role} className={[styles.banner, styles[tone], className].filter(Boolean).join(' ')} data-tone={tone}>
      <span className={styles.toneIcon}>{TONE_ICON[tone]}</span>
      <div className={styles.bannerBody}>
        <span className="sr-only">{TONE_LABEL[tone]}: </span>
        {title ? <span className={styles.bannerTitle}>{title}</span> : null}
        {children ? <div>{children}</div> : null}
      </div>
      {actions || onDismiss ? (
        <div className={styles.bannerActions}>
          {actions}
          {onDismiss ? <IconButton aria-label={dismissLabel} icon={<IconClose />} size="sm" onClick={onDismiss} /> : null}
        </div>
      ) : null}
    </div>
  );
}

export interface ToastOptions {
  title: string;
  description?: ReactNode;
  tone?: Tone;
  /** Auto-dismiss delay; errors default to staying until dismissed. */
  durationMs?: number | null;
  action?: ReactNode;
}

interface ToastRecord extends ToastOptions {
  id: number;
}

interface ToastContextValue {
  toast: (options: ToastOptions) => number;
  dismiss: (id: number) => void;
}

const ToastContext = createContext<ToastContextValue | null>(null);

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error('useToast must be used inside <ToastProvider>');
  return ctx;
}

function ToastItem({ record, paused, onDismiss }: { record: ToastRecord; paused: boolean; onDismiss: () => void }) {
  const tone = record.tone ?? 'info';
  const duration = record.durationMs === undefined ? (tone === 'error' ? null : 6000) : record.durationMs;
  useEffect(() => {
    if (duration === null || paused) return undefined;
    const t = setTimeout(onDismiss, duration);
    return () => clearTimeout(t);
  }, [duration, paused, onDismiss]);
  return (
    <li>
      <Banner tone={tone} title={record.title} actions={record.action} onDismiss={onDismiss} dismissLabel="Dismiss notification" className={styles.toast}>
        {record.description}
      </Banner>
    </li>
  );
}

/**
 * Hosts toasts in a polite live region (bottom right). Timers pause while the pointer or focus
 * is inside the region; error toasts persist until dismissed.
 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<ToastRecord[]>([]);
  const [paused, setPaused] = useState(false);
  const nextId = useRef(1);
  const dismiss = useCallback((id: number) => setToasts((list) => list.filter((t) => t.id !== id)), []);
  const toast = useCallback((options: ToastOptions) => {
    const id = nextId.current++;
    setToasts((list) => [...list.slice(-4), { ...options, id }]);
    return id;
  }, []);
  const value = useMemo(() => ({ toast, dismiss }), [toast, dismiss]);
  const dismissers = useRef(new Map<number, () => void>());
  const dismissFor = (id: number) => {
    let fn = dismissers.current.get(id);
    if (!fn) {
      fn = () => {
        dismissers.current.delete(id);
        dismiss(id);
      };
      dismissers.current.set(id, fn);
    }
    return fn;
  };
  return (
    <ToastContext.Provider value={value}>
      {children}
      {/* Pointer/focus handlers only pause auto-dismiss timers; they add no interaction. */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
      <section
        aria-label="Notifications"
        className={styles.region}
        onMouseEnter={() => setPaused(true)}
        onMouseLeave={() => setPaused(false)}
        onFocus={() => setPaused(true)}
        onBlur={() => setPaused(false)}
      >
        <ol className={styles.toastList} aria-live="polite" aria-relevant="additions text">
          {toasts.map((t) => (
            <ToastItem key={t.id} record={t} paused={paused} onDismiss={dismissFor(t.id)} />
          ))}
        </ol>
      </section>
    </ToastContext.Provider>
  );
}

export interface EmptyStateProps {
  title: ReactNode;
  /** What the area is for and why it is empty. */
  children?: ReactNode;
  icon?: ReactNode;
  /** Primary next step (Button/ButtonLink). */
  action?: ReactNode;
  secondaryAction?: ReactNode;
  /** 1 when the empty state is the whole page (404, route errors). */
  headingLevel?: 1 | 2 | 3;
  className?: string;
}

export function EmptyState({ title, children, icon, action, secondaryAction, headingLevel = 2, className }: EmptyStateProps) {
  const Heading = (['h1', 'h2', 'h3'] as const)[headingLevel - 1] ?? 'h2';
  return (
    <div className={[styles.empty, className].filter(Boolean).join(' ')}>
      {icon ? <span className={styles.emptyIcon}>{icon}</span> : null}
      <Heading className={styles.emptyTitle}>{title}</Heading>
      {children ? <div className={styles.emptyText}>{children}</div> : null}
      {action || secondaryAction ? (
        <div className={styles.emptyActions}>
          {action}
          {secondaryAction}
        </div>
      ) : null}
    </div>
  );
}

export interface SkeletonProps {
  width?: CSSProperties['width'];
  height?: CSSProperties['height'];
  className?: string;
}

/** Loading placeholder. Mark the loading container aria-busy; skeletons are hidden from AT. */
export function Skeleton({ width = '100%', height = 12, className }: SkeletonProps) {
  return <span aria-hidden="true" className={[styles.skeleton, className].filter(Boolean).join(' ')} style={{ width, height }} />;
}

export function SkeletonText({ lines = 3 }: { lines?: number }) {
  return (
    <span className={styles.skeletonText} aria-hidden="true">
      {Array.from({ length: lines }, (_, i) => (
        <Skeleton key={i} width={i === lines - 1 ? '60%' : '100%'} />
      ))}
    </span>
  );
}

export type BadgeTone = 'neutral' | 'accent' | 'success' | 'warning' | 'danger' | 'info';

const BADGE_CLASS: Record<BadgeTone, string | undefined> = {
  neutral: undefined,
  accent: styles.badgeAccent,
  success: styles.badgeSuccess,
  warning: styles.badgeWarning,
  danger: styles.badgeDanger,
  info: styles.badgeInfo,
};

/** Short metadata label. Its text must carry the meaning; tone is reinforcement only. */
export function Badge({ tone = 'neutral', children, className }: { tone?: BadgeTone; children: ReactNode; className?: string }) {
  return <span className={[styles.badge, BADGE_CLASS[tone], className].filter(Boolean).join(' ')}>{children}</span>;
}
