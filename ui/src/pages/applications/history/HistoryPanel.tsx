import { useId, useState } from 'react';
import { useRevisionMetadata, useRollbackApplication, useUpdateApplication } from '../../../api/queries/applications';
import type { Application } from '../../../api/types';
import { Badge, Banner, Button, ButtonLink, Checkbox, Dialog, Drawer, EmptyState, KeyValueList, Mono, SkeletonText, Timestamp, useToast } from '../../../design';
import { IconExternal } from '../../../design/icons';
import {
  appDetailsPath,
  appName,
  appScope,
  appSources,
  cloneJSON,
  commitURL,
  compareURL,
  durationBetween,
  initiatorLabel,
  isAutoSyncEnabled,
  isCommitSHA,
  isDeleting,
  shortRevision,
  sourceKind,
  sourceLocation,
  syncedRevisions,
  type HistoryEntry,
  type SpecSource,
} from '../shared/app';
import { ActionError } from '../shared/errors';
import styles from './History.module.css';

/** Source and revision of each position recorded in a history entry. */
export function entrySources(entry: HistoryEntry): { source: SpecSource | undefined; revision: string | undefined }[] {
  if (entry.sources && entry.sources.length > 0) return entry.sources.map((source, i) => ({ source, revision: entry.revisions?.[i] }));
  if (entry.source || entry.revision) return [{ source: entry.source, revision: entry.revision }];
  return (entry.revisions ?? []).map((revision) => ({ source: undefined, revision }));
}

export function canRollbackEntry(entry: HistoryEntry): boolean {
  return Boolean(entry.source) || (entry.sources?.length ?? 0) > 0;
}

function RevisionLink({ repoURL, revision }: { repoURL: string | undefined; revision: string | undefined }) {
  if (!revision) return <span>—</span>;
  const url = commitURL(repoURL, revision);
  return url ? (
    <a href={url} target="_blank" rel="noopener noreferrer" title={revision}>
      <Mono>{shortRevision(revision)}</Mono>
      <span className="sr-only"> (opens commit in a new tab)</span>
    </a>
  ) : (
    <Mono title={revision}>{shortRevision(revision)}</Mono>
  );
}

function CommitMetadata({ app, revision, sourceIndex }: { app: Application; revision: string; sourceIndex: number | undefined }) {
  const meta = useRevisionMetadata(appName(app), revision, appScope(app), sourceIndex === undefined ? {} : { sourceIndex });
  if (meta.isPending) {
    return (
      <div aria-busy="true" className={styles.meta}>
        <SkeletonText lines={2} />
      </div>
    );
  }
  if (meta.isError) return <p className={styles.muted}>Commit details are unavailable for {shortRevision(revision)}.</p>;
  const m = meta.data;
  return (
    <div className={styles.meta}>
      <KeyValueList
        items={[
          { key: 'author', label: 'Author', value: m.author },
          { key: 'date', label: 'Date', value: m.date ? <Timestamp value={m.date} /> : '' },
          { key: 'message', label: 'Message', value: m.message },
          { key: 'tags', label: 'Tags', value: m.tags?.join(', ') },
          { key: 'signature', label: 'Signature', value: m.signatureInfo },
        ]}
      />
    </div>
  );
}

