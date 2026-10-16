import { useId, useMemo, useState } from 'react';
import { useApplicationOperationsPolling, useOlderOperations } from '../../../api/hooks/apps-core';
import type { Application, Operation } from '../../../api/types';
import {
  Badge,
  Banner,
  Button,
  Drawer,
  EmptyState,
  FormField,
  KeyValueList,
  Mono,
  OperationPhaseStatus,
  Select,
  SkeletonText,
  Table,
  Timestamp,
  type BadgeTone,
  type Column,
} from '../../../design';
import { ErrorBanner } from '../../../shell/errors';
import {
  appName,
  appScope,
  durationBetween,
  initiatorLabel,
  isRunningPhase,
  resourceKey,
  shortRevision,
} from '../shared/app';
import styles from './Operations.module.css';
import { ApproveDeletionDialog, resourcesAwaitingApproval } from './ApproveDeletionDialog';
import { TerminateDialog } from './TerminateDialog';

type SyncResultResource = NonNullable<NonNullable<NonNullable<Operation['status']>['syncResult']>['resources']>[number];

/** Normalized operation: an Operation object, or the application's operationState summary. */
export interface OperationView {
  name: string;
  kind: 'Sync' | 'Rollback';
  phase: string | undefined;
  message: string | undefined;
  startedAt: string | undefined;
  finishedAt: string | undefined;
  initiatedBy: { automated?: boolean; username?: string } | undefined;
  dryRun: boolean;
  retryCount: number | undefined;
  progress: NonNullable<Operation['status']>['progress'];
  revisions: string[];
  warnings: string[];
  results: SyncResultResource[] | undefined;
}

export function operationView(op: Operation): OperationView {
  const status = op.status;
  const sync = op.spec.sync;
  const resultRevisions = status?.syncResult?.revisions ?? (status?.syncResult?.revision ? [status.syncResult.revision] : []);
  return {
    name: op.metadata.name ?? '',
    kind: op.spec.rollback ? 'Rollback' : 'Sync',
    phase: status?.phase,
    message: status?.message,
    startedAt: status?.startedAt,
    finishedAt: status?.finishedAt,
    initiatedBy: op.spec.initiatedBy,
    dryRun: sync?.dryRun ?? false,
    retryCount: status?.retryCount,
    progress: status?.progress,
    revisions: status?.plan?.resolvedRevisions ?? resultRevisions,
    warnings: status?.plan?.warnings ?? [],
    results: status?.syncResult?.resources,
  };
}

export function operationStateView(app: Application): OperationView | null {
  const state = app.status?.operationState;
  if (!state) return null;
  const sr = state.syncResult;
  return {
    name: state.operationName,
    kind: 'Sync',
    phase: state.phase,
    message: state.message,
    startedAt: state.startedAt,
    finishedAt: state.finishedAt,
    initiatedBy: state.initiatedBy,
    dryRun: false,
    retryCount: state.retryCount,
    progress: undefined,
    revisions: sr?.revisions ?? (sr?.revision ? [sr.revision] : []),
    warnings: [],
    results: undefined,
  };
}

const RESULT_LABEL: Record<string, { label: string; tone: BadgeTone }> = {
  Synced: { label: 'Synced', tone: 'success' },
  SyncFailed: { label: 'Sync failed', tone: 'danger' },
  Pruned: { label: 'Pruned', tone: 'info' },
  PruneSkipped: { label: 'Prune skipped', tone: 'warning' },
};

type ResultFilter = 'all' | 'failed' | 'pruned' | 'hooks' | 'synced';

function filterResults(results: readonly SyncResultResource[], filter: ResultFilter): SyncResultResource[] {
  switch (filter) {
    case 'failed':
      return results.filter((r) => r.status === 'SyncFailed' || r.hookPhase === 'Failed' || r.hookPhase === 'Error');
    case 'pruned':
      return results.filter((r) => r.status === 'Pruned' || r.status === 'PruneSkipped');
    case 'hooks':
      return results.filter((r) => Boolean(r.hookType));
    case 'synced':
      return results.filter((r) => r.status === 'Synced');
    case 'all':
      return [...results];
  }
}

const RESULT_COLUMNS: Column<SyncResultResource>[] = [
  { id: 'kind', header: 'Kind', cell: (r) => r.kind, sortValue: (r) => r.kind },
  { id: 'namespace', header: 'Namespace', cell: (r) => (r.namespace ? <Mono>{r.namespace}</Mono> : '—'), sortValue: (r) => r.namespace },
  { id: 'name', header: 'Name', cell: (r) => <Mono>{r.name}</Mono>, sortValue: (r) => r.name },
  {
    id: 'result',
    header: 'Result',
    cell: (r) => {
      if (!r.status) return '—';
      const meta = RESULT_LABEL[r.status] ?? { label: r.status, tone: 'neutral' as const };
      return <Badge tone={meta.tone}>{meta.label}</Badge>;
    },
    sortValue: (r) => r.status,
  },
  {
    id: 'hook',
    header: 'Hook',
    cell: (r) =>
      r.hookType ? (
        <span>
          {r.hookType} <OperationPhaseStatus phase={r.hookPhase} />
        </span>
      ) : (
        '—'
      ),
    sortValue: (r) => r.hookType,
  },
  { id: 'phase', header: 'Phase', cell: (r) => r.syncPhase ?? '—', sortValue: (r) => r.syncPhase },
  { id: 'message', header: 'Message', cell: (r) => <span className={styles.message}>{r.message ?? ''}</span> },
];

