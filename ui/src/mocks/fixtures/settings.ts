import type {
  Account,
  AppProject,
  ClusterCredentialsInfo,
  ClusterSpec,
  ConnectionState,
  GnuPGPublicKey,
  KubernetesEvent,
  ObjectMeta,
  RegisteredCluster,
  RepositoryCertificate,
  RepositoryCredentialInfo,
  RepositoryInfo,
  RepositorySpec,
  SCMProvider,
} from '../../api/types';

/**
 * Settings fixtures for tests and the dev mock mode. All fake: example.test hosts, no real
 * accounts, keys or secrets (key material below is random placeholder base64).
 */

const NS = 'patrol';
const meta = (name: string, extra: Partial<ObjectMeta> = {}): ObjectMeta => ({
  name,
  namespace: NS,
  uid: `uid-${name}`,
  resourceVersion: '10',
  creationTimestamp: '2026-09-01T10:00:00Z',
  ...extra,
});

/**
 * A repository as the repository routes return it (RepositoryInfo): the object, the connection
 * state copied to the top level, and credential flags. `hasCredentials` follows the server rule
 * (credentialsRef, scmProviderRef or Azure workload identity).
 */
export function repositoryFixture(
  name: string,
  spec: RepositorySpec,
  connection: ConnectionState = { status: 'Successful', attemptedAt: '2026-10-15T08:00:00Z' },
  inheritedCredentials = false,
): RepositoryInfo {
  const hasCredentials = Boolean(spec.credentialsRef ?? spec.scmProviderRef ?? spec.useAzureWorkloadIdentity);
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Repository',
    metadata: meta(name),
    spec,
    status: { connectionState: connection },
    connectionState: connection,
    hasCredentials,
    ...(!hasCredentials && inheritedCredentials ? { inheritedCredentials: true } : {}),
  };
}

export function repositoriesFixture(): RepositoryInfo[] {
  return [
    repositoryFixture('apps', {
      url: 'https://git.example.test/platform/apps.git',
      type: 'git',
      project: 'default',
      credentialsRef: { name: 'repo-apps' },
    }),
    repositoryFixture(
      'infra',
      { url: 'git@git.example.test:platform/infra.git', type: 'git', credentialsRef: { name: 'repo-infra' } },
      {
        status: 'Failed',
        message: 'ssh: handshake failed: knownhosts: key is unknown',
        attemptedAt: '2026-10-15T08:01:00Z',
      },
    ),
    repositoryFixture('stable', { url: 'https://charts.example.test/stable', type: 'helm', name: 'stable', project: 'finance' }),
    repositoryFixture(
      'registry',
      { url: 'oci://registry.example.test/charts', type: 'oci' },
      { status: 'Unknown', message: 'connection not checked yet' },
    ),
    repositoryFixture(
      'platform-tools',
      { url: 'https://git.example.test/platform/tools.git', type: 'git' },
      { status: 'Successful', attemptedAt: '2026-10-15T08:03:00Z' },
      true,
    ),
  ];
}

export function writeRepositoriesFixture(): RepositoryInfo[] {
  return [
    {
      ...repositoryFixture('apps-write', {
        url: 'https://git.example.test/platform/apps.git',
        type: 'git',
        purpose: 'write',
        credentialsRef: { name: 'repo-apps-write' },
      }),
    },
  ];
}

/** Credential templates as the repocreds routes return them (RepositoryCredentialInfo). */
export function repoCredsFixture(): RepositoryCredentialInfo[] {
  return [
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'RepositoryCredential',
      metadata: meta('platform-https'),
      spec: { urlPrefix: 'https://git.example.test/platform', type: 'git', credentialsRef: { name: 'creds-platform' } },
      hasCredentials: true,
      username: 'deploy-bot',
    },
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'RepositoryCredential',
      metadata: meta('github-app'),
      spec: {
        urlPrefix: 'https://github.example.test/acme',
        type: 'git',
        githubApp: { appID: 12345, enterpriseBaseURL: 'https://github.example.test/api/v3' },
        credentialsRef: { name: 'creds-github-app' },
      },
      hasCredentials: true,
    },
  ];
}

/**
 * Stored certificates. The list route omits `certData` (it is returned by create and delete
 * only); handlers strip it. certSubType is the SSH key type, or the public key algorithm of a
 * TLS certificate.
 */
