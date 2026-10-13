// Route table mirroring docs/architecture/api-contract.md. gen-api-types.mjs turns it into the
// `paths` section of the temporary OpenAPI document; src/api/coverage.ts then requires one typed
// client function per (method, path). scripts/api-routes.test.mjs checks this table against the
// contract document so the two cannot drift silently.
//
// Response kinds: a schema name (JSON), 'empty', 'sse:<Schema>', 'redirect', 'websocket',
// 'svg'. Request bodies: a schema name or 'gzip'.

/** @typedef {{ name: string, schema: Record<string, unknown>, description?: string }} QueryParam */
/**
 * @typedef {object} Route
 * @property {'GET'|'POST'|'PUT'|'PATCH'|'DELETE'} method
 * @property {string} path
 * @property {string} operationId
 * @property {string} tag
 * @property {string} summary
 * @property {QueryParam[]} [query]
 * @property {string} [body]
 * @property {string} response
 * @property {boolean} [prose] Documented in contract prose rather than a route table row.
 */

const str = { type: 'string' };
const bool = { type: 'boolean' };
const int = { type: 'integer', format: 'int64' };
const strs = { type: 'array', items: str };
const ints = { type: 'array', items: { type: 'integer', format: 'int32' } };

/** @param {string} name @param {Record<string, unknown>} schema @param {string} [description] @returns {QueryParam} */
const q = (name, schema, description) => ({ name, schema, ...(description ? { description } : {}) });

const appScope = [q('appNamespace', str), q('project', str)];
const resourceId = [
  q('namespace', str),
  q('resourceName', str),
  q('version', str),
  q('group', str),
  q('kind', str),
];
const listParams = [q('limit', int), q('continue', str), q('selector', str, 'Label selector')];
const logParams = [
  q('container', str),
  q('sinceSeconds', int),
  q('tailLines', int),
  q('follow', bool),
  q('filter', str),
  q('previous', bool),
  q('kind', str),
  q('group', str),
  q('resourceName', str),
  q('namespace', str),
  ...appScope,
];

/** @type {Route[]} */
const session = [
  { method: 'POST', path: '/api/v1/session', operationId: 'createSession', tag: 'session', summary: 'Local login', query: [q('cookie', bool, 'Set the patrol.token session cookie')], body: 'SessionCreateRequest', response: 'SessionResponse' },
  { method: 'DELETE', path: '/api/v1/session', operationId: 'deleteSession', tag: 'session', summary: 'Logout (revokes the session)', response: 'LogoutResponse' },
  { method: 'GET', path: '/api/v1/session/userinfo', operationId: 'getUserInfo', tag: 'session', summary: 'Current user', response: 'UserInfo' },
  { method: 'GET', path: '/api/v1/auth/login', operationId: 'startSSOLogin', tag: 'session', summary: 'Start the OIDC login flow (browser navigation)', query: [q('return_url', str)], response: 'redirect' },
  { method: 'GET', path: '/api/v1/auth/callback', operationId: 'ssoCallback', tag: 'session', summary: 'OIDC callback (IdP redirect target)', query: [q('code', str), q('state', str), q('error', str), q('error_description', str)], response: 'redirect' },
  { method: 'POST', path: '/api/v1/session/oidc-exchange', operationId: 'exchangeOIDCToken', tag: 'session', summary: 'Exchange an ID token for a Patrol session (CLI SSO)', body: 'OIDCExchangeRequest', response: 'SessionResponse' },
  { method: 'GET', path: '/api/v1/account', operationId: 'listAccounts', tag: 'accounts', summary: 'List accounts', response: 'AccountList' },
  { method: 'GET', path: '/api/v1/account/{name}', operationId: 'getAccount', tag: 'accounts', summary: 'Get account', response: 'Account' },
  { method: 'GET', path: '/api/v1/account/can-i/{resource}/{action}/{subresource}', operationId: 'canI', tag: 'accounts', summary: 'RBAC check for the caller (subresource may contain slashes)', response: 'CanIResponse' },
  { method: 'PUT', path: '/api/v1/account/password', operationId: 'updatePassword', tag: 'accounts', summary: 'Change password', body: 'UpdatePasswordRequest', response: 'empty' },
  { method: 'POST', path: '/api/v1/account/{name}/token', operationId: 'createAccountToken', tag: 'accounts', summary: 'Create API token', body: 'CreateAccountTokenRequest', response: 'TokenResponse' },
  { method: 'DELETE', path: '/api/v1/account/{name}/token/{id}', operationId: 'deleteAccountToken', tag: 'accounts', summary: 'Delete API token', response: 'empty' },
  { method: 'GET', path: '/api/v1/settings', operationId: 'getSettings', tag: 'settings', summary: 'UI/CLI settings (public subset when unauthenticated)', response: 'Settings' },
  { method: 'GET', path: '/api/v1/settings/plugins', operationId: 'getPluginSettings', tag: 'settings', summary: 'Config management plugins', response: 'PluginSettings' },
  { method: 'GET', path: '/api/v1/version', operationId: 'getVersion', tag: 'version', summary: 'Build and tool versions', response: 'Version' },
  { method: 'GET', path: '/api/version', operationId: 'getVersionAlias', tag: 'version', summary: 'Build and tool versions (alias)', response: 'Version' },
];

