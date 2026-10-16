import type { SCMProvider, SCMProviderWriteRequest } from '../../../api/types';
import type { FieldError } from '../common/feedback';
import { validateHTTPURL, validateObjectName, validatePositiveInteger, validateSecretKey } from '../common/validation';

export type ProviderType = SCMProvider['spec']['type'];
export type AuthMethod = 'none' | 'token' | 'basic' | 'githubApp';

export const PROVIDER_TYPES: { value: ProviderType; label: string; defaultAPI?: string; selfHosted: boolean }[] = [
  { value: 'github', label: 'GitHub', defaultAPI: 'https://api.github.com', selfHosted: false },
  { value: 'gitlab', label: 'GitLab', defaultAPI: 'https://gitlab.com/api/v4', selfHosted: false },
  { value: 'gitea', label: 'Gitea', selfHosted: true },
  { value: 'bitbucketCloud', label: 'Bitbucket Cloud', defaultAPI: 'https://api.bitbucket.org/2.0', selfHosted: false },
  { value: 'bitbucketServer', label: 'Bitbucket Data Center', selfHosted: true },
  { value: 'azureDevOps', label: 'Azure DevOps', defaultAPI: 'https://dev.azure.com', selfHosted: false },
  { value: 'awsCodeCommit', label: 'AWS CodeCommit', selfHosted: false },
];

export function typeLabel(type: string): string {
  return PROVIDER_TYPES.find((t) => t.value === type)?.label ?? type;
}

/**
 * Path segment of the server's webhook receiver for a provider type (/api/webhook/<name>), or
 * undefined when the server has no receiver for it. The server matches names case-insensitively
 * and accepts these spellings (internal/webhook providers and aliases). The route is hidden from
 * the published document, so the mapping lives here (UI-6 in integration-findings.md).
 */
export function webhookProviderName(type: string): string | undefined {
  const names: Record<string, string> = {
    github: 'github',
    gitlab: 'gitlab',
    gitea: 'gitea',
    bitbucketCloud: 'bitbucket',
    bitbucketServer: 'bitbucketserver',
    azureDevOps: 'azuredevops',
  };
  return names[type];
}

/**
 * A Secret key the provider references. SCM provider writes carry only these references: the
 * Secret is created outside Patrol (kubectl, GitOps, an external secrets operator) and its
 * values never pass through the API.
 */
export interface SecretField {
  name: string;
  key: string;
}

const emptySecret = (key: string): SecretField => ({ name: '', key });

export interface ProviderForm {
  name: string;
  type: ProviderType;
  apiURL: string;
  webURL: string;
  insecure: boolean;
  auth: AuthMethod;
  token: SecretField;
  /** Basic auth Secret holds keys "username" and "password". */
  basicSecret: string;
  appID: string;
  installationID: string;
  appKey: SecretField;
  webhookEnabled: boolean;
  webhook: SecretField;
  caEnabled: boolean;
  ca: SecretField;
  statusEnabled: boolean;
  statusMode: 'checks' | 'statuses';
  contextPrefix: string;
  rateLimitEnabled: boolean;
  requestsPerSecond: string;
  burst: string;
}

export function emptyProviderForm(): ProviderForm {
  return {
    name: '',
    type: 'github',
    apiURL: '',
    webURL: '',
    insecure: false,
    auth: 'token',
    token: emptySecret('token'),
    basicSecret: '',
    appID: '',
    installationID: '',
    appKey: emptySecret('privateKey'),
    webhookEnabled: false,
    webhook: emptySecret('secret'),
    caEnabled: false,
    ca: emptySecret('ca.crt'),
    statusEnabled: false,
    statusMode: 'statuses',
    contextPrefix: '',
    rateLimitEnabled: false,
    requestsPerSecond: '',
    burst: '',
  };
}

export function authOf(provider: SCMProvider): AuthMethod {
  const auth = provider.spec.auth;
  if (auth?.githubApp) return 'githubApp';
  if (auth?.basicAuthSecretRef) return 'basic';
  if (auth?.tokenSecretRef) return 'token';
  return 'none';
}

export function authSummary(provider: SCMProvider): string {
  const auth = provider.spec.auth;
  switch (authOf(provider)) {
    case 'githubApp':
      return `GitHub App ${auth?.githubApp?.appID ?? ''}`;
    case 'basic':
      return `Basic (Secret ${auth?.basicAuthSecretRef?.name ?? ''})`;
    case 'token':
      return `Token (Secret ${auth?.tokenSecretRef?.name ?? ''})`;
    default:
      return 'Anonymous';
  }
}

export function formFromProvider(p: SCMProvider): ProviderForm {
  const s = p.spec;
  const ref = (r: { name: string; key: string } | undefined, key: string): SecretField => (r ? { name: r.name, key: r.key } : emptySecret(key));
  return {
    ...emptyProviderForm(),
    name: p.metadata.name ?? '',
    type: s.type,
    apiURL: s.apiURL ?? '',
    webURL: s.webURL ?? '',
    insecure: s.insecure ?? false,
    auth: authOf(p),
    token: ref(s.auth?.tokenSecretRef, 'token'),
    basicSecret: s.auth?.basicAuthSecretRef?.name ?? '',
    appID: s.auth?.githubApp ? String(s.auth.githubApp.appID) : '',
    installationID: s.auth?.githubApp?.installationID ? String(s.auth.githubApp.installationID) : '',
    appKey: ref(s.auth?.githubApp?.privateKeySecretRef, 'privateKey'),
    webhookEnabled: Boolean(s.webhook),
    webhook: ref(s.webhook?.secretRef, 'secret'),
    caEnabled: Boolean(s.caSecretRef),
    ca: ref(s.caSecretRef, 'ca.crt'),
    statusEnabled: s.statusReporting?.enabled ?? false,
    statusMode: s.statusReporting?.mode ?? 'statuses',
    contextPrefix: s.statusReporting?.contextPrefix ?? '',
    rateLimitEnabled: Boolean(s.rateLimit),
    requestsPerSecond: s.rateLimit ? String(s.rateLimit.requestsPerSecond) : '',
    burst: s.rateLimit?.burst ? String(s.rateLimit.burst) : '',
  };
}

