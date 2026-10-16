import type {
  AppProjectList,
  Application,
  ApplicationInput,
  PluginSettings,
  RegisteredClusterList,
  RepositoryAppDetails,
  RepositoryAppList,
  RepositoryHelmChartList,
  RepositoryInfoList,
  RepositoryRefs,
} from '../../api/types';

/** Fixtures for the application creation wizard (registered repositories, projects, clusters, discovery). */

export const GIT_REPO = 'https://git.example.test/platform/apps.git';
export const HELM_REPO = 'https://charts.example.test/stable';
export const OCI_REPO = 'oci://registry.example.test/charts/podinfo';

const connected = { status: 'Successful' } as const;

export const createRepositoriesFixture: RepositoryInfoList = {
  items: [
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'Repository',
      metadata: { name: 'platform-apps', namespace: 'patrol' },
      spec: { url: GIT_REPO, type: 'git' },
      status: { connectionState: connected },
      connectionState: connected,
      hasCredentials: false,
    },
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'Repository',
      metadata: { name: 'stable-charts', namespace: 'patrol' },
      spec: { url: HELM_REPO, type: 'helm', name: 'stable' },
      status: { connectionState: connected },
      connectionState: connected,
      hasCredentials: false,
    },
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'Repository',
      metadata: { name: 'podinfo-oci', namespace: 'patrol' },
      spec: { url: OCI_REPO, type: 'oci' },
      status: { connectionState: connected },
      connectionState: connected,
      hasCredentials: false,
    },
  ],
  metadata: { resourceVersion: '10' },
};

export const createProjectsFixture: AppProjectList = {
  items: [
    { apiVersion: 'cd.patrol.example/v1alpha1', kind: 'AppProject', metadata: { name: 'default', namespace: 'patrol' }, spec: { sourceRepos: ['*'] } },
    { apiVersion: 'cd.patrol.example/v1alpha1', kind: 'AppProject', metadata: { name: 'payments', namespace: 'patrol' }, spec: { sourceRepos: [GIT_REPO] } },
  ],
  metadata: { resourceVersion: '11' },
};

export const createClustersFixture: RegisteredClusterList = {
  items: [
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'Cluster',
      metadata: {},
      spec: { name: 'in-cluster', server: 'https://kubernetes.default.svc' },
      credentials: {},
      inCluster: true,
    },
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'Cluster',
      metadata: { name: 'staging', namespace: 'patrol' },
      spec: { name: 'staging', server: 'https://staging.example.test' },
      credentials: { hasBearerToken: true },
    },
  ],
  metadata: { resourceVersion: '12' },
};

export const repoRefsFixture: RepositoryRefs = { branches: ['main', 'release-1.4'], tags: ['v1.4.0', 'v1.3.2'] };

export const repoAppsFixture: RepositoryAppList = {
  items: [
    { path: 'guestbook', type: 'Directory' },
    { path: 'helm-guestbook', type: 'Helm' },
    { path: 'kustomize-guestbook', type: 'Kustomize' },
  ],
};

export const helmChartsFixture: RepositoryHelmChartList = {
  items: [
    { name: 'nginx', versions: ['15.4.0', '15.3.1'] },
    { name: 'redis', versions: ['19.0.1'] },
  ],
};

/** OCI tags come back as RepositoryRefs (`tags`). */
export const ociTagsFixture: RepositoryRefs = { tags: ['6.7.0', '6.6.3'] };

export function appDetailsFixture(path: string | undefined, chart?: string): RepositoryAppDetails {
  if (chart || path === 'helm-guestbook') {
    return {
      type: 'Helm',
      helm: {
        name: chart ?? 'helm-guestbook',
        valueFiles: ['values.yaml', 'values-production.yaml'],
        values: 'replicaCount: 1\nimage:\n  tag: stable\n',
        parameters: [
          { name: 'replicaCount', value: '1' },
          { name: 'image.tag', value: 'stable' },
        ],
      },
    };
  }
  if (path === 'kustomize-guestbook') {
    return { type: 'Kustomize', kustomize: { images: ['registry.example.test/guestbook:v1'] } };
  }
  return { type: 'Directory', directory: {} };
}

export const pluginSettingsFixture: PluginSettings = { plugins: [{ name: 'envsubst' }] };

/** What POST /applications returns for a created app (server fills metadata and status). */
export function createdApplication(app: ApplicationInput): Application {
  return {
    ...app,
    metadata: { ...app.metadata, namespace: app.metadata?.namespace ?? 'patrol', uid: `uid-${app.metadata?.name ?? ''}`, resourceVersion: '1' },
    status: { health: { status: 'Missing' }, sync: { status: 'OutOfSync' } },
  };
}
