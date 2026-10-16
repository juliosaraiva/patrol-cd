/**
 * Stateful mock of the application routes used by the apps-core pages, so dev mock mode can
 * sync, roll back, terminate, edit and delete. Registered before the generic read-only
 * handlers. Tests reset it with resetAppsCoreMockState() (state is per test file).
 */
import { http, HttpResponse } from 'msw';
import type {
  Application,
  ApplicationInput,
  ApplicationManifestResponse,
  ApplicationOCIMetadata,
  ApplicationOperationResponse,
  ApplicationServerSideDiffRequest,
  ApplicationPatchRequest,
  ApplicationRollbackRequest,
  ApplicationSyncRequest,
  ApplicationSyncWindowsResponse,
  ApplicationChartDetails,
  ApplicationRevisionMetadata,
  ApplicationTree,
  EventList,
  LinksResponse,
  ManagedResourcesResponse,
  Operation,
  PluginSettings,
  RepositoryAppDetails,
  ResourceDiff,
  ResourceNode,
  ServerSideDiffResponse,
} from '../../api/types';
import { appsCoreApplications, appsCoreOperations, operationList } from '../fixtures/apps-core';
import { emptyObject, problem } from './util';

const CONTROL_PLANE = 'patrol';

interface Store {
  apps: Application[];
  operations: Record<string, Operation[]>;
  counter: number;
  resourceVersion: number;
}

function fresh(): Store {
  return { apps: appsCoreApplications().items, operations: appsCoreOperations(), counter: 10, resourceVersion: 100 };
}

let store = fresh();

export function resetAppsCoreMockState(): void {
  store = fresh();
}

/** Direct access for tests that need to inspect or seed the mock state. */
export function appsCoreMockStore(): Store {
  return store;
}

function clone<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T;
}

function find(name: string, url: URL): Application | undefined {
  const ns = url.searchParams.get('appNamespace') ?? CONTROL_PLANE;
  return store.apps.find((a) => a.metadata.name === name && (a.metadata.namespace ?? CONTROL_PLANE) === (ns || CONTROL_PLANE));
}

function notFound(name: string) {
  return problem(404, 'not_found', `application "${name}" not found`);
}

