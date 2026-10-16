/** Pure helpers over Application objects shared by the apps-core pages. */
import type { Application, AppScope } from '../../../api/types';
import { isHttpURL } from '../../../lib/url';

export type AppSpec = Application['spec'];
export type SpecSource = NonNullable<AppSpec['source']>;
export type SyncPolicy = NonNullable<AppSpec['syncPolicy']>;
export type RetryStrategy = NonNullable<SyncPolicy['retry']>;
export type AppCondition = NonNullable<NonNullable<Application['status']>['conditions']>[number];
export type HistoryEntry = NonNullable<NonNullable<Application['status']>['history']>[number];
export type OperationState = NonNullable<NonNullable<Application['status']>['operationState']>;
export type ManagedResourceStatus = NonNullable<NonNullable<Application['status']>['resources']>[number];

/** Annotation and finalizer keys (api/v1alpha1/keys.go). */
export const GROUP = 'cd.patrol.example';
export const ANNOTATION_REFRESH = `${GROUP}/refresh`;
export const ANNOTATION_DELETION_APPROVED = `${GROUP}/deletion-approved`;
export const ANNOTATION_MANAGED_BY_URL = `${GROUP}/managed-by-url`;
export const ANNOTATION_LINK_PREFIX = `link.${GROUP}/`;
export const FINALIZER_RESOURCES = `resources-finalizer.${GROUP}`;

/** Unique key of an application across namespaces: "<namespace>/<name>". */
export function appKey(app: Application): string {
  return `${app.metadata.namespace ?? ''}/${app.metadata.name ?? ''}`;
}

export function appName(app: Application): string {
  return app.metadata.name ?? '';
}

/** Details route; namespaced whenever the object carries a namespace (as upstream getAppUrl). */
export function appDetailsPath(app: Application, search = ''): string {
  const name = encodeURIComponent(appName(app));
  const ns = app.metadata.namespace;
  const base = ns ? `/applications/${encodeURIComponent(ns)}/${name}` : `/applications/${name}`;
  return search ? `${base}?${search}` : base;
}

export function appScope(app: Application): AppScope {
  return { appNamespace: app.metadata.namespace, project: app.spec.project };
}

/** Sources in position order: `spec.sources` when set, else the single `spec.source`. */
export function appSources(app: Application): SpecSource[] {
  const multi = app.spec.sources;
  if (multi && multi.length > 0) return multi;
  return app.spec.source ? [app.spec.source] : [];
}

export function isMultiSource(app: Application): boolean {
  return (app.spec.sources?.length ?? 0) > 0;
}

export function healthOf(app: Application): string {
  return app.status?.health?.status ?? 'Unknown';
}

export function syncOf(app: Application): string {
  return app.status?.sync?.status ?? 'Unknown';
}

export function operationPhaseOf(app: Application): string | undefined {
  return app.status?.operationState?.phase;
}

export function isRunningPhase(phase: string | undefined): boolean {
  return phase === 'Running' || phase === 'Terminating' || phase === 'Pending';
}

export function isDeleting(app: Application): boolean {
  return Boolean(app.metadata.deletionTimestamp);
}

/** Automated sync is on when `automated` is present and not explicitly disabled. */
export function isAutoSyncEnabled(app: Application): boolean {
  const automated = app.spec.syncPolicy?.automated;
  return automated !== undefined && automated.enabled !== false;
}

export function isRefreshing(app: Application): boolean {
  return Boolean(app.metadata.annotations?.[ANNOTATION_REFRESH]);
}

const SHA = /^[0-9a-f]{40}$|^[0-9a-f]{64}$/i;

export function isCommitSHA(revision: string | undefined): boolean {
  return Boolean(revision && SHA.test(revision));
}

/** First 7 characters of a commit SHA; other revisions (tags, chart versions) unchanged. */
export function shortRevision(revision: string | undefined): string {
  if (!revision) return '';
  return isCommitSHA(revision) ? revision.slice(0, 7) : revision;
}

/**
 * Browser URL of a Git repository: https URLs lose ".git", scp-style SSH URLs
 * (git@host:org/repo.git) and ssh:// URLs become https. Anything else yields null.
 */
export function repoWebURL(repoURL: string | undefined): string | null {
  if (!repoURL) return null;
  let url = repoURL.trim();
  const scp = /^[\w.-]+@([\w.-]+):(.+)$/.exec(url);
  if (scp) url = `https://${scp[1] ?? ''}/${scp[2] ?? ''}`;
  else if (url.startsWith('ssh://')) {
    try {
      const parsed = new URL(url);
      url = `https://${parsed.hostname}${parsed.pathname}`;
    } catch {
      return null;
    }
  }
  url = url.replace(/\/+$/, '').replace(/\.git$/, '');
  return isHttpURL(url) ? url : null;
}