/** @type {Route[]} */
const applications = [
  { method: 'GET', path: '/api/v1/applications', operationId: 'listApplications', tag: 'applications', summary: 'List applications', query: [q('projects', strs), q('repo', str), q('appNamespace', str), q('name', str), ...listParams], response: 'ApplicationList' },
  { method: 'GET', path: '/api/v1/stream/applications', operationId: 'watchApplications', tag: 'applications', summary: 'Watch applications (SSE)', query: [q('projects', strs), q('repo', str), q('appNamespace', str), q('name', str), q('selector', str), q('resourceVersion', str)], response: 'sse:ApplicationWatchEvent' },
  { method: 'POST', path: '/api/v1/applications', operationId: 'createApplication', tag: 'applications', summary: 'Create application', query: [q('upsert', bool), q('validate', bool)], body: 'Application', response: 'Application' },
  { method: 'GET', path: '/api/v1/applications/{name}', operationId: 'getApplication', tag: 'applications', summary: 'Get application (optionally refresh first)', query: [q('refresh', { type: 'string', enum: ['normal', 'hard'] }), ...appScope], response: 'Application' },
  { method: 'PUT', path: '/api/v1/applications/{name}', operationId: 'updateApplication', tag: 'applications', summary: 'Replace application', query: [q('validate', bool), ...appScope], body: 'Application', response: 'Application' },
  { method: 'PUT', path: '/api/v1/applications/{name}/spec', operationId: 'updateApplicationSpec', tag: 'applications', summary: 'Replace application spec', query: [q('validate', bool), ...appScope], body: 'ApplicationSpec', response: 'ApplicationSpec' },
  { method: 'PATCH', path: '/api/v1/applications/{name}', operationId: 'patchApplication', tag: 'applications', summary: 'Patch application', query: appScope, body: 'ApplicationPatchRequest', response: 'Application' },
  { method: 'DELETE', path: '/api/v1/applications/{name}', operationId: 'deleteApplication', tag: 'applications', summary: 'Delete application', query: [q('cascade', bool), q('propagationPolicy', { type: 'string', enum: ['foreground', 'background'] }), ...appScope], response: 'empty' },
  { method: 'POST', path: '/api/v1/applications/{name}/sync', operationId: 'syncApplication', tag: 'applications', summary: 'Sync (creates an Operation)', query: appScope, body: 'ApplicationSyncRequest', response: 'Operation' },
  { method: 'POST', path: '/api/v1/applications/{name}/rollback', operationId: 'rollbackApplication', tag: 'applications', summary: 'Roll back to a history entry', query: appScope, body: 'ApplicationRollbackRequest', response: 'Operation' },
  { method: 'DELETE', path: '/api/v1/applications/{name}/operation', operationId: 'terminateOperation', tag: 'applications', summary: 'Terminate the running operation', query: appScope, response: 'empty' },
  { method: 'GET', path: '/api/v1/applications/{name}/operations', operationId: 'listApplicationOperations', tag: 'applications', summary: 'List operations', query: appScope, response: 'OperationList' },
  { method: 'GET', path: '/api/v1/applications/{name}/operations/{op}', operationId: 'getApplicationOperation', tag: 'applications', summary: 'Get operation', query: appScope, response: 'Operation' },
  { method: 'GET', path: '/api/v1/applications/{name}/syncwindows', operationId: 'getApplicationSyncWindows', tag: 'applications', summary: 'Sync window state', query: appScope, response: 'ApplicationSyncWindowsState' },
  { method: 'GET', path: '/api/v1/applications/{name}/revisions/{revision}/metadata', operationId: 'getRevisionMetadata', tag: 'applications', summary: 'Git revision metadata', query: [q('sourceIndex', int), q('versionId', int), ...appScope], response: 'RevisionMetadata' },
  { method: 'GET', path: '/api/v1/applications/{name}/revisions/{revision}/chartdetails', operationId: 'getRevisionChartDetails', tag: 'applications', summary: 'Helm chart details', query: [q('sourceIndex', int), q('versionId', int), ...appScope], response: 'ChartDetails' },
  { method: 'GET', path: '/api/v1/applications/{name}/revisions/{revision}/ocimetadata', operationId: 'getOCIMetadata', tag: 'applications', summary: 'OCI artifact metadata', query: [q('sourceIndex', int), q('versionId', int), ...appScope], response: 'OCIMetadata' },
  { method: 'GET', path: '/api/v1/applications/{name}/manifests', operationId: 'getManifests', tag: 'applications', summary: 'Rendered manifests', query: [q('revision', str), q('sourcePositions', ints), q('revisions', strs), ...appScope], response: 'ManifestResponse' },
  { method: 'POST', path: '/api/v1/applications/{name}/manifests-with-files', operationId: 'getManifestsWithFiles', tag: 'applications', summary: 'Render manifests from uploaded local sources', query: appScope, body: 'gzip', response: 'ManifestResponse' },
  { method: 'GET', path: '/api/v1/applications/{name}/managed-resources', operationId: 'getManagedResources', tag: 'applications', summary: 'Managed resources with diff data', query: [...resourceId, ...appScope], response: 'ManagedResourcesResponse' },
  { method: 'POST', path: '/api/v1/applications/{name}/server-side-diff', operationId: 'serverSideDiff', tag: 'applications', summary: 'Server-side diff', query: appScope, body: 'ServerSideDiffRequest', response: 'ServerSideDiffResponse' },
  { method: 'GET', path: '/api/v1/applications/{name}/resource-tree', operationId: 'getResourceTree', tag: 'applications', summary: 'Resource tree', query: appScope, response: 'ApplicationTree' },
  { method: 'GET', path: '/api/v1/stream/applications/{name}/resource-tree', operationId: 'watchResourceTree', tag: 'applications', summary: 'Watch resource tree (SSE)', query: appScope, response: 'sse:ApplicationTreeEvent' },
  { method: 'GET', path: '/api/v1/applications/{name}/events', operationId: 'listApplicationEvents', tag: 'applications', summary: 'Kubernetes events', query: [q('resourceNamespace', str), q('resourceName', str), q('resourceUID', str), ...appScope], response: 'EventList' },
  { method: 'GET', path: '/api/v1/applications/{name}/resource', operationId: 'getResource', tag: 'applications', summary: 'Live manifest (secrets masked)', query: [...resourceId, ...appScope], response: 'ResourceManifest' },
  { method: 'POST', path: '/api/v1/applications/{name}/resource', operationId: 'patchResource', tag: 'applications', summary: 'Patch live resource', query: [...resourceId, ...appScope], body: 'ResourcePatchRequest', response: 'ResourceManifest' },
  { method: 'DELETE', path: '/api/v1/applications/{name}/resource', operationId: 'deleteResource', tag: 'applications', summary: 'Delete live resource', query: [...resourceId, q('force', bool), q('orphan', bool), ...appScope], response: 'empty' },
  { method: 'GET', path: '/api/v1/applications/{name}/resource/actions', operationId: 'listResourceActions', tag: 'applications', summary: 'Available resource actions', query: [...resourceId, ...appScope], response: 'ResourceActionsList' },
  { method: 'POST', path: '/api/v1/applications/{name}/resource/actions', operationId: 'runResourceAction', tag: 'applications', summary: 'Run resource action', query: [...resourceId, ...appScope], body: 'RunResourceActionRequest', response: 'empty' },
  { method: 'GET', path: '/api/v1/applications/{name}/logs', operationId: 'getApplicationLogs', tag: 'applications', summary: 'Aggregated logs (SSE)', query: [q('podName', str), ...logParams], response: 'sse:LogEntry' },
  { method: 'GET', path: '/api/v1/applications/{name}/pods/{pod}/logs', operationId: 'getPodLogs', tag: 'applications', summary: 'Pod logs (SSE)', query: logParams, response: 'sse:LogEntry' },
  { method: 'GET', path: '/api/v1/applications/{name}/links', operationId: 'listApplicationLinks', tag: 'applications', summary: 'Deep links', query: appScope, response: 'LinksResponse' },
  { method: 'GET', path: '/api/v1/applications/{name}/resource/links', operationId: 'listResourceLinks', tag: 'applications', summary: 'Resource deep links', query: [...resourceId, ...appScope], response: 'LinksResponse' },
  { method: 'GET', path: '/terminal', operationId: 'openTerminal', tag: 'applications', summary: 'Pod exec terminal (WebSocket)', query: [q('appName', str), q('appNamespace', str), q('projectName', str), q('namespace', str), q('pod', str), q('container', str), q('shell', str)], response: 'websocket' },
];

