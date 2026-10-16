import type {
  Application,
  ApplicationTree,
  AppProject,
  EventList,
  LinksResponse,
  LogEntry,
  ManagedResourcesResponse,
  ResourceActionsListResponse,
  ResourceDiff,
  ResourceNode,
} from '../../api/types';

/*
 * Fake data for the application resource views (workstream apps-resources): one application
 * ("shop") with a Deployment → ReplicaSet → Pods chain, a StatefulSet, a hook Job, networking
 * (Ingress → Service → Pods), a resource that requires pruning, a missing PVC and an orphaned
 * ConfigMap. Not real infrastructure.
 */

export const RESOURCES_APP = 'shop';

export function resourcesAppFixture(overrides: { name?: string; namespace?: string; annotations?: Record<string, string> } = {}): Application {
  const name = overrides.name ?? RESOURCES_APP;
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Application',
    metadata: {
      name,
      namespace: overrides.namespace ?? 'patrol',
      uid: `uid-app-${name}`,
      resourceVersion: '10',
      creationTimestamp: '2026-10-01T12:00:00Z',
      annotations: overrides.annotations,
    },
    spec: {
      project: 'default',
      source: { repoURL: 'https://git.example.test/platform/shop.git', path: 'deploy', targetRevision: 'main' },
      destination: { server: 'https://kubernetes.default.svc', namespace: 'shop' },
    },
    status: {
      health: { status: 'Degraded' },
      sync: { status: 'OutOfSync', revision: '4f2c1a9d3e7b' },
      resources: [
        { group: 'apps', version: 'v1', kind: 'Deployment', namespace: 'shop', name: 'web', status: 'OutOfSync', health: { status: 'Degraded', message: 'Deployment "web" exceeded its progress deadline' }, syncWave: 1 },
        { group: '', version: 'v1', kind: 'Service', namespace: 'shop', name: 'web', status: 'Synced', health: { status: 'Healthy' } },
        { group: 'networking.k8s.io', version: 'v1', kind: 'Ingress', namespace: 'shop', name: 'web', status: 'Synced', health: { status: 'Healthy' } },
        { group: '', version: 'v1', kind: 'ConfigMap', namespace: 'shop', name: 'web-config', status: 'Synced' },
        { group: '', version: 'v1', kind: 'Secret', namespace: 'shop', name: 'web-secret', status: 'Synced' },
        { group: 'apps', version: 'v1', kind: 'StatefulSet', namespace: 'shop', name: 'cache', status: 'Synced', health: { status: 'Healthy' } },
        { group: 'batch', version: 'v1', kind: 'Job', namespace: 'shop', name: 'db-migrate', status: 'Synced', hook: true, health: { status: 'Healthy' }, syncWave: -1 },
        { group: '', version: 'v1', kind: 'ConfigMap', namespace: 'shop', name: 'legacy', status: 'OutOfSync', requiresPruning: true },
        { group: '', version: 'v1', kind: 'PersistentVolumeClaim', namespace: 'shop', name: 'data', status: 'OutOfSync', health: { status: 'Missing' } },
      ],
    },
  };
}

function pod(name: string, parentUid: string, parentKind: string, parentName: string, opts: { health?: string; node?: string; restarts?: number; ready?: string; labels?: Record<string, string>; group?: string } = {}): ResourceNode {
  return {
    group: '',
    version: 'v1',
    kind: 'Pod',
    namespace: 'shop',
    name,
    uid: `uid-pod-${name}`,
    parentRefs: [{ group: opts.group ?? 'apps', kind: parentKind, namespace: 'shop', name: parentName, uid: parentUid }],
    health: { status: opts.health ?? 'Healthy', message: opts.health === 'Degraded' ? 'back-off 5m0s restarting failed container' : undefined },
    info: [
      { name: 'Status Reason', value: opts.health === 'Degraded' ? 'CrashLoopBackOff' : 'Running' },
      { name: 'Node', value: opts.node ?? 'node-a' },
      { name: 'Containers', value: opts.ready ?? '2/2' },
      { name: 'Restart Count', value: String(opts.restarts ?? 0) },
    ],
    images: ['registry.example.test/shop/web:1.4.2'],
    networkingInfo: { labels: opts.labels ?? { app: 'web' } },
    createdAt: '2026-10-15T08:00:00Z',
    resourceVersion: '100',
  };
}