function bump(app: Application): Application {
  store.resourceVersion += 1;
  app.metadata.resourceVersion = String(store.resourceVersion);
  return app;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

/** RFC 7386 JSON merge patch. */
function mergePatch(target: unknown, patch: unknown): unknown {
  if (!isRecord(patch)) return patch;
  const out: Record<string, unknown> = isRecord(target) ? { ...target } : {};
  for (const [k, v] of Object.entries(patch)) {
    if (v === null) delete out[k];
    else out[k] = mergePatch(out[k], v);
  }
  return out;
}

function newOperation(app: Application, spec: Partial<Operation['spec']>): Operation {
  store.counter += 1;
  const name = `${app.metadata.name ?? 'app'}-op-${store.counter}`;
  const now = new Date().toISOString();
  const op: Operation = {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Operation',
    metadata: { name, namespace: app.metadata.namespace ?? CONTROL_PLANE, creationTimestamp: now, resourceVersion: '1' },
    spec: { application: app.metadata.name ?? '', initiatedBy: { username: 'mock-admin' }, ...spec },
    status: { phase: 'Running', message: 'syncing', startedAt: now, progress: { syncPhase: 'Sync', wave: 0, completedTasks: 0, totalTasks: 3 } },
  };
  const key = app.metadata.name ?? '';
  store.operations[key] = [op, ...(store.operations[key] ?? [])];
  app.status = { ...app.status, operationState: { operationName: name, phase: 'Running', message: 'syncing', startedAt: now, initiatedBy: { username: 'mock-admin' } } };
  bump(app);
  return op;
}

/** Operation spec.sync for a sync request (the request is flat; the Operation nests it). */
function syncOperationOf(body: ApplicationSyncRequest): NonNullable<Operation['spec']['sync']> {
  const sync: NonNullable<Operation['spec']['sync']> = {};
  if (body.revision) sync.revision = body.revision;
  if (body.revisions) sync.revisions = body.revisions;
  if (body.prune) sync.prune = body.prune;
  if (body.dryRun) sync.dryRun = body.dryRun;
  if (body.strategy) sync.syncStrategy = body.strategy;
  if (body.resources) sync.resources = body.resources;
  if (body.manifests) sync.manifests = body.manifests;
  if (body.source) sync.source = body.source;
  if (body.sources) sync.sources = body.sources;
  // syncOptions, when present, replace the Application's.
  if (body.syncOptions) sync.syncOptions = body.syncOptions.items ?? [];
  return sync;
}

function operationResponse(app: Application, op: Operation | undefined): ApplicationOperationResponse {
  return op ? { application: clone(app), operation: clone(op) } : { application: clone(app) };
}

export const appsCoreHandlers = [
  http.get('*/api/v1/applications', () => HttpResponse.json({ items: clone(store.apps), metadata: { resourceVersion: '100' } })),
  http.get('*/api/v1/applications/:name', ({ params, request }) => {
    const url = new URL(request.url);
    const app = find(String(params.name), url);
    if (app) return HttpResponse.json(clone(app));
    // Like the generic read-only stub: an application of that name in another namespace is
    // served as if it lived in the requested one (keeps namespace-agnostic fixtures usable).
    const ns = url.searchParams.get('appNamespace');
    const byName = store.apps.find((a) => a.metadata.name === params.name);
    if (byName && ns) return HttpResponse.json({ ...clone(byName), metadata: { ...clone(byName.metadata), namespace: ns } });
    return notFound(String(params.name));
  }),
  http.put('*/api/v1/applications/:name', async ({ params, request }) => {
    const url = new URL(request.url);
    const app = find(String(params.name), url);
    if (!app) return notFound(String(params.name));
    const body = (await request.json()) as ApplicationInput;
    if (body.metadata?.resourceVersion && body.metadata.resourceVersion !== app.metadata.resourceVersion) {
      return problem(409, 'conflict', `Operation cannot be fulfilled on applications "${String(params.name)}": the object has been modified`);
    }
    const next = bump({ ...clone(body), metadata: clone(body.metadata ?? app.metadata), status: app.status });
    store.apps = store.apps.map((a) => (a === app ? next : a));
    return HttpResponse.json(clone(next));
  }),
  http.put('*/api/v1/applications/:name/spec', async ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    app.spec = (await request.json()) as Application['spec'];
    bump(app);
    return HttpResponse.json(clone(app.spec));
  }),
  http.patch('*/api/v1/applications/:name', async ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    const body = (await request.json()) as ApplicationPatchRequest;
    // patchType defaults to "json" (RFC 6902) on the server; the mock implements merge only.
    if (body.patchType !== 'merge') return problem(400, 'invalid_argument', 'only merge patches are supported by the mock');
    const patched = mergePatch(clone(app), JSON.parse(body.patch)) as Application;
    Object.assign(app, patched);
    bump(app);
    return HttpResponse.json(clone(app));
  }),
  http.delete('*/api/v1/applications/:name', ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    store.apps = store.apps.filter((a) => a !== app);
    return HttpResponse.json(emptyObject);
  }),
  http.post('*/api/v1/applications/:name/sync', async ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    const body = (await request.json()) as ApplicationSyncRequest;
    const op = newOperation(app, {
      sync: syncOperationOf(body),
      ...(body.retryStrategy ? { retry: body.retryStrategy } : {}),
      ...(body.infos ? { info: body.infos } : {}),
    });
    return HttpResponse.json(operationResponse(app, op));
  }),
  http.post('*/api/v1/applications/:name/rollback', async ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    const body = (await request.json()) as ApplicationRollbackRequest;
    const automated = app.spec.syncPolicy?.automated;
    if (automated && automated.enabled !== false) return problem(412, 'failed_precondition', 'rollback cannot be initiated when auto-sync is enabled');
    if (!(app.status?.history ?? []).some((h) => h.id === body.id)) {
      return problem(400, 'invalid_argument', `application ${CONTROL_PLANE}/${String(params.name)} does not have deployment with id ${body.id}`);
    }
    const op = newOperation(app, { rollback: { id: body.id }, sync: { ...(body.dryRun ? { dryRun: true } : {}), ...(body.prune ? { prune: true } : {}) } });
    return HttpResponse.json(operationResponse(app, op));
  }),
  http.delete('*/api/v1/applications/:name/operation', ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    const state = app.status?.operationState;
    if (!state || (state.phase !== 'Running' && state.phase !== 'Pending')) return problem(400, 'invalid_argument', 'no operation is in progress');
    state.phase = 'Terminating';
    const op = (store.operations[String(params.name)] ?? []).find((o) => o.metadata.name === state.operationName);
    if (op) {
      op.spec.cancel = true;
      if (op.status) op.status.phase = 'Terminating';
    }
    bump(app);
    return HttpResponse.json(operationResponse(app, op));
  }),
  // Newest first; `limit` (default 100, at most 1000) and an opaque `continue` token page through
  // older Operations, as the server does.
  http.get('*/api/v1/applications/:name/operations', ({ params, request }) => {
    const url = new URL(request.url);
    const app = find(String(params.name), url);
    if (!app) return notFound(String(params.name));
    const all = store.operations[String(params.name)] ?? [];
    const limit = Math.min(Number(url.searchParams.get('limit')) || 100, 1000);
    const offset = Number(url.searchParams.get('continue')?.replace(/^offset-/, '') ?? 0) || 0;
    const page = operationList(clone(all.slice(offset, offset + limit)));
    if (offset + limit < all.length) page.metadata = { ...page.metadata, continue: `offset-${offset + limit}` };
    return HttpResponse.json(page);
  }),
  http.get('*/api/v1/applications/:name/operations/:op', ({ params }) => {
    const op = (store.operations[String(params.name)] ?? []).find((o) => o.metadata.name === params.op);
    return op ? HttpResponse.json(clone(op)) : problem(404, 'not_found', `operation "${String(params.op)}" not found`);
  }),
  http.get('*/api/v1/applications/:name/syncwindows', () => HttpResponse.json({ activeWindows: [], assignedWindows: [], canSync: true } satisfies ApplicationSyncWindowsResponse)),
  http.get('*/api/v1/applications/:name/revisions/:revision/metadata', ({ params }) =>
    HttpResponse.json({
      author: 'Dana Developer <dana@example.test>',
      date: '2026-10-15T07:50:00Z',
      message: `Update guestbook to ${String(params.revision).slice(0, 7)}\n\nBumps the UI image.`,
      tags: [],
    } satisfies ApplicationRevisionMetadata),
  ),
  http.get('*/api/v1/applications/:name/revisions/:revision/ocimetadata', ({ params }) =>
    HttpResponse.json({
      version: String(params.revision),
      createdAt: '2026-10-14T12:00:00Z',
      authors: 'Platform Team',
      description: 'Example OCI artifact',
      sourceUrl: 'https://git.example.test/platform/apps',
    } satisfies ApplicationOCIMetadata),
  ),
  http.get('*/api/v1/applications/:name/manifests', ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    const manifests = (app.status?.resources ?? []).map((r) =>
      JSON.stringify({ apiVersion: r.group ? `${r.group}/${r.version ?? 'v1'}` : (r.version ?? 'v1'), kind: r.kind, metadata: { name: r.name, namespace: r.namespace } }),
    );
    const body: ApplicationManifestResponse = { manifests, namespace: app.spec.destination.namespace ?? '', server: app.spec.destination.server ?? '', revision: app.status?.sync?.revision ?? '' };
    if (app.status?.sourceType) body.sourceType = app.status.sourceType;
    return HttpResponse.json(body);
  }),
  http.post('*/api/v1/applications/:name/server-side-diff', async ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    const body = (await request.json()) as ApplicationServerSideDiffRequest;
    const items = (body.liveResources ?? []).map((r) => ({ ...r, predictedLiveState: r.liveState ?? '', modified: false }));
    return HttpResponse.json({ items, modified: false } satisfies ServerSideDiffResponse);
  }),
  http.get('*/api/v1/applications/:name/revisions/:revision/chartdetails', () =>
    HttpResponse.json({ description: 'Payments API chart', home: 'https://charts.example.test/payments', maintainers: ['payments-team'] } satisfies ApplicationChartDetails),
  ),
  http.get('*/api/v1/applications/:name/links', ({ params }) =>
    HttpResponse.json({ items: params.name === 'guestbook' ? [{ title: 'Runbook', url: 'https://runbooks.example.test/guestbook' }] : [] } satisfies LinksResponse),
  ),
  http.get('*/api/v1/applications/:name/resource-tree', ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    const nodes = (app.status?.resources ?? []).map((r): ResourceNode => ({
      group: r.group,
      version: r.version,
      kind: r.kind,
      namespace: r.namespace,
      name: r.name,
      uid: `uid-${r.kind ?? ''}-${r.name ?? ''}`,
      health: r.health,
    }));
    return HttpResponse.json({ nodes, orphanedNodes: [], hosts: [] } satisfies ApplicationTree);
  }),
  http.get('*/api/v1/applications/:name/managed-resources', ({ params, request }) => {
    const app = find(String(params.name), new URL(request.url));
    if (!app) return notFound(String(params.name));
    const items = (app.status?.resources ?? []).map((r): ResourceDiff => {
      const live = { apiVersion: r.group ? `${r.group}/${r.version ?? 'v1'}` : (r.version ?? 'v1'), kind: r.kind, metadata: { name: r.name, namespace: r.namespace }, spec: { replicas: 1 } };
      const desired = { ...live, spec: { replicas: r.status === 'OutOfSync' ? 3 : 1 } };
      return {
        group: r.group,
        kind: r.kind,
        namespace: r.namespace,
        name: r.name,
        liveState: JSON.stringify(live),
        normalizedLiveState: JSON.stringify(live),
        targetState: r.requiresPruning ? 'null' : JSON.stringify(desired),
        predictedLiveState: r.requiresPruning ? 'null' : JSON.stringify(desired),
        modified: r.status === 'OutOfSync',
        hook: r.hook ?? false,
      };
    });
    return HttpResponse.json({ items } satisfies ManagedResourcesResponse);
  }),
  http.get('*/api/v1/applications/:name/events', () => HttpResponse.json({ items: [] } satisfies EventList)),
  http.get('*/api/v1/settings/plugins', () => HttpResponse.json({ plugins: [{ name: 'cue' }, { name: 'tanka' }] } satisfies PluginSettings)),
  http.post('*/api/v1/repositories/:repo/appdetails', async ({ request }) => {
    const body = (await request.json()) as { source: { chart?: string; helm?: unknown; plugin?: unknown; kustomize?: unknown } };
    if (body.source.chart || body.source.helm) {
      return HttpResponse.json({
        type: 'Helm',
        helm: {
          valueFiles: ['values.yaml', 'values-prod.yaml'],
          parameters: [
            { name: 'image.tag', value: '1.4.2' },
            { name: 'replicaCount', value: '1' },
            { name: 'service.port', value: '8080' },
          ],
        },
      } satisfies RepositoryAppDetails);
    }
    if (body.source.plugin) {
      return HttpResponse.json({
        type: 'Plugin',
        plugin: {
          parametersAnnouncement: [
            { name: 'environment', title: 'Environment', tooltip: 'Target environment', required: true, string: 'staging' },
            { name: 'regions', title: 'Regions', collectionType: 'array', array: ['eu-west-1'] },
          ],
        },
      } satisfies RepositoryAppDetails);
    }
    return HttpResponse.json({ type: 'Kustomize', kustomize: { images: ['registry.example.test/search:1.9.0'] } } satisfies RepositoryAppDetails);
  }),
];