function hostOf(url: string): string {
  try {
    return new URL(url).hostname;
  } catch {
    return '';
  }
}

/** Commit page for a SHA revision on common Git hosts (null for tags, branches, charts). */
export function commitURL(repoURL: string | undefined, revision: string | undefined): string | null {
  if (!isCommitSHA(revision)) return null;
  const web = repoWebURL(repoURL);
  if (!web || !revision) return null;
  const host = hostOf(web);
  if (host.includes('gitlab')) return `${web}/-/commit/${revision}`;
  if (host.includes('bitbucket')) return `${web}/commits/${revision}`;
  return `${web}/commit/${revision}`;
}

/** Compare page between two revisions (history "diff to current"); null when unsupported. */
export function compareURL(repoURL: string | undefined, from: string | undefined, to: string | undefined): string | null {
  if (!from || !to || from === to) return null;
  const web = repoWebURL(repoURL);
  if (!web) return null;
  const host = hostOf(web);
  if (host.includes('bitbucket')) return `${web}/branches/compare/${to}%0D${from}`;
  if (host.includes('gitlab')) return `${web}/-/compare/${from}...${to}`;
  return `${web}/compare/${from}...${to}`;
}

/** "Helm", "Kustomize", "Directory", "Plugin" or "Git" for a source (spec first, then status). */
export function sourceKind(source: SpecSource, statusType?: string): string {
  if (source.chart || source.helm) return 'Helm';
  if (source.kustomize) return 'Kustomize';
  if (source.plugin) return 'Plugin';
  if (source.directory) return 'Directory';
  if (statusType) return statusType;
  if (source.ref && !source.path) return 'Reference';
  return 'Git';
}

/** Repository location of a source: "path" for Git, "chart@version" for Helm charts. */
export function sourceLocation(source: SpecSource): string {
  if (source.chart) return `${source.chart}${source.targetRevision ? `@${source.targetRevision}` : ''}`;
  return source.path ?? '';
}

/** Synced revision of each source position (status.sync.revisions or .revision). */
export function syncedRevisions(app: Application): (string | undefined)[] {
  const sync = app.status?.sync;
  const sources = appSources(app);
  if (isMultiSource(app)) return sources.map((_s, i) => sync?.revisions?.[i]);
  return [sync?.revision];
}

export function destinationLabel(app: Application): string {
  const d = app.spec.destination;
  return d.name ?? d.server ?? '';
}

export function clusterOf(app: Application): string {
  return destinationLabel(app);
}

/** Conditions with status True (or unset), errors first. */
export function activeConditions(app: Application): AppCondition[] {
  const all = (app.status?.conditions ?? []).filter((c) => (c.status as string | undefined) !== 'False');
  return [...all].sort((a, b) => conditionRank(a.type) - conditionRank(b.type));
}

export function conditionSeverity(type: string): 'error' | 'warning' | 'info' {
  if (type.endsWith('Error')) return 'error';
  if (type.endsWith('Warning')) return 'warning';
  return 'info';
}

function conditionRank(type: string): number {
  const severity = conditionSeverity(type);
  return severity === 'error' ? 0 : severity === 'warning' ? 1 : 2;
}

/** Human duration: "45s", "3m 12s", "2h 5m". */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '';
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

export function durationBetween(start: string | undefined, end: string | undefined, now = Date.now()): string {
  if (!start) return '';
  const from = Date.parse(start);
  const to = end ? Date.parse(end) : now;
  if (Number.isNaN(from) || Number.isNaN(to)) return '';
  return formatDuration(to - from);
}

/** Who started an operation or deployment. */
export function initiatorLabel(initiatedBy: { automated?: boolean; username?: string } | undefined): string {
  if (!initiatedBy) return 'Unknown';
  if (initiatedBy.automated) return 'Automated sync';
  return initiatedBy.username ?? 'Unknown';
}

/** Stable key for a managed resource: "group/kind/namespace/name" (as the resource views use). */
export function resourceKey(ref: { group?: string | undefined; kind?: string | undefined; namespace?: string | undefined; name?: string | undefined }): string {
  return `${ref.group ?? ''}/${ref.kind ?? ''}/${ref.namespace ?? ''}/${ref.name ?? ''}`;
}

/** Deep copy of plain JSON data (API objects). */
export function cloneJSON<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}
