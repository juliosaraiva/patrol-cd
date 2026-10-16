/**
 * Application fixtures for the apps-core pages (list, details, sync, history, operations,
 * parameters). Fake data only. The three applications keep the names, namespace and resource
 * versions of the shared `applicationsFixture` (src/mocks/fixtures/index.ts) so tests written
 * against it keep passing, but carry realistic sources, history, operations and conditions.
 */
import type { Application, ApplicationList, Operation, OperationList } from '../../api/types';

const NS = 'patrol';
const REPO = 'https://git.example.test/platform/apps.git';
export const GUESTBOOK_SHA = '4f2c1a9d3e7b5c6a8f9e0d1c2b3a4f5e6d7c8b9a';
export const GUESTBOOK_PREVIOUS_SHA = '9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b';
export const VALUES_SHA = '1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c';
export const SEARCH_SHA = 'c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00';

function meta(name: string, extra: Partial<Application['metadata']> = {}): Application['metadata'] {
  return { name, namespace: NS, resourceVersion: '1', creationTimestamp: '2026-10-01T12:00:00Z', uid: `uid-${name}`, ...extra };
}

export function guestbookApp(): Application {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Application',
    metadata: meta('guestbook', {
      labels: { team: 'web', env: 'prod' },
      annotations: { 'link.cd.patrol.example/dashboard': 'Dashboard|https://grafana.example.test/d/guestbook' },
      finalizers: ['resources-finalizer.cd.patrol.example'],
    }),
    spec: {
      project: 'default',
      source: { repoURL: 'https://github.com/example/guestbook.git', path: 'guestbook', targetRevision: 'main', directory: { recurse: true } },
      destination: { server: 'https://kubernetes.default.svc', namespace: 'guestbook' },
      syncPolicy: { automated: { enabled: true, prune: true, selfHeal: true }, syncOptions: ['CreateNamespace=true'] },
      revisionHistoryLimit: 10,
    },
    status: {
      health: { status: 'Healthy', lastTransitionTime: '2026-10-15T08:00:00Z' },
      sync: { status: 'Synced', revision: GUESTBOOK_SHA },
      sourceType: 'Directory',
      reconciledAt: '2026-10-16T09:00:00Z',
      summary: { images: ['registry.example.test/guestbook:v2'], externalURLs: ['https://guestbook.example.test'] },
      resources: [
        { version: 'v1', kind: 'Service', namespace: 'guestbook', name: 'guestbook-ui', status: 'Synced', health: { status: 'Healthy' } },
        { group: 'apps', version: 'v1', kind: 'Deployment', namespace: 'guestbook', name: 'guestbook-ui', status: 'Synced', health: { status: 'Healthy' } },
      ],
      operationState: {
        operationName: 'guestbook-sync-3',
        phase: 'Succeeded',
        message: 'successfully synced (all tasks run)',
        startedAt: '2026-10-15T07:58:00Z',
        finishedAt: '2026-10-15T07:59:30Z',
        initiatedBy: { username: 'mock-admin' },
        syncResult: { revision: GUESTBOOK_SHA },
      },
      history: [
        {
          id: 1,
          revision: GUESTBOOK_PREVIOUS_SHA,
          source: { repoURL: 'https://github.com/example/guestbook.git', path: 'guestbook', targetRevision: 'main' },
          deployStartedAt: '2026-10-10T10:00:00Z',
          deployedAt: '2026-10-10T10:01:00Z',
          initiatedBy: { automated: true },
          operationName: 'guestbook-sync-2',
        },
        {
          id: 2,
          revision: GUESTBOOK_SHA,
          source: { repoURL: 'https://github.com/example/guestbook.git', path: 'guestbook', targetRevision: 'main' },
          deployStartedAt: '2026-10-15T07:58:00Z',
          deployedAt: '2026-10-15T07:59:30Z',
          initiatedBy: { username: 'mock-admin' },
          operationName: 'guestbook-sync-3',
        },
      ],
    },
  };
}