/** @type {Route[]} */
const applicationSets = [
  { method: 'GET', path: '/api/v1/applicationsets', operationId: 'listApplicationSets', tag: 'applicationsets', summary: 'List ApplicationSets', query: [q('projects', strs), q('appsetNamespace', str), ...listParams], response: 'ApplicationSetList' },
  { method: 'GET', path: '/api/v1/stream/applicationsets', operationId: 'watchApplicationSets', tag: 'applicationsets', summary: 'Watch ApplicationSets (SSE)', query: [q('projects', strs), q('appsetNamespace', str), q('selector', str), q('resourceVersion', str)], response: 'sse:ApplicationSetWatchEvent' },
  { method: 'POST', path: '/api/v1/applicationsets', operationId: 'createApplicationSet', tag: 'applicationsets', summary: 'Create ApplicationSet', query: [q('upsert', bool), q('dryRun', bool)], body: 'ApplicationSet', response: 'ApplicationSet' },
  { method: 'GET', path: '/api/v1/applicationsets/{name}', operationId: 'getApplicationSet', tag: 'applicationsets', summary: 'Get ApplicationSet', query: [q('appsetNamespace', str)], response: 'ApplicationSet' },
  { method: 'DELETE', path: '/api/v1/applicationsets/{name}', operationId: 'deleteApplicationSet', tag: 'applicationsets', summary: 'Delete ApplicationSet', query: [q('appsetNamespace', str)], response: 'empty' },
  { method: 'POST', path: '/api/v1/applicationsets/generate', operationId: 'generateApplicationSet', tag: 'applicationsets', summary: 'Dry-run render of generated Applications', body: 'ApplicationSetGenerateRequest', response: 'ApplicationSetGenerateResponse' },
  { method: 'GET', path: '/api/v1/applicationsets/{name}/resource-tree', operationId: 'getApplicationSetResourceTree', tag: 'applicationsets', summary: 'ApplicationSet resource tree', query: [q('appsetNamespace', str)], response: 'ApplicationTree' },
  { method: 'GET', path: '/api/v1/applicationsets/{name}/events', operationId: 'listApplicationSetEvents', tag: 'applicationsets', summary: 'ApplicationSet events', query: [q('appsetNamespace', str)], response: 'EventList' },
];