export function certificatesFixture(): RepositoryCertificate[] {
  return [
    {
      serverName: 'git.example.test',
      certType: 'https',
      certSubType: 'rsa',
      certData: '-----BEGIN CERTIFICATE-----\nTUlJQmZha2VDZXJ0aWZpY2F0ZQ==\n-----END CERTIFICATE-----',
      certInfo: 'CN=git.example.test, O=Example Test CA',
    },
    {
      serverName: 'git.example.test',
      certType: 'ssh',
      certSubType: 'ssh-ed25519',
      certData: 'AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyMaterialForTestsOnly0000000000000000',
      certInfo: 'SHA256:aGVsbG8td29ybGQtZmFrZS1maW5nZXJwcmludA',
    },
    {
      serverName: 'github.example.test',
      certType: 'ssh',
      certSubType: 'ecdsa-sha2-nistp256',
      certData: 'AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBFakeKey0000',
      certInfo: 'SHA256:ZmFrZS1lY2RzYS1maW5nZXJwcmludC1mb3ItdGVzdHM',
    },
  ];
}

export function gpgKeysFixture(): GnuPGPublicKey[] {
  return [
    { keyID: '4AEE18F83AFDEB23', fingerprint: '5DE3 E050 9C47 EA3C F04A  42D3 4AEE 18F8 3AFD EB23', owner: 'Release Bot <release@example.test>', trust: 'ultimate', subType: 'rsa4096' },
    { keyID: '7C1F2A6B9D0E3F44', fingerprint: '1A2B 3C4D 5E6F 7081 92A3  B4C5 7C1F 2A6B 9D0E 3F44', owner: 'Platform Team <platform@example.test>', trust: 'unknown', subType: 'ed25519' },
  ];
}

/** A cluster as the cluster routes return it (RegisteredCluster): credential flags, never values. */
export function clusterFixture(
  name: string,
  spec: Partial<ClusterSpec> & { server: string },
  status: RegisteredCluster['status'] = {},
  extra: Partial<ObjectMeta> = {},
  credentials: ClusterCredentialsInfo = {},
): RegisteredCluster {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Cluster',
    metadata: meta(name, extra),
    spec: { name, ...spec },
    status,
    credentials,
  };
}

export function clustersFixture(): RegisteredCluster[] {
  return [
    clusterFixture(
      'in-cluster',
      { server: 'https://kubernetes.default.svc', namespaces: [] },
      {
        connectionState: { status: 'Successful', attemptedAt: '2026-10-15T08:00:00Z' },
        serverVersion: '1.33',
        info: { applicationsCount: 3, apiVersions: ['v1', 'apps/v1'], cacheInfo: { apisCount: 58, resourcesCount: 412, lastCacheSyncTime: '2026-10-15T07:55:00Z' } },
        assignedShard: 0,
      },
    ),
    clusterFixture(
      'prod-eu',
      {
        server: 'https://prod-eu.k8s.example.test',
        namespaces: ['payments', 'search'],
        project: 'finance',
        shard: 1,
        config: { credentialsRef: { name: 'cluster-prod-eu' }, tlsClientConfig: { serverName: 'prod-eu.k8s.example.test' } },
      },
      {
        connectionState: { status: 'Failed', message: 'dial tcp 10.0.0.12:443: i/o timeout', attemptedAt: '2026-10-15T08:02:00Z' },
        serverVersion: '1.32',
        info: { applicationsCount: 1, cacheInfo: { apisCount: 0, resourcesCount: 0 } },
        assignedShard: 1,
      },
      { labels: { env: 'prod', region: 'eu' }, annotations: { 'team.example.test/owner': 'payments' } },
      { hasBearerToken: true, hasCAData: true },
    ),
  ];
}

