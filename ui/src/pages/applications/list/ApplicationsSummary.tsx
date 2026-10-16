import { useId, type ReactNode } from 'react';
import type { Application } from '../../../api/types';
import { HEALTH_STATES, HealthStatus, Mono, SYNC_STATES, SyncStatus } from '../../../design';
import { clusterOf, healthOf, syncOf } from '../shared/app';
import type { Dimension } from './filters';
import styles from './List.module.css';

interface Row {
  value: string;
  label: ReactNode;
  count: number;
}

function countBy(apps: readonly Application[], key: (app: Application) => string, fixed: readonly string[] = []): Map<string, number> {
  const counts = new Map<string, number>(fixed.map((v) => [v, 0]));
  for (const app of apps) {
    const value = key(app);
    counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  return counts;
}

function SummaryTable({
  title,
  dimension,
  rows,
  total,
  onFilter,
}: {
  title: string;
  dimension: Dimension;
  rows: readonly Row[];
  total: number;
  onFilter: (dimension: Dimension, value: string) => void;
}) {
  const id = useId();
  return (
    <section className={styles.summarySection} aria-labelledby={id}>
      <h2 id={id}>{title}</h2>
      <table className={styles.summaryTable}>
        <caption className="sr-only">Applications {title.toLowerCase()}</caption>
        <thead>
          <tr>
            <th scope="col">{title.replace(/^By /, '')}</th>
            <th scope="col" className={styles.num}>
              Count
            </th>
            <th scope="col">Share</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => {
            const share = total > 0 ? Math.round((row.count / total) * 100) : 0;
            return (
              <tr key={row.value}>
                <th scope="row">
                  {row.count > 0 ? (
                    <button type="button" className={styles.linkButton} onClick={() => onFilter(dimension, row.value)}>
                      {row.label}
                      <span className="sr-only">: show these applications</span>
                    </button>
                  ) : (
                    row.label
                  )}
                </th>
                <td className={styles.num}>{row.count}</td>
                <td>
                  <span className={styles.barTrack} aria-hidden="true">
                    <span className={styles.bar} style={{ width: `${share}%` }} />
                  </span>
                  <span className="sr-only">{share} percent</span>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </section>
  );
}

/** Summary view: counts by sync, health, project and cluster for the filtered applications. */
export function ApplicationsSummary({ apps, onFilter }: { apps: readonly Application[]; onFilter: (dimension: Dimension, value: string) => void }) {
  const total = apps.length;
  const sync = countBy(apps, syncOf, SYNC_STATES);
  const health = countBy(apps, healthOf, HEALTH_STATES);
  const projects = countBy(apps, (a) => a.spec.project);
  const clusters = countBy(apps, clusterOf);
  const sorted = (m: Map<string, number>) => [...m.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  return (
    <div>
      <p className={styles.total}>
        {total} application{total === 1 ? '' : 's'} in the current filter.
      </p>
      <div className={styles.summaryGrid}>
        <SummaryTable title="By sync status" dimension="sync" total={total} onFilter={onFilter} rows={[...sync.entries()].map(([value, count]) => ({ value, count, label: <SyncStatus status={value} /> }))} />
        <SummaryTable
          title="By health"
          dimension="health"
          total={total}
          onFilter={onFilter}
          rows={[...health.entries()].map(([value, count]) => ({ value, count, label: <HealthStatus status={value} /> }))}
        />
        <SummaryTable title="By project" dimension="projects" total={total} onFilter={onFilter} rows={sorted(projects).map(([value, count]) => ({ value, count, label: value }))} />
        <SummaryTable
          title="By cluster"
          dimension="clusters"
          total={total}
          onFilter={onFilter}
          rows={sorted(clusters).map(([value, count]) => ({ value, count, label: <Mono>{value}</Mono> }))}
        />
      </div>
    </div>
  );
}
