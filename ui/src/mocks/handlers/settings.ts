import { http, HttpResponse } from 'msw';
import type {
  Account,
  AppProject,
  ClusterInput,
  ConnectionState,
  GnuPGPublicKey,
  ProjectCreateRequest,
  ProjectTokenCreateRequest,
  ProjectTokenResponse,
  ProjectUpdateRequest,
  RegisteredCluster,
  RepositoryCertificate,
  RepositoryCertificateList,
  RepositoryCredentialInfo,
  RepositoryCredentialWriteRequest,
  RepositoryCredentials,
  RepositoryInfo,
  RepositoryWriteRequest,
  SCMProvider,
  SCMProviderWriteRequest,
} from '../../api/types';
import { parseCron, windowState } from '../../pages/settings/common/cron';
import { parseGoDuration } from '../../pages/settings/common/duration';
import { applicationsFixture } from '../fixtures';
import {
  accountsFixture,
  certificatesFixture,
  clustersFixture,
  gpgKeysFixture,
  notificationCatalogFixture,
  projectEventsFixture,
  projectsFixture,
  repoCredsFixture,
  repositoriesFixture,
  scmProvidersFixture,
  writeRepositoriesFixture,
} from '../fixtures/settings';
import { emptyObject, problem } from './util';

/**
 * Stateful Settings mocks (repositories, credentials, certificates, GnuPG keys, clusters,
 * projects, accounts, SCM providers, notification catalog). Writes change the in-memory store so
 * dev mock mode behaves like a server; tests call resetSettingsMockStore() before each test.
 * Shapes follow api/openapi/openapi.json (checked by src/mocks/contract.test.ts).
 *
 * Mock rules: a repository URL containing "unreachable" fails validation with a timeout, and a
 * password "wrong" fails with an authentication error. Deleting a project used by an application
 * fails with failed_precondition, as on the server. As on the server, `spec.credentialsRef` is
 * managed by the server: a write that sets a different one is rejected (send `credentials`).
 */

function freshStore() {
  return {
    repos: { read: repositoriesFixture(), write: writeRepositoriesFixture() } as Record<'read' | 'write', RepositoryInfo[]>,
    creds: { read: repoCredsFixture(), write: [] as RepositoryCredentialInfo[] } as Record<'read' | 'write', RepositoryCredentialInfo[]>,
    certificates: certificatesFixture(),
    gpgKeys: gpgKeysFixture(),
    clusters: clustersFixture(),
    projects: projectsFixture(),
    accounts: accountsFixture(),
    scmProviders: scmProvidersFixture(),
    tokenCounter: 0,
    resourceVersion: 100,
  };
}

export let settingsStore = freshStore();

export function resetSettingsMockStore(): void {
  settingsStore = freshStore();
}

const clone = <T>(value: T): T => structuredClone(value);
/** List envelope of CRD-backed lists: `{metadata: {resourceVersion}, items}`. */
const list = <T>(items: T[]) => ({ items: clone(items), metadata: { resourceVersion: String(settingsStore.resourceVersion) } });
const nextVersion = () => String((settingsStore.resourceVersion += 1));

/** MSW may hand back encoded or decoded path params depending on the URL; normalize. */
function param(value: unknown): string {
  const s = Array.isArray(value) ? value.join('/') : String(value);
  try {
    return s.includes('%') ? decodeURIComponent(s) : s;
  } catch {
    return s;
  }
}

function purposeOf(request: Request): 'read' | 'write' {
  return new URL(request.url).pathname.includes('/write-') ? 'write' : 'read';
}

