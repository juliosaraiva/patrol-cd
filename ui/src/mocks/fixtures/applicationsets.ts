import type { Application, ApplicationSet, ApplicationSetList, ApplicationSetTree, EventList } from '../../api/types';

/**
 * ApplicationSet fixtures (tests and dev mock mode). Fake data: no real repositories, clusters
 * or accounts.
 */

const repo = 'https://git.example.test/platform/apps.git';

function owner(name: string, uid: string) {
  return [{ apiVersion: 'cd.patrol.example/v1alpha1', kind: 'ApplicationSet', name, uid, controller: true, blockOwnerDeletion: true }];
}

export const guestbookAppSet: ApplicationSet = {
  apiVersion: 'cd.patrol.example/v1alpha1',
  kind: 'ApplicationSet',
  metadata: {
    name: 'guestbook',
    namespace: 'patrol',
    uid: 'uid-appset-guestbook',
    resourceVersion: '41',
    creationTimestamp: '2026-10-01T10:00:00Z',
    labels: { team: 'web' },
  },
  spec: {
    goTemplate: true,
    goTemplateOptions: ['missingkey=error'],
    generators: [
      {
        list: {
          elements: [
            { cluster: 'staging', url: 'https://staging.example.test' },
            { cluster: 'production', url: 'https://production.example.test' },
          ],
        },
      },
    ],
    strategy: {
      type: 'RollingSync',
      rollingSync: {
        steps: [
          { matchExpressions: [{ key: 'env', operator: 'In', values: ['staging'] }] },
          { matchExpressions: [{ key: 'env', operator: 'In', values: ['production'] }], maxUpdate: '50%' },
        ],
      },
    },
    syncPolicy: { applicationsSync: 'create-update', preserveResourcesOnDeletion: true },
    template: {
      metadata: { name: 'guestbook-{{.cluster}}', labels: { env: '{{.cluster}}' } },
      spec: {
        project: 'default',
        source: { repoURL: repo, targetRevision: 'main', path: 'guestbook' },
        destination: { server: '{{.url}}', namespace: 'guestbook' },
        syncPolicy: { automated: { prune: true, selfHeal: true } },
      },
    },
  },
  status: {
    conditions: [
      {
        type: 'ResourcesUpToDate',
        status: 'True',
        reason: 'ApplicationSetUpToDate',
        message: 'All applications have been generated successfully',
        lastTransitionTime: '2026-10-15T09:00:00Z',
      },
      {
        type: 'ErrorOccurred',
        status: 'False',
        reason: 'ApplicationSetUpToDate',
        message: 'Successfully generated parameters for all Applications',
        lastTransitionTime: '2026-10-15T09:00:00Z',
      },
      {
        type: 'RolloutProgressing',
        status: 'True',
        reason: 'ApplicationSetModified',
        message: 'ApplicationSet Rollout Rollout started',
        lastTransitionTime: '2026-10-15T09:01:00Z',
      },
    ],
    health: { status: 'Progressing' },
    resources: [
      { group: 'cd.patrol.example', version: 'v1alpha1', kind: 'Application', name: 'guestbook-staging', namespace: 'patrol', status: 'Synced', health: { status: 'Healthy' } },
      {
        group: 'cd.patrol.example',
        version: 'v1alpha1',
        kind: 'Application',
        name: 'guestbook-production',
        namespace: 'patrol',
        status: 'OutOfSync',
        health: { status: 'Progressing' },
      },
    ],
    resourcesCount: 2,
    applicationStatus: [
      {
        application: 'guestbook-staging',
        status: 'Healthy',
        step: '1',
        message: 'Application resource became Healthy, updating status from Progressing to Healthy.',
        lastTransitionTime: '2026-10-15T09:05:00Z',
        targetRevisions: ['4f2c1a9d3e7b'],
      },
      {
        application: 'guestbook-production',
        status: 'Progressing',
        step: '2',
        message: 'Application resource completed a sync successfully, updating status from Pending to Progressing.',
        lastTransitionTime: '2026-10-15T09:06:00Z',
        targetRevisions: ['4f2c1a9d3e7b'],
      },
    ],
  },
};

