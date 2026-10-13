// Hand-declared request/response schemas for the non-CRD routes in
// docs/architecture/api-contract.md. They are PROVISIONAL: shapes follow the contract text and
// the Argo CD v3.5.5 REST payloads the contract says it stays compatible with. Once
// patrol-server publishes api/openapi/openapi.json, gen-api-types.mjs reads that document
// instead and these declarations stop being used (see docs/architecture/components/ui.md).

/** @typedef {Record<string, unknown>} Schema */

const str = { type: 'string' };
const bool = { type: 'boolean' };
const int = { type: 'integer', format: 'int64' };
const int32 = { type: 'integer', format: 'int32' };
const time = { type: 'string', format: 'date-time' };
const strs = { type: 'array', items: str };
const strMap = { type: 'object', additionalProperties: str };
const anyObject = { type: 'object', additionalProperties: true };

/** @param {string} name */
const ref = (name) => ({ $ref: `#/components/schemas/${name}` });
/** @param {Schema} items */
const arr = (items) => ({ type: 'array', items });
/** @param {...string} values */
const enumOf = (...values) => ({ type: 'string', enum: values });
/**
 * @param {Record<string, Schema>} properties
 * @param {string[]} [required]
 * @param {string} [description]
 * @returns {Schema}
 */
const obj = (properties, required = [], description) => ({
  type: 'object',
  ...(description ? { description } : {}),
  properties,
  ...(required.length > 0 ? { required } : {}),
});
/** @param {string} item @param {string} [description] */
const list = (item, description) =>
  obj({ items: arr(ref(item)), metadata: ref('ListMeta') }, ['items'], description);

/**
 * @param {(kind: string, pointer: string) => Schema} crd Returns a converted copy of a CRD
 *   sub-schema, e.g. crd('Application', '/properties/spec').
 * @returns {Record<string, Schema>}
 */