/** @type {Route[]} */
const projects = [
  { method: 'GET', path: '/api/v1/projects', operationId: 'listProjects', tag: 'projects', summary: 'List projects', query: listParams, response: 'AppProjectList' },
  { method: 'POST', path: '/api/v1/projects', operationId: 'createProject', tag: 'projects', summary: 'Create project', query: [q('upsert', bool)], body: 'AppProject', response: 'AppProject' },
  { method: 'GET', path: '/api/v1/projects/{name}', operationId: 'getProject', tag: 'projects', summary: 'Get project', response: 'AppProject' },
  { method: 'PUT', path: '/api/v1/projects/{name}', operationId: 'updateProject', tag: 'projects', summary: 'Replace project', body: 'AppProject', response: 'AppProject' },
  { method: 'DELETE', path: '/api/v1/projects/{name}', operationId: 'deleteProject', tag: 'projects', summary: 'Delete project', response: 'empty' },
  { method: 'GET', path: '/api/v1/projects/{name}/detailed', operationId: 'getDetailedProject', tag: 'projects', summary: 'Project with global projects and scoped repositories/clusters', response: 'DetailedProject' },
  { method: 'GET', path: '/api/v1/projects/{name}/globalprojects', operationId: 'getGlobalProjects', tag: 'projects', summary: 'Global projects that apply', response: 'AppProjectList' },
  { method: 'GET', path: '/api/v1/projects/{name}/events', operationId: 'listProjectEvents', tag: 'projects', summary: 'Project events', response: 'EventList' },
  { method: 'GET', path: '/api/v1/projects/{name}/syncwindows', operationId: 'getProjectSyncWindows', tag: 'projects', summary: 'Active sync windows', response: 'SyncWindowsState' },
  { method: 'GET', path: '/api/v1/projects/{name}/links', operationId: 'listProjectLinks', tag: 'projects', summary: 'Project deep links', response: 'LinksResponse' },
  { method: 'POST', path: '/api/v1/projects/{project}/roles/{role}/token', operationId: 'createProjectRoleToken', tag: 'projects', summary: 'Create project role token', body: 'ProjectTokenCreateRequest', response: 'TokenResponse' },
  { method: 'DELETE', path: '/api/v1/projects/{project}/roles/{role}/token/{iat}', operationId: 'deleteProjectRoleToken', tag: 'projects', summary: 'Delete project role token', query: [q('id', str)], response: 'empty' },
];