/** Phase, progress, revisions, warnings and per-resource results of one operation. */
export function OperationDetails({ view }: { view: OperationView }) {
  const id = useId();
  const [filter, setFilter] = useState<ResultFilter>('all');
  const results = useMemo(() => filterResults(view.results ?? [], filter), [view.results, filter]);
  const progress = view.progress;
  const total = Number(progress?.totalTasks ?? 0);
  const done = Number(progress?.completedTasks ?? 0);
  const hooks = (view.results ?? []).filter((r) => r.hookType);
  return (
    <div className={styles.stack}>
      <section className={styles.section} aria-label="Operation summary">
        <div className={styles.headerRow}>
          <OperationPhaseStatus phase={view.phase} />
          <span>
            {view.kind}
            {view.dryRun ? ' (dry run)' : ''}
          </span>
          {view.name ? <Mono>{view.name}</Mono> : null}
        </div>
        {view.message ? <p>{view.message}</p> : null}
        <KeyValueList
          items={[
            { key: 'initiator', label: 'Initiated by', value: initiatorLabel(view.initiatedBy) },
            { key: 'started', label: 'Started', value: <Timestamp value={view.startedAt} /> },
            { key: 'finished', label: 'Finished', value: view.finishedAt ? <Timestamp value={view.finishedAt} /> : isRunningPhase(view.phase) ? 'Running' : '' },
            { key: 'duration', label: 'Duration', value: durationBetween(view.startedAt, view.finishedAt) },
            { key: 'retries', label: 'Retries', value: view.retryCount ? String(view.retryCount) : '0' },
            {
              key: 'revisions',
              label: view.revisions.length > 1 ? 'Revisions' : 'Revision',
              value: view.revisions.length > 0 ? <Mono title={view.revisions.join(', ')}>{view.revisions.map(shortRevision).join(', ')}</Mono> : '',
            },
          ]}
        />
      </section>
      {progress && (total > 0 || progress.syncPhase) ? (
        <section className={styles.section} aria-labelledby={`${id}-progress`}>
          <h3 id={`${id}-progress`}>Progress</h3>
          <div className={styles.progress}>
            <span>
              {progress.syncPhase ? `Phase ${progress.syncPhase}` : 'Phase pending'}
              {progress.wave !== undefined ? `, wave ${progress.wave}` : ''}
              {total > 0 ? `: ${done} of ${total} tasks completed` : ''}
            </span>
            {total > 0 ? <progress max={total} value={done} aria-label={`${done} of ${total} tasks completed`} /> : null}
          </div>
        </section>
      ) : null}
      {view.warnings.length > 0 ? (
        <Banner tone="warning" title="Warnings">
          <ul className={styles.list}>
            {view.warnings.map((w) => (
              <li key={w}>{w}</li>
            ))}
          </ul>
        </Banner>
      ) : null}
      {hooks.length > 0 ? (
        <p className={styles.muted}>
          {hooks.length} hook{hooks.length === 1 ? '' : 's'}: {hooks.map((h) => `${h.hookType ?? ''} ${h.kind}/${h.name} (${h.hookPhase ?? 'Pending'})`).join(', ')}
        </p>
      ) : null}
      <section className={styles.section} aria-labelledby={`${id}-results`}>
        <h3 id={`${id}-results`}>Resource results</h3>
        {view.results === undefined ? (
          <p className={styles.muted}>Per-resource results are recorded on the Operation object; they appear once it is loaded.</p>
        ) : (
          <>
            <div className={styles.filters}>
              <FormField label="Show">
                <Select
                  size="sm"
                  value={filter}
                  onChange={(e) => setFilter(e.target.value as ResultFilter)}
                  options={[
                    { value: 'all', label: `All (${view.results.length})` },
                    { value: 'failed', label: 'Failed' },
                    { value: 'pruned', label: 'Pruned' },
                    { value: 'hooks', label: 'Hooks' },
                    { value: 'synced', label: 'Synced' },
                  ]}
                />
              </FormField>
            </div>
            <Table
              caption="Resource results"
              columns={RESULT_COLUMNS}
              rows={results}
              getRowKey={(r) => `${resourceKey(r)}/${r.hookType ?? ''}/${r.syncPhase ?? ''}`}
              getRowLabel={(r) => `${r.kind} ${r.name}`}
              empty="No resources match this filter."
            />
          </>
        )}
      </section>
    </div>
  );
}

export interface OperationPanelProps {
  app: Application;
  open: boolean;
  onClose: () => void;
  /** Operation shown in detail; defaults to the application's current operation. */
  selected: string | undefined;
  onSelect: (name: string) => void;
}

