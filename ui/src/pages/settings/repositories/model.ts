import type {
  RepositoryCredentialSpec,
  RepositoryCredentialWriteRequest,
  RepositoryInfo,
  RepositorySpec,
  RepositoryWriteRequest,
} from '../../../api/types';
import type { FieldError } from '../common/feedback';
import { validateHTTPURL, validatePositiveInteger, validateProxy, validateRepoURL } from '../common/validation';

/** Connection methods offered by the connect dialog (upstream: SSH, HTTPS, GitHub App, Google Cloud, Azure). */
export type ConnectionMethod = 'https' | 'ssh' | 'githubApp' | 'helm' | 'oci' | 'gcp' | 'azure';

export const METHOD_LABELS: Record<ConnectionMethod, string> = {
  https: 'HTTPS',
  ssh: 'SSH',
  githubApp: 'GitHub App',
  helm: 'Helm',
  oci: 'OCI',
  gcp: 'Google Cloud',
  azure: 'Azure service principal',
};

/** Methods that can push (write repositories / write credentials for the source hydrator). */
export const WRITE_METHODS: readonly ConnectionMethod[] = ['https', 'ssh', 'githubApp'];
export const READ_METHODS: readonly ConnectionMethod[] = ['https', 'ssh', 'githubApp', 'helm', 'oci', 'gcp', 'azure'];

export type HTTPSAuth = 'none' | 'basic' | 'bearer';
export type GitHubAppSource = 'key' | 'scmProvider';
export type FormMode = 'repository' | 'template';

export interface RepoFormState {
  method: ConnectionMethod;
  name: string;
  project: string;
  /** Repository URL, or the URL prefix of a credential template. */
  url: string;
  httpsAuth: HTTPSAuth;
  username: string;
  password: string;
  bearerToken: string;
  tlsClientCertData: string;
  tlsClientCertKey: string;
  insecure: boolean;
  forceHTTPBasicAuth: boolean;
  enableLFS: boolean;
  proxy: string;
  noProxy: string;
  depth: string;
  sshPrivateKey: string;
  insecureIgnoreHostKey: boolean;
  githubSource: GitHubAppSource;
  appID: string;
  installationID: string;
  enterpriseBaseURL: string;
  githubAppPrivateKey: string;
  scmProvider: string;
  enableOCI: boolean;
  insecureOCIForceHTTP: boolean;
  useAzureWorkloadIdentity: boolean;
  gcpServiceAccountKey: string;
  azureTenantID: string;
  azureClientID: string;
  azureClientSecret: string;
  azureEndpoint: string;
}

export function emptyForm(method: ConnectionMethod = 'https'): RepoFormState {
  return {
    method,
    name: '',
    project: '',
    url: '',
    httpsAuth: 'basic',
    username: '',
    password: '',
    bearerToken: '',
    tlsClientCertData: '',
    tlsClientCertKey: '',
    insecure: false,
    forceHTTPBasicAuth: false,
    enableLFS: false,
    proxy: '',
    noProxy: '',
    depth: '',
    sshPrivateKey: '',
    insecureIgnoreHostKey: false,
    githubSource: 'key',
    appID: '',
    installationID: '',
    enterpriseBaseURL: '',
    githubAppPrivateKey: '',
    scmProvider: '',
    enableOCI: false,
    insecureOCIForceHTTP: false,
    useAzureWorkloadIdentity: false,
    gcpServiceAccountKey: '',
    azureTenantID: '',
    azureClientID: '',
    azureClientSecret: '',
    azureEndpoint: '',
  };
}

export function isSSHURL(url: string): boolean {
  return url.startsWith("ssh://") || /^[\w.-]+@[\w.-]+:/.test(url);
}

/** Connection method an existing repository was set up with. */
export function methodOf(spec: RepositorySpec | RepositoryCredentialSpec, url: string): ConnectionMethod {
  if (spec.type === 'helm') return 'helm';
  if (spec.type === 'oci') return 'oci';
  if (spec.githubApp || spec.scmProviderRef) return 'githubApp';
  if (spec.azureServicePrincipal) return 'azure';
  if (isSSHURL(url)) return 'ssh';
  return 'https';
}

