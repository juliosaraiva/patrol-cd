import type { ReactNode } from 'react';
import type { ConnectionStatusCode, HealthStatusCode, OperationPhase, SyncStatusCode } from '../../api/types';
import { CONNECTION_GLYPHS, HEALTH_GLYPHS, OPERATION_GLYPHS, SYNC_GLYPHS } from './glyphs';
import styles from './Status.module.css';

interface StatusBaseProps {
  /** Hide the text label visually (it stays available to assistive technology). */
  hideLabel?: boolean;
  /** Extra text announced after the label, e.g. a health message. */
  description?: string | undefined;
  className?: string;
}

function StatusView({
  family,
  state,
  label,
  glyph,
  hideLabel,
  description,
  className,
  stateClass,
}: StatusBaseProps & { family: string; state: string; label: string; glyph: ReactNode; stateClass: string | undefined }) {
  const classes = [styles.status, stateClass, family === 'operation' ? styles.operation : undefined, className]
    .filter(Boolean)
    .join(' ');
  return (
    <span className={classes} data-status-family={family} data-status={state} title={hideLabel ? label : description}>
      {glyph}
      <span className={hideLabel ? 'sr-only' : styles.label}>{label}</span>
      {description ? <span className="sr-only">: {description}</span> : null}
    </span>
  );
}

function known<K extends string>(map: Record<K, unknown>, value: string | undefined, fallback: K): K {
  return value !== undefined && Object.hasOwn(map, value) ? (value as K) : fallback;
}

export const HEALTH_STATES: readonly HealthStatusCode[] = ['Healthy', 'Progressing', 'Degraded', 'Suspended', 'Missing', 'Unknown'];
export const SYNC_STATES: readonly SyncStatusCode[] = ['Synced', 'OutOfSync', 'Unknown'];
export const OPERATION_PHASES: readonly OperationPhase[] = [
  'Pending',
  'Running',
  'Terminating',
  'Succeeded',
  'Failed',
  'Error',
  'Cancelled',
  'Rejected',
];
export const CONNECTION_STATES: readonly ConnectionStatusCode[] = ['Successful', 'Failed', 'Unknown'];

const SYNC_LABELS: Record<SyncStatusCode, string> = { Synced: 'Synced', OutOfSync: 'Out of sync', Unknown: 'Unknown' };

export interface HealthStatusProps extends StatusBaseProps {
  /** Health status string from the API; unrecognized values render as Unknown with their text. */
  status: string | undefined;
}

/** Application/resource health: solid silhouette glyph + label. */
export function HealthStatus({ status, ...rest }: HealthStatusProps) {
  const state = known(HEALTH_GLYPHS, status, 'Unknown');
  return (
    <StatusView
      family="health"
      state={state}
      label={status && state === 'Unknown' && status !== 'Unknown' ? status : state}
      glyph={<span className={styles.glyph}>{HEALTH_GLYPHS[state]}</span>}
      stateClass={styles[`health${state}`]}
      {...rest}
    />
  );
}

export interface SyncStatusProps extends StatusBaseProps {
  status: string | undefined;
}

/** Desired-vs-live comparison: boxed comparison glyph + label. */
export function SyncStatus({ status, ...rest }: SyncStatusProps) {
  const state = known(SYNC_GLYPHS, status, 'Unknown');
  return (
    <StatusView
      family="sync"
      state={state}
      label={SYNC_LABELS[state]}
      glyph={<span className={styles.syncBox}>{SYNC_GLYPHS[state]}</span>}
      stateClass={styles[`sync${state}`]}
      {...rest}
    />
  );
}

export interface OperationPhaseProps extends StatusBaseProps {
  phase: string | undefined;
}

/** Sync/rollback operation phase: tag with a process glyph. */
export function OperationPhaseStatus({ phase, ...rest }: OperationPhaseProps) {
  const state = known(OPERATION_GLYPHS, phase, 'Pending');
  const label = phase && phase !== state ? phase : state;
  return (
    <StatusView
      family="operation"
      state={state}
      label={label}
      glyph={<span className={styles.glyph}>{OPERATION_GLYPHS[state]}</span>}
      stateClass={styles[`op${state}`]}
      {...rest}
    />
  );
}

export interface ConnectionStatusProps extends StatusBaseProps {
  status: string | undefined;
}

/** Repository/cluster connection state: endpoint-and-link glyph + label. */
export function ConnectionStatus({ status, ...rest }: ConnectionStatusProps) {
  const state = known(CONNECTION_GLYPHS, status, 'Unknown');
  return (
    <StatusView
      family="connection"
      state={state}
      label={state}
      glyph={<span className={styles.glyph}>{CONNECTION_GLYPHS[state]}</span>}
      stateClass={styles[`conn${state}`]}
      {...rest}
    />
  );
}