const repoScope = [q('appProject', str)];

/**
 * @param {'' | 'write-'} prefix
 * @param {boolean} prose
 * @returns {Route[]}
 */
function repositoryCrud(prefix, prose) {
  const base = `/api/v1/${prefix}repositories`;
  const id = prefix ? 'WriteRepository' : 'Repository';
  const extra = prose ? { prose: true } : {};
  return [
    { method: 'GET', path: base, operationId: `list${id === 'Repository' ? 'Repositories' : 'WriteRepositories'}`, tag: 'repositories', summary: `List ${prefix}repositories`, query: [q('forceRefresh', bool), ...repoScope], response: 'RepositoryList', ...extra },
    { method: 'POST', path: base, operationId: `create${id}`, tag: 'repositories', summary: `Create ${prefix}repository`, query: [q('upsert', bool), q('credsOnly', bool)], body: 'RepositoryWriteRequest', response: 'Repository', ...extra },
    { method: 'GET', path: `${base}/{repo}`, operationId: `get${id}`, tag: 'repositories', summary: `Get ${prefix}repository by URL`, query: [q('forceRefresh', bool), ...repoScope], response: 'Repository', ...extra },
    { method: 'PUT', path: `${base}/{repo}`, operationId: `update${id}`, tag: 'repositories', summary: `Update ${prefix}repository`, query: repoScope, body: 'RepositoryWriteRequest', response: 'Repository', ...extra },
    { method: 'DELETE', path: `${base}/{repo}`, operationId: `delete${id}`, tag: 'repositories', summary: `Delete ${prefix}repository`, query: repoScope, response: 'empty', ...extra },
    { method: 'POST', path: `${base}/{repo}/validate`, operationId: `validate${id}`, tag: 'repositories', summary: `Test ${prefix}repository connection without saving`, body: 'RepositoryWriteRequest', response: 'ConnectionState', ...extra },
  ];
}

/**
 * @param {'' | 'write-'} prefix
 * @param {boolean} prose
 * @returns {Route[]}
 */