function slug(value: string): string {
  return value
    .toLowerCase()
    .replace(/^[a-z]+:\/\//, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 50);
}

const hasSecretMaterial = (credentials: RepositoryCredentials | undefined) =>
  credentials !== undefined && Object.values(credentials).some((v) => typeof v === 'string' && v !== '');

function validateConnection(body: RepositoryWriteRequest): ConnectionState {
  const url = body.spec.url;
  const attemptedAt = new Date().toISOString();
  if (url.includes('unreachable')) {
    return { status: 'Failed', message: `dial tcp: lookup ${url}: i/o timeout`, attemptedAt };
  }
  if (body.credentials?.password === 'wrong') {
    return { status: 'Failed', message: 'authentication required: invalid username or password', attemptedAt };
  }
  return { status: 'Successful', attemptedAt };
}

/** Whether a credential template of the purpose matches the URL (longest-prefix rule, simplified). */
function templateMatches(purpose: 'read' | 'write', url: string): boolean {
  return settingsStore.creds[purpose].some((c) => url.startsWith(c.spec.urlPrefix));
}

/** The server manages spec.credentialsRef: a write may only repeat the current one. */
function credentialsRefRejected(sent: { name: string } | undefined, current: { name: string } | undefined) {
  return sent !== undefined && sent.name !== current?.name
    ? problem(400, 'invalid_argument', 'spec.credentialsRef is managed by the server; send credentials instead')
    : undefined;
}

function storedRepository(body: RepositoryWriteRequest, purpose: 'read' | 'write', existing?: RepositoryInfo): RepositoryInfo {
  const name = existing?.metadata.name ?? (slug(body.spec.url) || 'repository');
  const credentialsRef = hasSecretMaterial(body.credentials) ? { name: `repo-${name}` } : existing?.spec.credentialsRef;
  const spec = { ...body.spec, purpose: purpose === 'write' ? ('write' as const) : body.spec.purpose, credentialsRef };
  if (!credentialsRef) delete spec.credentialsRef;
  const connectionState = validateConnection(body);
  const hasCredentials = Boolean(spec.credentialsRef ?? spec.scmProviderRef ?? spec.useAzureWorkloadIdentity);
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Repository',
    metadata: { ...existing?.metadata, name, namespace: 'patrol', resourceVersion: nextVersion() },
    spec,
    status: { connectionState },
    connectionState,
    hasCredentials,
    ...(!hasCredentials && templateMatches(purpose, spec.url) ? { inheritedCredentials: true } : {}),
  };
}

function storedCredential(body: RepositoryCredentialWriteRequest, purpose: 'read' | 'write', existing?: RepositoryCredentialInfo): RepositoryCredentialInfo {
  const name = existing?.metadata.name ?? (slug(body.spec.urlPrefix) || 'creds');
  const credentialsRef = hasSecretMaterial(body.credentials) ? { name: `creds-${name}` } : existing?.spec.credentialsRef;
  const spec = { ...body.spec, purpose: purpose === 'write' ? ('write' as const) : body.spec.purpose, credentialsRef };
  if (!credentialsRef) delete spec.credentialsRef;
  const username = body.credentials?.username ?? existing?.username;
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'RepositoryCredential',
    metadata: { ...existing?.metadata, name, namespace: 'patrol', resourceVersion: nextVersion() },
    spec,
    hasCredentials: Boolean(spec.credentialsRef ?? spec.scmProviderRef ?? spec.useAzureWorkloadIdentity),
    ...(username ? { username } : {}),
  };
}

