import { useState } from 'react';
import { Link } from 'react-router';
import type { ApplicationSet, ApplicationSetTree, KubernetesEvent } from '../../api/types';
import {
  Badge,
  Button,
  HealthStatus,
  Mono,
  SyncStatus,
  Table,
  Timestamp,
  type BadgeTone,
  type Column,
} from '../../design';
import { IconWarning } from '../../design/icons';
import {
  applicationPath,
  appCount,
  appSetHealth,
  conditionGuidance,
  isProblem,
  lastUpdated,
  problemConditions,
  type AppSetCondition,
  type GeneratedAppRow,
} from './model';
import styles from './ApplicationSets.module.css';

const PROGRESSIVE_TONES: Record<string, BadgeTone> = { Healthy: 'success', Progressing: 'info', Pending: 'neutral', Waiting: 'neutral' };

/** Progressive sync step and status as text ("Step 2 · Progressing"); color only reinforces it. */
export function ProgressiveStep({ row }: { row: Pick<GeneratedAppRow, 'step' | 'progressiveStatus'> }) {
  if (!row.step && !row.progressiveStatus) return <span className={styles.secondary}>—</span>;
  const status = row.progressiveStatus ?? 'Unknown';
  return <Badge tone={PROGRESSIVE_TONES[status] ?? 'warning'}>{`${row.step ? `Step ${row.step} · ` : ''}${status}`}</Badge>;
}

/** Health, conditions, generated application count and last update of an ApplicationSet. */
export function StatusBar({ set, onShowConditions }: { set: ApplicationSet; onShowConditions: () => void }) {
  const problems = problemConditions(set);
  const conditions = set.status?.conditions ?? [];
  const strategy = set.spec.strategy?.type ?? 'AllAtOnce';
  return (
    <section aria-label="ApplicationSet status">
    <dl className={styles.statusBar}>
      <div className={styles.statusItem}>
        <dt>Health</dt>
        <dd>
          <HealthStatus status={appSetHealth(set)} />
        </dd>
      </div>
      <div className={styles.statusItem}>
        <dt>Conditions</dt>
        <dd>
          <button type="button" className={styles.inlineLink} onClick={onShowConditions}>
            {problems.length > 0 ? `${problems.length} need attention` : `${conditions.length} reported`}
          </button>
        </dd>
      </div>
      <div className={styles.statusItem}>
        <dt>Applications</dt>
        <dd>{appCount(set)}</dd>
      </div>
      <div className={styles.statusItem}>
        <dt>Strategy</dt>
        <dd>{strategy === 'RollingSync' ? `RollingSync, ${set.spec.strategy?.rollingSync?.steps?.length ?? 0} steps` : 'AllAtOnce'}</dd>
      </div>
      <div className={styles.statusItem}>
        <dt>Last updated</dt>
        <dd>
          <Timestamp value={lastUpdated(set)} />
        </dd>
      </div>
    </dl>
    </section>
  );
}

export function GeneratedAppsTable({
  rows,
  controlPlaneNamespace,
  showProgressive,
  onOpen,
}: {
  rows: GeneratedAppRow[];
  controlPlaneNamespace: string | undefined;
  showProgressive: boolean;
  onOpen: (row: GeneratedAppRow) => void;
}) {
  const columns: Column<GeneratedAppRow>[] = [
    {
      id: 'name',
      header: 'Application',
      sortValue: (r) => r.name,
      cell: (r) => (
        <Link to={applicationPath(r.name, r.namespace, controlPlaneNamespace)}>
          <Mono>{r.name}</Mono>
        </Link>
      ),
    },
    { id: 'namespace', header: 'Namespace', sortValue: (r) => r.namespace, cell: (r) => <Mono>{r.namespace}</Mono> },
    { id: 'health', header: 'Health', sortValue: (r) => r.health, cell: (r) => <HealthStatus status={r.health} /> },
    { id: 'sync', header: 'Sync', sortValue: (r) => r.sync, cell: (r) => <SyncStatus status={r.sync} /> },
  ];
  if (showProgressive) {
    columns.push(
      { id: 'step', header: 'Progressive sync', sortValue: (r) => `${r.step ?? ''}${r.progressiveStatus ?? ''}`, cell: (r) => <ProgressiveStep row={r} /> },
      { id: 'message', header: 'Step message', cell: (r) => r.progressiveMessage ?? '' },
    );
  }
  return (
    <Table
      caption="Generated applications"
      columns={columns}
      rows={rows}
      getRowKey={(r) => r.key}
      getRowLabel={(r) => r.name}
      onRowActivate={onOpen}
      defaultSort={{ columnId: 'name', direction: 'ascending' }}
      height={rows.length > 50 ? 560 : undefined}
      empty="No applications generated yet. When generators return parameters, the controller creates one Application per parameter set."
    />
  );
}