/** Form state for editing an existing repository; secret fields stay empty (write-only). */
export function formFromRepository(repo: RepositoryInfo): RepoFormState {
  const s = repo.spec;
  return {
    ...emptyForm(methodOf(s, s.url)),
    name: s.name ?? '',
    project: s.project ?? '',
    url: s.url,
    insecure: s.insecure ?? false,
    forceHTTPBasicAuth: s.forceHTTPBasicAuth ?? false,
    enableLFS: s.enableLFS ?? false,
    proxy: s.proxy ?? '',
    noProxy: s.noProxy ?? '',
    depth: s.depth ? String(s.depth) : '',
    insecureIgnoreHostKey: s.insecureIgnoreHostKey ?? false,
    githubSource: s.scmProviderRef ? 'scmProvider' : 'key',
    appID: s.githubApp ? String(s.githubApp.appID) : '',
    installationID: s.githubApp?.installationID ? String(s.githubApp.installationID) : '',
    enterpriseBaseURL: s.githubApp?.enterpriseBaseURL ?? '',
    scmProvider: s.scmProviderRef?.name ?? '',
    enableOCI: s.enableOCI ?? false,
    insecureOCIForceHTTP: s.insecureOCIForceHTTP ?? false,
    useAzureWorkloadIdentity: s.useAzureWorkloadIdentity ?? false,
    azureTenantID: s.azureServicePrincipal?.tenantID ?? '',
    azureClientID: s.azureServicePrincipal?.clientID ?? '',
    azureEndpoint: s.azureServicePrincipal?.activeDirectoryEndpoint ?? '',
    httpsAuth: 'basic',
  };
}

function compact<T extends object>(value: T): T {
  return Object.fromEntries(Object.entries(value).filter(([, v]) => v !== undefined && v !== '' && v !== false)) as T;
}

const trimmed = (v: string) => (v.trim() === '' ? undefined : v.trim());
/** Secrets keep their exact bytes (keys end with a newline); only all-whitespace counts as empty. */
const secret = (v: string) => (v.trim() === '' ? undefined : v);

function credentialsOf(state: RepoFormState): RepositoryWriteRequest['credentials'] {
  const c: NonNullable<RepositoryWriteRequest['credentials']> = {};
  switch (state.method) {
    case 'https':
    case 'helm':
    case 'oci':
      if (!state.useAzureWorkloadIdentity) {
        if (state.httpsAuth === 'basic') {
          c.username = trimmed(state.username);
          c.password = secret(state.password);
        } else if (state.httpsAuth === 'bearer') {
          c.bearerToken = secret(state.bearerToken);
        }
      }
      c.tlsClientCertData = secret(state.tlsClientCertData);
      c.tlsClientCertKey = secret(state.tlsClientCertKey);
      break;
    case 'ssh':
      c.sshPrivateKey = secret(state.sshPrivateKey);
      break;
    case 'githubApp':
      if (state.githubSource === 'key') c.githubAppPrivateKey = secret(state.githubAppPrivateKey);
      c.tlsClientCertData = secret(state.tlsClientCertData);
      c.tlsClientCertKey = secret(state.tlsClientCertKey);
      break;
    case 'gcp':
      c.gcpServiceAccountKey = secret(state.gcpServiceAccountKey);
      break;
    case 'azure':
      c.azureServicePrincipalClientSecret = secret(state.azureClientSecret);
      break;
    default:
      break;
  }
  const out = compact(c);
  return Object.keys(out).length > 0 ? out : undefined;
}

function sharedSpec(state: RepoFormState) {
  const type: 'git' | 'helm' | 'oci' = state.method === 'helm' ? 'helm' : state.method === 'oci' ? 'oci' : 'git';
  const github =
    state.method === 'githubApp' && state.githubSource === 'key'
      ? compact({
          appID: Number(state.appID),
          installationID: state.installationID.trim() ? Number(state.installationID) : undefined,
          enterpriseBaseURL: trimmed(state.enterpriseBaseURL),
        })
      : undefined;
  return compact({
    type,
    proxy: trimmed(state.proxy),
    noProxy: trimmed(state.noProxy),
    forceHTTPBasicAuth: state.method === 'https' && state.httpsAuth === 'basic' ? state.forceHTTPBasicAuth : undefined,
    enableOCI: state.method === 'helm' ? state.enableOCI : undefined,
    insecureOCIForceHTTP: state.method === 'oci' || (state.method === 'helm' && state.enableOCI) ? state.insecureOCIForceHTTP : undefined,
    useAzureWorkloadIdentity: ['https', 'helm', 'oci'].includes(state.method) ? state.useAzureWorkloadIdentity : undefined,
    githubApp: github,
    scmProviderRef: state.method === 'githubApp' && state.githubSource === 'scmProvider' ? { name: state.scmProvider } : undefined,
    azureServicePrincipal:
      state.method === 'azure'
        ? compact({ tenantID: state.azureTenantID.trim(), clientID: state.azureClientID.trim(), activeDirectoryEndpoint: trimmed(state.azureEndpoint) })
        : undefined,
  });
}