export function resourcesTreeFixture(): ApplicationTree {
  return {
    nodes: [
      {
        group: 'apps',
        version: 'v1',
        kind: 'Deployment',
        namespace: 'shop',
        name: 'web',
        uid: 'uid-deploy-web',
        health: { status: 'Degraded', message: 'Deployment "web" exceeded its progress deadline' },
        info: [{ name: 'Revision', value: 'Rev:7' }],
        images: ['registry.example.test/shop/web:1.4.2'],
        createdAt: '2026-10-01T12:05:00Z',
        resourceVersion: '91',
      },
      { group: 'apps', version: 'v1', kind: 'ReplicaSet', namespace: 'shop', name: 'web-7d9f', uid: 'uid-rs-new', parentRefs: [{ group: 'apps', kind: 'Deployment', namespace: 'shop', name: 'web', uid: 'uid-deploy-web' }], health: { status: 'Progressing' }, info: [{ name: 'Revision', value: 'Rev:7' }], createdAt: '2026-10-15T08:00:00Z' },
      { group: 'apps', version: 'v1', kind: 'ReplicaSet', namespace: 'shop', name: 'web-5c4b', uid: 'uid-rs-old', parentRefs: [{ group: 'apps', kind: 'Deployment', namespace: 'shop', name: 'web', uid: 'uid-deploy-web' }], health: { status: 'Healthy' }, info: [{ name: 'Revision', value: 'Rev:6' }], createdAt: '2026-10-10T08:00:00Z' },
      pod('web-7d9f-a1', 'uid-rs-new', 'ReplicaSet', 'web-7d9f', { node: 'node-a' }),
      pod('web-7d9f-b2', 'uid-rs-new', 'ReplicaSet', 'web-7d9f', { node: 'node-b', health: 'Degraded', restarts: 12, ready: '1/2' }),
      pod('web-7d9f-c3', 'uid-rs-new', 'ReplicaSet', 'web-7d9f', { node: 'node-b' }),
      {
        group: '',
        version: 'v1',
        kind: 'Service',
        namespace: 'shop',
        name: 'web',
        uid: 'uid-svc-web',
        health: { status: 'Healthy' },
        networkingInfo: { targetLabels: { app: 'web' } },
        createdAt: '2026-10-01T12:05:00Z',
      },
      {
        group: 'networking.k8s.io',
        version: 'v1',
        kind: 'Ingress',
        namespace: 'shop',
        name: 'web',
        uid: 'uid-ing-web',
        health: { status: 'Healthy' },
        networkingInfo: {
          targetRefs: [{ group: '', kind: 'Service', namespace: 'shop', name: 'web' }],
          ingress: [{ ip: '203.0.113.10' }],
          externalURLs: ['https://shop.example.test/'],
        },
        createdAt: '2026-10-01T12:05:00Z',
      },
      { group: '', version: 'v1', kind: 'ConfigMap', namespace: 'shop', name: 'web-config', uid: 'uid-cm-web', createdAt: '2026-10-01T12:05:00Z' },
      { group: '', version: 'v1', kind: 'Secret', namespace: 'shop', name: 'web-secret', uid: 'uid-secret-web', createdAt: '2026-10-01T12:05:00Z' },
      { group: 'apps', version: 'v1', kind: 'StatefulSet', namespace: 'shop', name: 'cache', uid: 'uid-sts-cache', health: { status: 'Healthy' }, createdAt: '2026-10-01T12:05:00Z' },
      pod('cache-0', 'uid-sts-cache', 'StatefulSet', 'cache', { node: 'node-a', labels: { app: 'cache' } }),
      { group: 'batch', version: 'v1', kind: 'Job', namespace: 'shop', name: 'db-migrate', uid: 'uid-job-migrate', health: { status: 'Healthy' }, createdAt: '2026-10-15T07:59:00Z' },
      pod('db-migrate-x7k', 'uid-job-migrate', 'Job', 'db-migrate', { node: 'node-a', labels: { job: 'db-migrate' }, ready: '0/1', group: 'batch' }),
      { group: '', version: 'v1', kind: 'ConfigMap', namespace: 'shop', name: 'legacy', uid: 'uid-cm-legacy', createdAt: '2026-09-01T12:05:00Z' },
    ],
    orphanedNodes: [{ group: '', version: 'v1', kind: 'ConfigMap', namespace: 'shop', name: 'stray', uid: 'uid-cm-stray', createdAt: '2026-09-20T10:00:00Z' }],
    hosts: [
      {
        name: 'node-a',
        resourcesInfo: [
          { resourceName: 'cpu', requestedByApp: 750, requestedByNeighbors: 1250, capacity: 4000 },
          { resourceName: 'memory', requestedByApp: 1610612736, requestedByNeighbors: 2147483648, capacity: 8589934592 },
        ],
        systemInfo: {
          machineID: 'mock-machine-a',
          systemUUID: 'mock-uuid-a',
          bootID: 'mock-boot-a',
          kernelVersion: '6.8.0',
          osImage: 'Linux',
          containerRuntimeVersion: 'containerd://2.1.0',
          kubeletVersion: 'v1.36.0',
          kubeProxyVersion: 'v1.36.0',
          operatingSystem: 'linux',
          architecture: 'amd64',
        },
        labels: { 'topology.kubernetes.io/zone': 'zone-a' },
      },
      {
        name: 'node-b',
        resourcesInfo: [{ resourceName: 'cpu', requestedByApp: 500, requestedByNeighbors: 2000, capacity: 4000 }],
      },
    ],
  };
}

