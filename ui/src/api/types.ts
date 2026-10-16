/**
 * Stable names for the generated API types (src/api/schema.d.ts, generated from the published
 * api/openapi/openapi.json). Pages import from here, never from ./schema directly. Names follow
 * the server's schema names; the few differences are documented next to the alias.
 */
import type { components, operations } from './schema';

type Schemas = components['schemas'];

/** operationId of every operation in the published document. */
export type OperationId = keyof operations;

/** Query parameters of an operation. */
export type QueryOf<Op extends OperationId> = NonNullable<operations[Op]['parameters']['query']>;

export type ObjectMeta = Schemas['ObjectMeta'];
export type ListMeta = Schemas['ListMeta'];

/**
 * Objects read from the server. Kubernetes always returns `metadata` on stored objects, but the
 * CRD schemas leave it optional because the same schema describes request bodies. Request
 * bodies use the generated shapes (`...Input` aliases below); see UI-1 in
 * docs/verification/integration-findings.md.
 */
type Stored<T extends { metadata?: ObjectMeta }> = Omit<T, 'metadata'> & { metadata: ObjectMeta };
/** A list response whose items are `T`. */
type ListOf<L extends { items: unknown[] }, T> = Omit<L, 'items'> & { items: T[] };

// CRD objects --------------------------------------------------------------------------------

export type Application = Stored<Schemas['Application']>;
/** An Application as sent in a request body (create, update). */
export type ApplicationInput = Schemas['Application'];
export type ApplicationList = ListOf<Schemas['ApplicationList'], Application>;
export type ApplicationSpec = Schemas['ApplicationSpec'];
export type ApplicationSource = Schemas['ApplicationSpecSource'];
export type ApplicationDestination = Schemas['ApplicationSpecDestination'];
export type ApplicationStatus = NonNullable<Application['status']>;
export type ResourceHealth = Schemas['ApplicationStatusResourcesItemHealth'];
export type Operation = Stored<Schemas['Operation']>;
export type OperationList = ListOf<Schemas['OperationList'], Operation>;
export type OperationPhase = NonNullable<NonNullable<Operation['status']>['phase']>;
export type AppProject = Stored<Schemas['AppProject']>;
export type AppProjectInput = Schemas['AppProject'];
export type AppProjectList = ListOf<Schemas['AppProjectList'], AppProject>;
export type SyncWindow = Schemas['AppProjectSpecSyncWindowsItem'];
export type ApplicationSet = Stored<Schemas['ApplicationSet']>;
export type ApplicationSetInput = Schemas['ApplicationSet'];
export type ApplicationSetList = ListOf<Schemas['ApplicationSetList'], ApplicationSet>;
/** Repository object (CRD); the repository routes return RepositoryInfo. */
export type Repository = Stored<Schemas['Repository']>;
export type RepositorySpec = Schemas['RepositorySpec'];
export type RepositoryCredential = Stored<Schemas['RepositoryCredential']>;
export type RepositoryCredentialSpec = Schemas['RepositoryCredentialSpec'];
/** Cluster object (CRD); the cluster routes return RegisteredCluster. */
export type Cluster = Stored<Schemas['Cluster']>;
export type ClusterSpec = Schemas['ClusterSpec'];
export type SCMProvider = Stored<Schemas['SCMProvider']>;
export type SCMProviderSpec = Schemas['SCMProviderSpec'];
export type SCMProviderList = ListOf<Schemas['SCMProviderList'], SCMProvider>;

// Errors -------------------------------------------------------------------------------------

export type Problem = Schemas['Problem'];
export type ErrorCode = NonNullable<Problem['code']>;

// Session, accounts, settings ----------------------------------------------------------------

export type UserInfo = Schemas['UserInfo'];
export type SessionCreateRequest = Schemas['SessionCreateRequest'];
export type SessionResponse = Schemas['SessionResponse'];
export type LogoutResponse = Schemas['LogoutResponse'];
export type OIDCExchangeRequest = Schemas['OIDCExchangeRequest'];
export type Account = Schemas['Account'];
export type AccountList = Schemas['AccountList'];
export type AccountToken = Schemas['AccountToken'];
export type CanIResponse = Schemas['CanIResponse'];
export type UpdatePasswordRequest = Schemas['UpdatePasswordRequest'];
export type CreateAccountTokenRequest = Schemas['CreateAccountTokenRequest'];
export type TokenResponse = Schemas['TokenResponse'];
export type Settings = Schemas['Settings'];
export type PluginSettings = Schemas['PluginSettings'];
export type Version = Schemas['Version'];
/** Version fields holding a plain string (all but `extraBuildInfo`). */
export type VersionStringField = { [K in keyof Version]-?: NonNullable<Version[K]> extends string ? K : never }[keyof Version];

