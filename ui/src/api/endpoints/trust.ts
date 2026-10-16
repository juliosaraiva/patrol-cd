import { request, seg, type CallOptions } from '../client';
import type {
  GnuPGPublicKey,
  GnuPGPublicKeyCreateResponse,
  GnuPGPublicKeyList,
  RepositoryCertificateList,
  SCMProvider,
  SCMProviderList,
  SCMProviderWriteRequest,
} from '../types';

export interface CertificateQuery {
  /** File glob on the server name; empty matches everything. */
  hostNamePattern?: string;
  /** "" or "*" (all), "ssh", "https" ("tls" is an alias). */
  certType?: '' | '*' | 'https' | 'ssh' | 'tls';
  /** SSH key type filter. */
  certSubType?: string;
}

/** GET /api/v1/certificates */
export function listCertificates(query: CertificateQuery = {}, options: CallOptions = {}): Promise<RepositoryCertificateList> {
  return request('/api/v1/certificates', { query: { ...query }, signal: options.signal });
}

/**
 * POST /api/v1/certificates — TLS certificates (PEM in `certData`) and SSH known-hosts entries
 * (`certSubType` key type, base64 key in `certData`). certData is plain text, not base64 of it.
 */
export function createCertificates(body: RepositoryCertificateList, query: { upsert?: boolean } = {}, options: CallOptions = {}): Promise<RepositoryCertificateList> {
  return request('/api/v1/certificates', { method: 'POST', query, body, signal: options.signal });
}

/** DELETE /api/v1/certificates — removes every certificate matching the query. */
export function deleteCertificates(query: CertificateQuery, options: CallOptions = {}): Promise<RepositoryCertificateList> {
  return request('/api/v1/certificates', { method: 'DELETE', query: { ...query }, signal: options.signal });
}

/** GET /api/v1/gpgkeys */
export function listGPGKeys(options: CallOptions = {}): Promise<GnuPGPublicKeyList> {
  return request('/api/v1/gpgkeys', { signal: options.signal });
}

/** POST /api/v1/gpgkeys — body carries ASCII-armored `keyData`; existing keys are skipped. */
export function createGPGKey(body: GnuPGPublicKey, query: { upsert?: boolean } = {}, options: CallOptions = {}): Promise<GnuPGPublicKeyCreateResponse> {
  return request('/api/v1/gpgkeys', { method: 'POST', query, body, signal: options.signal });
}

/** GET /api/v1/gpgkeys/{keyID} */
export function getGPGKey(keyID: string, options: CallOptions = {}): Promise<GnuPGPublicKey> {
  return request(`/api/v1/gpgkeys/${seg(keyID)}`, { signal: options.signal });
}

/** DELETE /api/v1/gpgkeys/{keyID} */
export async function deleteGPGKey(keyID: string, options: CallOptions = {}): Promise<void> {
  await request(`/api/v1/gpgkeys/${seg(keyID)}`, { method: 'DELETE', signal: options.signal });
}

/** GET /api/v1/scmproviders */
export function listSCMProviders(options: CallOptions = {}): Promise<SCMProviderList> {
  return request('/api/v1/scmproviders', { signal: options.signal });
}

/** POST /api/v1/scmproviders — secrets are referenced through the spec's `secretRef`s, never sent. */
export function createSCMProvider(body: SCMProviderWriteRequest, options: CallOptions = {}): Promise<SCMProvider> {
  return request('/api/v1/scmproviders', { method: 'POST', body, signal: options.signal });
}

/** GET /api/v1/scmproviders/{name} */
export function getSCMProvider(name: string, options: CallOptions = {}): Promise<SCMProvider> {
  return request(`/api/v1/scmproviders/${seg(name)}`, { signal: options.signal });
}

/** PUT /api/v1/scmproviders/{name} */
export function updateSCMProvider(name: string, body: SCMProviderWriteRequest, options: CallOptions = {}): Promise<SCMProvider> {
  return request(`/api/v1/scmproviders/${seg(name)}`, { method: 'PUT', body, signal: options.signal });
}

/** DELETE /api/v1/scmproviders/{name} */
export async function deleteSCMProvider(name: string, options: CallOptions = {}): Promise<void> {
  await request(`/api/v1/scmproviders/${seg(name)}`, { method: 'DELETE', signal: options.signal });
}