export const previewsAppSet: ApplicationSet = {
  apiVersion: 'cd.patrol.example/v1alpha1',
  kind: 'ApplicationSet',
  metadata: { name: 'shop-previews', namespace: 'patrol', uid: 'uid-appset-previews', resourceVersion: '52', creationTimestamp: '2026-10-05T08:00:00Z' },
  spec: {
    goTemplate: true,
    generators: [
      {
        pullRequest: {
          github: { owner: 'acme', repo: 'shop', labels: ['preview'] },
          filters: [{ branchMatch: '^feature/.*' }],
          requeueAfterSeconds: 300,
        },
      },
    ],
    syncPolicy: { applicationsSync: 'sync' },
    template: {
      metadata: {
        name: 'shop-pr-{{.number}}',
        labels: { 'preview.example.test/pr-number': '{{.number}}' },
        annotations: { 'preview.example.test/branch': '{{.branch}}', 'preview.example.test/head-sha': '{{.head_sha}}' },
      },
      spec: {
        project: 'previews',
        source: { repoURL: 'https://github.com/acme/shop.git', targetRevision: '{{.head_sha}}', path: 'deploy/preview' },
        destination: { server: 'https://kubernetes.default.svc', namespace: 'shop-pr-{{.number}}' },
        syncPolicy: { automated: { prune: true }, syncOptions: ['CreateNamespace=true'] },
      },
    },
  },
  status: {
    conditions: [
      {
        type: 'ResourcesUpToDate',
        status: 'True',
        reason: 'ApplicationSetUpToDate',
        message: 'All applications have been generated successfully',
        lastTransitionTime: '2026-10-14T10:00:00Z',
      },
    ],
    health: { status: 'Healthy' },
    resources: [
      { group: 'cd.patrol.example', kind: 'Application', name: 'shop-pr-42', namespace: 'patrol', status: 'Synced', health: { status: 'Healthy' } },
      { group: 'cd.patrol.example', kind: 'Application', name: 'shop-pr-57', namespace: 'patrol', status: 'OutOfSync', health: { status: 'Degraded' } },
    ],
  },
};

export const brokenAppSet: ApplicationSet = {
  apiVersion: 'cd.patrol.example/v1alpha1',
  kind: 'ApplicationSet',
  metadata: { name: 'clusters-addons', namespace: 'team-infra', uid: 'uid-appset-addons', resourceVersion: '60', creationTimestamp: '2026-10-10T08:00:00Z' },
  spec: {
    generators: [
      {
        matrix: {
          generators: [
            { git: { repoURL: repo, revision: 'main', directories: [{ path: 'addons/*' }, { path: 'addons/legacy', exclude: true }] } },
            { clusters: { selector: { matchLabels: { tier: 'platform' } } } },
          ],
        },
      },
    ],
    template: {
      metadata: { name: '{{path.basename}}-{{name}}' },
      spec: {
        project: 'infra',
        source: { repoURL: repo, targetRevision: 'main', path: '{{path}}' },
        destination: { server: '{{server}}', namespace: '{{path.basename}}' },
      },
    },
  },
  status: {
    conditions: [
      {
        type: 'ErrorOccurred',
        status: 'True',
        reason: 'ApplicationGenerationFromParamsError',
        message: 'failed to get params for second generator in the matrix generator: cluster selector matched no clusters',
        lastTransitionTime: '2026-10-15T07:00:00Z',
      },
      {
        type: 'ParametersGenerated',
        status: 'False',
        reason: 'ErrorOccurred',
        message: 'cluster selector matched no clusters',
        lastTransitionTime: '2026-10-15T07:00:00Z',
      },
    ],
    health: { status: 'Degraded', message: 'failed to generate parameters' },
  },
};

export const applicationSetsFixture: ApplicationSetList = {
  items: [guestbookAppSet, previewsAppSet, brokenAppSet],
  metadata: { resourceVersion: '200' },
};