const repositoryHandlers = [
  ...(['repositories', 'write-repositories'] as const).flatMap((root) => [
    http.get(`*/api/v1/${root}`, ({ request }) => {
      const url = new URL(request.url);
      const project = url.searchParams.get('appProject');
      const items = settingsStore.repos[purposeOf(request)].filter((r) => !project || r.spec.project === project);
      return HttpResponse.json(list(items));
    }),
    http.post(`*/api/v1/${root}`, async ({ request }) => {
      const purpose = purposeOf(request);
      const body = (await request.json()) as RepositoryWriteRequest;
      const upsert = new URL(request.url).searchParams.get('upsert') === 'true';
      if (body.spec.url.includes('forbidden')) return problem(403, 'permission_denied', 'permission denied: repositories, create');
      const repos = settingsStore.repos[purpose];
      const index = repos.findIndex((r) => r.spec.url === body.spec.url && (r.spec.project ?? '') === (body.spec.project ?? ''));
      if (index >= 0 && !upsert) {
        return problem(409, 'already_exists', `repository "${body.spec.url}" already exists`);
      }
      const rejected = credentialsRefRejected(body.spec.credentialsRef, repos[index]?.spec.credentialsRef);
      if (rejected) return rejected;
      const stored = storedRepository(body, purpose, index >= 0 ? repos[index] : undefined);
      if (index >= 0) repos[index] = stored;
      else repos.push(stored);
      return HttpResponse.json(clone(stored));
    }),
    http.post(`*/api/v1/${root}/:repo/validate`, async ({ request }) => {
      const body = (await request.json()) as RepositoryWriteRequest;
      return HttpResponse.json(validateConnection(body));
    }),
    http.get(`*/api/v1/${root}/:repo`, ({ request, params }) => {
      const url = param(params.repo);
      const repo = settingsStore.repos[purposeOf(request)].find((r) => r.spec.url === url);
      // Missing and forbidden repositories both answer 403 (as the server).
      return repo ? HttpResponse.json(clone(repo)) : problem(403, 'permission_denied', `permission denied: repositories, get, ${url}`);
    }),
    http.put(`*/api/v1/${root}/:repo`, async ({ request, params }) => {
      const purpose = purposeOf(request);
      const url = param(params.repo);
      const body = (await request.json()) as RepositoryWriteRequest;
      const repos = settingsStore.repos[purpose];
      const index = repos.findIndex((r) => r.spec.url === url);
      const existing = repos[index];
      if (!existing) return problem(403, 'permission_denied', `permission denied: repositories, update, ${url}`);
      if (body.metadata?.resourceVersion && body.metadata.resourceVersion !== existing.metadata.resourceVersion) {
        return problem(409, 'conflict', `repository "${url}" was modified`);
      }
      const rejected = credentialsRefRejected(body.spec.credentialsRef, existing.spec.credentialsRef);
      if (rejected) return rejected;
      const stored = storedRepository(body, purpose, existing);
      repos[index] = stored;
      return HttpResponse.json(clone(stored));
    }),
    http.delete(`*/api/v1/${root}/:repo`, ({ request, params }) => {
      const purpose = purposeOf(request);
      const url = param(params.repo);
      const before = settingsStore.repos[purpose].length;
      settingsStore.repos[purpose] = settingsStore.repos[purpose].filter((r) => r.spec.url !== url);
      if (settingsStore.repos[purpose].length === before) return problem(403, 'permission_denied', `permission denied: repositories, delete, ${url}`);
      return HttpResponse.json(emptyObject);
    }),
  ]),
  ...(['repocreds', 'write-repocreds'] as const).flatMap((root) => [
    http.get(`*/api/v1/${root}`, ({ request }) => HttpResponse.json(list(settingsStore.creds[purposeOf(request)]))),
    http.post(`*/api/v1/${root}`, async ({ request }) => {
      const purpose = purposeOf(request);
      const body = (await request.json()) as RepositoryCredentialWriteRequest;
      const creds = settingsStore.creds[purpose];
      const index = creds.findIndex((c) => c.spec.urlPrefix === body.spec.urlPrefix);
      if (index >= 0 && new URL(request.url).searchParams.get('upsert') !== 'true') {
        return problem(409, 'already_exists', `repository credentials for "${body.spec.urlPrefix}" already exist`);
      }
      const rejected = credentialsRefRejected(body.spec.credentialsRef, creds[index]?.spec.credentialsRef);
      if (rejected) return rejected;
      const stored = storedCredential(body, purpose, creds[index]);
      if (index >= 0) creds[index] = stored;
      else creds.push(stored);
      return HttpResponse.json(clone(stored));
    }),
    http.put(`*/api/v1/${root}/:url`, async ({ request, params }) => {
      const purpose = purposeOf(request);
      const prefix = param(params.url);
      const body = (await request.json()) as RepositoryCredentialWriteRequest;
      if (body.spec.urlPrefix !== prefix) return problem(400, 'invalid_argument', 'the URL prefix of the path must equal spec.urlPrefix');
      const creds = settingsStore.creds[purpose];
      const index = creds.findIndex((c) => c.spec.urlPrefix === prefix);
      const existing = creds[index];
      if (!existing) return problem(404, 'not_found', `repository credentials "${prefix}" not found`);
      const rejected = credentialsRefRejected(body.spec.credentialsRef, existing.spec.credentialsRef);
      if (rejected) return rejected;
      const stored = storedCredential(body, purpose, existing);
      creds[index] = stored;
      return HttpResponse.json(clone(stored));
    }),
    http.delete(`*/api/v1/${root}/:url`, ({ request, params }) => {
      const purpose = purposeOf(request);
      const prefix = param(params.url);
      const before = settingsStore.creds[purpose].length;
      settingsStore.creds[purpose] = settingsStore.creds[purpose].filter((c) => c.spec.urlPrefix !== prefix);
      if (settingsStore.creds[purpose].length === before) return problem(404, 'not_found', `repository credentials "${prefix}" not found`);
      return HttpResponse.json(emptyObject);
    }),
  ]),
];