/**
 * Write request for a repository. Secret material goes only in the write-only `credentials`
 * object, which the server stores in a Secret it manages; `spec.credentialsRef` is
 * server-managed, so it is only repeated unchanged on updates (absent `credentials` keeps the
 * stored ones).
 */
export function toRepositoryRequest(state: RepoFormState, purpose: 'read' | 'write', existing?: RepositoryInfo): RepositoryWriteRequest {
  const spec: RepositorySpec = {
    ...sharedSpec(state),
    ...compact({
      url: state.url.trim(),
      name: trimmed(state.name),
      project: trimmed(state.project),
      insecure: state.method !== 'gcp' && state.method !== 'azure' ? state.insecure : undefined,
      enableLFS: state.method === 'https' || state.method === 'ssh' || state.method === 'githubApp' ? state.enableLFS : undefined,
      depth: state.depth.trim() ? Number(state.depth) : undefined,
      insecureIgnoreHostKey: state.method === 'ssh' ? state.insecureIgnoreHostKey : undefined,
      purpose: purpose === 'write' ? ('write' as const) : undefined,
    }),
    url: state.url.trim(),
  };
  // Keep the existing Secret reference when no new secret material is entered.
  if (existing?.spec.credentialsRef) spec.credentialsRef = existing.spec.credentialsRef;
  const request: RepositoryWriteRequest = {
    metadata: existing ? { name: existing.metadata.name, namespace: existing.metadata.namespace, resourceVersion: existing.metadata.resourceVersion } : {},
    spec,
  };
  const credentials = credentialsOf(state);
  if (credentials) request.credentials = credentials;
  return request;
}

/** Write request for a credential template matched by URL prefix. */
export function toCredentialRequest(state: RepoFormState, purpose: 'read' | 'write'): RepositoryCredentialWriteRequest {
  const request: RepositoryCredentialWriteRequest = {
    metadata: {},
    spec: { ...sharedSpec(state), urlPrefix: state.url.trim(), ...(purpose === 'write' ? { purpose: 'write' as const } : {}) },
  };
  const credentials = credentialsOf(state);
  if (credentials) request.credentials = credentials;
  return request;
}

export const fieldId = (key: keyof RepoFormState) => `repo-${key}`;

/**
 * Client-side checks per method. `editing`: secrets may stay empty (the stored ones are kept).
 * `mode`: templates take a URL prefix and skip repository-only fields.
 */
