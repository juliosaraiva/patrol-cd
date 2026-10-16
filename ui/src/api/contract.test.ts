/**
 * The typed client against the published OpenAPI document (api/openapi/openapi.json):
 *
 * - every exported client function maps to operations of the document (or says why it is not a
 *   documented route), and every operation of the document is used by the client or listed in
 *   `unusedOperations` with the reason;
 * - each mapping is exercised: the request the function sends (under a non-root
 *   `<base href="/patrol/">`) has the operation's method and path, only declared query
 *   parameters, the CSRF header on mutations, and a JSON body that validates strictly against the
 *   operation's request schema;
 * - the default MSW handlers answer that request with a body that validates strictly against
 *   the operation's response schema (problem documents for errors).
 */
import { getResponse, http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { appsCoreApplications, GUESTBOOK_SHA, guestbookApp } from '../mocks/fixtures/apps-core';
import { guestbookAppSet } from '../mocks/fixtures/applicationsets';
import { projectsFixture } from '../mocks/fixtures/settings';
import { handlers, resetMockState } from '../mocks/handlers';
import { resetAppsCoreMockState } from '../mocks/handlers/apps-core';
import { resetAppsResourcesRequests } from '../mocks/handlers/apps-resources';
import { resetSettingsMockStore } from '../mocks/handlers/settings';
import { server } from '../mocks/server';
import {
  findOperation,
  matchPath,
  parseEventStream,
  queryParameterNames,
  requestSchema,
  responseSchema,
  specOperations,
  validateSchema,
  type SpecOperation,
} from '../test/openapi';
import * as applicationsApi from './endpoints/applications';
import * as applicationSetsApi from './endpoints/applicationsets';
import * as clustersApi from './endpoints/clusters';
import * as miscApi from './endpoints/misc';
import * as projectsApi from './endpoints/projects';
import * as repositoriesApi from './endpoints/repositories';
import * as sessionApi from './endpoints/session';
import * as trustApi from './endpoints/trust';
import type { OperationId } from './types';

const modules = {
  session: sessionApi,
  applications: applicationsApi,
  applicationsets: applicationSetsApi,
  projects: projectsApi,
  repositories: repositoriesApi,
  trust: trustApi,
  clusters: clustersApi,
  misc: miscApi,
};

type Exports = typeof sessionApi &
  typeof applicationsApi &
  typeof applicationSetsApi &
  typeof projectsApi &
  typeof repositoriesApi &
  typeof trustApi &
  typeof clustersApi &
  typeof miscApi;
type ClientFunction = { [K in keyof Exports]: Exports[K] extends (...args: never[]) => unknown ? K : never }[keyof Exports];

interface Case {
  op: OperationId;
  /** Calls the client function: a request (promise), a stream (handle), a URL or a WebSocket. */
  call: () => unknown;
}
type Entry = readonly Case[] | { notInDocument: string };

const APP = 'guestbook';
const scope = { appNamespace: 'patrol', project: 'default' };
const web = { group: 'apps', version: 'v1', kind: 'Deployment', namespace: 'shop', name: 'web' };
const REPO = 'https://git.example.test/platform/apps.git';
const PREFIX = 'https://git.example.test/platform';
const PROD = { server: 'https://prod-eu.k8s.example.test' };
const noop = { onEvent: () => undefined };

const newApplication = {
  apiVersion: 'cd.patrol.example/v1alpha1',
  kind: 'Application',
  metadata: { name: 'new-app', namespace: 'patrol' },
  spec: {
    project: 'default',
    source: { repoURL: REPO, path: 'new-app', targetRevision: 'HEAD' },
    destination: { server: 'https://kubernetes.default.svc', namespace: 'new-app' },
  },
};

const repoWrite = {
  metadata: {},
  spec: { url: 'https://git.example.test/platform/new.git', type: 'git' as const },
  credentials: { username: 'robot', password: 'example-password' },
};

/**
 * One entry per exported client function. A function that serves several operations (the
 * `purpose: 'write'` repository variants, the extension proxy methods) has a case for each.
 */
const clientCases = {
  // Session, accounts, settings.
  createSession: [{ op: 'createSession', call: () => sessionApi.createSession({ username: 'admin', password: 'example-password' }) }],
  deleteSession: [{ op: 'deleteSession', call: () => sessionApi.deleteSession() }],
  getUserInfo: [{ op: 'getUserInfo', call: () => sessionApi.getUserInfo() }],
  ssoLoginURL: [{ op: 'startSSOLogin', call: () => sessionApi.ssoLoginURL('https://patrol.example.test/patrol/applications') }],
  listAccounts: [{ op: 'listAccounts', call: () => sessionApi.listAccounts() }],
  getAccount: [{ op: 'getAccount', call: () => sessionApi.getAccount('admin') }],
  canI: [{ op: 'canI', call: () => sessionApi.canI('applications', 'sync', 'default/guestbook') }],
  updatePassword: [{ op: 'updatePassword', call: () => sessionApi.updatePassword({ name: 'admin', currentPassword: 'old-example', newPassword: 'new-example' }) }],
  createAccountToken: [{ op: 'createAccountToken', call: () => sessionApi.createAccountToken('admin', { id: 'ci', expiresIn: 3600 }) }],
  deleteAccountToken: [{ op: 'deleteAccountToken', call: () => sessionApi.deleteAccountToken('admin', 'laptop') }],
  getSettings: [{ op: 'getSettings', call: () => sessionApi.getSettings() }],
  getPluginSettings: [{ op: 'getPluginSettings', call: () => sessionApi.getPluginSettings() }],
  getVersion: [{ op: 'getVersion', call: () => sessionApi.getVersion() }],

  // Applications.
  listApplications: [{ op: 'listApplications', call: () => applicationsApi.listApplications({ projects: ['default'], selector: 'team=web', limit: 50, continue: 'token' }) }],
  watchApplications: [{ op: 'watchApplications', call: () => applicationsApi.watchApplications({ projects: ['default'], resourceVersion: '100' }, noop) }],
  createApplication: [{ op: 'createApplication', call: () => applicationsApi.createApplication(newApplication, { upsert: false, validate: true }) }],
  getApplication: [{ op: 'getApplication', call: () => applicationsApi.getApplication(APP, { ...scope, refresh: 'hard' }) }],
  updateApplication: [{ op: 'updateApplication', call: () => applicationsApi.updateApplication(guestbookApp(), { validate: true }) }],
  updateApplicationSpec: [{ op: 'updateApplicationSpec', call: () => applicationsApi.updateApplicationSpec(APP, guestbookApp().spec, { validate: false, ...scope }) }],
  patchApplication: [
    { op: 'patchApplication', call: () => applicationsApi.patchApplication(APP, { patch: JSON.stringify({ metadata: { labels: { a: 'b' } } }), patchType: 'merge' }, scope) },
  ],
  deleteApplication: [{ op: 'deleteApplication', call: () => applicationsApi.deleteApplication(APP, { cascade: true, propagationPolicy: 'foreground', ...scope }) }],
  syncApplication: [
    {
      op: 'syncApplication',
      call: () =>
        applicationsApi.syncApplication(
          APP,
          {
            prune: true,
            dryRun: false,
            revision: 'main',
            strategy: { hook: { force: false } },
            syncOptions: { items: ['CreateNamespace=true'] },
            resources: [{ group: 'apps', kind: 'Deployment', namespace: 'guestbook', name: 'guestbook-ui' }],
            retryStrategy: { limit: 5, backoff: { duration: '5s', factor: 2, maxDuration: '3m' } },
          },
          scope,
        ),
    },
  ],
  rollbackApplication: [{ op: 'rollbackApplication', call: () => applicationsApi.rollbackApplication('search', { id: 1, prune: false, dryRun: true }, scope) }],
  terminateOperation: [{ op: 'terminateApplicationOperation', call: () => applicationsApi.terminateOperation(APP, scope) }],
  listApplicationOperations: [{ op: 'listApplicationOperations', call: () => applicationsApi.listApplicationOperations(APP, { ...scope, limit: 10 }) }],
  getApplicationOperation: [{ op: 'getApplicationOperation', call: () => applicationsApi.getApplicationOperation(APP, 'guestbook-sync-3', scope) }],
  getApplicationSyncWindows: [{ op: 'getApplicationSyncWindows', call: () => applicationsApi.getApplicationSyncWindows(APP, scope) }],
  getRevisionMetadata: [{ op: 'getApplicationRevisionMetadata', call: () => applicationsApi.getRevisionMetadata(APP, GUESTBOOK_SHA, { sourceIndex: 0, ...scope }) }],
  getRevisionChartDetails: [{ op: 'getApplicationRevisionChartDetails', call: () => applicationsApi.getRevisionChartDetails('payments', '1.4.2', { sourceIndex: 0, versionId: 3 }) }],
  getOCIMetadata: [{ op: 'getApplicationRevisionOCIMetadata', call: () => applicationsApi.getOCIMetadata(APP, '6.7.0', { sourceIndex: 0 }) }],
  getManifests: [{ op: 'getApplicationManifests', call: () => applicationsApi.getManifests(APP, { revisions: ['main', 'v1'], sourcePositions: [1, 2], noCache: true, ...scope }) }],
  getManagedResources: [{ op: 'getManagedResources', call: () => applicationsApi.getManagedResources(APP, { kind: 'Deployment', group: 'apps', name: 'guestbook-ui', namespace: 'guestbook', ...scope }) }],
  serverSideDiff: [
    {
      op: 'serverSideDiff',
      call: () =>
        applicationsApi.serverSideDiff(
          APP,
          { liveResources: [{ group: 'apps', kind: 'Deployment', namespace: 'guestbook', name: 'guestbook-ui', liveState: '{}' }], targetManifests: ['{"kind":"Deployment"}'] },
          scope,
        ),
    },
  ],
  getResourceTree: [{ op: 'getResourceTree', call: () => applicationsApi.getResourceTree(APP, scope) }],
  parseTreeEvent: { notInDocument: 'Decoder for watchResourceTree events, not a route.' },
  watchResourceTree: [{ op: 'watchResourceTree', call: () => applicationsApi.watchResourceTree(APP, scope, noop) }],
  listApplicationEvents: [{ op: 'listApplicationEvents', call: () => applicationsApi.listApplicationEvents('shop', { resourceName: 'web-7d9f-b2', resourceNamespace: 'shop', resourceUID: 'uid', ...scope }) }],
  getResource: [{ op: 'getResource', call: () => applicationsApi.getResource('shop', web, scope) }],
  patchResource: [{ op: 'patchResource', call: () => applicationsApi.patchResource('shop', web, { patch: '{"spec":{"replicas":2}}', patchType: 'merge' }, scope) }],
  deleteResource: [{ op: 'deleteResource', call: () => applicationsApi.deleteResource('shop', web, { force: true, orphan: false, ...scope }) }],
  listResourceActions: [{ op: 'listResourceActions', call: () => applicationsApi.listResourceActions('shop', web, scope) }],
  runResourceAction: [
    { op: 'runResourceAction', call: () => applicationsApi.runResourceAction('shop', web, { action: 'scale', resourceActionParameters: [{ name: 'replicas', value: '3' }] }, scope) },
  ],
  streamApplicationLogs: [
    {
      op: 'getApplicationLogs',
      call: () =>
        applicationsApi.streamApplicationLogs('shop', { kind: 'Deployment', group: 'apps', resourceName: 'web', namespace: 'shop', container: 'web', tailLines: 100, follow: false, ...scope }, noop),
    },
  ],
  streamPodLogs: [{ op: 'getPodLogs', call: () => applicationsApi.streamPodLogs('shop', 'web-7d9f-a1', { container: 'web', sinceSeconds: 60, previous: false, follow: false, ...scope }, noop) }],
  applicationLogsURL: [{ op: 'getApplicationLogs', call: () => applicationsApi.applicationLogsURL('shop', { kind: 'Deployment', resourceName: 'web', namespace: 'shop', ...scope }) }],
  listApplicationLinks: [{ op: 'listApplicationLinks', call: () => applicationsApi.listApplicationLinks(APP, scope) }],
  listResourceLinks: [{ op: 'listApplicationResourceLinks', call: () => applicationsApi.listResourceLinks('shop', web, scope) }],
  terminalURL: [
    { op: 'openTerminal', call: () => applicationsApi.terminalURL({ appName: 'shop', appNamespace: 'patrol', projectName: 'default', namespace: 'shop', pod: 'web-7d9f-a1', container: 'web', shell: 'bash' }) },
  ],
  openTerminal: [{ op: 'openTerminal', call: () => applicationsApi.openTerminal({ appName: 'shop', projectName: 'default', namespace: 'shop', pod: 'web-7d9f-a1', container: 'web' }) }],

  // ApplicationSets.
  listApplicationSets: [{ op: 'listApplicationSets', call: () => applicationSetsApi.listApplicationSets({ projects: ['default'], appsetNamespace: 'patrol', limit: 10 }) }],
  watchApplicationSets: [{ op: 'watchApplicationSets', call: () => applicationSetsApi.watchApplicationSets({ appsetNamespace: 'patrol', resourceVersion: '5' }, noop) }],
  createApplicationSet: [{ op: 'createApplicationSet', call: () => applicationSetsApi.createApplicationSet(guestbookAppSet, { upsert: true, dryRun: true }) }],
  getApplicationSet: [{ op: 'getApplicationSet', call: () => applicationSetsApi.getApplicationSet('guestbook', 'patrol') }],
  deleteApplicationSet: [{ op: 'deleteApplicationSet', call: () => applicationSetsApi.deleteApplicationSet('guestbook', 'patrol', { cascade: false }) }],
  generateApplicationSet: [{ op: 'generateApplicationSet', call: () => applicationSetsApi.generateApplicationSet({ applicationSet: guestbookAppSet }) }],
  getApplicationSetResourceTree: [{ op: 'getApplicationSetResourceTree', call: () => applicationSetsApi.getApplicationSetResourceTree('guestbook', 'patrol') }],
  listApplicationSetEvents: [{ op: 'listApplicationSetEvents', call: () => applicationSetsApi.listApplicationSetEvents('guestbook', 'patrol') }],

  // Projects.
  listProjects: [{ op: 'listProjects', call: () => projectsApi.listProjects({ selector: 'team=finance' }) }],
  createProject: [
    {
      op: 'createProject',
      call: () =>
        projectsApi.createProject({ apiVersion: 'cd.patrol.example/v1alpha1', kind: 'AppProject', metadata: { name: 'team-a' }, spec: { sourceRepos: ['*'], destinations: [{ server: '*', namespace: 'team-a' }] } }, { upsert: true }),
    },
  ],
  getProject: [{ op: 'getProject', call: () => projectsApi.getProject('finance') }],
  updateProject: [{ op: 'updateProject', call: () => projectsApi.updateProject(projectsFixture()[1] ?? { metadata: { name: 'finance' }, spec: {} }) }],
  deleteProject: [{ op: 'deleteProject', call: () => projectsApi.deleteProject('team-unused') }],
  getDetailedProject: [{ op: 'getDetailedProject', call: () => projectsApi.getDetailedProject('finance') }],
  getGlobalProjects: [{ op: 'getGlobalProjects', call: () => projectsApi.getGlobalProjects('finance') }],
  listProjectEvents: [{ op: 'listProjectEvents', call: () => projectsApi.listProjectEvents('finance') }],
  getProjectSyncWindows: [{ op: 'getProjectSyncWindows', call: () => projectsApi.getProjectSyncWindows('finance') }],
  listProjectLinks: [{ op: 'listProjectLinks', call: () => projectsApi.listProjectLinks('finance') }],
  createProjectRoleToken: [{ op: 'createProjectToken', call: () => projectsApi.createProjectRoleToken('finance', 'deployer', { id: 'ci', expiresIn: 3600, description: 'CI' }) }],
  deleteProjectRoleToken: [{ op: 'deleteProjectToken', call: () => projectsApi.deleteProjectRoleToken('finance', 'deployer', 1760000000, 'ci-pipeline') }],

  // Repositories and credential templates (read and write purposes).
  listRepositories: [
    { op: 'listRepositories', call: () => repositoriesApi.listRepositories({ forceRefresh: true, appProject: 'default' }) },
    { op: 'listWriteRepositories', call: () => repositoriesApi.listRepositories({}, { purpose: 'write' }) },
  ],
  createRepository: [
    { op: 'createRepository', call: () => repositoriesApi.createRepository(repoWrite, { upsert: false }) },
    { op: 'createWriteRepository', call: () => repositoriesApi.createRepository({ ...repoWrite, spec: { ...repoWrite.spec, purpose: 'write' } }, {}, { purpose: 'write' }) },
  ],
  getRepository: [
    { op: 'getRepository', call: () => repositoriesApi.getRepository(REPO, { appProject: 'default' }) },
    { op: 'getWriteRepository', call: () => repositoriesApi.getRepository(REPO, {}, { purpose: 'write' }) },
  ],
  updateRepository: [
    { op: 'updateRepository', call: () => repositoriesApi.updateRepository(REPO, { metadata: { resourceVersion: '10' }, spec: { url: REPO, type: 'git', enableLFS: true } }, { appProject: 'default' }) },
    { op: 'updateWriteRepository', call: () => repositoriesApi.updateRepository(REPO, { spec: { url: REPO, type: 'git', purpose: 'write' } }, {}, { purpose: 'write' }) },
  ],
  deleteRepository: [
    { op: 'deleteRepository', call: () => repositoriesApi.deleteRepository(REPO, { appProject: 'default' }) },
    { op: 'deleteWriteRepository', call: () => repositoriesApi.deleteRepository(REPO, {}, { purpose: 'write' }) },
  ],
  validateRepository: [
    { op: 'validateRepositoryAccess', call: () => repositoriesApi.validateRepository(repoWrite.spec.url, repoWrite) },
    { op: 'validateWriteRepositoryAccess', call: () => repositoriesApi.validateRepository(repoWrite.spec.url, repoWrite, { purpose: 'write' }) },
  ],
  listRepositoryRefs: [{ op: 'listRepositoryRefs', call: () => repositoriesApi.listRepositoryRefs(REPO, { appProject: 'default' }) }],
  listRepositoryOCITags: [{ op: 'listRepositoryOCITags', call: () => repositoriesApi.listRepositoryOCITags('oci://registry.example.test/charts/podinfo') }],
  listRepositoryApps: [{ op: 'listRepositoryApps', call: () => repositoriesApi.listRepositoryApps(REPO, { revision: 'main', appName: 'new-app', appProject: 'default' }) }],
  getRepositoryAppDetails: [
    {
      op: 'getRepositoryAppDetails',
      call: () => repositoriesApi.getRepositoryAppDetails(REPO, { source: { repoURL: REPO, path: 'helm-guestbook', targetRevision: 'main' }, appName: 'new-app', appProject: 'default' }),
    },
  ],
  listRepositoryHelmCharts: [{ op: 'listRepositoryHelmCharts', call: () => repositoriesApi.listRepositoryHelmCharts('https://charts.example.test/stable') }],
  listRepositoryCredentials: [
    { op: 'listRepositoryCredentials', call: () => repositoriesApi.listRepositoryCredentials() },
    { op: 'listWriteRepositoryCredentials', call: () => repositoriesApi.listRepositoryCredentials({ purpose: 'write' }) },
  ],
  createRepositoryCredential: [
    { op: 'createRepositoryCredential', call: () => repositoriesApi.createRepositoryCredential({ spec: { urlPrefix: 'https://git.example.test/team', type: 'git' }, credentials: { bearerToken: 'example-token' } }) },
    {
      op: 'createWriteRepositoryCredential',
      call: () => repositoriesApi.createRepositoryCredential({ spec: { urlPrefix: 'https://git.example.test/team', purpose: 'write' }, credentials: { username: 'bot', password: 'example' } }, {}, { purpose: 'write' }),
    },
  ],
  updateRepositoryCredential: [
    { op: 'updateRepositoryCredential', call: () => repositoriesApi.updateRepositoryCredential(PREFIX, { spec: { urlPrefix: PREFIX, type: 'git', credentialsRef: { name: 'creds-platform' } } }) },
    { op: 'updateWriteRepositoryCredential', call: () => repositoriesApi.updateRepositoryCredential(PREFIX, { spec: { urlPrefix: PREFIX, purpose: 'write' } }, { purpose: 'write' }) },
  ],
  deleteRepositoryCredential: [
    { op: 'deleteRepositoryCredential', call: () => repositoriesApi.deleteRepositoryCredential(PREFIX) },
    { op: 'deleteWriteRepositoryCredential', call: () => repositoriesApi.deleteRepositoryCredential(PREFIX, { purpose: 'write' }) },
  ],

  // Certificates, GnuPG keys, SCM providers.
  listCertificates: [{ op: 'listCertificates', call: () => trustApi.listCertificates({ certType: 'ssh', hostNamePattern: '*.example.test' }) }],
  createCertificates: [
    {
      op: 'createCertificates',
      call: () =>
        trustApi.createCertificates(
          { items: [{ serverName: 'git.internal.test', certType: 'https', certData: '-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----' }] },
          { upsert: true },
        ),
    },
  ],
  deleteCertificates: [{ op: 'deleteCertificates', call: () => trustApi.deleteCertificates({ hostNamePattern: 'git.example.test', certType: 'ssh', certSubType: 'ssh-ed25519' }) }],
  listGPGKeys: [{ op: 'listGPGKeys', call: () => trustApi.listGPGKeys() }],
  createGPGKey: [{ op: 'createGPGKeys', call: () => trustApi.createGPGKey({ keyData: '-----BEGIN PGP PUBLIC KEY BLOCK-----\nx\n-----END PGP PUBLIC KEY BLOCK-----' }, { upsert: true }) }],
  getGPGKey: [{ op: 'getGPGKey', call: () => trustApi.getGPGKey('4AEE18F83AFDEB23') }],
  deleteGPGKey: [{ op: 'deleteGPGKey', call: () => trustApi.deleteGPGKey('4AEE18F83AFDEB23') }],
  listSCMProviders: [{ op: 'listSCMProviders', call: () => trustApi.listSCMProviders() }],
  createSCMProvider: [
    {
      op: 'createSCMProvider',
      call: () => trustApi.createSCMProvider({ metadata: { name: 'gitea-lab' }, spec: { type: 'gitea', apiURL: 'https://gitea.example.test/api/v1', auth: { tokenSecretRef: { name: 'scm-gitea', key: 'token' } } } }),
    },
  ],
  getSCMProvider: [{ op: 'getSCMProvider', call: () => trustApi.getSCMProvider('github-main') }],
  updateSCMProvider: [
    {
      op: 'updateSCMProvider',
      call: () =>
        trustApi.updateSCMProvider('gitlab-internal', {
          metadata: { name: 'gitlab-internal', resourceVersion: '10' },
          spec: { type: 'gitlab', apiURL: 'https://gitlab.example.test/api/v4', auth: { tokenSecretRef: { name: 'scm-gitlab', key: 'token' } }, webhook: { secretRef: { name: 'scm-hook', key: 'secret' } } },
        }),
    },
  ],
  deleteSCMProvider: [{ op: 'deleteSCMProvider', call: () => trustApi.deleteSCMProvider('gitlab-internal') }],

  // Clusters.
  listClusters: [{ op: 'listClusters', call: () => clustersApi.listClusters({ name: 'prod-eu', limit: 10 }) }],
  createCluster: [
    { op: 'createCluster', call: () => clustersApi.createCluster({ server: 'https://new.example.test', name: 'new', config: { bearerToken: 'example-token', tlsClientConfig: { caData: 'Q0E=' } } }, { upsert: false }) },
  ],
  getCluster: [
    { op: 'getCluster', call: () => clustersApi.getCluster(PROD) },
    { op: 'getCluster', call: () => clustersApi.getCluster({ name: 'prod-eu' }) },
  ],
  updateCluster: [
    { op: 'updateCluster', call: () => clustersApi.updateCluster(PROD, { server: PROD.server, name: 'prod-europe', namespaces: ['payments'], resourceVersion: '10' }, ['name', 'namespaces']) },
  ],
  deleteCluster: [{ op: 'deleteCluster', call: () => clustersApi.deleteCluster(PROD) }],
  rotateClusterAuth: [{ op: 'rotateClusterAuth', call: () => clustersApi.rotateClusterAuth(PROD) }],
  invalidateClusterCache: [{ op: 'invalidateClusterCache', call: () => clustersApi.invalidateClusterCache(PROD) }],

  // Notifications, badge, extensions.
  listNotificationServices: [{ op: 'listNotificationServices', call: () => miscApi.listNotificationServices() }],
  listNotificationTriggers: [{ op: 'listNotificationTriggers', call: () => miscApi.listNotificationTriggers() }],
  listNotificationTemplates: [{ op: 'listNotificationTemplates', call: () => miscApi.listNotificationTemplates() }],
  webhookURL: { notInDocument: 'Builds the URL SCM providers POST deliveries to (receiveProviderWebhook); the UI only displays it.' },
  badgeURL: [{ op: 'getBadge', call: () => miscApi.badgeURL({ name: APP, appNamespace: 'patrol', project: ['default', 'finance'], revision: true, showAppName: true, width: 120 }) }],
  invokeExtension: (['GET', 'POST', 'PUT', 'PATCH', 'DELETE'] as const).map((method) => ({
    op: `invokeExtension${method[0]}${method.slice(1).toLowerCase()}` as OperationId,
    call: () =>
      miscApi.invokeExtension('metrics', '/api/v1/query/range', {
        method,
        ...(method === 'GET' || method === 'DELETE' ? {} : { body: { q: 'up' } }),
        application: { namespace: 'patrol', name: APP },
        project: 'default',
      }),
  })),
} satisfies Record<ClientFunction, Entry>;

/** Operations of the document the UI deliberately does not call, with the reason. */
const unusedOperations: Partial<Record<OperationId, string>> = {
  getVersionAlias: 'Legacy alias of getVersion (GET /api/version) for upstream CLIs; the UI calls GET /api/v1/version.',
  exchangeOIDCToken: 'CLI single sign-on (exchange an identity-provider ID token for a session); browser sessions use startSSOLogin and the session cookie.',
  ssoCallback: 'Identity-provider redirect target of the browser login flow; the UI never calls or links it.',
  getApplicationManifestsWithFiles: 'Renders a local directory uploaded as a gzipped tar (CLI --local); the browser UI has no local sources.',
  receiveWebhook: 'SCM/registry webhook receiver, called by the providers, never by the UI.',
  receiveProviderWebhook: 'SCM/registry webhook receiver, called by the providers; the UI only displays its URL (webhookURL).',
};

/** Operations whose request the default MSW handlers do not answer (dev mock mode), with the reason. */
const unmockedOperations: Partial<Record<OperationId, string>> = {
  startSSOLogin: 'Browser navigation to the identity provider (a URL, never fetched).',
  openTerminal: 'WebSocket; terminal tests stub WebSocket.',
  getBadge: 'Image URL, never fetched.',
  invokeExtensionGet: 'Proxy to an extension backend; extension tests stub it.',
  invokeExtensionPost: 'Proxy to an extension backend; extension tests stub it.',
  invokeExtensionPut: 'Proxy to an extension backend; extension tests stub it.',
  invokeExtensionPatch: 'Proxy to an extension backend; extension tests stub it.',
  invokeExtensionDelete: 'Proxy to an extension backend; extension tests stub it.',
};

interface Captured {
  method: string;
  url: URL;
  headers: Headers;
  body: string;
  request: Request;
}

const BASE = '/patrol/';

function setBase(href: string) {
  const base = document.createElement('base');
  base.href = href;
  document.head.prepend(base);
}

function resetMocks() {
  resetMockState();
  resetAppsCoreMockState();
  resetSettingsMockStore();
  resetAppsResourcesRequests();
}

/** Runs a case with every request captured (and answered with a dummy body). */
async function run(c: Case): Promise<{ captured: Captured[]; url?: URL }> {
  const captured: Captured[] = [];
  server.use(
    http.all('*', async ({ request }) => {
      const copy = request.clone();
      captured.push({ method: request.method, url: new URL(request.url), headers: request.headers, body: await request.clone().text(), request: copy });
      if ((request.headers.get('Accept') ?? '').includes('text/event-stream')) {
        return new HttpResponse('', { headers: { 'Content-Type': 'text/event-stream' } });
      }
      return HttpResponse.json({});
    }),
  );
  const sockets: string[] = [];
  vi.stubGlobal(
    'WebSocket',
    class {
      static readonly OPEN = 1;
      readyState = 0;
      constructor(url: string) {
        sockets.push(url);
      }
      close() {
        this.readyState = 3;
      }
      addEventListener() {
        // Events are not exercised here: only the URL the socket opens is checked.
      }
    },
  );
  const result = c.call();
  if (typeof result === 'string') return { captured, url: new URL(result) };
  if (sockets[0]) return { captured, url: new URL(sockets[0]) };
  if (result instanceof Promise) await result;
  else if (result && typeof result === 'object' && 'close' in result && typeof result.close === 'function') {
    await vi.waitFor(() => expect(captured.length).toBeGreaterThan(0));
    (result.close as () => void)();
  }
  return { captured };
}

/**
 * Operations whose request body is passed through unchanged (the extension proxy). The document
 * declares an optional opaque body (any media type, no schema) for the methods other than GET
 * (UI-3); its content is not validated.
 */
const OPAQUE_BODY = new Set<string>(['invokeExtensionPost', 'invokeExtensionPut', 'invokeExtensionPatch', 'invokeExtensionDelete', 'invokeExtensionGet']);

function checkRequest(op: SpecOperation, method: string, url: URL, headers: Headers | undefined, body: string): void {
  const label = `${op.operationId} (${op.method} ${op.path})`;
  expect(method, label).toBe(op.method);
  expect(url.pathname.startsWith(BASE), `${label}: ${url.pathname} is not under ${BASE}`).toBe(true);
  const relative = url.pathname.slice(BASE.length - 1);
  expect(matchPath(op, relative), `${label}: ${relative} does not match the path template`).not.toBeNull();
  const declared = queryParameterNames(op);
  for (const key of url.searchParams.keys()) expect(declared.has(key), `${label}: query parameter "${key}" is not declared`).toBe(true);
  if (!headers) return;
  if (method !== 'GET') expect(headers.get('X-Requested-With'), `${label}: CSRF header`).toBe('patrol');
  if (op.operation['x-patrol-stream'] && url.searchParams.get('download') !== 'true') {
    expect(headers.get('Accept'), `${label}: stream Accept header`).toContain('text/event-stream');
  }
  const schema = requestSchema(op, 'application/json');
  if (OPAQUE_BODY.has(op.operationId)) return;
  if (schema) {
    expect(headers.get('Content-Type'), `${label}: request media type`).toContain('application/json');
    const errors = validateSchema(schema, JSON.parse(body) as unknown, { strict: true });
    expect(errors, `${label}: request body`).toEqual([]);
  } else {
    expect(body, `${label}: the operation takes no request body`).toBe('');
  }
}

/** Validates what the default MSW handlers answer to a captured request. */
async function checkMockResponse(op: SpecOperation, request: Request): Promise<'mocked' | 'unmocked'> {
  resetMocks();
  const response = await getResponse(handlers, request.clone());
  if (!response) return 'unmocked';
  const label = `${op.operationId}: mock ${response.status}`;
  const type = response.headers.get('Content-Type') ?? '';
  if (response.status >= 400) {
    expect(type, label).toContain('application/problem+json');
    const schema = responseSchema(op, response.status, 'application/problem+json');
    expect(schema, label).toBeDefined();
    expect(validateSchema(schema!, await response.json(), { strict: true }), label).toEqual([]);
    return 'mocked';
  }
  if (type.includes('text/event-stream')) {
    expect(op.operation['x-patrol-stream'], `${label}: event stream for a non-stream operation`).toBe(true);
    const isWatch = op.path.startsWith('/api/v1/stream/');
    if (isWatch) {
      await response.body?.cancel();
      return 'mocked';
    }
    // Log routes: LogEntry events, validated against the documented text/event-stream schema.
    const eventSchema = responseSchema(op, 200, 'text/event-stream');
    expect(eventSchema, `${label}: no documented event schema`).toBeDefined();
    const events = parseEventStream(await response.text());
    expect(events.length, label).toBeGreaterThan(0);
    for (const e of events) expect(validateSchema(eventSchema!, JSON.parse(e.data), { strict: true }), `${label} event`).toEqual([]);
    expect(JSON.parse(events.at(-1)?.data ?? '{}')).toMatchObject({ last: true });
    return 'mocked';
  }
  const mediaType = type.includes('json') ? 'application/json' : type.split(';')[0] ?? '';
  const schema = responseSchema(op, response.status, mediaType);
  expect(schema, `${label}: ${mediaType} is not a declared response of status ${response.status}`).toBeDefined();
  if (mediaType === 'application/json') {
    expect(validateSchema(schema!, await response.json(), { strict: true }), label).toEqual([]);
  }
  return 'mocked';
}

const allCases = Object.entries(clientCases).flatMap(([fn, entry]) => ('notInDocument' in entry ? [] : entry.map((c, i) => ({ fn, index: i, c }))));

describe('client ↔ published OpenAPI document', () => {
  beforeEach(() => {
    resetMocks();
    setBase(BASE);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('maps every exported client function (compile time: satisfies Record<ClientFunction, Entry>)', () => {
    const exported = Object.values(modules)
      .flatMap((m) => Object.entries(m))
      .filter(([, v]) => typeof v === 'function')
      .map(([k]) => k)
      .sort();
    expect(Object.keys(clientCases).sort()).toEqual(exported);
  });

  it('uses every operation of the document or lists why not', () => {
    const used = new Set(allCases.map(({ c }) => c.op));
    const documented = specOperations().map((o) => o.operationId);
    const unused = Object.keys(unusedOperations);
    expect(documented.filter((id) => !used.has(id as OperationId) && !unused.includes(id)), 'operations neither used nor listed').toEqual([]);
    expect(unused.filter((id) => used.has(id as OperationId)), 'listed as unused but called').toEqual([]);
    expect(unused.filter((id) => !documented.includes(id)), 'listed but not in the document').toEqual([]);
    for (const reason of Object.values(unusedOperations)) expect(reason).toMatch(/\S/);
  });

  it.each(allCases.map(({ fn, index, c }) => [`${fn}#${index} → ${c.op}`, c] as const))('%s sends the documented request', async (_name, c) => {
    const op = findOperation(c.op);
    const { captured, url } = await run(c);
    if (url) {
      // URL builders and the WebSocket: no fetch; the URL itself must match the operation.
      expect(captured).toHaveLength(0);
      if (url.protocol === 'ws:' || url.protocol === 'wss:') expect(op.operationId).toBe('openTerminal');
      checkRequest(op, 'GET', url, undefined, '');
      return;
    }
    expect(captured, `${c.op}: exactly one request`).toHaveLength(1);
    const req = captured[0];
    if (!req) return;
    checkRequest(op, req.method, req.url, req.headers, req.body);
  });

  // One sequential, CPU-bound sweep over every client case (~3.5 s idle, over 10 s under heavy
  // load), so it gets its own timeout instead of the per-test default.
  it('answers every called operation from the default mocks with documented shapes', { timeout: 60_000 }, async () => {
    const called = new Set<string>();
    const mocked = new Set<string>();
    for (const { c } of allCases) {
      const op = findOperation(c.op);
      called.add(c.op);
      resetMocks();
      const { captured } = await run(c);
      server.resetHandlers();
      const req = captured[0];
      if (req && (await checkMockResponse(op, req.request)) === 'mocked') mocked.add(c.op);
    }
    expect([...called].filter((id) => !mocked.has(id)).sort()).toEqual(Object.keys(unmockedOperations).sort());
  });

  it('keeps the fixtures that double as request bodies valid', () => {
    for (const app of appsCoreApplications().items) {
      const schema = requestSchema(findOperation('updateApplication'), 'application/json')!;
      expect(validateSchema(schema, app, { strict: true }), app.metadata.name).toEqual([]);
    }
  });
});