// Applications -------------------------------------------------------------------------------

export type ApplicationSyncRequest = Schemas['ApplicationSyncRequest'];
export type ApplicationRollbackRequest = Schemas['ApplicationRollbackRequest'];
export type ApplicationPatchRequest = Schemas['ApplicationPatchRequest'];
/** Sync, rollback and terminate: the Operation created (or cancelled) and the Application. */
export type ApplicationOperationResponse = Omit<Schemas['ApplicationOperationResponse'], 'application' | 'operation'> & {
  application?: Application;
  operation?: Operation;
};
export type SyncOperationResource = Schemas['OperationSpecSyncResourcesItem'];
export type ApplicationSyncWindow = Schemas['ApplicationSyncWindow'];
export type ApplicationSyncWindowsResponse = Schemas['ApplicationSyncWindowsResponse'];
export type ApplicationRevisionMetadata = Schemas['ApplicationRevisionMetadata'];
export type ApplicationChartDetails = Schemas['ApplicationChartDetails'];
export type ApplicationOCIMetadata = Schemas['ApplicationOCIMetadata'];
export type ApplicationManifestResponse = Schemas['ApplicationManifestResponse'];
export type ResourceDiff = Schemas['ResourceDiff'];
export type ManagedResourcesResponse = Schemas['ManagedResourcesResponse'];
export type ApplicationServerSideDiffRequest = Schemas['ApplicationServerSideDiffRequest'];
export type ServerSideDiffResponse = Schemas['ServerSideDiffResponse'];
export type ResourceRef = Schemas['ResourceRef'];
export type ResourceNode = Schemas['ResourceNode'];
export type InfoItem = Schemas['InfoItem'];
export type HostInfo = Schemas['HostInfo'];
export type ApplicationTree = Schemas['ApplicationTree'];
/** Kubernetes Event (schema `Event`; renamed to avoid the DOM `Event` type). */
export type KubernetesEvent = Schemas['Event'];
export type EventList = Schemas['EventList'];
export type ApplicationResourceResponse = Schemas['ApplicationResourceResponse'];
export type ApplicationResourcePatchRequest = Schemas['ApplicationResourcePatchRequest'];
export type ResourceAction = Schemas['ResourceAction'];
export type ResourceActionsListResponse = Schemas['ResourceActionsListResponse'];
export type ResourceActionRunRequest = Schemas['ResourceActionRunRequest'];
export type LogEntry = Schemas['LogEntry'];
export type LinkInfo = Schemas['LinkInfo'];
export type LinksResponse = Schemas['LinksResponse'];
export type TerminalMessage = Schemas['TerminalMessage'];

// ApplicationSets ----------------------------------------------------------------------------

export type ApplicationSetGenerateRequest = Schemas['ApplicationSetGenerateRequest'];
export type ApplicationSetGenerateResponse = Omit<Schemas['ApplicationSetGenerateResponse'], 'applications'> & {
  applications: Application[];
};
export type ApplicationSetTree = Schemas['ApplicationSetTree'];
export type ApplicationSetTreeNode = Schemas['ApplicationSetTreeNode'];

// Projects -----------------------------------------------------------------------------------

export type ProjectCreateRequest = Schemas['ProjectCreateRequest'];
export type ProjectUpdateRequest = Schemas['ProjectUpdateRequest'];
export type ProjectDetailed = Omit<Schemas['ProjectDetailed'], 'project' | 'globalProjects' | 'repositories'> & {
  project: AppProject;
  globalProjects: AppProject[];
  repositories: Repository[];
};
export type ProjectGlobalProjectsResponse = ListOf<Schemas['ProjectGlobalProjectsResponse'], AppProject>;
export type ProjectSyncWindowsResponse = Schemas['ProjectSyncWindowsResponse'];
export type ProjectTokenCreateRequest = Schemas['ProjectTokenCreateRequest'];
export type ProjectTokenResponse = Schemas['ProjectTokenResponse'];

// Repositories, credential templates, certificates, GnuPG keys, SCM providers ---------------