function certMatches(cert: RepositoryCertificate, url: URL): boolean {
  const host = url.searchParams.get('hostNamePattern');
  const type = url.searchParams.get('certType');
  const subType = url.searchParams.get('certSubType');
  const wantType = type === 'tls' ? 'https' : type;
  return (!host || cert.serverName === host) && (!wantType || wantType === '*' || cert.certType === wantType) && (!subType || cert.certSubType === subType);
}

/** The list route never returns certData (create and delete do). */
const withoutData = ({ certData: _certData, ...cert }: RepositoryCertificate): RepositoryCertificate => cert;

const trustHandlers = [
  http.get('*/api/v1/certificates', ({ request }) => {
    const url = new URL(request.url);
    return HttpResponse.json({ items: settingsStore.certificates.filter((c) => certMatches(c, url)).map(withoutData) } satisfies RepositoryCertificateList);
  }),
  http.post('*/api/v1/certificates', async ({ request }) => {
    const body = (await request.json()) as RepositoryCertificateList;
    const created: RepositoryCertificate[] = [];
    for (const cert of body.items) {
      if (cert.certType === 'https' && !(cert.certData ?? '').includes('BEGIN CERTIFICATE')) {
        return problem(400, 'invalid_argument', `invalid PEM data for "${cert.serverName}"`);
      }
      const subType = cert.certType === 'ssh' ? (cert.certSubType ?? (cert.certData ?? '').split(' ')[1] ?? 'ssh') : 'rsa';
      const stored: RepositoryCertificate = {
        serverName: cert.serverName,
        certType: cert.certType,
        certSubType: subType,
        certData: cert.certData,
        certInfo: cert.certType === 'ssh' ? `SHA256:mock${settingsStore.certificates.length}` : `CN=${cert.serverName}`,
      };
      const exists = settingsStore.certificates.some(
        (c) => c.serverName === stored.serverName && c.certType === stored.certType && c.certSubType === stored.certSubType,
      );
      if (exists && new URL(request.url).searchParams.get('upsert') !== 'true') {
        return problem(409, 'already_exists', `a ${stored.certType} certificate of type ${subType} for "${stored.serverName}" already exists`);
      }
      settingsStore.certificates = settingsStore.certificates.filter(
        (c) => !(c.serverName === stored.serverName && c.certType === stored.certType && c.certSubType === stored.certSubType),
      );
      settingsStore.certificates.push(stored);
      created.push(stored);
    }
    return HttpResponse.json({ items: clone(created) } satisfies RepositoryCertificateList);
  }),
  http.delete('*/api/v1/certificates', ({ request }) => {
    const url = new URL(request.url);
    const removed = settingsStore.certificates.filter((c) => certMatches(c, url));
    settingsStore.certificates = settingsStore.certificates.filter((c) => !certMatches(c, url));
    return HttpResponse.json({ items: clone(removed) } satisfies RepositoryCertificateList);
  }),
  http.get('*/api/v1/gpgkeys', () => HttpResponse.json({ items: clone(settingsStore.gpgKeys) })),
  http.post('*/api/v1/gpgkeys', async ({ request }) => {
    const body = (await request.json()) as GnuPGPublicKey;
    if (!(body.keyData ?? '').includes('BEGIN PGP PUBLIC KEY BLOCK')) {
      return problem(400, 'invalid_argument', 'unable to parse GnuPG public key: no armored data found');
    }
    const keyID = `MOCK${String(settingsStore.gpgKeys.length + 1).padStart(12, '0')}`;
    if (settingsStore.gpgKeys.some((k) => k.keyData === body.keyData)) {
      return HttpResponse.json({ created: { items: [] }, skipped: [settingsStore.gpgKeys.find((k) => k.keyData === body.keyData)?.keyID ?? ''] });
    }
    const key: GnuPGPublicKey = { keyID, fingerprint: `${keyID} 0000`, owner: 'Imported <imported@example.test>', trust: 'unknown', subType: 'rsa4096', keyData: body.keyData };
    settingsStore.gpgKeys.push(key);
    return HttpResponse.json({ created: { items: [clone(key)] }, skipped: [] });
  }),
  http.get('*/api/v1/gpgkeys/:keyID', ({ params }) => {
    const key = settingsStore.gpgKeys.find((k) => k.keyID === param(params.keyID));
    return key ? HttpResponse.json(clone(key)) : problem(404, 'not_found', `GnuPG key ${param(params.keyID)} not found`);
  }),
  http.delete('*/api/v1/gpgkeys/:keyID', ({ params }) => {
    const id = param(params.keyID);
    settingsStore.gpgKeys = settingsStore.gpgKeys.filter((k) => k.keyID !== id);
    return HttpResponse.json(emptyObject);
  }),
];