export function paymentsApp(): Application {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Application',
    metadata: meta('payments', {
      labels: { team: 'finance', env: 'prod' },
      annotations: {
        'notice.cd.patrol.example/content': 'Payments freeze until Friday: only hotfixes.',
        'notice.cd.patrol.example/severity': 'warning',
        'notice.cd.patrol.example/url': 'https://status.example.test/freeze',
      },
    }),
    spec: {
      project: 'finance',
      sources: [
        {
          repoURL: 'https://charts.example.test',
          chart: 'payments',
          targetRevision: '1.4.2',
          helm: { valueFiles: ['$values/payments/values-prod.yaml'], parameters: [{ name: 'replicaCount', value: '3' }] },
        },
        { repoURL: REPO, targetRevision: 'main', ref: 'values' },
      ],
      destination: { name: 'prod-eu', namespace: 'payments' },
    },
    status: {
      health: { status: 'Degraded', lastTransitionTime: '2026-10-16T08:30:00Z' },
      sync: { status: 'OutOfSync', revisions: ['1.4.2', VALUES_SHA] },
      sourceTypes: ['Helm', 'Directory'],
      conditions: [
        {
          type: 'ComparisonError',
          status: 'True',
          reason: 'ManifestGenerationFailed',
          message: 'failed to render chart payments: values file $values/payments/values-prod.yaml not found',
          lastTransitionTime: '2026-10-16T08:30:00Z',
        },
        {
          type: 'SharedResourceWarning',
          status: 'True',
          reason: 'SharedResource',
          message: 'ConfigMap payments/shared-config is part of applications payments and billing',
          lastTransitionTime: '2026-10-16T08:00:00Z',
        },
      ],
      resources: [
        { group: 'apps', version: 'v1', kind: 'Deployment', namespace: 'payments', name: 'payments-api', status: 'OutOfSync', health: { status: 'Degraded', message: 'Deployment exceeded its progress deadline' } },
        { version: 'v1', kind: 'ConfigMap', namespace: 'payments', name: 'payments-legacy', status: 'OutOfSync', requiresPruning: true },
        { version: 'v1', kind: 'Service', namespace: 'payments', name: 'payments-api', status: 'Synced', health: { status: 'Healthy' } },
        { group: 'batch', version: 'v1', kind: 'Job', namespace: 'payments', name: 'payments-migrate', status: 'Synced', hook: true },
      ],
      operationState: {
        operationName: 'payments-sync-5',
        phase: 'Failed',
        message: 'one or more objects failed to apply',
        startedAt: '2026-10-16T08:20:00Z',
        finishedAt: '2026-10-16T08:21:00Z',
        initiatedBy: { username: 'mock-admin' },
      },
      history: [
        {
          id: 4,
          revisions: ['1.4.1', GUESTBOOK_PREVIOUS_SHA],
          sources: [
            { repoURL: 'https://charts.example.test', chart: 'payments', targetRevision: '1.4.1' },
            { repoURL: REPO, targetRevision: 'main', ref: 'values' },
          ],
          deployStartedAt: '2026-10-12T09:00:00Z',
          deployedAt: '2026-10-12T09:02:00Z',
          initiatedBy: { username: 'mock-admin' },
        },
      ],
    },
  };
}

export function searchApp(): Application {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Application',
    metadata: meta('search'),
    spec: {
      project: 'default',
      source: {
        repoURL: REPO,
        path: 'search/overlays/prod',
        targetRevision: 'main',
        kustomize: { images: ['search=registry.example.test/search:1.9.0'], namePrefix: 'prod-' },
      },
      destination: { server: 'https://kubernetes.default.svc', namespace: 'search' },
      syncPolicy: { retry: { limit: 3, backoff: { duration: '10s', factor: 2, maxDuration: '5m' } } },
    },
    status: {
      health: { status: 'Progressing' },
      sync: { status: 'Synced', revision: SEARCH_SHA },
      sourceType: 'Kustomize',
      resources: [{ group: 'apps', version: 'v1', kind: 'StatefulSet', namespace: 'search', name: 'prod-search', status: 'Synced', health: { status: 'Progressing' } }],
      operationState: {
        operationName: 'search-sync-7',
        phase: 'Running',
        message: 'waiting for healthy state of apps/StatefulSet/prod-search',
        startedAt: '2026-10-16T09:10:00Z',
        initiatedBy: { username: 'mock-admin' },
      },
    },
  };
}