export function projectsFixture(): AppProject[] {
  return [
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'AppProject',
      metadata: meta('default'),
      spec: {
        description: 'Default project: any source, any destination.',
        sourceRepos: ['*'],
        destinations: [{ server: '*', namespace: '*' }],
        clusterResourceWhitelist: [{ group: '*', kind: '*' }],
      },
    },
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'AppProject',
      metadata: meta('finance', { labels: { team: 'finance' } }),
      spec: {
        description: 'Payments and ledger services.',
        sourceRepos: ['https://git.example.test/platform/*', '!https://git.example.test/platform/sandbox.git'],
        sourceNamespaces: ['finance-apps'],
        destinations: [
          { server: 'https://kubernetes.default.svc', namespace: 'payments' },
          { name: 'prod-eu', namespace: 'payments-*' },
        ],
        destinationServiceAccounts: [{ server: 'https://kubernetes.default.svc', namespace: 'payments', defaultServiceAccount: 'payments-deployer' }],
        clusterResourceWhitelist: [{ group: '', kind: 'Namespace' }],
        namespaceResourceBlacklist: [{ group: '', kind: 'ResourceQuota' }],
        roles: [
          {
            name: 'deployer',
            description: 'CI deploys',
            policies: ['p, proj:finance:deployer, applications, sync, finance/*, allow'],
            groups: ['finance-ci'],
            jwtTokens: [{ iat: 1760000000, exp: 1823072000, id: 'ci-pipeline' }],
          },
        ],
        syncWindows: [
          { kind: 'deny', schedule: '0 22 * * *', duration: '8h', timeZone: 'Europe/Berlin', applications: ['payments'], manualSync: true, description: 'Nightly freeze' },
          { kind: 'allow', schedule: '0 9 * * 1-5', duration: '9h', namespaces: ['payments'], clusters: ['prod-eu'], timeZone: 'UTC' },
        ],
        signatureKeys: [{ keyID: '4AEE18F83AFDEB23' }],
        orphanedResources: { warn: true, ignore: [{ group: '', kind: 'ConfigMap', name: 'kube-root-ca.crt' }] },
      },
      status: { jwtTokensByRole: { deployer: { items: [{ iat: 1760000000, exp: 1823072000, id: 'ci-pipeline' }] } } },
    },
  ];
}

export function projectEventsFixture(project: string): KubernetesEvent[] {
  return [
    {
      metadata: { name: `${project}.1`, namespace: NS, uid: `${project}-ev-1` },
      involvedObject: { kind: 'AppProject', name: project, namespace: NS },
      reason: 'ResourceUpdated',
      message: 'admin updated project',
      type: 'Normal',
      count: 2,
      firstTimestamp: '2026-10-14T09:00:00Z',
      lastTimestamp: '2026-10-15T09:00:00Z',
      reportingComponent: 'patrol-server',
      reportingInstance: '',
    },
  ];
}

export function accountsFixture(): Account[] {
  return [
    { name: 'admin', enabled: true, capabilities: ['login', 'apiKey'], tokens: [{ id: 'laptop', issuedAt: 1759000000, expiresAt: 0 }] },
    { name: 'ci-bot', enabled: true, capabilities: ['apiKey'], tokens: [{ id: 'pipeline', issuedAt: 1759100000, expiresAt: 1822172000 }] },
    { name: 'viewer', enabled: false, capabilities: ['login'], tokens: [] },
  ];
}

export function scmProvidersFixture(): SCMProvider[] {
  return [
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'SCMProvider',
      metadata: meta('github-main'),
      spec: {
        type: 'github',
        apiURL: 'https://api.github.example.test',
        auth: { githubApp: { appID: 4242, installationID: 99, privateKeySecretRef: { name: 'scm-github-app', key: 'privateKey' } } },
        webhook: { secretRef: { name: 'scm-github-webhook', key: 'secret' } },
        statusReporting: { enabled: true, mode: 'checks', contextPrefix: 'patrol' },
      },
      status: {
        capabilities: ['repositories', 'branches', 'pullRequests', 'webhooks', 'commitStatus'],
        rateLimit: { limit: 5000, remaining: 4875, resetAt: '2026-10-15T09:00:00Z' },
        lastCheckedAt: '2026-10-15T08:30:00Z',
        conditions: [{ type: 'Ready', status: 'True', reason: 'Authenticated', message: 'GitHub App installation token issued', lastTransitionTime: '2026-10-15T08:30:00Z' }],
      },
    },
    {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'SCMProvider',
      metadata: meta('gitlab-internal'),
      spec: {
        type: 'gitlab',
        apiURL: 'https://gitlab.example.test/api/v4',
        auth: { tokenSecretRef: { name: 'scm-gitlab', key: 'token' } },
        statusReporting: { enabled: false },
      },
      status: {
        capabilities: ['repositories', 'branches', 'pullRequests'],
        conditions: [{ type: 'Ready', status: 'False', reason: 'AuthenticationFailed', message: '401 Unauthorized', lastTransitionTime: '2026-10-15T08:31:00Z' }],
      },
    },
  ];
}

export const notificationCatalogFixture = {
  services: [{ name: 'slack' }, { name: 'webhook.ops' }],
  triggers: [{ name: 'on-sync-failed' }, { name: 'on-health-degraded' }, { name: 'on-deployed' }],
  templates: [{ name: 'app-sync-failed' }, { name: 'app-health-degraded' }, { name: 'app-deployed' }],
};