function findCluster(request: Request, id: string): RegisteredCluster | undefined {
  const byName = new URL(request.url).searchParams.get('id.type') === 'name';
  return settingsStore.clusters.find((c) => (byName ? c.spec.name === id : c.spec.server === id));
}

/**
 * Applies a ClusterInput (flat, as the API takes it) to a stored cluster. With `updatedFields`
 * only those fields change (ClusterUpdatableFields); without them the whole cluster is replaced.
 * Credentials in `config` are write-only: they only set the credential flags.
 */
function applyClusterInput(cluster: RegisteredCluster, input: ClusterInput, fields: string[]): void {
  const all = fields.length === 0;
  const want = (f: string) => all || fields.includes(f);
  if (want('name')) cluster.spec.name = input.name ?? input.server;
  if (want('namespaces')) cluster.spec.namespaces = input.namespaces ?? [];
  if (want('clusterResources')) cluster.spec.clusterResources = input.clusterResources ?? false;
  if (want('project')) {
    if (input.project) cluster.spec.project = input.project;
    else delete cluster.spec.project;
  }
  if (want('shard')) {
    if (input.shard !== undefined && input.shard >= 0) cluster.spec.shard = input.shard;
    else delete cluster.spec.shard;
  }
  if (want('labels')) cluster.metadata.labels = input.labels ?? {};
  if (want('annotations')) cluster.metadata.annotations = input.annotations ?? {};
  if (want('config')) {
    const config = input.config ?? {};
    cluster.credentials = {
      ...(config.bearerToken ? { hasBearerToken: true } : {}),
      ...(config.username || config.password ? { hasBasicAuth: true } : {}),
      ...(config.tlsClientConfig?.caData ? { hasCAData: true } : {}),
      ...(config.tlsClientConfig?.certData ? { hasClientCertificate: true } : {}),
      ...(config.execProviderConfig ? { hasExecProviderConfig: true } : {}),
    };
  }
}

const clusterHandlers = [
  http.get('*/api/v1/clusters', () => HttpResponse.json(list(settingsStore.clusters))),
  http.post('*/api/v1/clusters', async ({ request }) => {
    const body = (await request.json()) as ClusterInput;
    const existing = settingsStore.clusters.find((c) => c.spec.server === body.server);
    if (existing && new URL(request.url).searchParams.get('upsert') !== 'true') {
      return problem(409, 'already_exists', `cluster "${body.server}" already exists`);
    }
    const name = body.name ?? body.server;
    const cluster: RegisteredCluster = existing ?? {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'Cluster',
      metadata: { name: slug(name) || 'cluster', namespace: 'patrol', uid: `uid-cluster-${slug(name)}` },
      spec: { name, server: body.server },
      credentials: {},
    };
    applyClusterInput(cluster, body, []);
    cluster.metadata.resourceVersion = nextVersion();
    if (!existing) settingsStore.clusters.push(cluster);
    return HttpResponse.json(clone(cluster));
  }),
  http.get('*/api/v1/clusters/:server', ({ request, params }) => {
    const cluster = findCluster(request, param(params.server));
    // Missing and forbidden clusters both answer 403 (as the server).
    return cluster ? HttpResponse.json(clone(cluster)) : problem(403, 'permission_denied', `permission denied: clusters, get, ${param(params.server)}`);
  }),
  http.put('*/api/v1/clusters/:server', async ({ request, params }) => {
    const cluster = findCluster(request, param(params.server));
    if (!cluster) return problem(403, 'permission_denied', `permission denied: clusters, update, ${param(params.server)}`);
    const body = (await request.json()) as ClusterInput;
    if (body.server !== cluster.spec.server) return problem(400, 'invalid_argument', 'the server URL of a cluster cannot be changed');
    if (body.resourceVersion && body.resourceVersion !== cluster.metadata.resourceVersion) {
      return problem(409, 'conflict', `cluster "${cluster.spec.server}" was modified`);
    }
    const fields = new URL(request.url).searchParams.getAll('updatedFields').flatMap((f) => f.split(','));
    applyClusterInput(cluster, body, fields);
    cluster.metadata.resourceVersion = nextVersion();
    return HttpResponse.json(clone(cluster));
  }),
  http.delete('*/api/v1/clusters/:server', ({ request, params }) => {
    const cluster = findCluster(request, param(params.server));
    if (!cluster) return problem(403, 'permission_denied', `permission denied: clusters, delete, ${param(params.server)}`);
    settingsStore.clusters = settingsStore.clusters.filter((c) => c !== cluster);
    return HttpResponse.json(emptyObject);
  }),
  http.post('*/api/v1/clusters/:server/rotate-auth', ({ request, params }) => {
    const cluster = findCluster(request, param(params.server));
    if (!cluster) return problem(403, 'permission_denied', `permission denied: clusters, update, ${param(params.server)}`);
    if (cluster.spec.server === 'https://kubernetes.default.svc') {
      return problem(400, 'invalid_argument', 'the in-cluster destination uses the controller service account; there is no bearer token to rotate');
    }
    return HttpResponse.json(emptyObject);
  }),
  http.post('*/api/v1/clusters/:server/invalidate-cache', ({ request, params }) => {
    const cluster = findCluster(request, param(params.server));
    if (!cluster) return problem(403, 'permission_denied', `permission denied: clusters, update, ${param(params.server)}`);
    cluster.status = { ...cluster.status, info: { ...cluster.status?.info, cacheInfo: { apisCount: 0, resourcesCount: 0 } } };
    return HttpResponse.json(clone(cluster));
  }),
];