export function appsCoreApplications(): ApplicationList {
  return { items: [guestbookApp(), paymentsApp(), searchApp()], metadata: { resourceVersion: '100' } };
}

function operation(app: string, name: string, extra: Partial<Operation['spec']>, status: Operation['status']): Operation {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Operation',
    metadata: { name, namespace: NS, creationTimestamp: status?.startedAt ?? '2026-10-01T00:00:00Z', resourceVersion: '1' },
    spec: { application: app, initiatedBy: { username: 'mock-admin' }, sync: { revision: 'main' }, ...extra },
    status,
  };
}

export function appsCoreOperations(): Record<string, Operation[]> {
  return {
    guestbook: [
      operation(
        'guestbook',
        'guestbook-sync-3',
        {},
        {
          phase: 'Succeeded',
          message: 'successfully synced (all tasks run)',
          startedAt: '2026-10-15T07:58:00Z',
          finishedAt: '2026-10-15T07:59:30Z',
          progress: { syncPhase: 'PostSync', wave: 0, completedTasks: 3, totalTasks: 3 },
          plan: { resolvedRevisions: [GUESTBOOK_SHA] },
          syncResult: {
            revision: GUESTBOOK_SHA,
            resources: [
              { group: '', version: 'v1', kind: 'Service', namespace: 'guestbook', name: 'guestbook-ui', status: 'Synced', syncPhase: 'Sync', message: 'service/guestbook-ui configured' },
              { group: 'apps', version: 'v1', kind: 'Deployment', namespace: 'guestbook', name: 'guestbook-ui', status: 'Synced', syncPhase: 'Sync', message: 'deployment.apps/guestbook-ui configured' },
              { group: 'batch', version: 'v1', kind: 'Job', namespace: 'guestbook', name: 'smoke-test', hookType: 'PostSync', hookPhase: 'Succeeded', syncPhase: 'PostSync', message: 'job completed' },
            ],
          },
        },
      ),
      operation(
        'guestbook',
        'guestbook-sync-2',
        { initiatedBy: { automated: true } },
        {
          phase: 'Succeeded',
          message: 'successfully synced (all tasks run)',
          startedAt: '2026-10-10T10:00:00Z',
          finishedAt: '2026-10-10T10:01:00Z',
          syncResult: { revision: GUESTBOOK_PREVIOUS_SHA, resources: [] },
        },
      ),
    ],
    payments: [
      operation(
        'payments',
        'payments-sync-5',
        {},
        {
          phase: 'Failed',
          message: 'one or more objects failed to apply',
          startedAt: '2026-10-16T08:20:00Z',
          finishedAt: '2026-10-16T08:21:00Z',
          plan: { resolvedRevisions: ['1.4.2', VALUES_SHA], warnings: ['ConfigMap payments/shared-config is managed by another application'] },
          syncResult: {
            revisions: ['1.4.2', VALUES_SHA],
            resources: [
              {
                group: 'apps',
                version: 'v1',
                kind: 'Deployment',
                namespace: 'payments',
                name: 'payments-api',
                status: 'SyncFailed',
                syncPhase: 'Sync',
                message: 'admission webhook denied the request: missing resource limits',
              },
              { group: '', version: 'v1', kind: 'ConfigMap', namespace: 'payments', name: 'payments-legacy', status: 'PruneSkipped', syncPhase: 'Sync', message: 'ignored (requires pruning)' },
            ],
          },
        },
      ),
    ],
    search: [
      operation(
        'search',
        'search-sync-7',
        {},
        {
          phase: 'Running',
          message: 'waiting for healthy state of apps/StatefulSet/prod-search',
          startedAt: '2026-10-16T09:10:00Z',
          progress: { syncPhase: 'Sync', wave: 1, completedTasks: 2, totalTasks: 5, startedAt: '2026-10-16T09:10:30Z' },
          syncResult: {
            revision: SEARCH_SHA,
            resources: [{ group: 'apps', version: 'v1', kind: 'StatefulSet', namespace: 'search', name: 'prod-search', status: 'Synced', syncPhase: 'Sync', message: 'statefulset.apps/prod-search configured' }],
          },
        },
      ),
    ],
  };
}

export function operationList(items: readonly Operation[]): OperationList {
  return { items: [...items], metadata: { resourceVersion: '1' } };
}
