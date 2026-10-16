import { useMemo } from 'react';
import type { KubernetesEvent } from '../../../../api/types';
import { Badge, EmptyState, Skeleton, Table, Timestamp, type Column } from '../../../../design';
import { ErrorBanner } from '../../../../shell/errors';
import styles from './Details.module.css';

export interface EventsTabProps {
  label: string;
  events: KubernetesEvent[] | undefined;
  loading: boolean;
  error: unknown;
  onRetry: () => void;
}

function lastSeen(e: KubernetesEvent): string | undefined {
  return e.lastTimestamp ?? e.eventTime ?? e.firstTimestamp ?? e.metadata?.creationTimestamp;
}

export function warningCount(events: readonly KubernetesEvent[] | undefined): number {
  return (events ?? []).filter((e) => e.type === 'Warning').length;
}

/** Kubernetes events of one resource, newest first. */
export function EventsTab({ label, events, loading, error, onRetry }: EventsTabProps) {
  const rows = useMemo(() => [...(events ?? [])].sort((a, b) => (lastSeen(b) ?? '').localeCompare(lastSeen(a) ?? '')), [events]);
  if (loading) {
    return (
      <div aria-busy="true" className={styles.tabBody}>
        <span className="sr-only">Loading events</span>
        <Skeleton height={120} />
      </div>
    );
  }
  if (error) return <ErrorBanner error={error} onRetry={onRetry} title="Could not load events" />;
  if (rows.length === 0) {
    return (
      <EmptyState title="No events" headingLevel={3}>
        <p>Kubernetes keeps events for about an hour, so a resource that has been stable for a while has none.</p>
      </EmptyState>
    );
  }
  const columns: Column<KubernetesEvent>[] = [
    {
      id: 'type',
      header: 'Type',
      width: '100px',
      sortValue: (e) => e.type,
      cell: (e) => (e.type === 'Warning' ? <Badge tone="warning">Warning</Badge> : <Badge>{e.type ?? 'Normal'}</Badge>),
    },
    { id: 'reason', header: 'Reason', sortValue: (e) => e.reason, cell: (e) => e.reason ?? '—' },
    { id: 'message', header: 'Message', cell: (e) => <span className={styles.eventMessage}>{e.message ?? ''}</span> },
    { id: 'count', header: 'Count', align: 'end', width: '72px', sortValue: (e) => e.count, cell: (e) => e.count ?? 1 },
    { id: 'first', header: 'First seen', sortValue: (e) => e.firstTimestamp, cell: (e) => <Timestamp value={e.firstTimestamp ?? e.eventTime} /> },
    { id: 'last', header: 'Last seen', sortValue: (e) => lastSeen(e), cell: (e) => <Timestamp value={lastSeen(e)} /> },
  ];
  return (
    <Table
      caption={`Events of ${label}`}
      columns={columns}
      rows={rows}
      getRowKey={(e) => e.metadata?.uid ?? `${e.reason ?? ''}-${lastSeen(e) ?? ''}-${e.message ?? ''}`}
      height={rows.length > 200 ? 480 : undefined}
    />
  );
}