const secretRef = (f: SecretField) => ({ name: f.name.trim(), key: f.key.trim() });

/** Create/update body: metadata and spec; every secret is a `secretRef` (no values are sent). */
export function toProviderRequest(f: ProviderForm, existing?: SCMProvider): SCMProviderWriteRequest {
  const spec: SCMProvider['spec'] = { type: f.type };
  if (f.apiURL.trim()) spec.apiURL = f.apiURL.trim();
  if (f.webURL.trim()) spec.webURL = f.webURL.trim();
  if (f.insecure) spec.insecure = true;
  switch (f.auth) {
    case 'token':
      spec.auth = { tokenSecretRef: secretRef(f.token) };
      break;
    case 'basic':
      spec.auth = { basicAuthSecretRef: { name: f.basicSecret.trim() } };
      break;
    case 'githubApp':
      spec.auth = {
        githubApp: {
          appID: Number(f.appID),
          ...(f.installationID.trim() ? { installationID: Number(f.installationID) } : {}),
          privateKeySecretRef: secretRef(f.appKey),
        },
      };
      break;
    default:
      break;
  }
  if (f.webhookEnabled) spec.webhook = { secretRef: secretRef(f.webhook) };
  if (f.caEnabled) spec.caSecretRef = secretRef(f.ca);
  if (f.statusEnabled) {
    spec.statusReporting = { enabled: true, mode: f.statusMode, ...(f.contextPrefix.trim() ? { contextPrefix: f.contextPrefix.trim() } : {}) };
  } else if (existing?.spec.statusReporting) {
    spec.statusReporting = { enabled: false };
  }
  if (f.rateLimitEnabled) {
    spec.rateLimit = { requestsPerSecond: Number(f.requestsPerSecond), ...(f.burst.trim() ? { burst: Number(f.burst) } : {}) };
  }
  return {
    metadata: { name: f.name.trim(), ...(existing ? { namespace: existing.metadata.namespace, resourceVersion: existing.metadata.resourceVersion } : {}) },
    spec,
  };
}

export const providerFieldId = (key: string) => `scm-${key}`;

function checkSecret(errors: FieldError[], prefix: string, f: SecretField, what: string) {
  const nameError = validateObjectName(f.name, `${what} Secret name`);
  if (nameError) errors.push({ id: providerFieldId(`${prefix}-name`), message: nameError });
  const keyError = validateSecretKey(f.key);
  if (keyError) errors.push({ id: providerFieldId(`${prefix}-key`), message: `${what}: ${keyError}` });
}

export function validateProviderForm(f: ProviderForm, editing: boolean): FieldError[] {
  const errors: FieldError[] = [];
  const add = (key: string, message: string | null) => {
    if (message) errors.push({ id: providerFieldId(key), message });
  };
  if (!editing) add('name', validateObjectName(f.name));
  const type = PROVIDER_TYPES.find((t) => t.value === f.type);
  add('apiURL', validateHTTPURL(f.apiURL, 'API URL', type?.selfHosted ?? false));
  add('webURL', validateHTTPURL(f.webURL, 'Web URL', false));
  switch (f.auth) {
    case 'token':
      checkSecret(errors, 'token', f.token, 'Token');
      break;
    case 'basic':
      add('basicSecret', validateObjectName(f.basicSecret, 'Basic auth Secret name'));
      break;
    case 'githubApp':
      add('appID', validatePositiveInteger(f.appID, 'App ID', true));
      add('installationID', validatePositiveInteger(f.installationID, 'Installation ID'));
      checkSecret(errors, 'appKey', f.appKey, 'Private key');
      break;
    default:
      break;
  }
  if (f.webhookEnabled) checkSecret(errors, 'webhook', f.webhook, 'Webhook secret');
  if (f.caEnabled) checkSecret(errors, 'ca', f.ca, 'CA certificate');
  if (f.statusEnabled && f.statusMode === 'checks' && (f.type !== 'github' || f.auth !== 'githubApp')) {
    add('statusMode', 'Check runs need a GitHub provider authenticated as a GitHub App; use commit statuses instead.');
  }
  if (f.rateLimitEnabled) {
    const rps = Number(f.requestsPerSecond);
    if (f.requestsPerSecond.trim() === '' || !Number.isFinite(rps) || rps <= 0) add('rps', 'Requests per second must be a positive number.');
    add('burst', validatePositiveInteger(f.burst, 'Burst'));
  }
  return errors;
}

/** Ready condition of the provider (authentication and API reachability). */
export function readiness(p: SCMProvider): { status: 'Successful' | 'Failed' | 'Unknown'; message: string | undefined } {
  const ready = p.status?.conditions?.find((c) => c.type === 'Ready');
  if (!ready) return { status: 'Unknown', message: 'Not checked yet' };
  const message = [ready.reason, ready.message].filter(Boolean).join(': ');
  return { status: ready.status === 'True' ? 'Successful' : ready.status === 'False' ? 'Failed' : 'Unknown', message: message || undefined };
}