/** Repository as returned by the repository routes: the object plus credential flags (never secrets). */
export type RepositoryInfo = Schemas['RepositoryInfo'];
export type RepositoryInfoList = Schemas['RepositoryInfoList'];
export type ConnectionState = Schemas['RepositoryStatusConnectionState'];
/** Repository create/update/validate body; `credentials` is write-only (stored in a Secret). */
export type RepositoryWriteRequest = Schemas['RepositoryWriteRequest'];
/** Write-only secret material of repositories and credential templates. */
export type RepositoryCredentials = Schemas['RepositoryCredentials'];
/** Credential template as returned by the repocreds routes (`hasCredentials`, never secrets). */
export type RepositoryCredentialInfo = Schemas['RepositoryCredentialInfo'];
export type RepositoryCredentialInfoList = Schemas['RepositoryCredentialInfoList'];
export type RepositoryCredentialWriteRequest = Schemas['RepositoryCredentialWriteRequest'];
export type RepositoryRefs = Schemas['RepositoryRefs'];
export type RepositoryApp = Schemas['RepositoryApp'];
export type RepositoryAppList = Schemas['RepositoryAppList'];
export type RepositoryAppDetailsQuery = Schemas['RepositoryAppDetailsQuery'];
export type RepositoryAppDetails = Schemas['RepositoryAppDetails'];
export type RepositoryHelmChart = Schemas['RepositoryHelmChart'];
export type RepositoryHelmChartList = Schemas['RepositoryHelmChartList'];
/** TLS certificate or SSH known-hosts entry; `certData` is plain text (PEM or key line). */
export type RepositoryCertificate = Schemas['RepositoryCertificate'];
export type RepositoryCertificateList = Schemas['RepositoryCertificateList'];
export type GnuPGPublicKey = Schemas['GnuPGPublicKey'];
export type GnuPGPublicKeyList = Schemas['GnuPGPublicKeyList'];
export type GnuPGPublicKeyCreateResponse = Schemas['GnuPGPublicKeyCreateResponse'];
/** SCM provider create/update body: metadata and spec only; secrets are referenced by `secretRef`s. */
export type SCMProviderWriteRequest = Schemas['SCMProviderWriteRequest'];

// Clusters -----------------------------------------------------------------------------------

/** Cluster as returned by the cluster routes: the object plus credential flags (never secrets). */
export type RegisteredCluster = Schemas['RegisteredCluster'];
export type RegisteredClusterList = Schemas['RegisteredClusterList'];
export type ClusterCredentialsInfo = Schemas['ClusterCredentialsInfo'];
/** Cluster create/update body; `config` carries write-only credentials. */
export type ClusterInput = Schemas['ClusterInput'];
export type ClusterConfigInput = Schemas['ClusterConfigInput'];

// Notifications ------------------------------------------------------------------------------

export type NotificationServiceList = Schemas['NotificationServiceList'];
export type NotificationTriggerList = Schemas['NotificationTriggerList'];
export type NotificationTemplateList = Schemas['NotificationTemplateList'];

// Streams ------------------------------------------------------------------------------------

/** Watch event types the server sends (`type` is a plain string in the document; UI-4). */
export type WatchEventType = 'ADDED' | 'MODIFIED' | 'DELETED';
/** `data:` payload of the watch streams: `{type, object}`. */
export interface WatchEvent<T> {
  type: string;
  object: T;
}
export type ApplicationWatchEvent = WatchEvent<Application>;
export type ApplicationSetWatchEvent = WatchEvent<ApplicationSet>;
export type ApplicationTreeEvent = WatchEvent<ApplicationTree>;

// UI-side vocabulary -------------------------------------------------------------------------

/** Health states shared by Application, resources and ApplicationSet status. */
export type HealthStatusCode = 'Healthy' | 'Progressing' | 'Degraded' | 'Suspended' | 'Missing' | 'Unknown';
export type SyncStatusCode = 'Synced' | 'OutOfSync' | 'Unknown';
export type ConnectionStatusCode = 'Successful' | 'Failed' | 'Unknown';

/** Identifies a live resource for the /resource* routes. */
export interface ResourceIdentifier {
  group?: string | undefined;
  version?: string | undefined;
  kind: string;
  namespace?: string | undefined;
  /** Sent as `resourceName`. */
  name: string;
}

/** Scope shared by application routes (apps in any namespace, project hint for RBAC). */
export interface AppScope {
  appNamespace?: string | undefined;
  project?: string | undefined;
}