export function handSchemas(crd) {
  return {
    ErrorCode: enumOf(
      'not_found',
      'permission_denied',
      'invalid_argument',
      'conflict',
      'already_exists',
      'failed_precondition',
      'unauthenticated',
      'unavailable',
      'internal',
      'resource_exhausted',
    ),
    Problem: obj(
      {
        type: str,
        title: str,
        status: int32,
        detail: str,
        instance: str,
        code: ref('ErrorCode'),
      },
      ['title', 'status'],
      'RFC 9457 problem details (application/problem+json).',
    ),
    Empty: obj({}),
    ListMeta: obj({ continue: str, resourceVersion: str, remainingItemCount: int }),
    OwnerReference: obj(
      { apiVersion: str, kind: str, name: str, uid: str, controller: bool, blockOwnerDeletion: bool },
      ['apiVersion', 'kind', 'name', 'uid'],
    ),
    ObjectMeta: obj({
      name: str,
      generateName: str,
      namespace: str,
      uid: str,
      resourceVersion: str,
      generation: int,
      creationTimestamp: time,
      deletionTimestamp: time,
      deletionGracePeriodSeconds: int,
      labels: strMap,
      annotations: strMap,
      ownerReferences: arr(ref('OwnerReference')),
      finalizers: strs,
    }),
    ObjectReference: obj({
      kind: str,
      namespace: str,
      name: str,
      uid: str,
      apiVersion: str,
      resourceVersion: str,
      fieldPath: str,
    }),

    // Watch envelopes for /api/v1/stream/... (SSE `data:` payloads).
    WatchEventType: enumOf('ADDED', 'MODIFIED', 'DELETED'),
    ApplicationWatchEvent: obj({ type: ref('WatchEventType'), object: ref('Application') }, ['type', 'object']),
    ApplicationSetWatchEvent: obj({ type: ref('WatchEventType'), object: ref('ApplicationSet') }, ['type', 'object']),
    ApplicationTreeEvent: obj({ type: ref('WatchEventType'), object: ref('ApplicationTree') }, ['type', 'object']),

    // Session and accounts.
    SessionCreateRequest: obj({ username: str, password: str }, ['username', 'password']),
    SessionResponse: obj({ token: str }),
    LogoutResponse: obj({ logoutURL: { ...str, description: 'OIDC end-session URL when configured' } }),
    OIDCExchangeRequest: obj({ idToken: str }, ['idToken']),
    UserInfo: obj({ loggedIn: bool, username: str, iss: str, groups: strs }, ['loggedIn']),
    AccountToken: obj({ id: str, issuedAt: int, expiresAt: int }, ['id']),
    Account: obj(
      { name: str, enabled: bool, capabilities: strs, tokens: arr(ref('AccountToken')) },
      ['name'],
    ),
    AccountList: obj({ items: arr(ref('Account')) }, ['items']),
    CanIResponse: obj({ value: enumOf('yes', 'no') }, ['value']),
    UpdatePasswordRequest: obj({ name: str, currentPassword: str, newPassword: str }, ['currentPassword', 'newPassword']),
    CreateAccountTokenRequest: obj({ id: str, expiresIn: { ...int, description: 'Seconds; 0 = no expiry' } }),
    TokenResponse: obj({ token: str }, ['token']),

    // Settings and version.
    Settings: obj(
      {
        url: str,
        statusBadgeEnabled: bool,
        statusBadgeRootUrl: str,
        googleAnalytics: obj({ trackingID: str, anonymizeUsers: bool }),
        dexConfig: obj({ connectors: arr(obj({ name: str, type: str }, ['name'])) }),
        oidcConfig: obj({
          name: str,
          issuer: str,
          clientID: str,
          cliClientID: str,
          scopes: strs,
          enablePKCEAuthentication: bool,
        }),
        help: obj({ chatUrl: str, chatText: str, binaryUrls: strMap }),
        userLoginsDisabled: bool,
        anonymousEnabled: { ...bool, description: 'Provisional: users.anonymous.enabled' },
        kustomizeVersions: strs,
        kustomizeOptions: obj({ buildOptions: str, binaryPath: str }),
        uiCssURL: str,
        uiBannerContent: str,
        uiBannerURL: str,
        uiBannerPermanent: bool,
        uiBannerPosition: enumOf('', 'top', 'bottom', 'both'),
        execEnabled: bool,
        appsInAnyNamespaceEnabled: bool,
        hydratorEnabled: bool,
        syncWithReplaceAllowed: bool,
        controllerNamespace: str,
        appLabelKey: str,
        trackingMethod: str,
        passwordPattern: str,
        resourceOverrides: { type: 'object', additionalProperties: anyObject },
      },
      [],
      'Settings for the UI and CLI. Fields beyond the public subset are only returned to authenticated callers.',
    ),
    PluginSettings: obj({ plugins: arr(obj({ name: str }, ['name'])) }),
    Version: obj({
      version: str,
      buildDate: str,
      gitCommit: str,
      gitTreeState: str,
      goVersion: str,
      compiler: str,
      platform: str,
      kustomizeVersion: str,
      helmVersion: str,
      kubectlVersion: str,
      jsonnetVersion: str,
    }),

    // Application operations.
    ApplicationSpec: crd('Application', '/properties/spec'),
    ApplicationSource: crd('Application', '/properties/spec/properties/source'),
    SyncWindow: crd('AppProject', '/properties/spec/properties/syncWindows/items'),
    ApplicationSyncRequest: {
      allOf: [
        crd('Operation', '/properties/spec/properties/sync'),
        obj({
          retryStrategy: crd('Operation', '/properties/spec/properties/retry'),
          infos: crd('Operation', '/properties/spec/properties/info'),
        }),
      ],
    },
    ApplicationRollbackRequest: obj({ id: int, prune: bool, dryRun: bool }, ['id']),
    ApplicationPatchRequest: obj({ patch: str, patchType: enumOf('json', 'merge') }, ['patch', 'patchType']),
    ApplicationSyncWindowsState: obj({
      activeWindows: arr(ref('SyncWindow')),
      assignedWindows: arr(ref('SyncWindow')),
      canSync: bool,
    }),
    RevisionMetadata: obj({ author: str, date: time, tags: strs, message: str, signatureInfo: str }),
    ChartDetails: obj({ description: str, maintainers: strs, home: str }),
    OCIMetadata: obj({
      createdAt: str,
      authors: str,
      imageUrl: str,
      docsUrl: str,
      sourceUrl: str,
      version: str,
      description: str,
    }),
    ManifestResponse: obj({
      manifests: strs,
      namespace: str,
      server: str,
      revision: str,
      sourceType: str,
      verifyResult: str,
    }),
    ResourceDiff: obj({
      group: str,
      kind: str,
      namespace: str,
      name: str,
      targetState: { ...str, description: 'JSON; secrets masked' },
      liveState: { ...str, description: 'JSON; secrets masked' },
      normalizedLiveState: str,
      predictedLiveState: str,
      hook: bool,
      modified: bool,
      resourceVersion: str,
    }),
    ManagedResourcesResponse: obj({ items: arr(ref('ResourceDiff')) }, ['items']),
    ServerSideDiffRequest: obj({ liveResources: arr(ref('ResourceDiff')), targetManifests: strs }),
    ServerSideDiffResponse: obj({ items: arr(ref('ResourceDiff')), modified: bool }),

    // Resource tree.
    ResourceRef: obj({ group: str, version: str, kind: str, namespace: str, name: str, uid: str }, ['kind', 'name']),
    InfoItem: obj({ name: str, value: str }),
    ResourceHealth: obj({ status: str, message: str }),
    ResourceNetworkingInfo: obj({
      targetLabels: strMap,
      targetRefs: arr(ref('ResourceRef')),
      labels: strMap,
      ingress: arr(obj({ hostname: str, ip: str })),
      externalURLs: strs,
    }),
    ResourceNode: obj(
      {
        group: str,
        version: str,
        kind: str,
        namespace: str,
        name: str,
        uid: str,
        parentRefs: arr(ref('ResourceRef')),
        info: arr(ref('InfoItem')),
        networkingInfo: ref('ResourceNetworkingInfo'),
        resourceVersion: str,
        images: strs,
        health: ref('ResourceHealth'),
        createdAt: time,
      },
      ['kind', 'name'],
    ),
    HostInfo: obj({
      name: str,
      resourcesInfo: arr(
        obj({ resourceName: str, requestedByApp: int, requestedByNeighbors: int, capacity: int }),
      ),
      systemInfo: strMap,
      labels: strMap,
    }),
    ApplicationTree: obj({
      nodes: arr(ref('ResourceNode')),
      orphanedNodes: arr(ref('ResourceNode')),
      hosts: arr(ref('HostInfo')),
    }),
    KubernetesEvent: obj({
      metadata: ref('ObjectMeta'),
      involvedObject: ref('ObjectReference'),
      reason: str,
      message: str,
      source: obj({ component: str, host: str }),
      firstTimestamp: time,
      lastTimestamp: time,
      eventTime: time,
      count: int32,
      type: str,
      action: str,
      reportingComponent: str,
      reportingInstance: str,
    }),
    EventList: list('KubernetesEvent'),
    ResourceManifest: obj({ manifest: str }, ['manifest']),
    ResourcePatchRequest: obj({ patch: str, patchType: enumOf('json', 'merge', 'strategic') }, ['patch', 'patchType']),
    ResourceActionParam: obj({ name: str, value: str, type: str, default: str }, ['name']),
    ResourceAction: obj(
      { name: str, displayName: str, iconClass: str, disabled: bool, params: arr(ref('ResourceActionParam')) },
      ['name'],
    ),
    ResourceActionsList: obj({ actions: arr(ref('ResourceAction')) }),
    RunResourceActionRequest: obj(
      { action: str, resourceActionParameters: arr(obj({ name: str, value: str }, ['name'])) },
      ['action'],
    ),
    LogEntry: obj({ content: str, timeStamp: time, timeStampStr: str, podName: str, last: bool }),
    LinkInfo: obj({ title: str, url: str, description: str, iconClass: str }, ['title', 'url']),
    LinksResponse: obj({ items: arr(ref('LinkInfo')) }),
    TerminalMessage: obj(
      { operation: enumOf('stdin', 'stdout', 'resize'), data: str, rows: int32, cols: int32 },
      ['operation'],
      'WebSocket frame exchanged on /terminal.',
    ),

    // ApplicationSets.
    ApplicationSetGenerateRequest: obj({ applicationSet: ref('ApplicationSet') }, ['applicationSet']),
    ApplicationSetGenerateResponse: obj({ applications: arr(ref('Application')) }),

    // Projects.
    DetailedProject: obj({
      project: ref('AppProject'),
      globalProjects: arr(ref('AppProject')),
      repositories: arr(ref('Repository')),
      clusters: arr(ref('Cluster')),
    }),
    SyncWindowsState: obj({ windows: arr(ref('SyncWindow')) }),
    ProjectTokenCreateRequest: obj({ description: str, expiresIn: int, id: str }),

    // Repositories, credentials, certificates, GPG keys, SCM providers, clusters.
    ConnectionState: crd('Repository', '/properties/status/properties/connectionState'),
    RepositoryCredentials: obj(
      {
        username: str,
        password: str,
        sshPrivateKey: str,
        tlsClientCertData: str,
        tlsClientCertKey: str,
        githubAppPrivateKey: str,
        gcpServiceAccountKey: str,
        bearerToken: str,
        azureServicePrincipalClientSecret: str,
      },
      [],
      'Write-only secret material; the server stores it in a Secret referenced by credentialsRef.',
    ),
    RepositoryWriteRequest: {
      allOf: [ref('Repository'), obj({ credentials: ref('RepositoryCredentials') })],
    },
    RepositoryCredentialWriteRequest: {
      allOf: [ref('RepositoryCredential'), obj({ credentials: ref('RepositoryCredentials') })],
    },
    RepoRefs: obj({ branches: strs, tags: strs }),
    OCITags: obj({ tags: strs }),
    RepoAppsResponse: obj({ items: arr(obj({ type: str, path: str }, ['path'])) }),
    RepoAppDetailsRequest: obj(
      { source: ref('ApplicationSource'), appName: str, appProject: str, sourceIndex: int32, versionId: int32 },
      ['source'],
    ),
    RepoAppDetailsResponse: obj({
      type: str,
      directory: anyObject,
      helm: obj({
        name: str,
        valueFiles: strs,
        values: str,
        parameters: arr(obj({ name: str, value: str })),
        fileParameters: arr(obj({ name: str, path: str })),
      }),
      kustomize: obj({ images: strs }),
      plugin: anyObject,
    }),
    HelmChart: obj({ name: str, versions: strs }, ['name']),
    HelmChartsResponse: obj({ items: arr(ref('HelmChart')) }),
    Certificate: obj(
      { serverName: str, certType: enumOf('https', 'ssh'), certSubType: str, certData: str, certInfo: str },
      ['serverName', 'certType'],
    ),
    CertificateList: list('Certificate'),
    GnuPGPublicKey: obj({ keyID: str, fingerprint: str, owner: str, trust: str, subType: str, keyData: str }),
    GnuPGPublicKeyList: list('GnuPGPublicKey'),
    GnuPGPublicKeyCreateResponse: obj({ created: ref('GnuPGPublicKeyList'), skipped: strs }),
    SCMProviderWriteRequest: {
      allOf: [
        ref('SCMProvider'),
        obj({
          credentials: obj(
            { token: str, username: str, password: str, githubAppPrivateKey: str, webhookSecret: str, caCert: str },
            [],
            'Write-only secret material stored in the referenced Secrets.',
          ),
        }),
      ],
    },
    ClusterWriteRequest: {
      allOf: [
        ref('Cluster'),
        obj({
          credentials: obj(
            { bearerToken: str, username: str, password: str, certData: str, keyData: str, caData: str },
            [],
            'Write-only secret material stored in the Secret referenced by spec.config.credentialsRef.',
          ),
        }),
      ],
    },

    // Notifications and extensions.
    NotificationService: obj({ name: str }, ['name']),
    NotificationServiceList: obj({ items: arr(ref('NotificationService')) }),
    NotificationTrigger: obj({ name: str }, ['name']),
    NotificationTriggerList: obj({ items: arr(ref('NotificationTrigger')) }),
    NotificationTemplate: obj({ name: str }, ['name']),
    NotificationTemplateList: obj({ items: arr(ref('NotificationTemplate')) }),
    ExtensionResponse: { description: 'Opaque response from the extension backend.' },
  };
}

/** CRD list wrappers: <Kind>List for every CRD schema. @param {string[]} kinds */
export function listSchemas(kinds) {
  return Object.fromEntries(kinds.map((kind) => [`${kind}List`, list(kind)]));
}

export const objectMetaRef = ref('ObjectMeta');