function HistoryItem({
  app,
  entry,
  latest,
  onRollback,
}: {
  app: Application;
  entry: HistoryEntry;
  latest: boolean;
  onRollback: (entry: HistoryEntry) => void;
}) {
  const headingId = useId();
  const [expanded, setExpanded] = useState(false);
  const positions = entrySources(entry);
  const current = syncedRevisions(app);
  const multi = positions.length > 1;
  return (
    <li className={styles.entry}>
      <div className={styles.entryHeader}>
        <h3 id={headingId}>Deployment {entry.id}</h3>
        {latest ? <Badge tone="accent">Current</Badge> : null}
        <span className={styles.spacer} />
        <Button size="sm" variant={latest ? 'secondary' : 'primary'} onClick={() => onRollback(entry)} disabled={!canRollbackEntry(entry)}>
          {latest ? 'Redeploy' : 'Roll back'}
          <span className="sr-only"> to deployment {entry.id}</span>
        </Button>
      </div>
      <KeyValueList
        items={[
          { key: 'deployed', label: 'Deployed', value: <Timestamp value={entry.deployedAt} /> },
          { key: 'duration', label: 'Duration', value: durationBetween(entry.deployStartedAt, entry.deployedAt) },
          { key: 'initiator', label: 'Initiated by', value: initiatorLabel(entry.initiatedBy) },
        ]}
      />
      <ul className={styles.sources} aria-label={`Sources of deployment ${entry.id}`}>
        {positions.map(({ source, revision }, i) => {
          const compare = !source?.chart && revision && current[i] ? compareURL(source?.repoURL, revision, current[i]) : null;
          return (
            <li key={i}>
              {multi ? `Source ${i + 1}: ` : ''}
              {source ? (
                <>
                  {sourceKind(source)} <Mono>{source.repoURL}</Mono> {sourceLocation(source) ? <Mono>{sourceLocation(source)}</Mono> : null}{' '}
                </>
              ) : null}
              at <RevisionLink repoURL={source?.repoURL} revision={revision} />
              {compare ? (
                <>
                  {' · '}
                  <a href={compare} target="_blank" rel="noopener noreferrer">
                    Compare with current <IconExternal size={12} />
                    <span className="sr-only"> (opens in a new tab)</span>
                  </a>
                </>
              ) : null}
            </li>
          );
        })}
      </ul>
      {positions.some((p) => isCommitSHA(p.revision) && !p.source?.chart) ? (
        <div>
          <Button size="sm" variant="ghost" aria-expanded={expanded} onClick={() => setExpanded(!expanded)}>
            {expanded ? 'Hide commit details' : 'Show commit details'}
          </Button>
          {expanded
            ? positions.map((p, i) =>
                p.revision && isCommitSHA(p.revision) && !p.source?.chart ? (
                  <CommitMetadata key={i} app={app} revision={p.revision} sourceIndex={multi ? i : undefined} />
                ) : null,
              )
            : null}
        </div>
      ) : null}
    </li>
  );
}