/** A tree with `pods` pods spread over `replicaSets` ReplicaSets of one Deployment (scale tests). */
export function largeTreeFixture(pods: number, replicaSets = 10): { app: Application; tree: ApplicationTree } {
  const app = resourcesAppFixture({ name: 'big' });
  if (app.status) {
    app.status.resources = [{ group: 'apps', version: 'v1', kind: 'Deployment', namespace: 'shop', name: 'big', status: 'Synced', health: { status: 'Healthy' } }];
  }
  const nodes: ResourceNode[] = [{ group: 'apps', version: 'v1', kind: 'Deployment', namespace: 'shop', name: 'big', uid: 'uid-big', health: { status: 'Healthy' } }];
  for (let r = 0; r < replicaSets; r += 1) {
    nodes.push({ group: 'apps', version: 'v1', kind: 'ReplicaSet', namespace: 'shop', name: `big-rs${r}`, uid: `uid-big-rs${r}`, parentRefs: [{ group: 'apps', kind: 'Deployment', namespace: 'shop', name: 'big', uid: 'uid-big' }], health: { status: 'Healthy' } });
  }
  for (let p = 0; p < pods; p += 1) {
    const r = p % replicaSets;
    nodes.push(pod(`big-rs${r}-p${p}`, `uid-big-rs${r}`, 'ReplicaSet', `big-rs${r}`, { node: `node-${p % 7}`, health: p % 50 === 0 ? 'Degraded' : 'Healthy' }));
  }
  return { app, tree: { nodes, orphanedNodes: [], hosts: [] } };
}

function manifest(apiVersion: string, kind: string, name: string, extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    apiVersion,
    kind,
    metadata: {
      name,
      namespace: 'shop',
      labels: { app: 'web', 'app.kubernetes.io/part-of': 'shop' },
      annotations: { 'cd.patrol.example/tracking-id': `shop:${kind}:shop/${name}` },
      managedFields: [{ manager: 'patrol-controller', operation: 'Apply' }],
    },
    ...extra,
  };
}