/** Applications generated by the fixtures above (ownerReferences set). */
export function generatedApplication(
  name: string,
  set: ApplicationSet,
  overrides: { health?: string; sync?: string; labels?: Record<string, string>; annotations?: Record<string, string>; revision?: string; project?: string } = {},
): Application {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Application',
    metadata: {
      name,
      namespace: set.metadata.namespace ?? 'patrol',
      uid: `uid-${name}`,
      resourceVersion: '5',
      creationTimestamp: '2026-10-12T12:00:00Z',
      labels: overrides.labels,
      annotations: overrides.annotations,
      ownerReferences: owner(set.metadata.name ?? '', set.metadata.uid ?? ''),
    },
    spec: {
      project: overrides.project ?? set.spec.template.spec.project,
      source: { repoURL: set.spec.template.spec.source?.repoURL ?? repo, path: set.spec.template.spec.source?.path, targetRevision: overrides.revision ?? 'main' },
      destination: { server: 'https://kubernetes.default.svc', namespace: name },
    },
    status: { health: { status: overrides.health ?? 'Healthy' }, sync: { status: overrides.sync ?? 'Synced', revision: '4f2c1a9d3e7b' } },
  };
}

export const generatedApplications: Application[] = [
  generatedApplication('guestbook-staging', guestbookAppSet, { labels: { env: 'staging' } }),
  generatedApplication('guestbook-production', guestbookAppSet, { labels: { env: 'production' }, health: 'Progressing', sync: 'OutOfSync' }),
  generatedApplication('shop-pr-42', previewsAppSet, {
    labels: { 'preview.example.test/pr-number': '42' },
    annotations: { 'preview.example.test/branch': 'feature/cart', 'preview.example.test/head-sha': '9f8e7d6c5b4a39281706f5e4d3c2b1a098765432' },
    revision: '9f8e7d6c5b4a39281706f5e4d3c2b1a098765432',
  }),
  generatedApplication('shop-pr-57', previewsAppSet, {
    labels: { 'preview.example.test/pr-number': '57' },
    annotations: { 'preview.example.test/branch': 'feature/search', 'preview.example.test/head-sha': '0123456789abcdef0123456789abcdef01234567' },
    revision: '0123456789abcdef0123456789abcdef01234567',
    health: 'Degraded',
    sync: 'OutOfSync',
  }),
];

/** ApplicationSetTree: one node per generated Application (from status.resources), with its sync status in `info`. */
export const guestbookAppSetTree: ApplicationSetTree = {
  nodes: [
    {
      group: 'cd.patrol.example',
      version: 'v1alpha1',
      kind: 'Application',
      namespace: 'patrol',
      name: 'guestbook-staging',
      parentRefs: [{ group: 'cd.patrol.example', version: 'v1alpha1', kind: 'ApplicationSet', namespace: 'patrol', name: 'guestbook', uid: 'uid-appset-guestbook' }],
      info: [{ name: 'Sync Status', value: 'Synced' }],
      health: { status: 'Healthy' },
    },
    {
      group: 'cd.patrol.example',
      version: 'v1alpha1',
      kind: 'Application',
      namespace: 'patrol',
      name: 'guestbook-production',
      parentRefs: [{ group: 'cd.patrol.example', version: 'v1alpha1', kind: 'ApplicationSet', namespace: 'patrol', name: 'guestbook', uid: 'uid-appset-guestbook' }],
      info: [{ name: 'Sync Status', value: 'OutOfSync' }],
      health: { status: 'Progressing' },
    },
  ],
};

export const guestbookAppSetEvents: EventList = {
  items: [
    {
      metadata: { name: 'guestbook.1', namespace: 'patrol', uid: 'ev-1' },
      involvedObject: { kind: 'ApplicationSet', name: 'guestbook', namespace: 'patrol' },
      reason: 'ApplicationSetUpdated',
      message: 'Updated Application "guestbook-production"',
      type: 'Normal',
      count: 3,
      firstTimestamp: '2026-10-15T08:00:00Z',
      lastTimestamp: '2026-10-15T09:06:00Z',
      reportingComponent: 'applicationset-controller',
      reportingInstance: '',
    },
    {
      metadata: { name: 'guestbook.2', namespace: 'patrol', uid: 'ev-2' },
      involvedObject: { kind: 'ApplicationSet', name: 'guestbook', namespace: 'patrol' },
      reason: 'RolloutStepPending',
      message: 'Waiting for step 1 applications to become Healthy',
      type: 'Warning',
      count: 1,
      lastTimestamp: '2026-10-15T09:01:00Z',
      reportingComponent: 'applicationset-controller',
      reportingInstance: '',
    },
  ],
};