function findProject(name: string): AppProject | undefined {
  return settingsStore.projects.find((p) => p.metadata.name === name);
}

function activeWindows(project: AppProject) {
  return (project.spec.syncWindows ?? []).filter((w) => {
    const cron = parseCron(w.schedule ?? '');
    const duration = parseGoDuration(w.duration ?? '');
    return cron.ok && duration !== null && windowState(cron.schedule, duration, Date.now(), w.timeZone ?? 'UTC', 0).active;
  });
}

const projectHandlers = [
  http.get('*/api/v1/projects', () => HttpResponse.json(list(settingsStore.projects))),
  http.post('*/api/v1/projects', async ({ request }) => {
    const { project: body, upsert } = (await request.json()) as ProjectCreateRequest;
    const name = body.metadata?.name ?? '';
    if (findProject(name) && !upsert) return problem(409, 'already_exists', `project "${name}" already exists`);
    const stored: AppProject = {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'AppProject',
      ...body,
      metadata: { ...body.metadata, namespace: 'patrol', uid: `uid-${name}`, resourceVersion: nextVersion(), creationTimestamp: new Date().toISOString() },
    };
    settingsStore.projects = [...settingsStore.projects.filter((p) => p.metadata.name !== name), stored];
    return HttpResponse.json(clone(stored));
  }),
  http.get('*/api/v1/projects/:name', ({ params }) => {
    const project = findProject(param(params.name));
    return project ? HttpResponse.json(clone(project)) : problem(404, 'not_found', `project "${param(params.name)}" not found`);
  }),
  http.put('*/api/v1/projects/:name', async ({ params, request }) => {
    const name = param(params.name);
    const index = settingsStore.projects.findIndex((p) => p.metadata.name === name);
    const existing = settingsStore.projects[index];
    if (!existing) return problem(404, 'not_found', `project "${name}" not found`);
    const { project: body } = (await request.json()) as ProjectUpdateRequest;
    if (body.metadata?.resourceVersion !== existing.metadata.resourceVersion) {
      return problem(409, 'conflict', `the object has been modified; please apply your changes to the latest version (project "${name}")`);
    }
    const stored: AppProject = { ...body, metadata: { ...body.metadata, resourceVersion: nextVersion() } };
    settingsStore.projects[index] = stored;
    return HttpResponse.json(clone(stored));
  }),
  http.delete('*/api/v1/projects/:name', ({ params }) => {
    const name = param(params.name);
    if (!findProject(name)) return problem(404, 'not_found', `project "${name}" not found`);
    const apps = applicationsFixture.items.filter((a) => a.spec.project === name);
    if (apps.length > 0) {
      return problem(412, 'failed_precondition', `project "${name}" is referenced by ${apps.length} application(s)`);
    }
    settingsStore.projects = settingsStore.projects.filter((p) => p.metadata.name !== name);
    return HttpResponse.json(emptyObject);
  }),
  http.get('*/api/v1/projects/:name/detailed', ({ params }) => {
    const name = param(params.name);
    const project = findProject(name);
    if (!project) return problem(404, 'not_found', `project "${name}" not found`);
    // repositories are the Repository objects (no credential flags); clusters are RegisteredClusters.
    return HttpResponse.json({
      project: clone(project),
      globalProjects: [],
      repositories: settingsStore.repos.read
        .filter((r) => r.spec.project === name)
        .map(({ apiVersion, kind, metadata, spec, status }) => clone({ apiVersion, kind, metadata, spec, status })),
      clusters: clone(settingsStore.clusters.filter((c) => c.spec.project === name)),
    });
  }),
  http.get('*/api/v1/projects/:name/globalprojects', () => HttpResponse.json({ items: [] })),
  http.get('*/api/v1/projects/:name/links', () => HttpResponse.json({ items: [] })),
  http.get('*/api/v1/projects/:name/events', ({ params }) => {
    const name = param(params.name);
    return HttpResponse.json({ items: name === 'finance' ? projectEventsFixture(name) : [] });
  }),
  http.get('*/api/v1/projects/:name/syncwindows', ({ params }) => {
    const project = findProject(param(params.name));
    return HttpResponse.json({ windows: project ? clone(activeWindows(project)) : [] });
  }),
  http.post('*/api/v1/projects/:project/roles/:role/token', async ({ params, request }) => {
    const project = findProject(param(params.project));
    const role = project?.spec.roles?.find((r) => r.name === param(params.role));
    if (!project || !role) return problem(404, 'not_found', `role "${param(params.role)}" not found in project "${param(params.project)}"`);
    const body = (await request.json()) as ProjectTokenCreateRequest;
    settingsStore.tokenCounter += 1;
    const iat = 1_760_100_000 + settingsStore.tokenCounter;
    const token = { iat, exp: body.expiresIn ? iat + body.expiresIn : undefined, id: body.id ?? `token-${settingsStore.tokenCounter}` };
    role.jwtTokens = [...(role.jwtTokens ?? []), token];
    project.status = {
      ...project.status,
      jwtTokensByRole: { ...project.status?.jwtTokensByRole, [role.name]: { items: [...(project.status?.jwtTokensByRole?.[role.name]?.items ?? []), token] } },
    };
    project.metadata.resourceVersion = nextVersion();
    const response: ProjectTokenResponse = { token: `mock.jwt.${settingsStore.tokenCounter}`, id: token.id, issuedAt: iat };
    if (token.exp) response.expiresAt = token.exp;
    return HttpResponse.json(response);
  }),
  http.delete('*/api/v1/projects/:project/roles/:role/token/:iat', ({ params, request }) => {
    const project = findProject(param(params.project));
    const role = project?.spec.roles?.find((r) => r.name === param(params.role));
    if (!project || !role) return problem(404, 'not_found', 'role not found');
    const iat = Number(param(params.iat));
    // Like the server: by id (jti) when given, since tokens may share an iat second; else by iat.
    const id = new URL(request.url).searchParams.get('id');
    const keep = (t: { iat: number; id?: string | undefined }) => (id ? (t.id !== undefined && t.id !== '' ? t.id : String(t.iat)) !== id : t.iat !== iat);
    role.jwtTokens = (role.jwtTokens ?? []).filter(keep);
    const byRole = project.status?.jwtTokensByRole?.[role.name];
    if (byRole) byRole.items = (byRole.items ?? []).filter(keep);
    project.metadata.resourceVersion = nextVersion();
    return HttpResponse.json(emptyObject);
  }),
];

