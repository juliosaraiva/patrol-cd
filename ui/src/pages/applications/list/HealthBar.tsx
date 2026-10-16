import type { Application } from '../../../api/types';
import { HEALTH_STATES, HealthStatus } from '../../../design';
import { healthOf } from '../shared/app';
import styles from './List.module.css';

const SEGMENT_CLASS: Record<string, string | undefined> = {
  Healthy: styles.segHealthy,
  Progressing: styles.segProgressing,
  Degraded: styles.segDegraded,
  Suspended: styles.segSuspended,
  Missing: styles.segMissing,
  Unknown: styles.segUnknown,
};

/** Health of the filtered applications: a proportional bar (decorative) plus labelled counts. */
export function HealthBar({ apps }: { apps: readonly Application[] }) {
  const counts = new Map<string, number>();
  for (const app of apps) counts.set(healthOf(app), (counts.get(healthOf(app)) ?? 0) + 1);
  const states = [...HEALTH_STATES, ...[...counts.keys()].filter((k) => !(HEALTH_STATES as readonly string[]).includes(k))].filter((s) => (counts.get(s) ?? 0) > 0);
  if (apps.length === 0) return null;
  return (
    <div className={styles.healthBar} role="group" aria-label="Health of the listed applications">
      <div className={styles.healthTrack} aria-hidden="true">
        {states.map((s) => (
          <span key={s} className={[styles.segment, SEGMENT_CLASS[s] ?? styles.segUnknown].join(' ')} style={{ flexGrow: counts.get(s) ?? 0 }} />
        ))}
      </div>
      <ul className={styles.healthCounts}>
        {states.map((s) => (
          <li key={s}>
            <HealthStatus status={s} /> <span>{counts.get(s)}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}
