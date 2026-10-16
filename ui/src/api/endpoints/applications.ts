import { resolveServerURL } from '../basePath';
import { buildURL, request, seg, type CallOptions, type QueryParams } from '../client';
import { openStream, type StreamHandle, type StreamOptions } from '../sse';
import type {
  Application,
  ApplicationChartDetails,
  ApplicationInput,
  ApplicationList,
  ApplicationManifestResponse,
  ApplicationOCIMetadata,
  ApplicationOperationResponse,
  ApplicationPatchRequest,
  ApplicationResourcePatchRequest,
  ApplicationResourceResponse,
  ApplicationRevisionMetadata,
  ApplicationRollbackRequest,
  ApplicationServerSideDiffRequest,
  ApplicationSpec,
  ApplicationSyncRequest,
  ApplicationSyncWindowsResponse,
  ApplicationTree,
  ApplicationTreeEvent,
  ApplicationWatchEvent,
  AppScope,
  EventList,
  LinksResponse,
  LogEntry,
  ManagedResourcesResponse,
  Operation,
  OperationList,
  QueryOf,
  ResourceActionRunRequest,
  ResourceActionsListResponse,
  ResourceIdentifier,
  ServerSideDiffResponse,
} from '../types';

export type ListApplicationsQuery = QueryOf<'listApplications'>;
export type WatchApplicationsQuery = QueryOf<'watchApplications'>;
export type DeleteApplicationQuery = Pick<QueryOf<'deleteApplication'>, 'cascade' | 'propagationPolicy'>;
export type ApplicationLogsQuery = Omit<QueryOf<'getApplicationLogs'>, 'appNamespace' | 'project' | 'download'>;
export type PodLogsQuery = Omit<QueryOf<'getPodLogs'>, 'appNamespace' | 'project' | 'download'>;
export type ListApplicationOperationsQuery = Pick<QueryOf<'listApplicationOperations'>, 'limit' | 'continue'>;
export type RefreshType = 'normal' | 'hard';

/** Stream options callers may set; path, query and parsing are fixed per route. */
export type StreamCallbacks<T> = Omit<StreamOptions<T>, 'path' | 'query' | 'parse'>;

const base = (name: string) => `/api/v1/applications/${seg(name)}`;

function scopeQuery(scope: AppScope | undefined): QueryParams {
  return { appNamespace: scope?.appNamespace, project: scope?.project };
}

function resourceQuery(resource: ResourceIdentifier): QueryParams {
  return {
    namespace: resource.namespace,
    resourceName: resource.name,
    version: resource.version,
    group: resource.group,
    kind: resource.kind,
  };
}

/** GET /api/v1/applications */
export function listApplications(query: ListApplicationsQuery = {}, options: CallOptions = {}): Promise<ApplicationList> {
  return request('/api/v1/applications', { query, signal: options.signal });
}

/** GET /api/v1/stream/applications (SSE). Pass the list's resourceVersion to avoid a gap. */
export function watchApplications(
  query: WatchApplicationsQuery,
  callbacks: StreamCallbacks<ApplicationWatchEvent>,
): StreamHandle {
  return openStream({ ...callbacks, path: '/api/v1/stream/applications', query });
}

/** POST /api/v1/applications */
export function createApplication(
  app: ApplicationInput,
  query: { upsert?: boolean; validate?: boolean } = {},
  options: CallOptions = {},
): Promise<Application> {
  return request('/api/v1/applications', { method: 'POST', query, body: app, signal: options.signal });
}

/** GET /api/v1/applications/{name}; `refresh` sets the refresh annotation and waits briefly. */
export function getApplication(
  name: string,
  scope?: AppScope & { refresh?: RefreshType },
  options: CallOptions = {},
): Promise<Application> {
  return request(base(name), { query: { ...scopeQuery(scope), refresh: scope?.refresh }, signal: options.signal });
}

/** PUT /api/v1/applications/{name} */
export function updateApplication(
  app: ApplicationInput,
  query: { validate?: boolean } & AppScope = {},
  options: CallOptions = {},
): Promise<Application> {
  const name = app.metadata?.name ?? '';
  return request(base(name), {
    method: 'PUT',
    query: { validate: query.validate, ...scopeQuery({ appNamespace: query.appNamespace ?? app.metadata?.namespace, project: query.project }) },
    body: app,
    signal: options.signal,
  });
}