export function ConditionsList({ conditions }: { conditions: readonly AppSetCondition[] }) {
  if (conditions.length === 0) {
    return <p className={styles.muted}>No conditions reported yet. The controller writes them after its first reconciliation of this ApplicationSet.</p>;
  }
  const sorted = [...conditions].sort((a, b) => Number(isProblem(b)) - Number(isProblem(a)));
  return (
    <ul className={styles.conditions} aria-label="Conditions">
      {sorted.map((c) => {
        const guidance = conditionGuidance(c);
        const problem = isProblem(c);
        return (
          <li key={`${c.type}-${c.reason}`} className={[styles.condition, problem ? styles.conditionProblem : guidance ? styles.conditionInfo : ''].join(' ')}>
            <div className={styles.conditionHeader}>
              {problem ? <IconWarning title="Needs attention" /> : null}
              <span>{c.type}</span>
              <Badge tone={problem ? 'danger' : 'neutral'}>{`Status: ${c.status}`}</Badge>
              <span className={styles.conditionMeta}>
                Reason <Mono>{c.reason}</Mono>
              </span>
            </div>
            {c.message ? <p className={styles.muted}>{c.message}</p> : null}
            {guidance ? <p>{guidance}</p> : null}
            {c.lastTransitionTime ? (
              <span className={styles.conditionMeta}>
                Changed <Timestamp value={c.lastTransitionTime} />
              </span>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}

/** Generated Applications rendered per step of the tree; more on request (large sets). */
export const TREE_PAGE = 200;

/**
 * The ApplicationSet with its generated Applications (resource tree, ASET-125). Nodes come from
 * GET .../resource-tree (ApplicationSetTree: one node per generated Application, its sync status
 * in the "Sync Status" info item); when it is empty, from status.resources.
 */
export function AppSetTree({
  set,
  tree,
  rows,
  controlPlaneNamespace,
}: {
  set: ApplicationSet;
  tree: ApplicationSetTree | undefined;
  rows: GeneratedAppRow[];
  controlPlaneNamespace: string | undefined;
}) {
  const nodes = (tree?.nodes ?? []).filter((n) => n.kind === 'Application');
  const byKey = new Map(rows.map((r) => [`${r.namespace}/${r.name}`, r]));
  const children =
    nodes.length > 0
      ? nodes.map((n) => {
          const namespace = n.namespace ?? set.metadata.namespace ?? '';
          const row = byKey.get(`${namespace}/${n.name}`);
          const treeSync = n.info?.find((i) => i.name === 'Sync Status')?.value;
          return {
            key: `${namespace}/${n.name}`,
            name: n.name,
            namespace,
            health: row?.health ?? n.health?.status ?? 'Unknown',
            sync: row?.sync ?? treeSync ?? 'Unknown',
          };
        })
      : rows.map((r) => ({ key: r.key, name: r.name, namespace: r.namespace, health: r.health, sync: r.sync }));
  children.sort((a, b) => a.name.localeCompare(b.name));
  const [limit, setLimit] = useState(TREE_PAGE);
  const shown = children.slice(0, limit);
  return (
    <ul className={styles.tree} aria-label="Resource tree">
      <li>
        <div className={styles.treeNode}>
          <span className={styles.treeKind}>ApplicationSet</span>
          <Mono>{set.metadata.name}</Mono>
          <HealthStatus status={appSetHealth(set)} />
          <span className={styles.secondary}>{`${children.length} application${children.length === 1 ? '' : 's'}`}</span>
        </div>
        {children.length > 0 ? (
          <ul className={styles.treeChildren} aria-label={`Applications of ${set.metadata.name ?? ''}`}>
            {shown.map((c) => (
              <li key={c.key} className={styles.treeNode}>
                <span className={styles.treeKind}>Application</span>
                <Link to={applicationPath(c.name, c.namespace, controlPlaneNamespace)}>
                  <Mono>{c.name}</Mono>
                </Link>
                <HealthStatus status={c.health} />
                <SyncStatus status={c.sync} />
              </li>
            ))}
          </ul>
        ) : null}
        {children.length > shown.length ? (
          <div className={styles.treeMore}>
            <Button size="sm" onClick={() => setLimit((n) => n + TREE_PAGE)}>
              {`Show ${Math.min(TREE_PAGE, children.length - shown.length)} more of ${children.length - shown.length} remaining`}
            </Button>
          </div>
        ) : null}
      </li>
    </ul>
  );
}

type EventRow = KubernetesEvent & { key: string };

export function EventsTable({ events }: { events: readonly KubernetesEvent[] }) {
  const rows: EventRow[] = events.map((e, i) => ({ ...e, key: e.metadata?.uid ?? `${e.reason ?? ''}-${i}` }));
  const columns: Column<EventRow>[] = [
    {
      id: 'type',
      header: 'Type',
      sortValue: (e) => e.type,
      cell: (e) =>
        e.type === 'Warning' ? (
          <span className={styles.conditionCount}>
            <IconWarning />
            Warning
          </span>
        ) : (
          (e.type ?? 'Normal')
        ),
    },
    { id: 'reason', header: 'Reason', sortValue: (e) => e.reason, cell: (e) => e.reason ?? '' },
    { id: 'message', header: 'Message', cell: (e) => e.message ?? '' },
    { id: 'count', header: 'Count', align: 'end', sortValue: (e) => e.count ?? 1, cell: (e) => e.count ?? 1 },
    {
      id: 'last',
      header: 'Last seen',
      sortValue: (e) => e.lastTimestamp ?? e.eventTime ?? e.firstTimestamp,
      cell: (e) => <Timestamp value={e.lastTimestamp ?? e.eventTime ?? e.firstTimestamp} />,
    },
  ];
  return (
    <Table
      caption="Events"
      columns={columns}
      rows={rows}
      getRowKey={(e) => e.key}
      defaultSort={{ columnId: 'last', direction: 'descending' }}
      empty="No events. Kubernetes keeps events for about an hour; changes older than that are not listed."
    />
  );
}
