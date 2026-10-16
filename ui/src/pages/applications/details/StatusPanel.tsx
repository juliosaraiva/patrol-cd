import { useId, type ReactNode } from 'react';
import { useApplicationSyncWindows, useRevisionMetadata } from '../../../api/queries/applications';
import type { Application } from '../../../api/types';
import { Button, HealthStatus, Mono, OperationPhaseStatus, SyncStatus, Timestamp } from '../../../design';
import { isHttpURL } from '../../../lib/url';
import {
  appName,
  appScope,
  appSources,
  commitURL,
  healthOf,
  initiatorLabel,
  isAutoSyncEnabled,
  isCommitSHA,
  isMultiSource,
  repoWebURL,
  shortRevision,
  sourceKind,
  sourceLocation,
  syncOf,
  syncedRevisions,
} from '../shared/app';
import styles from './Details.module.css';

function ExternalAnchor({ href, children, label }: { href: string; children: ReactNode; label?: string }) {
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" title={label}>
      {children}
      <span className="sr-only"> (opens in a new tab)</span>
    </a>
  );
}

/** Revision text with a commit link when the host is known. */
export function RevisionRef({ repoURL, revision }: { repoURL: string | undefined; revision: string | undefined }) {
  if (!revision) return <span>—</span>;
  const url = commitURL(repoURL, revision);
  const text = <Mono title={revision}>{shortRevision(revision)}</Mono>;
  return url ? <ExternalAnchor href={url}>{text}</ExternalAnchor> : text;
}

function CommitSummary({ app, revision }: { app: Application; revision: string }) {
  const meta = useRevisionMetadata(appName(app), revision, appScope(app));
  if (!meta.data) return null;
  const subject = meta.data.message?.split('\n')[0];
  return (
    <p className={styles.statusDetail}>
      {subject ? <span>{subject}</span> : null}
      {meta.data.author ? <span> · {meta.data.author}</span> : null}
    </p>
  );
}

function autoSyncText(app: Application): string {
  const automated = app.spec.syncPolicy?.automated;
  if (!isAutoSyncEnabled(app) || !automated) return 'Auto-sync disabled';
  const extras = [automated.prune ? 'prune' : '', automated.selfHeal ? 'self-heal' : '', automated.allowEmpty ? 'allow empty' : ''].filter(Boolean);
  return extras.length > 0 ? `Auto-sync enabled (${extras.join(', ')})` : 'Auto-sync enabled';
}

/** Health, sync, last operation and revisions of an application (parity API-100). */
export function StatusPanel({ app, onOpenOperation, extensions }: { app: Application; onOpenOperation: () => void; extensions?: ReactNode }) {
  const windows = useApplicationSyncWindows(appName(app), appScope(app));
  const sources = appSources(app);
  const synced = syncedRevisions(app);
  const op = app.status?.operationState;
  const firstSource = sources[0];
  const firstRevision = synced[0];
  const showCommit = !isMultiSource(app) && firstSource && !firstSource.chart && isCommitSHA(firstRevision);
  const blocked = windows.data?.canSync === false;
  const id = useId();
  return (
    <div className={styles.statusGrid}>
      <section className={styles.statusCell} aria-labelledby={`${id}-health`}>
        <h2 id={`${id}-health`}>App health</h2>
        <div className={styles.statusValue}>
          <HealthStatus status={healthOf(app)} />
        </div>
        {app.status?.health?.lastTransitionTime ? (
          <p className={styles.statusDetail}>
            Since <Timestamp value={app.status.health.lastTransitionTime} />
          </p>
        ) : null}
      </section>
      <section className={styles.statusCell} aria-labelledby={`${id}-sync`}>
        <h2 id={`${id}-sync`}>Sync status</h2>
        <div className={styles.statusValue}>
          <SyncStatus status={syncOf(app)} />
          {!isMultiSource(app) && firstRevision ? (
            <span className={styles.statusDetail}>
              to <RevisionRef repoURL={firstSource?.repoURL} revision={firstRevision} />
            </span>
          ) : null}
        </div>
        {showCommit && firstRevision ? <CommitSummary app={app} revision={firstRevision} /> : null}
        <p className={styles.statusDetail}>{autoSyncText(app)}</p>
        {blocked ? <p className={styles.statusDetail}>A sync window currently blocks manual syncs.</p> : null}
        {!blocked && (windows.data?.activeWindows?.length ?? 0) > 0 ? <p className={styles.statusDetail}>A sync window is active.</p> : null}
      </section>
      <section className={styles.statusCell} aria-labelledby={`${id}-op`}>
        <h2 id={`${id}-op`}>Last operation</h2>
        {op ? (
          <>
            <div className={styles.statusValue}>
              <OperationPhaseStatus phase={op.phase} />
            </div>
            <p className={styles.statusDetail}>
              {initiatorLabel(op.initiatedBy)}
              {op.startedAt ? (
                <>
                  {', started '}
                  <Timestamp value={op.startedAt} />
                </>
              ) : null}
            </p>
            {op.message ? <p className={styles.statusDetail}>{op.message}</p> : null}
            <div>
              <Button size="sm" onClick={onOpenOperation}>
                Operation details
              </Button>
            </div>
          </>
        ) : (
          <p className={styles.statusDetail}>Never synced from Patrol.</p>
        )}
      </section>
      <section className={styles.statusCell} aria-labelledby={`${id}-source`}>
        <h2 id={`${id}-source`}>{sources.length > 1 ? `Sources (${sources.length})` : 'Source'}</h2>
        {sources.length === 0 ? (
          <p className={styles.statusDetail}>No source configured.</p>
        ) : (
          <ul className={styles.revisionList}>
            {sources.map((s, i) => {
              const web = repoWebURL(s.repoURL);
              const repo = <Mono>{s.repoURL}</Mono>;
              return (
                <li key={i}>
                  <span>{sourceKind(s, app.status?.sourceTypes?.[i] ?? (i === 0 ? app.status?.sourceType : undefined))}</span>{' '}
                  {web && !s.chart ? <ExternalAnchor href={web}>{repo}</ExternalAnchor> : s.chart && isHttpURL(s.repoURL) ? <ExternalAnchor href={s.repoURL}>{repo}</ExternalAnchor> : repo}
                  {sourceLocation(s) ? (
                    <>
                      {' '}
                      <Mono>{sourceLocation(s)}</Mono>
                    </>
                  ) : null}
                  {s.ref ? (
                    <>
                      {' '}
                      (ref <Mono>{s.ref}</Mono>)
                    </>
                  ) : null}
                  <br />
                  <span className={styles.statusDetail}>
                    Tracks <Mono>{s.chart ? s.targetRevision ?? 'latest' : s.targetRevision ?? 'HEAD'}</Mono>
                    {synced[i] && !s.chart ? (
                      <>
                        {', synced to '}
                        <RevisionRef repoURL={s.repoURL} revision={synced[i]} />
                      </>
                    ) : null}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
        <ImagesSummary app={app} />
      </section>
      {extensions}
    </div>
  );
}

function ImagesSummary({ app }: { app: Application }) {
  const images = app.status?.summary?.images ?? [];
  if (images.length === 0) return null;
  return (
    <p className={styles.statusDetail}>
      Images: <Mono>{images.slice(0, 3).join(', ')}</Mono>
      {images.length > 3 ? ` and ${images.length - 3} more` : ''}
    </p>
  );
}
