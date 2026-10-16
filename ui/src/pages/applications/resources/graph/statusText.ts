/** Spoken/visible labels and color tokens for status values drawn inside SVG nodes. */

const HEALTH = new Set(['Healthy', 'Progressing', 'Degraded', 'Suspended', 'Missing', 'Unknown']);
const SYNC_LABELS: Record<string, string> = { Synced: 'Synced', OutOfSync: 'Out of sync', Unknown: 'Unknown sync status' };

export type HealthState = 'Healthy' | 'Progressing' | 'Degraded' | 'Suspended' | 'Missing' | 'Unknown';
export type SyncState = 'Synced' | 'OutOfSync' | 'Unknown';

export function healthState(status: string | undefined): HealthState {
  return status && HEALTH.has(status) ? (status as HealthState) : 'Unknown';
}

export function syncState(status: string | undefined): SyncState {
  return status === 'Synced' || status === 'OutOfSync' ? status : 'Unknown';
}

export function healthLabel(status: string | undefined): string {
  if (!status) return 'Unknown health';
  return HEALTH.has(status) ? status : `${status} (unrecognized health)`;
}

export function syncLabel(status: string | undefined): string {
  return SYNC_LABELS[syncState(status)] ?? 'Unknown sync status';
}

export const HEALTH_COLOR: Record<HealthState, string> = {
  Healthy: 'var(--status-healthy)',
  Progressing: 'var(--status-progressing)',
  Degraded: 'var(--status-degraded)',
  Suspended: 'var(--status-suspended)',
  Missing: 'var(--status-missing)',
  Unknown: 'var(--status-unknown)',
};

export const SYNC_COLOR: Record<SyncState, string> = {
  Synced: 'var(--status-synced)',
  OutOfSync: 'var(--status-outofsync)',
  Unknown: 'var(--status-unknown)',
};
