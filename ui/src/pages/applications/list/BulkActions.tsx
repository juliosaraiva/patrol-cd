import { useState } from 'react';
import { Link } from 'react-router';
import { describeError } from '../../../shell/errors';
import { useDeleteApplication, useRefreshApplication, useSyncApplication } from '../../../api/queries/applications';
import { useSettings } from '../../../api/queries/resources';
import type { Application } from '../../../api/types';
import { Badge, Banner, Button, Checkbox, Dialog, FormField, Input, Mono, RadioGroup, Table, useToast, type BadgeTone, type Column } from '../../../design';
import { deletionQuery, type DeletionMode } from '../delete/DeleteDialog';
import { appDetailsPath, appKey, appName, appScope } from '../shared/app';
import { ManualFlagsFields, RetryFields, SyncOptionsFields, type ManualFlags } from '../sync/SyncOptionsFields';
import { buildSyncRequest, DEFAULT_RETRY, initialSyncForm, optionValue, validateRetry, type RetryForm } from '../sync/syncOptions';
import styles from './List.module.css';

export type BulkKind = 'sync' | 'refresh' | 'delete';

type ResultState = 'pending' | 'running' | 'succeeded' | 'failed';

export interface BulkResult {
  app: Application;
  state: ResultState;
  message?: string | undefined;
  operation?: string | undefined;
}

const STATE: Record<ResultState, { label: string; tone: BadgeTone }> = {
  pending: { label: 'Waiting', tone: 'neutral' },
  running: { label: 'In progress', tone: 'info' },
  succeeded: { label: 'Succeeded', tone: 'success' },
  failed: { label: 'Failed', tone: 'danger' },
};

const CONCURRENCY = 4;

/** Runs `task` for every item with bounded concurrency, reporting each settlement. */
export async function runBounded<T>(items: readonly T[], limit: number, task: (item: T) => Promise<void>): Promise<void> {
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const item = items[next];
      next += 1;
      if (item !== undefined) await task(item);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
}

const TITLES: Record<BulkKind, (n: number) => string> = {
  sync: (n) => `Sync ${n} application${n === 1 ? '' : 's'}`,
  refresh: (n) => `Refresh ${n} application${n === 1 ? '' : 's'}`,
  delete: (n) => `Delete ${n} application${n === 1 ? '' : 's'}`,
};