function deployment(replicas: number, tag: string): Record<string, unknown> {
  return manifest('apps/v1', 'Deployment', 'web', {
    spec: {
      replicas,
      selector: { matchLabels: { app: 'web' } },
      template: {
        metadata: { labels: { app: 'web' } },
        spec: {
          initContainers: [{ name: 'migrate-check', image: 'registry.example.test/shop/tools:1.0' }],
          containers: [
            { name: 'web', image: `registry.example.test/shop/web:${tag}`, ports: [{ containerPort: 8080 }] },
            { name: 'sidecar', image: 'registry.example.test/shop/proxy:2.1' },
          ],
        },
      },
    },
  });
}

/** Live manifests by "kind/name", as JSON text like the API returns. */
export function liveManifestFixture(kind: string, name: string): string | null {
  switch (`${kind}/${name}`) {
    case 'Deployment/web':
      return JSON.stringify(deployment(3, '1.4.2'));
    case 'Secret/web-secret':
      return JSON.stringify({ ...manifest('v1', 'Secret', 'web-secret'), type: 'Opaque', data: { password: '++++++++', token: '++++++++' } });
    case 'Service/web':
      return JSON.stringify(manifest('v1', 'Service', 'web', { spec: { selector: { app: 'web' }, ports: [{ port: 80, targetPort: 8080 }] } }));
    default:
      if (kind === 'Pod') {
        return JSON.stringify(
          manifest('v1', 'Pod', name, {
            spec: { containers: [{ name: 'web', image: 'registry.example.test/shop/web:1.4.2' }, { name: 'sidecar', image: 'registry.example.test/shop/proxy:2.1' }] },
          }),
        );
      }
      return JSON.stringify(manifest(kind === 'Ingress' ? 'networking.k8s.io/v1' : 'v1', kind, name));
  }
}

function diff(group: string, kind: string, name: string, live: unknown, target: unknown, extra: Partial<ResourceDiff> = {}): ResourceDiff {
  const liveText = live === null ? '' : JSON.stringify(live);
  const targetText = target === null ? '' : JSON.stringify(target);
  return {
    group,
    kind,
    namespace: 'shop',
    name,
    liveState: liveText,
    normalizedLiveState: liveText,
    targetState: targetText,
    predictedLiveState: targetText,
    modified: liveText !== targetText,
    ...extra,
  };
}

export function managedResourcesFixture(): ManagedResourcesResponse {
  const svc = { apiVersion: 'v1', kind: 'Service', metadata: { name: 'web', namespace: 'shop' }, spec: { selector: { app: 'web' } } };
  const strip = (m: Record<string, unknown>) => {
    const metadata = { ...(m.metadata as Record<string, unknown>) };
    delete metadata.managedFields;
    return { ...m, metadata };
  };
  return {
    items: [
      diff('apps', 'Deployment', 'web', strip(deployment(3, '1.4.2')), strip(deployment(4, '1.5.0'))),
      diff('', 'Service', 'web', svc, svc),
      diff('', 'ConfigMap', 'legacy', { apiVersion: 'v1', kind: 'ConfigMap', metadata: { name: 'legacy', namespace: 'shop' }, data: { mode: 'old' } }, null),
      diff('batch', 'Job', 'db-migrate', { kind: 'Job', spec: { a: 1 } }, { kind: 'Job', spec: { a: 2 } }, { hook: true }),
      diff('', 'PersistentVolumeClaim', 'data', null, { apiVersion: 'v1', kind: 'PersistentVolumeClaim', metadata: { name: 'data', namespace: 'shop' }, spec: { resources: { requests: { storage: '1Gi' } } } }),
    ],
  };
}