function repoCredsCrud(prefix, prose) {
  const base = `/api/v1/${prefix}repocreds`;
  const id = prefix ? 'WriteRepositoryCredential' : 'RepositoryCredential';
  const extra = prose ? { prose: true } : {};
  return [
    { method: 'GET', path: base, operationId: `list${id}s`, tag: 'repocreds', summary: `List ${prefix}credential templates`, response: 'RepositoryCredentialList', ...extra },
    { method: 'POST', path: base, operationId: `create${id}`, tag: 'repocreds', summary: `Create ${prefix}credential template`, query: [q('upsert', bool)], body: 'RepositoryCredentialWriteRequest', response: 'RepositoryCredential', ...extra },
    { method: 'PUT', path: `${base}/{url}`, operationId: `update${id}`, tag: 'repocreds', summary: `Update ${prefix}credential template`, body: 'RepositoryCredentialWriteRequest', response: 'RepositoryCredential', ...extra },
    { method: 'DELETE', path: `${base}/{url}`, operationId: `delete${id}`, tag: 'repocreds', summary: `Delete ${prefix}credential template`, response: 'empty', ...extra },
  ];
}

/** @type {Route[]} */
const repositories = [
  ...repositoryCrud('', false),
  { method: 'GET', path: '/api/v1/repositories/{repo}/refs', operationId: 'listRepositoryRefs', tag: 'repositories', summary: 'Branches and tags', query: repoScope, response: 'RepoRefs' },
  { method: 'GET', path: '/api/v1/repositories/{repo}/oci-tags', operationId: 'listRepositoryOCITags', tag: 'repositories', summary: 'OCI tags', query: repoScope, response: 'OCITags' },
  { method: 'GET', path: '/api/v1/repositories/{repo}/apps', operationId: 'listRepositoryApps', tag: 'repositories', summary: 'Detected application paths', query: [q('revision', str), q('appName', str), ...repoScope], response: 'RepoAppsResponse' },
  { method: 'POST', path: '/api/v1/repositories/{repo}/appdetails', operationId: 'getRepositoryAppDetails', tag: 'repositories', summary: 'Tool-specific source details', body: 'RepoAppDetailsRequest', response: 'RepoAppDetailsResponse' },
  { method: 'GET', path: '/api/v1/repositories/{repo}/helmcharts', operationId: 'listRepositoryHelmCharts', tag: 'repositories', summary: 'Helm charts in a chart repository', query: repoScope, response: 'HelmChartsResponse' },
  ...repositoryCrud('write-', true),
  ...repoCredsCrud('', false),
  ...repoCredsCrud('write-', true),
  { method: 'GET', path: '/api/v1/certificates', operationId: 'listCertificates', tag: 'certificates', summary: 'List TLS certificates and SSH known hosts', query: [q('hostNamePattern', str), q('certType', str), q('certSubType', str)], response: 'CertificateList' },
  { method: 'POST', path: '/api/v1/certificates', operationId: 'createCertificates', tag: 'certificates', summary: 'Add certificates', query: [q('upsert', bool)], body: 'CertificateList', response: 'CertificateList' },
  { method: 'DELETE', path: '/api/v1/certificates', operationId: 'deleteCertificates', tag: 'certificates', summary: 'Remove certificates matching the query', query: [q('hostNamePattern', str), q('certType', str), q('certSubType', str)], response: 'CertificateList' },
  { method: 'GET', path: '/api/v1/gpgkeys', operationId: 'listGPGKeys', tag: 'gpgkeys', summary: 'List GnuPG public keys', response: 'GnuPGPublicKeyList' },
  { method: 'POST', path: '/api/v1/gpgkeys', operationId: 'createGPGKey', tag: 'gpgkeys', summary: 'Add GnuPG public key', query: [q('upsert', bool)], body: 'GnuPGPublicKey', response: 'GnuPGPublicKeyCreateResponse' },
  { method: 'GET', path: '/api/v1/gpgkeys/{keyID}', operationId: 'getGPGKey', tag: 'gpgkeys', summary: 'Get GnuPG public key', response: 'GnuPGPublicKey' },
  { method: 'DELETE', path: '/api/v1/gpgkeys/{keyID}', operationId: 'deleteGPGKey', tag: 'gpgkeys', summary: 'Delete GnuPG public key', response: 'empty' },
  { method: 'GET', path: '/api/v1/scmproviders', operationId: 'listSCMProviders', tag: 'scmproviders', summary: 'List SCM providers', response: 'SCMProviderList' },
  { method: 'POST', path: '/api/v1/scmproviders', operationId: 'createSCMProvider', tag: 'scmproviders', summary: 'Create SCM provider', body: 'SCMProviderWriteRequest', response: 'SCMProvider' },
  { method: 'GET', path: '/api/v1/scmproviders/{name}', operationId: 'getSCMProvider', tag: 'scmproviders', summary: 'Get SCM provider', response: 'SCMProvider' },
  { method: 'PUT', path: '/api/v1/scmproviders/{name}', operationId: 'updateSCMProvider', tag: 'scmproviders', summary: 'Update SCM provider', body: 'SCMProviderWriteRequest', response: 'SCMProvider' },
  { method: 'DELETE', path: '/api/v1/scmproviders/{name}', operationId: 'deleteSCMProvider', tag: 'scmproviders', summary: 'Delete SCM provider', response: 'empty' },
];