function findAccount(name: string): Account | undefined {
  return settingsStore.accounts.find((a) => a.name === name);
}

const accountHandlers = [
  http.get('*/api/v1/account', () => HttpResponse.json({ items: clone(settingsStore.accounts) })),
  http.get('*/api/v1/account/:name', ({ params }) => {
    const account = findAccount(param(params.name));
    return account ? HttpResponse.json(clone(account)) : problem(404, 'not_found', `account "${param(params.name)}" does not exist`);
  }),
  http.post('*/api/v1/account/:name/token', async ({ params, request }) => {
    const account = findAccount(param(params.name));
    if (!account) return problem(404, 'not_found', `account "${param(params.name)}" does not exist`);
    if (!account.capabilities?.includes('apiKey')) {
      return problem(412, 'failed_precondition', `account "${account.name}" does not have the apiKey capability`);
    }
    const body = (await request.json()) as { id?: string; expiresIn?: number };
    settingsStore.tokenCounter += 1;
    const id = body.id ?? `token-${settingsStore.tokenCounter}`;
    if (account.tokens?.some((t) => t.id === id)) return problem(409, 'already_exists', `account already has a token with ID "${id}"`);
    const issuedAt = 1_760_100_000 + settingsStore.tokenCounter;
    account.tokens = [...(account.tokens ?? []), { id, issuedAt, expiresAt: body.expiresIn ? issuedAt + body.expiresIn : 0 }];
    return HttpResponse.json({ token: `mock.account.token.${settingsStore.tokenCounter}` });
  }),
  http.delete('*/api/v1/account/:name/token/:id', ({ params }) => {
    const account = findAccount(param(params.name));
    if (!account) return problem(404, 'not_found', `account "${param(params.name)}" does not exist`);
    account.tokens = (account.tokens ?? []).filter((t) => t.id !== param(params.id));
    return HttpResponse.json(emptyObject);
  }),
];

