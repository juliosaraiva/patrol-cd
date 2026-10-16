import { useMemo, useState } from 'react';
import { Button, EmptyState, HealthStatus, Mono, RadioGroup, Table, Timestamp, type Column } from '../../../../design';
import { IconChevronDown, IconChevronRight } from '../../../../design/icons';
import { kindCaption } from '../model/kinds';
import { formatQuantity, groupPods, podFacts, type HostInfo, type PodGroup, type PodGroupMode } from '../model/pods';
import type { ResourceItem, ResourceModel } from '../model/resources';
import { KindIcon } from '../shared/KindIcon';
import styles from './Views.module.css';

export interface PodsViewProps {
  model: ResourceModel;
  hosts: readonly HostInfo[];
  matcher: (item: ResourceItem) => boolean;
  mode: PodGroupMode;
  onModeChange: (mode: PodGroupMode) => void;
  onSelect: (item: ResourceItem) => void;
}

const GROUP_PAGE = 20;

const MODE_OPTIONS: { value: PodGroupMode; label: string }[] = [
  { value: 'node', label: 'Node' },
  { value: 'parentResource', label: 'Parent resource' },
  { value: 'topLevelResource', label: 'Top-level resource' },
];

function HostResources({ host }: { host: HostInfo }) {
  const resources = host.resourcesInfo ?? [];
  if (resources.length === 0) return null;
  return (
    <ul className={styles.meters} aria-label={`Resources of node ${host.name ?? ''}`}>
      {resources.map((r) => {
        const capacity = r.capacity ?? 0;
        const app = r.requestedByApp ?? 0;
        const others = r.requestedByNeighbors ?? 0;
        const pct = (v: number) => (capacity > 0 ? Math.min(100, (v / capacity) * 100) : 0);
        const name = r.resourceName ?? '';
        const text = `${name}: ${formatQuantity(name, app)} requested by this application, ${formatQuantity(name, others)} by other workloads, capacity ${formatQuantity(name, capacity)}`;
        return (
          <li key={name} className={styles.meter}>
            <span className={styles.meterLabel}>{name === 'cpu' ? 'CPU' : name === 'memory' ? 'Memory' : name}</span>
            <span className={styles.meterTrack} role="img" aria-label={text}>
              <span className={styles.meterApp} style={{ width: `${pct(app)}%` }} />
              <span className={styles.meterOthers} style={{ width: `${pct(others)}%` }} />
            </span>
            <span className={styles.meterText} aria-hidden="true">
              {formatQuantity(name, app)} app · {formatQuantity(name, others)} others · {formatQuantity(name, capacity)} capacity
            </span>
          </li>
        );
      })}
    </ul>
  );
}

function GroupSection({ group, expanded, onToggle, onSelect }: { group: PodGroup; expanded: boolean; onToggle: () => void; onSelect: (item: ResourceItem) => void }) {
  const columns: Column<ResourceItem>[] = [
    { id: 'name', header: 'Pod', sortValue: (p) => p.name, cell: (p) => <Mono>{p.name}</Mono> },
    {
      id: 'health',
      header: 'Health',
      sortValue: (p) => p.health?.status,
      cell: (p) => <HealthStatus status={p.health?.status} description={p.health?.message} />,
    },
    { id: 'status', header: 'Status', sortValue: (p) => podFacts(p).phase, cell: (p) => podFacts(p).phase ?? '—' },
    { id: 'ready', header: 'Containers ready', sortValue: (p) => podFacts(p).ready, cell: (p) => podFacts(p).ready ?? '—' },
    {
      id: 'restarts',
      header: 'Restarts',
      align: 'end',
      sortValue: (p) => podFacts(p).restarts,
      cell: (p) => podFacts(p).restarts ?? '—',
    },
    { id: 'created', header: 'Created', sortValue: (p) => p.createdAt, cell: (p) => <Timestamp value={p.createdAt} /> },
  ];
  const degraded = group.pods.filter((p) => p.health?.status === 'Degraded').length;
  const headingId = `pods-${group.id.replace(/[^a-zA-Z0-9_-]/g, '_')}`;
  return (
    <section className={styles.podGroup} aria-labelledby={headingId}>
      <h3 id={headingId} className={styles.podGroupHeading}>
        <button type="button" className={styles.disclosure} aria-expanded={expanded} onClick={onToggle}>
          {expanded ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
          {group.kind ? <KindIcon kind={group.kind} size={18} /> : null}
          <span className={styles.podGroupKind}>{group.kind === 'node' ? 'Node' : group.kind ? kindCaption(group.kind) : ''}</span>
          <Mono>{group.title}</Mono>
          <span className={styles.podGroupCount}>
            {group.pods.length} {group.pods.length === 1 ? 'pod' : 'pods'}
            {degraded > 0 ? `, ${degraded} degraded` : ''}
          </span>
        </button>
      </h3>
      {group.host?.labels && Object.keys(group.host.labels).length > 0 ? (
        <ul className={styles.hostLabels} aria-label={`Labels of node ${group.title}`}>
          {Object.entries(group.host.labels)
            .sort(([a], [b]) => a.localeCompare(b))
            .map(([k, v]) => (
              <li key={k}>
                <Mono>{`${k}=${v}`}</Mono>
              </li>
            ))}
        </ul>
      ) : null}
      {group.host ? <HostResources host={group.host} /> : null}
      {expanded ? (
        group.pods.length > 0 ? (
          <Table
            caption={`Pods on ${group.title}`}
            columns={columns}
            rows={group.pods}
            getRowKey={(p) => p.key}
            onRowActivate={onSelect}
            height={group.pods.length > 200 ? 480 : undefined}
          />
        ) : (
          <p className={styles.muted}>No pods of this application run here.</p>
        )
      ) : null}
    </section>
  );
}

/** Pods grouped by node (with node resource usage), parent resource or top-level resource. */
export function PodsView({ model, hosts, matcher, mode, onModeChange, onSelect }: PodsViewProps) {
  const groups = useMemo(() => groupPods(model, hosts, mode, matcher), [model, hosts, mode, matcher]);
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(() => new Set());
  const [limit, setLimit] = useState(GROUP_PAGE);
  const podCount = groups.reduce((n, g) => n + g.pods.length, 0);

  return (
    <div className={styles.viewBody}>
      <RadioGroup legend="Group pods by" orientation="horizontal" value={mode} onChange={onModeChange} options={MODE_OPTIONS} />
      {podCount === 0 && groups.every((g) => g.pods.length === 0) ? (
        <EmptyState title="No pods" headingLevel={3}>
          <p>This application has no pods matching the filters. Workloads create pods once they are synced and scheduled.</p>
        </EmptyState>
      ) : (
        <>
          {groups.slice(0, limit).map((group) => (
            <GroupSection
              key={group.id}
              group={group}
              expanded={!collapsed.has(group.id)}
              onToggle={() =>
                setCollapsed((s) => {
                  const next = new Set(s);
                  if (next.has(group.id)) next.delete(group.id);
                  else next.add(group.id);
                  return next;
                })
              }
              onSelect={onSelect}
            />
          ))}
          {groups.length > limit ? (
            <Button onClick={() => setLimit((l) => l + GROUP_PAGE)}>
              Show {Math.min(GROUP_PAGE, groups.length - limit)} more groups ({groups.length - limit} remaining)
            </Button>
          ) : null}
        </>
      )}
    </div>
  );
}
