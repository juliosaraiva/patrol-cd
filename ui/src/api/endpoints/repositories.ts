import { request, seg, type CallOptions } from '../client';
import type {
  ConnectionState,
  QueryOf,
  RepositoryAppDetails,
  RepositoryAppDetailsQuery,
  RepositoryAppList,
  RepositoryCredentialInfo,
  RepositoryCredentialInfoList,
  RepositoryCredentialWriteRequest,
  RepositoryHelmChartList,
  RepositoryInfo,
  RepositoryInfoList,
  RepositoryRefs,
  RepositoryWriteRequest,
} from '../types';

export type ListRepositoriesQuery = QueryOf<'listRepositories'>;

/**
 * Repositories are addressed by URL-encoded repository URL (Argo-compatible). `purpose: 'write'`
 * switches to the /write-repositories and /write-repocreds variants used by the hydrator.
 * Responses are RepositoryInfo / RepositoryCredentialInfo: the object plus `hasCredentials`
 * (and `inheritedCredentials` for repositories), never secret material. Request bodies carry
 * secrets only in the write-only `credentials` object.
 */
export type RepositoryPurpose = 'read' | 'write';

const repoRoot = (purpose: RepositoryPurpose) =>
  purpose === 'write' ? '/api/v1/write-repositories' : '/api/v1/repositories';
const credsRoot = (purpose: RepositoryPurpose) => (purpose === 'write' ? '/api/v1/write-repocreds' : '/api/v1/repocreds');
const repoPath = (url: string, purpose: RepositoryPurpose = 'read') => `${repoRoot(purpose)}/${seg(url)}`;

interface RepoOptions extends CallOptions {
  purpose?: RepositoryPurpose;
}

/** GET /api/v1/repositories (or /write-repositories) */
export function listRepositories(query: ListRepositoriesQuery = {}, options: RepoOptions = {}): Promise<RepositoryInfoList> {
  return request(repoRoot(options.purpose ?? 'read'), { query, signal: options.signal });
}

/** POST /api/v1/repositories (or /write-repositories) */
export function createRepository(
  body: RepositoryWriteRequest,
  query: { upsert?: boolean; credsOnly?: boolean } = {},
  options: RepoOptions = {},
): Promise<RepositoryInfo> {
  return request(repoRoot(options.purpose ?? 'read'), { method: 'POST', query, body, signal: options.signal });
}

/** GET /api/v1/repositories/{repo} */
export function getRepository(
  url: string,
  query: { forceRefresh?: boolean; appProject?: string } = {},
  options: RepoOptions = {},
): Promise<RepositoryInfo> {
  return request(repoPath(url, options.purpose), { query, signal: options.signal });
}

/** PUT /api/v1/repositories/{repo} */
export function updateRepository(
  url: string,
  body: RepositoryWriteRequest,
  query: { appProject?: string } = {},
  options: RepoOptions = {},
): Promise<RepositoryInfo> {
  return request(repoPath(url, options.purpose), { method: 'PUT', query, body, signal: options.signal });
}

/** DELETE /api/v1/repositories/{repo} */
export async function deleteRepository(url: string, query: { appProject?: string } = {}, options: RepoOptions = {}): Promise<void> {
  await request(repoPath(url, options.purpose), { method: 'DELETE', query, signal: options.signal });
}

/** POST /api/v1/repositories/{repo}/validate — test a connection without saving. */
export function validateRepository(url: string, body: RepositoryWriteRequest, options: RepoOptions = {}): Promise<ConnectionState> {
  return request(`${repoPath(url, options.purpose)}/validate`, { method: 'POST', body, signal: options.signal });
}

/** GET /api/v1/repositories/{repo}/refs */
export function listRepositoryRefs(url: string, query: { appProject?: string } = {}, options: CallOptions = {}): Promise<RepositoryRefs> {
  return request(`${repoPath(url)}/refs`, { query, signal: options.signal });
}

/** GET /api/v1/repositories/{repo}/oci-tags — tags are in `tags` (RepositoryRefs). */
export function listRepositoryOCITags(url: string, query: { appProject?: string } = {}, options: CallOptions = {}): Promise<RepositoryRefs> {
  return request(`${repoPath(url)}/oci-tags`, { query, signal: options.signal });
}

/** GET /api/v1/repositories/{repo}/apps */
export function listRepositoryApps(
  url: string,
  query: { revision?: string; appName?: string; appProject?: string } = {},
  options: CallOptions = {},
): Promise<RepositoryAppList> {
  return request(`${repoPath(url)}/apps`, { query, signal: options.signal });
}

/** POST /api/v1/repositories/{repo}/appdetails */
export function getRepositoryAppDetails(
  url: string,
  body: RepositoryAppDetailsQuery,
  options: CallOptions = {},
): Promise<RepositoryAppDetails> {
  return request(`${repoPath(url)}/appdetails`, { method: 'POST', body, signal: options.signal });
}

/** GET /api/v1/repositories/{repo}/helmcharts */
export function listRepositoryHelmCharts(
  url: string,
  query: { appProject?: string } = {},
  options: CallOptions = {},
): Promise<RepositoryHelmChartList> {
  return request(`${repoPath(url)}/helmcharts`, { query, signal: options.signal });
}

/** GET /api/v1/repocreds (or /write-repocreds) */
export function listRepositoryCredentials(options: RepoOptions = {}): Promise<RepositoryCredentialInfoList> {
  return request(credsRoot(options.purpose ?? 'read'), { signal: options.signal });
}

/** POST /api/v1/repocreds (or /write-repocreds) */
export function createRepositoryCredential(
  body: RepositoryCredentialWriteRequest,
  query: { upsert?: boolean } = {},
  options: RepoOptions = {},
): Promise<RepositoryCredentialInfo> {
  return request(credsRoot(options.purpose ?? 'read'), { method: 'POST', query, body, signal: options.signal });
}

/** PUT /api/v1/repocreds/{url} */
export function updateRepositoryCredential(
  urlPrefix: string,
  body: RepositoryCredentialWriteRequest,
  options: RepoOptions = {},
): Promise<RepositoryCredentialInfo> {
  return request(`${credsRoot(options.purpose ?? 'read')}/${seg(urlPrefix)}`, { method: 'PUT', body, signal: options.signal });
}

/** DELETE /api/v1/repocreds/{url} */
export async function deleteRepositoryCredential(urlPrefix: string, options: RepoOptions = {}): Promise<void> {
  await request(`${credsRoot(options.purpose ?? 'read')}/${seg(urlPrefix)}`, { method: 'DELETE', signal: options.signal });
}