export function validateRepoForm(state: RepoFormState, options: { editing: boolean; mode: FormMode }): FieldError[] {
  const errors: FieldError[] = [];
  const add = (key: keyof RepoFormState, message: string | null) => {
    if (message) errors.push({ id: fieldId(key), message });
  };
  const { editing, mode } = options;
  const urlKind = state.method === 'ssh' ? 'ssh' : state.method === 'helm' ? 'helm' : state.method === 'oci' ? 'oci' : 'https';
  if (mode === 'template') {
    if (state.url.trim() === '') add('url', 'URL prefix is required.');
    else if (/\s/.test(state.url)) add('url', 'URL prefix must not contain spaces.');
  } else {
    add('url', validateRepoURL(state.url, urlKind));
  }
  if (mode === 'repository' && state.method === 'helm' && state.name.trim() === '') add('name', 'Name is required for Helm repositories.');
  add('proxy', validateProxy(state.proxy));
  if (mode === 'repository') add('depth', validatePositiveInteger(state.depth, 'Depth'));

  const needsSecret = !editing;
  switch (state.method) {
    case 'https':
    case 'helm':
    case 'oci':
      if (!state.useAzureWorkloadIdentity) {
        if (state.httpsAuth === 'basic' && state.password.trim() !== '' && state.username.trim() === '') {
          add('username', 'Enter the username for this password.');
        }
        if (state.httpsAuth === 'bearer' && needsSecret && state.bearerToken.trim() === '') add('bearerToken', 'Enter the bearer token.');
      }
      if ((state.tlsClientCertData.trim() === '') !== (state.tlsClientCertKey.trim() === '')) {
        add(state.tlsClientCertData.trim() === '' ? 'tlsClientCertData' : 'tlsClientCertKey', 'Provide both the TLS client certificate and its key, or neither.');
      }
      break;
    case 'ssh':
      if (needsSecret && state.sshPrivateKey.trim() === '') add('sshPrivateKey', 'Paste the SSH private key.');
      else if (state.sshPrivateKey.trim() !== '' && !state.sshPrivateKey.includes('PRIVATE KEY-----')) {
        add('sshPrivateKey', 'The key must be a PEM or OpenSSH private key ("-----BEGIN … PRIVATE KEY-----").');
      }
      break;
    case 'githubApp':
      if (state.githubSource === 'scmProvider') {
        if (state.scmProvider === '') add('scmProvider', 'Choose an SCM provider.');
      } else {
        add('appID', validatePositiveInteger(state.appID, 'App ID', true));
        add('installationID', validatePositiveInteger(state.installationID, 'Installation ID'));
        add('enterpriseBaseURL', validateHTTPURL(state.enterpriseBaseURL, 'Enterprise base URL', false));
        if (needsSecret && state.githubAppPrivateKey.trim() === '') add('githubAppPrivateKey', 'Paste the GitHub App private key.');
        else if (state.githubAppPrivateKey.trim() !== '' && !state.githubAppPrivateKey.includes('PRIVATE KEY-----')) {
          add('githubAppPrivateKey', 'The private key must be in PEM format ("-----BEGIN RSA PRIVATE KEY-----").');
        }
      }
      break;
    case 'gcp':
      if (needsSecret && state.gcpServiceAccountKey.trim() === '') add('gcpServiceAccountKey', 'Paste the service account key JSON.');
      else if (state.gcpServiceAccountKey.trim() !== '') {
        try {
          const parsed: unknown = JSON.parse(state.gcpServiceAccountKey);
          if (!parsed || typeof parsed !== 'object' || (parsed as { type?: unknown }).type !== 'service_account') {
            add('gcpServiceAccountKey', 'The key JSON must have "type": "service_account".');
          }
        } catch {
          add('gcpServiceAccountKey', 'The service account key must be valid JSON.');
        }
      }
      break;
    case 'azure':
      if (state.azureTenantID.trim() === '') add('azureTenantID', 'Tenant ID is required.');
      if (state.azureClientID.trim() === '') add('azureClientID', 'Client ID is required.');
      if (needsSecret && state.azureClientSecret.trim() === '') add('azureClientSecret', 'Client secret is required.');
      add('azureEndpoint', validateHTTPURL(state.azureEndpoint, 'Active Directory endpoint', false));
      break;
    default:
      break;
  }
  return errors;
}

/** Next step for a failed connection test, keyed off the server message. */
export function connectionGuidance(message: string | undefined): string {
  const m = message ?? '';
  if (/knownhosts|host key|known_hosts/i.test(m)) {
    return 'The SSH host key of this server is not trusted yet. Add it under Certificates and known hosts, then test again.';
  }
  if (/x509|certificate/i.test(m)) {
    return "The server's TLS certificate is not trusted. Add its CA certificate under Certificates and known hosts, or enable “Skip server verification” for testing only.";
  }
  if (/auth|401|403|credential|password|permission|denied/i.test(m)) {
    return 'Check the username, password, token or key, and that the account can read this repository.';
  }
  if (/timeout|no such host|lookup|refused|dial tcp|unreachable/i.test(m)) {
    return 'Patrol could not reach the server. Check the URL, DNS and firewall rules, and the proxy settings.';
  }
  if (/not found|404/i.test(m)) return 'The repository was not found. Check the URL and that the credentials can see it.';
  return 'Check the URL and credentials, then test the connection again.';
}

/** Default template prefix for a repository URL: everything up to the last path segment. */
export function suggestedPrefix(url: string): string {
  const trimmedUrl = url.trim();
  const cut = trimmedUrl.lastIndexOf('/');
  return cut > trimmedUrl.indexOf('//') + 1 ? trimmedUrl.slice(0, cut) : trimmedUrl;
}