const clusterId = [q('id.type', { type: 'string', enum: ['url', 'name'] }, 'Interpret {server} as a cluster name when "name"')];

/** @type {Route[]} */
const clusters = [
  { method: 'GET', path: '/api/v1/clusters', operationId: 'listClusters', tag: 'clusters', summary: 'List clusters', query: [q('server', str), q('name', str)], response: 'ClusterList' },
  { method: 'POST', path: '/api/v1/clusters', operationId: 'createCluster', tag: 'clusters', summary: 'Register cluster', query: [q('upsert', bool)], body: 'ClusterWriteRequest', response: 'Cluster' },
  { method: 'GET', path: '/api/v1/clusters/{server}', operationId: 'getCluster', tag: 'clusters', summary: 'Get cluster', query: clusterId, response: 'Cluster' },
  { method: 'PUT', path: '/api/v1/clusters/{server}', operationId: 'updateCluster', tag: 'clusters', summary: 'Update cluster', query: [q('updatedFields', strs), ...clusterId], body: 'ClusterWriteRequest', response: 'Cluster' },
  { method: 'DELETE', path: '/api/v1/clusters/{server}', operationId: 'deleteCluster', tag: 'clusters', summary: 'Remove cluster', query: clusterId, response: 'empty' },
  { method: 'POST', path: '/api/v1/clusters/{server}/rotate-auth', operationId: 'rotateClusterAuth', tag: 'clusters', summary: 'Rotate cluster credentials', query: clusterId, response: 'empty' },
  { method: 'POST', path: '/api/v1/clusters/{server}/invalidate-cache', operationId: 'invalidateClusterCache', tag: 'clusters', summary: 'Invalidate live-state cache', query: clusterId, response: 'Cluster' },
];

/** @type {Route[]} */
const misc = [
  { method: 'GET', path: '/api/v1/notifications/services', operationId: 'listNotificationServices', tag: 'notifications', summary: 'Configured notification services', response: 'NotificationServiceList' },
  { method: 'GET', path: '/api/v1/notifications/triggers', operationId: 'listNotificationTriggers', tag: 'notifications', summary: 'Configured notification triggers', response: 'NotificationTriggerList' },
  { method: 'GET', path: '/api/v1/notifications/templates', operationId: 'listNotificationTemplates', tag: 'notifications', summary: 'Configured notification templates', response: 'NotificationTemplateList' },
  { method: 'POST', path: '/api/webhook', operationId: 'receiveWebhook', tag: 'webhooks', summary: 'SCM webhook (provider detected from headers)', response: 'empty' },
  { method: 'POST', path: '/api/webhook/{provider}', operationId: 'receiveProviderWebhook', tag: 'webhooks', summary: 'SCM webhook for a named provider', response: 'empty' },
  { method: 'GET', path: '/api/badge', operationId: 'getBadge', tag: 'badge', summary: 'Status badge', query: [q('name', str), q('appNamespace', str), q('project', str), q('revision', bool), q('showAppName', bool), q('width', int), q('keepFullRevision', bool)], response: 'svg' },
  ...(/** @type {const} */ (['GET', 'POST', 'PUT', 'PATCH', 'DELETE'])).map((method) => ({
    method,
    path: '/extensions/{extension}/{path}',
    operationId: `invokeExtension${method[0]}${method.slice(1).toLowerCase()}`,
    tag: 'extensions',
    summary: 'Proxy extension call (path may contain slashes)',
    response: 'ExtensionResponse',
  })),
];

/** @type {Route[]} */
export const routes = [...session, ...applications, ...applicationSets, ...projects, ...repositories, ...clusters, ...misc];