export function resourceEventsFixture(name: string | null): EventList {
  if (name === 'web-7d9f-b2') {
    return {
      items: [
        {
          metadata: { name: 'web-7d9f-b2.1', namespace: 'shop', uid: 'ev-1', creationTimestamp: '2026-10-16T09:00:00Z' },
          involvedObject: { kind: 'Pod', name: 'web-7d9f-b2', namespace: 'shop' },
          reason: 'BackOff',
          message: 'Back-off restarting failed container web',
          type: 'Warning',
          count: 12,
          firstTimestamp: '2026-10-16T08:00:00Z',
          lastTimestamp: '2026-10-16T09:00:00Z',
          reportingComponent: 'kubelet',
          reportingInstance: 'node-a',
        },
        {
          metadata: { name: 'web-7d9f-b2.2', namespace: 'shop', uid: 'ev-2', creationTimestamp: '2026-10-16T08:00:00Z' },
          involvedObject: { kind: 'Pod', name: 'web-7d9f-b2', namespace: 'shop' },
          reason: 'Pulled',
          message: 'Container image already present on machine',
          type: 'Normal',
          count: 1,
          firstTimestamp: '2026-10-16T08:00:00Z',
          lastTimestamp: '2026-10-16T08:00:00Z',
          reportingComponent: 'kubelet',
          reportingInstance: 'node-a',
        },
      ],
    };
  }
  if (name === 'web') {
    return {
      items: [
        {
          metadata: { name: 'web.1', namespace: 'shop', uid: 'ev-3' },
          involvedObject: { kind: 'Deployment', name: 'web', namespace: 'shop' },
          reason: 'ScalingReplicaSet',
          message: 'Scaled up replica set web-7d9f to 3',
          type: 'Normal',
          count: 1,
          firstTimestamp: '2026-10-15T08:00:00Z',
          lastTimestamp: '2026-10-15T08:00:00Z',
          reportingComponent: 'kubelet',
          reportingInstance: 'node-a',
        },
      ],
    };
  }
  return { items: [] };
}

export function resourceActionsFixture(kind: string): ResourceActionsListResponse {
  if (kind === 'Deployment') {
    return {
      actions: [
        { name: 'restart', displayName: 'Restart' },
        { name: 'pause', displayName: 'Pause', disabled: true },
        // Action discovery declares parameter names only (ResourceActionParam, as upstream).
        { name: 'scale', displayName: 'Scale', params: [{ name: 'replicas' }] },
      ],
    };
  }
  return { actions: [] };
}

export function resourceLinksFixture(kind: string): LinksResponse {
  return kind === 'Deployment'
    ? { items: [{ title: 'Dashboard', url: 'https://metrics.example.test/d/web', description: 'Request rate and latency' }, { title: 'Unsafe', url: 'javascript:alert(1)' }] }
    : { items: [] };
}

export function resourcesProjectFixture(name: string): AppProject {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'AppProject',
    metadata: { name, namespace: 'patrol' },
    spec: { sourceRepos: ['*'], destinations: [{ server: '*', namespace: '*' }], orphanedResources: { warn: false } },
  };
}

export const logLinesFixture: LogEntry[] = [
  { content: 'starting web server on :8080', podName: 'web-7d9f-a1', timeStamp: '2026-10-16T09:00:00Z', timeStampStr: '2026-10-16T09:00:00Z', last: false },
  { content: 'GET /healthz 200', podName: 'web-7d9f-a1', timeStamp: '2026-10-16T09:00:01Z', timeStampStr: '2026-10-16T09:00:01Z', last: false },
  { content: 'ERROR connection refused: cache:6379', podName: 'web-7d9f-b2', timeStamp: '2026-10-16T09:00:02Z', timeStampStr: '2026-10-16T09:00:02Z', last: false },
  { content: 'GET /api/cart 200', podName: 'web-7d9f-c3', timeStamp: '2026-10-16T09:00:03Z', timeStampStr: '2026-10-16T09:00:03Z', last: false },
];