function RollbackDialog({ app, entry, latest, onClose }: { app: Application; entry: HistoryEntry; latest: boolean; onClose: () => void }) {
  const rollback = useRollbackApplication();
  const update = useUpdateApplication();
  const { toast } = useToast();
  const [prune, setPrune] = useState(false);
  const [dryRun, setDryRun] = useState(false);
  const [disableAutoSync, setDisableAutoSync] = useState(false);
  const autoSync = isAutoSyncEnabled(app);
  const deleting = isDeleting(app);
  const recorded = canRollbackEntry(entry);
  const positions = entrySources(entry);
  const blocked = deleting || !recorded || (autoSync && !disableAutoSync);
  const verb = latest ? 'Redeploy' : 'Roll back';

  const run = async () => {
    try {
      if (autoSync && disableAutoSync) {
        const next = cloneJSON(app);
        const automated = next.spec.syncPolicy?.automated;
        if (next.spec.syncPolicy && automated) next.spec.syncPolicy.automated = { ...automated, enabled: false };
        await update.mutateAsync({ app: next, validate: false });
      }
      const { operation } = await rollback.mutateAsync({ name: appName(app), scope: appScope(app), request: { id: entry.id, prune, dryRun } });
      const opName = operation?.metadata.name ?? '';
      toast({
        tone: 'success',
        title: `${latest ? 'Redeploy' : 'Rollback'} to deployment ${entry.id} started`,
        description: opName ? `Operation ${opName}` : undefined,
        action: (
          <ButtonLink size="sm" to={appDetailsPath(app, `panel=operation${opName ? `&op=${encodeURIComponent(opName)}` : ''}`)}>
            View operation
          </ButtonLink>
        ),
      });
      onClose();
    } catch {
      // Rendered below.
    }
  };

  const error = update.error ?? rollback.error;
  return (
    <Dialog
      open
      onClose={onClose}
      size="lg"
      title={`${verb} to deployment ${entry.id}?`}
      description="The manifests recorded for this deployment are applied without sync hooks. The application spec is not changed, so the application reports Out of sync until you sync it to its target revision again."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={blocked} loading={rollback.isPending || update.isPending} onClick={() => void run()}>
            {verb}
          </Button>
        </>
      }
    >
      <div className={styles.stack}>
        <ul className={styles.sources} aria-label="Revisions to deploy">
          {positions.map(({ source, revision }, i) => (
            <li key={i}>
              {positions.length > 1 ? `Source ${i + 1}: ` : ''}
              {source ? <Mono>{source.repoURL}</Mono> : null} at <Mono title={revision}>{shortRevision(revision) || '—'}</Mono>
            </li>
          ))}
        </ul>
        {deleting ? (
          <Banner tone="error" title="The application is being deleted">
            Rollbacks are refused while the application has a deletion timestamp.
          </Banner>
        ) : null}
        {!recorded ? (
          <Banner tone="error" title="This deployment cannot be rolled back">
            It has no recorded source. Sync to its revision instead.
          </Banner>
        ) : null}
        {positions.length > 1 ? (
          <Banner tone="info" title="Multi-source application">
            All {positions.length} sources return to the revisions recorded in this deployment; a single source cannot be rolled back on its own.
          </Banner>
        ) : null}
        {autoSync ? (
          <Banner tone="warning" title="Auto-sync is enabled">
            <p>
              The server refuses rollbacks while auto-sync is enabled, because the next automated sync would return to the target revision.
              Disable auto-sync to continue; re-enable it in the application details when you are ready.
            </p>
            <Checkbox label="Disable auto-sync, then roll back" checked={disableAutoSync} onChange={(e) => setDisableAutoSync(e.target.checked)} />
          </Banner>
        ) : null}
        <Checkbox label="Prune" description="Delete resources that are not part of the recorded deployment." checked={prune} onChange={(e) => setPrune(e.target.checked)} />
        <Checkbox label="Dry run" description="Validate the rollback without changing the cluster." checked={dryRun} onChange={(e) => setDryRun(e.target.checked)} />
        {error ? <ActionError error={error} action={latest ? 'redeploy' : 'roll back'} onRetry={() => void run()} /> : null}
      </div>
    </Dialog>
  );
}

export interface HistoryPanelProps {
  app: Application;
  open: boolean;
  onClose: () => void;
}

/** Deployment history and rollback drawer (parity API-099). */
export function HistoryPanel({ app, open, onClose }: HistoryPanelProps) {
  const [target, setTarget] = useState<HistoryEntry | null>(null);
  const history = [...(app.status?.history ?? [])].sort((a, b) => b.id - a.id);
  const limit = app.spec.revisionHistoryLimit ?? 10;
  return (
    <Drawer open={open} onClose={onClose} title={`History of ${appName(app)}`} description="Successful syncs, newest first. Roll back to redeploy an earlier revision." width="760px">
      {history.length === 0 ? (
        <EmptyState title="No deployments recorded yet" headingLevel={3}>
          <p>
            Each successful, non-dry-run sync of the whole application adds an entry here (the newest {limit} are kept, set by revision history
            limit). Sync the application to record its first deployment.
          </p>
        </EmptyState>
      ) : (
        <ol className={styles.list} aria-label="Deployments">
          {history.map((entry, i) => (
            <HistoryItem key={entry.id} app={app} entry={entry} latest={i === 0} onRollback={setTarget} />
          ))}
        </ol>
      )}
      {appSources(app).length === 0 ? <p className={styles.muted}>This application has no source.</p> : null}
      {target ? <RollbackDialog app={app} entry={target} latest={target.id === history[0]?.id} onClose={() => setTarget(null)} /> : null}
    </Drawer>
  );
}