/** SCM provider writes carry metadata and spec only; secrets are referenced by the spec's secretRefs. */
function storedProvider(body: SCMProviderWriteRequest, existing?: SCMProvider): SCMProvider {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'SCMProvider',
    metadata: { ...existing?.metadata, ...body.metadata, namespace: 'patrol', resourceVersion: nextVersion() },
    spec: body.spec,
    status: existing?.status ?? {},
  };
}

const scmHandlers = [
  http.get('*/api/v1/scmproviders', () => HttpResponse.json(list(settingsStore.scmProviders))),
  http.post('*/api/v1/scmproviders', async ({ request }) => {
    const body = (await request.json()) as SCMProviderWriteRequest;
    const name = body.metadata.name ?? '';
    if (settingsStore.scmProviders.some((p) => p.metadata.name === name)) {
      return problem(409, 'already_exists', `SCM provider "${name}" already exists`);
    }
    const stored = storedProvider(body);
    settingsStore.scmProviders.push(stored);
    return HttpResponse.json(clone(stored));
  }),
  http.get('*/api/v1/scmproviders/:name', ({ params }) => {
    const provider = settingsStore.scmProviders.find((p) => p.metadata.name === param(params.name));
    return provider ? HttpResponse.json(clone(provider)) : problem(404, 'not_found', `SCM provider "${param(params.name)}" not found`);
  }),
  http.put('*/api/v1/scmproviders/:name', async ({ params, request }) => {
    const name = param(params.name);
    const index = settingsStore.scmProviders.findIndex((p) => p.metadata.name === name);
    const existing = settingsStore.scmProviders[index];
    if (!existing) return problem(404, 'not_found', `SCM provider "${name}" not found`);
    const body = (await request.json()) as SCMProviderWriteRequest;
    if (body.metadata.resourceVersion && body.metadata.resourceVersion !== existing.metadata.resourceVersion) {
      return problem(409, 'conflict', `SCM provider "${name}" was modified`);
    }
    const stored = storedProvider(body, existing);
    settingsStore.scmProviders[index] = stored;
    return HttpResponse.json(clone(stored));
  }),
  http.delete('*/api/v1/scmproviders/:name', ({ params }) => {
    const name = param(params.name);
    settingsStore.scmProviders = settingsStore.scmProviders.filter((p) => p.metadata.name !== name);
    return HttpResponse.json(emptyObject);
  }),
];

const notificationHandlers = [
  http.get('*/api/v1/notifications/services', () => HttpResponse.json({ items: notificationCatalogFixture.services })),
  http.get('*/api/v1/notifications/triggers', () => HttpResponse.json({ items: notificationCatalogFixture.triggers })),
  http.get('*/api/v1/notifications/templates', () => HttpResponse.json({ items: notificationCatalogFixture.templates })),
];

export const settingsHandlers = [
  ...repositoryHandlers,
  ...trustHandlers,
  ...clusterHandlers,
  ...projectHandlers,
  ...accountHandlers,
  ...scmHandlers,
  ...notificationHandlers,
];