/** PUT /api/v1/applications/{name}/spec */
export function updateApplicationSpec(
  name: string,
  spec: ApplicationSpec,
  query: { validate?: boolean } & AppScope = {},
  options: CallOptions = {},
): Promise<ApplicationSpec> {
  return request(`${base(name)}/spec`, {
    method: 'PUT',
    query: { validate: query.validate, ...scopeQuery(query) },
    body: spec,
    signal: options.signal,
  });
}

/** PATCH /api/v1/applications/{name} */
export function patchApplication(
  name: string,
  body: ApplicationPatchRequest,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<Application> {
  return request(base(name), { method: 'PATCH', query: scopeQuery(scope), body, signal: options.signal });
}

/** DELETE /api/v1/applications/{name} (the response body is an empty object) */
export async function deleteApplication(
  name: string,
  query: DeleteApplicationQuery & AppScope = {},
  options: CallOptions = {},
): Promise<void> {
  await request(base(name), {
    method: 'DELETE',
    query: { cascade: query.cascade, propagationPolicy: query.propagationPolicy, ...scopeQuery(query) },
    signal: options.signal,
  });
}

/** POST /api/v1/applications/{name}/sync — creates an Operation; returns it with the Application. */
export function syncApplication(
  name: string,
  body: ApplicationSyncRequest,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<ApplicationOperationResponse> {
  return request(`${base(name)}/sync`, { method: 'POST', query: scopeQuery(scope), body, signal: options.signal });
}

/** POST /api/v1/applications/{name}/rollback — creates a rollback Operation. */
export function rollbackApplication(
  name: string,
  body: ApplicationRollbackRequest,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<ApplicationOperationResponse> {
  return request(`${base(name)}/rollback`, { method: 'POST', query: scopeQuery(scope), body, signal: options.signal });
}

/**
 * DELETE /api/v1/applications/{name}/operation — requests cancellation of the running Operation
 * (the server sets its spec.cancel); returns the Operation being terminated.
 */
export function terminateOperation(name: string, scope?: AppScope, options: CallOptions = {}): Promise<ApplicationOperationResponse> {
  return request(`${base(name)}/operation`, { method: 'DELETE', query: scopeQuery(scope), signal: options.signal });
}

/** GET /api/v1/applications/{name}/operations — newest first; `limit`/`continue` paginate. */
export function listApplicationOperations(
  name: string,
  scope?: AppScope & ListApplicationOperationsQuery,
  options: CallOptions = {},
): Promise<OperationList> {
  return request(`${base(name)}/operations`, {
    query: { ...scopeQuery(scope), limit: scope?.limit, continue: scope?.continue },
    signal: options.signal,
  });
}

/** GET /api/v1/applications/{name}/operations/{op} */
export function getApplicationOperation(name: string, op: string, scope?: AppScope, options: CallOptions = {}): Promise<Operation> {
  return request(`${base(name)}/operations/${seg(op)}`, { query: scopeQuery(scope), signal: options.signal });
}

/** GET /api/v1/applications/{name}/syncwindows */
export function getApplicationSyncWindows(
  name: string,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<ApplicationSyncWindowsResponse> {
  return request(`${base(name)}/syncwindows`, { query: scopeQuery(scope), signal: options.signal });
}

export interface RevisionQuery extends AppScope {
  sourceIndex?: number | undefined;
  versionId?: number | undefined;
}

function revisionPath(name: string, revision: string, leaf: string): string {
  return `${base(name)}/revisions/${seg(revision)}/${leaf}`;
}

function revisionQuery(query: RevisionQuery | undefined): QueryParams {
  return { sourceIndex: query?.sourceIndex, versionId: query?.versionId, ...scopeQuery(query) };
}

/** GET /api/v1/applications/{name}/revisions/{revision}/metadata */
export function getRevisionMetadata(name: string, revision: string, query?: RevisionQuery, options: CallOptions = {}): Promise<ApplicationRevisionMetadata> {
  return request(revisionPath(name, revision, 'metadata'), { query: revisionQuery(query), signal: options.signal });
}

/** GET /api/v1/applications/{name}/revisions/{revision}/chartdetails */
export function getRevisionChartDetails(name: string, revision: string, query?: RevisionQuery, options: CallOptions = {}): Promise<ApplicationChartDetails> {
  return request(revisionPath(name, revision, 'chartdetails'), { query: revisionQuery(query), signal: options.signal });
}

/** GET /api/v1/applications/{name}/revisions/{revision}/ocimetadata */
export function getOCIMetadata(name: string, revision: string, query?: RevisionQuery, options: CallOptions = {}): Promise<ApplicationOCIMetadata> {
  return request(revisionPath(name, revision, 'ocimetadata'), { query: revisionQuery(query), signal: options.signal });
}

/** GET /api/v1/applications/{name}/manifests */
export function getManifests(
  name: string,
  query: { revision?: string; sourcePositions?: number[]; revisions?: string[]; noCache?: boolean } & AppScope = {},
  options: CallOptions = {},
): Promise<ApplicationManifestResponse> {
  return request(`${base(name)}/manifests`, {
    query: {
      revision: query.revision,
      sourcePositions: query.sourcePositions,
      revisions: query.revisions,
      noCache: query.noCache,
      ...scopeQuery(query),
    },
    signal: options.signal,
  });
}

/** GET /api/v1/applications/{name}/managed-resources — optionally narrowed to one resource. */
export function getManagedResources(
  name: string,
  query: Partial<ResourceIdentifier> & AppScope = {},
  options: CallOptions = {},
): Promise<ManagedResourcesResponse> {
  return request(`${base(name)}/managed-resources`, {
    query: {
      namespace: query.namespace,
      resourceName: query.name,
      version: query.version,
      group: query.group,
      kind: query.kind,
      ...scopeQuery(query),
    },
    signal: options.signal,
  });
}

/** POST /api/v1/applications/{name}/server-side-diff */
export function serverSideDiff(name: string, body: ApplicationServerSideDiffRequest, scope?: AppScope, options: CallOptions = {}): Promise<ServerSideDiffResponse> {
  return request(`${base(name)}/server-side-diff`, { method: 'POST', query: scopeQuery(scope), body, signal: options.signal });
}

/** GET /api/v1/applications/{name}/resource-tree */
export function getResourceTree(name: string, scope?: AppScope, options: CallOptions = {}): Promise<ApplicationTree> {
  return request(`${base(name)}/resource-tree`, { query: scopeQuery(scope), signal: options.signal });
}

/**
 * Decodes one resource-tree stream event: `{type, object}` (WatchEvent_ApplicationTree). The
 * server sends ADDED with the current tree, MODIFIED on every change and DELETED (empty tree)
 * when the Application is gone. Anything else is rejected (and skipped by openStream).
 */
export function parseTreeEvent(data: string): ApplicationTreeEvent {
  const parsed = JSON.parse(data) as Partial<ApplicationTreeEvent> | null;
  if (!parsed || typeof parsed.type !== 'string' || typeof parsed.object !== 'object' || parsed.object === null) {
    throw new Error('resource-tree event without {type, object}');
  }
  return { type: parsed.type, object: parsed.object };
}

/** GET /api/v1/stream/applications/{name}/resource-tree (SSE) */
export function watchResourceTree(
  name: string,
  scope: AppScope | undefined,
  callbacks: StreamCallbacks<ApplicationTreeEvent>,
): StreamHandle {
  return openStream({
    ...callbacks,
    path: `/api/v1/stream/applications/${seg(name)}/resource-tree`,
    query: scopeQuery(scope),
    parse: parseTreeEvent,
  });
}

/** GET /api/v1/applications/{name}/events — app events, or one resource's when given. */
export function listApplicationEvents(
  name: string,
  query: { resourceNamespace?: string; resourceName?: string; resourceUID?: string } & AppScope = {},
  options: CallOptions = {},
): Promise<EventList> {
  return request(`${base(name)}/events`, {
    query: {
      resourceNamespace: query.resourceNamespace,
      resourceName: query.resourceName,
      resourceUID: query.resourceUID,
      ...scopeQuery(query),
    },
    signal: options.signal,
  });
}

/** GET /api/v1/applications/{name}/resource — live manifest (secrets masked). */
export function getResource(
  name: string,
  resource: ResourceIdentifier,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<ApplicationResourceResponse> {
  return request(`${base(name)}/resource`, { query: { ...resourceQuery(resource), ...scopeQuery(scope) }, signal: options.signal });
}

/** POST /api/v1/applications/{name}/resource — patch a live resource. */
export function patchResource(
  name: string,
  resource: ResourceIdentifier,
  body: ApplicationResourcePatchRequest,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<ApplicationResourceResponse> {
  return request(`${base(name)}/resource`, {
    method: 'POST',
    query: { ...resourceQuery(resource), ...scopeQuery(scope) },
    body,
    signal: options.signal,
  });
}

/** DELETE /api/v1/applications/{name}/resource */
export async function deleteResource(
  name: string,
  resource: ResourceIdentifier,
  query: { force?: boolean; orphan?: boolean } & AppScope = {},
  options: CallOptions = {},
): Promise<void> {
  await request(`${base(name)}/resource`, {
    method: 'DELETE',
    query: { ...resourceQuery(resource), force: query.force, orphan: query.orphan, ...scopeQuery(query) },
    signal: options.signal,
  });
}

/** GET /api/v1/applications/{name}/resource/actions */
export function listResourceActions(
  name: string,
  resource: ResourceIdentifier,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<ResourceActionsListResponse> {
  return request(`${base(name)}/resource/actions`, { query: { ...resourceQuery(resource), ...scopeQuery(scope) }, signal: options.signal });
}

/** POST /api/v1/applications/{name}/resource/actions */
export async function runResourceAction(
  name: string,
  resource: ResourceIdentifier,
  body: ResourceActionRunRequest,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<void> {
  await request(`${base(name)}/resource/actions`, {
    method: 'POST',
    query: { ...resourceQuery(resource), ...scopeQuery(scope) },
    body,
    signal: options.signal,
  });
}

/*
 * Log routes stream LogEntry events and end with an entry whose `last` is true. The server
 * sends heartbeat comments on every stream, but the contract only promises them for
 * /api/v1/stream/*, so the heartbeat timeout is off here (a quiet container is not a dead
 * connection). A non-follow request ends normally after the last line (maxRetries: 0, no
 * reconnect). A follow stream reconnects only after a real disconnect; the request is repeated
 * as is, so lines from the tail window can be delivered again.
 */
const LOG_STREAM_DEFAULTS = { heartbeatTimeoutMs: 0 } as const;

/** GET /api/v1/applications/{name}/logs (SSE) — logs aggregated over the selected resource. */
export function streamApplicationLogs(
  name: string,
  query: ApplicationLogsQuery & AppScope,
  callbacks: StreamCallbacks<LogEntry>,
): StreamHandle {
  return openStream({
    ...LOG_STREAM_DEFAULTS,
    maxRetries: query.follow ? undefined : 0,
    ...callbacks,
    path: `${base(name)}/logs`,
    query: { ...query },
  });
}

/** GET /api/v1/applications/{name}/pods/{pod}/logs (SSE) */
export function streamPodLogs(
  name: string,
  pod: string,
  query: PodLogsQuery & AppScope,
  callbacks: StreamCallbacks<LogEntry>,
): StreamHandle {
  return openStream({
    ...LOG_STREAM_DEFAULTS,
    maxRetries: query.follow ? undefined : 0,
    ...callbacks,
    path: `${base(name)}/pods/${seg(pod)}/logs`,
    query: { ...query },
  });
}

/**
 * GET /api/v1/applications/{name}/logs?download=true — the lines as a text attachment (same
 * route, non-following); used for "download" links.
 */
export function applicationLogsURL(name: string, query: ApplicationLogsQuery & AppScope): string {
  return buildURL(`${base(name)}/logs`, { ...query, follow: false, download: true }).href;
}

/** GET /api/v1/applications/{name}/links */
export function listApplicationLinks(name: string, scope?: AppScope, options: CallOptions = {}): Promise<LinksResponse> {
  return request(`${base(name)}/links`, { query: scopeQuery(scope), signal: options.signal });
}

/** GET /api/v1/applications/{name}/resource/links */
export function listResourceLinks(
  name: string,
  resource: ResourceIdentifier,
  scope?: AppScope,
  options: CallOptions = {},
): Promise<LinksResponse> {
  return request(`${base(name)}/resource/links`, { query: { ...resourceQuery(resource), ...scopeQuery(scope) }, signal: options.signal });
}

export interface TerminalTarget {
  appName: string;
  appNamespace?: string | undefined;
  projectName: string;
  namespace: string;
  pod: string;
  container: string;
  shell?: string | undefined;
}

/** GET (WebSocket) /terminal — ws(s):// URL for the pod exec terminal. */
export function terminalURL(target: TerminalTarget): string {
  const url = resolveServerURL('/terminal');
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  for (const [key, value] of Object.entries(target)) {
    if (typeof value === 'string' && value !== '') url.searchParams.set(key, value);
  }
  return url.href;
}

/** Opens the terminal WebSocket (frames are TerminalMessage JSON). */
export function openTerminal(target: TerminalTarget): WebSocket {
  return new WebSocket(terminalURL(target));
}
