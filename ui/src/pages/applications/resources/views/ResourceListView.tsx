import { useMemo } from 'react';
import { Badge, HealthStatus, Mono, SyncStatus, Table, Timestamp, type Column, type SortState } from '../../../../design';
import { kindCaption } from '../model/kinds';
import { compareItems, type ResourceItem } from '../model/resources';
import { KindIcon } from '../shared/KindIcon';
import styles from './Views.module.css';

export interface ResourceListViewProps {
  items: readonly ResourceItem[];
  onSelect: (item: ResourceItem) => void;
  sort: SortState | null;
  onSortChange: (sort: SortState | null) => void;
}

/** Sortable table of every resource; windowed above 200 rows. */
export function ResourceListView({ items, onSelect, sort, onSortChange }: ResourceListViewProps) {
  const rows = useMemo(() => [...items].sort(compareItems), [items]);
  const columns: Column<ResourceItem>[] = [
    {
      id: 'kind',
      header: 'Kind',
      width: '180px',
      sortValue: (r) => r.kind,
      cell: (r) => (
        <span className={styles.kindCell}>
          <KindIcon kind={r.kind} size={16} />
          <span>{r.kind}</span>
          <span className={styles.caption}>{kindCaption(r.kind)}</span>
        </span>
      ),
    },
    {
      id: 'name',
      header: 'Name',
      sortValue: (r) => r.name,
      cell: (r) => (
        <span className={styles.nameCell}>
          <Mono>{r.name}</Mono>
          {r.hook ? <Badge tone="info">Hook</Badge> : null}
          {r.requiresPruning ? <Badge tone="warning">Requires pruning</Badge> : null}
          {r.orphaned ? <Badge>Orphaned</Badge> : null}
        </span>
      ),
    },
    { id: 'namespace', header: 'Namespace', sortValue: (r) => r.namespace, cell: (r) => (r.namespace ? <Mono>{r.namespace}</Mono> : '—') },
    { id: 'group', header: 'Group', sortValue: (r) => r.group, cell: (r) => (r.group ? <Mono>{r.group}</Mono> : 'core') },
    { id: 'sync', header: 'Sync status', sortValue: (r) => r.sync, cell: (r) => (r.managed ? <SyncStatus status={r.sync} /> : '—') },
    {
      id: 'health',
      header: 'Health',
      sortValue: (r) => r.health?.status,
      cell: (r) => (r.health?.status ? <HealthStatus status={r.health.status} description={r.health.message} /> : '—'),
    },
    { id: 'wave', header: 'Sync wave', align: 'end', width: '96px', sortValue: (r) => r.syncWave, cell: (r) => (r.syncWave ?? '—') },
    { id: 'age', header: 'Created', sortValue: (r) => r.createdAt, cell: (r) => <Timestamp value={r.createdAt} /> },
  ];
  return (
    <Table
      caption={`Resources (${rows.length})`}
      columns={columns}
      rows={rows}
      getRowKey={(r) => r.key}
      getRowLabel={(r) => `${r.kind} ${r.name}`}
      onRowActivate={onSelect}
      sort={sort}
      onSortChange={onSortChange}
      height={rows.length > 200 ? 560 : undefined}
      empty="No resources match the filters."
    />
  );
}