/** Bulk sync / refresh / delete with confirmation and per-application results (parity API-090). */
export function BulkActionDialog({ kind, apps, onClose }: { kind: BulkKind; apps: readonly Application[]; onClose: () => void }) {
  const settings = useSettings();
  const sync = useSyncApplication();
  const refresh = useRefreshApplication();
  const del = useDeleteApplication();
  const { toast } = useToast();
  const [phase, setPhase] = useState<'configure' | 'running' | 'done'>('configure');
  const [results, setResults] = useState<Map<string, BulkResult>>(new Map());
  const [flags, setFlags] = useState<ManualFlags>({ prune: false, dryRun: false, applyOnly: false, force: false });
  const [syncOptions, setSyncOptions] = useState<string[]>([]);
  const [retry, setRetry] = useState<RetryForm>(DEFAULT_RETRY);
  const [forceConfirmed, setForceConfirmed] = useState(false);
  const [refreshType, setRefreshType] = useState<'normal' | 'hard'>('normal');
  const [mode, setMode] = useState<DeletionMode>('foreground');
  const [typed, setTyped] = useState('');
  const [attempted, setAttempted] = useState(false);

  const count = apps.length;
  const replaceAllowed = settings.data?.syncWithReplaceAllowed ?? false;
  const retryErrors = validateRetry(retry);
  const problems: string[] = [];
  if (kind === 'sync' && flags.force && !forceConfirmed) problems.push('Confirm that you understand the Force option.');
  if (kind === 'sync' && Object.keys(retryErrors).length > 0) problems.push('Fix the retry settings.');
  if (kind === 'sync' && replaceAllowed && optionValue(syncOptions, 'Replace') === 'true' && typed !== 'replace') problems.push("Type 'replace' to confirm the Replace option.");
  if (kind === 'delete' && typed !== 'delete') problems.push("Type 'delete' to confirm.");

  const set = (key: string, result: BulkResult) =>
    setResults((prev) => {
      const next = new Map(prev);
      next.set(key, result);
      return next;
    });

  const runOne = async (app: Application): Promise<boolean> => {
    const key = appKey(app);
    set(key, { app, state: 'running' });
    try {
      if (kind === 'sync') {
        const form = { ...initialSyncForm(app), ...flags, syncOptions: [...new Set([...(app.spec.syncPolicy?.syncOptions ?? []), ...syncOptions])], retry, resources: null };
        const { operation } = await sync.mutateAsync({ name: appName(app), scope: appScope(app), request: buildSyncRequest(app, form) });
        set(key, { app, state: 'succeeded', message: flags.dryRun ? 'Dry run started' : 'Sync started', operation: operation?.metadata.name });
      } else if (kind === 'refresh') {
        await refresh.mutateAsync({ name: appName(app), scope: appScope(app), type: refreshType });
        set(key, { app, state: 'succeeded', message: refreshType === 'hard' ? 'Hard refreshed' : 'Refreshed' });
      } else {
        await del.mutateAsync({ name: appName(app), scope: appScope(app), ...deletionQuery(mode) });
        set(key, { app, state: 'succeeded', message: 'Deletion started' });
      }
      return true;
    } catch (err) {
      const guidance = describeError(err);
      set(key, { app, state: 'failed', message: `${guidance.title}: ${guidance.message}` });
      return false;
    }
  };

  const start = async () => {
    setAttempted(true);
    if (problems.length > 0) return;
    setResults(new Map(apps.map((app) => [appKey(app), { app, state: 'pending' as const }])));
    setPhase('running');
    let failed = 0;
    await runBounded(apps, CONCURRENCY, async (app) => {
      if (!(await runOne(app))) failed += 1;
    });
    setPhase('done');
    toast({
      tone: failed > 0 ? 'warning' : 'success',
      title: `${TITLES[kind](count)}: ${count - failed} succeeded${failed > 0 ? `, ${failed} failed` : ''}`,
      description: failed > 0 ? 'See the results for the failed applications.' : undefined,
    });
  };

  const rows = [...results.values()];
  const succeeded = rows.filter((r) => r.state === 'succeeded').length;
  const failedCount = rows.filter((r) => r.state === 'failed').length;
  const columns: Column<BulkResult>[] = [
    { id: 'app', header: 'Application', cell: (r) => <Mono>{appName(r.app)}</Mono> },
    { id: 'state', header: 'Result', cell: (r) => <Badge tone={STATE[r.state].tone}>{STATE[r.state].label}</Badge> },
    {
      id: 'details',
      header: 'Details',
      cell: (r) => (
        <span>
          {r.message ?? ''}
          {r.operation ? (
            <>
              {' '}
              <Link to={appDetailsPath(r.app, `panel=operation&op=${encodeURIComponent(r.operation)}`)}>View operation</Link>
            </>
          ) : null}
        </span>
      ),
    },
  ];

  return (
    <Dialog
      open
      size="lg"
      onClose={onClose}
      title={TITLES[kind](count)}
      description={phase === 'configure' ? 'Review the options, then confirm. Each application is processed separately.' : undefined}
      footer={
        phase === 'configure' ? (
          <>
            <Button onClick={onClose}>Cancel</Button>
            <Button variant={kind === 'delete' ? 'danger' : 'primary'} onClick={() => void start()}>
              {TITLES[kind](count)}
            </Button>
          </>
        ) : (
          <Button onClick={onClose} disabled={phase === 'running'}>
            Close
          </Button>
        )
      }
    >
      {phase === 'configure' ? (
        <div className={styles.dialogStack}>
          <ul className={styles.appNames} aria-label="Selected applications">
            {apps.map((app) => (
              <li key={appKey(app)}>
                <Mono>{appKey(app)}</Mono>
              </li>
            ))}
          </ul>
          {kind === 'sync' ? (
            <>
              <ManualFlagsFields value={flags} onChange={setFlags} />
              {flags.prune ? (
                <Banner tone="warning" title="Prune deletes resources">
                  Resources no longer defined in each application&apos;s source are deleted from the cluster.
                </Banner>
              ) : null}
              {flags.force ? (
                <Banner tone="warning" title="Force is destructive">
                  <Checkbox label="I understand that Force deletes and recreates resources" checked={forceConfirmed} onChange={(e) => setForceConfirmed(e.target.checked)} />
                </Banner>
              ) : null}
              <SyncOptionsFields options={syncOptions} onChange={setSyncOptions} replaceAllowed={replaceAllowed} legend="Additional sync options" />
              {replaceAllowed && optionValue(syncOptions, 'Replace') === 'true' ? (
                <FormField label="Type 'replace' to confirm">
                  <Input value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
                </FormField>
              ) : null}
              <RetryFields value={retry} onChange={setRetry} errors={attempted ? retryErrors : {}} />
            </>
          ) : null}
          {kind === 'refresh' ? (
            <RadioGroup
              legend="Refresh type"
              value={refreshType}
              onChange={setRefreshType}
              options={[
                { value: 'normal', label: 'Normal', description: 'Compare with the source using cached manifests when inputs are unchanged.' },
                { value: 'hard', label: 'Hard', description: 'Regenerate manifests and invalidate cached cluster state.' },
              ]}
            />
          ) : null}
          {kind === 'delete' ? (
            <>
              <RadioGroup
                legend="Deletion mode"
                value={mode}
                onChange={setMode}
                options={[
                  { value: 'foreground', label: 'Foreground (cascade)', description: 'Delete managed resources, then the applications.' },
                  { value: 'background', label: 'Background (cascade)', description: 'Remove the applications now; resources are deleted afterwards.' },
                  { value: 'non-cascading', label: 'Non-cascading', description: 'Remove only the applications; resources stay in the cluster.' },
                ]}
              />
              <FormField label="Type 'delete' to confirm" error={attempted && typed !== 'delete' ? "Type 'delete' exactly." : undefined}>
                <Input value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
              </FormField>
            </>
          ) : null}
          {attempted && problems.length > 0 ? (
            <Banner tone="error" live title="Cannot start yet">
              <ul>
                {problems.map((p) => (
                  <li key={p}>{p}</li>
                ))}
              </ul>
            </Banner>
          ) : null}
        </div>
      ) : (
        <div className={styles.dialogStack}>
          <p role="status">
            {phase === 'running' ? `Processing: ${succeeded + failedCount} of ${count} done.` : `Finished: ${succeeded} succeeded, ${failedCount} failed.`}
          </p>
          <Table caption="Results" columns={columns} rows={rows} getRowKey={(r) => appKey(r.app)} getRowLabel={(r) => appName(r.app)} />
        </div>
      )}
    </Dialog>
  );
}
