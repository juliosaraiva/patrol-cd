import { useProjectEvents } from '../../../api/queries/resources';
import type { KubernetesEvent } from '../../../api/types';
import { Badge, Button, EmptyState, IconRefresh, Table, Timestamp } from '../../../design';
import { ErrorBanner } from '../../../shell/errors';
import { LoadingBlock } from '../common/feedback';
import styles from '../common/common.module.css';

const lastSeen = (e: KubernetesEvent) => e.lastTimestamp ?? e.eventTime ?? e.firstTimestamp ?? e.metadata?.creationTimestamp;

/** Kubernetes events of the AppProject object (API-105). */
export function EventsTab({ name }: { name: string }) {
  const events = useProjectEvents(name);
  const items = events.data?.items ?? [];
  if (events.isPending) return <LoadingBlock label="Loading events" />;
  if (events.isError) return <ErrorBanner error={events.error} onRetry={() => void events.refetch()} />;
  return (
    <div className={styles.stack}>
      <div className={styles.sectionHeader}>
        <p className={styles.muted}>Events recorded for this project. Kubernetes keeps events for a limited time (one hour by default).</p>
        <Button size="sm" icon={<IconRefresh />} onClick={() => void events.refetch()} loading={events.isFetching}>
          Refresh
        </Button>
      </div>
      {items.length === 0 ? (
        <EmptyState headingLevel={3} title="No recent events">
          Changes to the project and denied operations are recorded here as they happen.
        </EmptyState>
      ) : (
        <Table
          caption="Project events"
          className={styles.wideTable}
          rows={items}
          getRowKey={(e) => e.metadata?.uid ?? `${e.reason ?? ''}-${lastSeen(e) ?? ''}`}
          defaultSort={{ columnId: 'last', direction: 'descending' }}
          columns={[
            {
              id: 'type',
              header: 'Type',
              width: '90px',
              cell: (e) => <Badge tone={e.type === 'Warning' ? 'warning' : 'neutral'}>{e.type ?? 'Normal'}</Badge>,
              sortValue: (e) => e.type,
            },
            { id: 'reason', header: 'Reason', width: '170px', cell: (e) => e.reason ?? '—', sortValue: (e) => e.reason },
            { id: 'message', header: 'Message', cell: (e) => <span title={e.message}>{e.message ?? '—'}</span> },
            { id: 'count', header: 'Count', width: '70px', align: 'end', cell: (e) => e.count ?? 1, sortValue: (e) => e.count ?? 1 },
            { id: 'first', header: 'First seen', width: '130px', cell: (e) => <Timestamp value={e.firstTimestamp ?? e.eventTime} />, sortValue: (e) => e.firstTimestamp },
            { id: 'last', header: 'Last seen', width: '130px', cell: (e) => <Timestamp value={lastSeen(e)} />, sortValue: lastSeen },
          ]}
        />
      )}
    </div>
  );
}