const HISTORY_COLUMNS = (current: string): Column<Operation>[] => [
  {
    id: 'name',
    header: 'Operation',
    cell: (op) => (
      <span>
        <Mono>{op.metadata.name}</Mono>
        {op.metadata.name === current ? <span className="sr-only"> (shown above)</span> : null}
      </span>
    ),
  },
  { id: 'kind', header: 'Type', cell: (op) => (op.spec.rollback ? 'Rollback' : 'Sync') },
  { id: 'phase', header: 'Phase', cell: (op) => <OperationPhaseStatus phase={op.status?.phase} /> },
  { id: 'started', header: 'Started', cell: (op) => <Timestamp value={op.status?.startedAt ?? op.metadata.creationTimestamp} /> },
  { id: 'duration', header: 'Duration', cell: (op) => durationBetween(op.status?.startedAt, op.status?.finishedAt) || '—' },
  { id: 'initiator', header: 'Initiated by', cell: (op) => initiatorLabel(op.spec.initiatedBy) },
];

/** Operation progress and history drawer (parity API-100). */
export function OperationPanel({ app, open, onClose, selected, onSelect }: OperationPanelProps) {
  const name = appName(app);
  const scope = appScope(app);
  const stateView = operationStateView(app);
  const running = isRunningPhase(stateView?.phase);
  const operations = useApplicationOperationsPolling(name, scope, { poll: open && running, enabled: open });
  const older = useOlderOperations(name, scope, operations.data?.metadata?.continue);
  const [confirmTerminate, setConfirmTerminate] = useState(false);
  const [confirmApprove, setConfirmApprove] = useState(false);
  const historyId = useId();

  // First page (newest, polled) plus older pages loaded on request; an Operation appears once.
  const items = useMemo(() => {
    const first = operations.data?.items ?? [];
    const seen = new Set(first.map((op) => op.metadata.name));
    return [...first, ...older.items.filter((op) => !seen.has(op.metadata.name))];
  }, [operations.data, older.items]);
  const currentName = selected ?? stateView?.name ?? items[0]?.metadata.name ?? '';
  const currentOp = items.find((op) => op.metadata.name === currentName);
  const view = currentOp ? operationView(currentOp) : stateView?.name === currentName ? stateView : null;
  const viewRunning = isRunningPhase(view?.phase);
  const awaiting = resourcesAwaitingApproval(app);

  return (
    <Drawer open={open} onClose={onClose} title={`Operations of ${name}`} description="Progress and results of syncs and rollbacks." width="960px">
      <div className={styles.stack}>
        <div className={styles.actions}>
          {viewRunning && view?.name === stateView?.name ? (
            <Button variant="danger" onClick={() => setConfirmTerminate(true)}>
              Terminate operation
            </Button>
          ) : null}
          <Button onClick={() => void operations.refetch()} loading={operations.isFetching && !operations.isPending}>
            Refresh
          </Button>
        </div>
        {awaiting.length > 0 ? (
          <Banner
            tone="warning"
            title={`${awaiting.length} resource(s) wait for deletion approval`}
            actions={
              <Button size="sm" onClick={() => setConfirmApprove(true)}>
                Confirm pruning
              </Button>
            }
          >
            These resources use Prune=confirm or Delete=confirm. The operation removes them only after someone approves.
          </Banner>
        ) : null}
        {operations.isError ? (
          <ErrorBanner error={operations.error} onRetry={() => void operations.refetch()} title="Could not load Operation objects" />
        ) : null}
        {operations.isPending && !view ? (
          <div aria-busy="true">
            <SkeletonText lines={5} />
          </div>
        ) : view ? (
          <OperationDetails view={view} />
        ) : (
          <EmptyState title="No operation yet" headingLevel={3}>
            <p>Sync the application to start an operation; its progress and per-resource results appear here.</p>
          </EmptyState>
        )}
        {items.length > 0 ? (
          <section className={styles.section} aria-labelledby={historyId}>
            <h3 id={historyId}>Operation history</h3>
            <p className={styles.muted}>Select an operation and press Enter to show its details.</p>
            <Table
              caption="Operation history"
              columns={HISTORY_COLUMNS(currentName)}
              rows={items}
              getRowKey={(op) => op.metadata.name ?? ''}
              onRowActivate={(op) => onSelect(op.metadata.name ?? '')}
            />
            {older.error ? <ErrorBanner error={older.error} onRetry={older.loadMore} title="Could not load older operations" /> : null}
            {older.hasMore ? (
              <div className={styles.actions}>
                <Button onClick={older.loadMore} loading={older.isLoading}>
                  Load older operations
                </Button>
              </div>
            ) : null}
          </section>
        ) : null}
      </div>

      <TerminateDialog app={app} open={confirmTerminate} onClose={() => setConfirmTerminate(false)} onTerminated={() => void operations.refetch()} />
      <ApproveDeletionDialog app={app} open={confirmApprove} onClose={() => setConfirmApprove(false)} />
    </Drawer>
  );
}
