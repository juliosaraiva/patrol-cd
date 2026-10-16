import { request, seg, type CallOptions } from '../client';
import type { ClusterInput, QueryOf, RegisteredCluster, RegisteredClusterList } from '../types';

export type ListClustersQuery = Pick<QueryOf<'listClusters'>, 'server' | 'name' | 'selector' | 'limit' | 'continue'>;

/**
 * Clusters are addressed by URL-encoded server URL, or by name with `idType: 'name'`
 * (`?id.type=name`).
 */
export interface ClusterRef {
  server?: string;
  name?: string;
}

function clusterPath(ref: ClusterRef): { path: string; idType: 'name' | undefined } {
  if (ref.server) return { path: `/api/v1/clusters/${seg(ref.server)}`, idType: undefined };
  if (ref.name) return { path: `/api/v1/clusters/${seg(ref.name)}`, idType: 'name' };
  throw new Error('cluster reference needs a server URL or a name');
}

/** GET /api/v1/clusters */
export function listClusters(query: ListClustersQuery = {}, options: CallOptions = {}): Promise<RegisteredClusterList> {
  return request('/api/v1/clusters', { query, signal: options.signal });
}

/** POST /api/v1/clusters — `body.config` carries the write-only credentials. */
export function createCluster(body: ClusterInput, query: { upsert?: boolean } = {}, options: CallOptions = {}): Promise<RegisteredCluster> {
  return request('/api/v1/clusters', { method: 'POST', query, body, signal: options.signal });
}

/** GET /api/v1/clusters/{server} */
export function getCluster(ref: ClusterRef, options: CallOptions = {}): Promise<RegisteredCluster> {
  const { path, idType } = clusterPath(ref);
  return request(path, { query: { 'id.type': idType }, signal: options.signal });
}

/** PUT /api/v1/clusters/{server}; `updatedFields` limits the update (e.g. ["name","namespaces"]). */
export function updateCluster(
  ref: ClusterRef,
  body: ClusterInput,
  updatedFields: string[] = [],
  options: CallOptions = {},
): Promise<RegisteredCluster> {
  const { path, idType } = clusterPath(ref);
  return request(path, { method: 'PUT', query: { updatedFields, 'id.type': idType }, body, signal: options.signal });
}

/** DELETE /api/v1/clusters/{server} */
export async function deleteCluster(ref: ClusterRef, options: CallOptions = {}): Promise<void> {
  const { path, idType } = clusterPath(ref);
  await request(path, { method: 'DELETE', query: { 'id.type': idType }, signal: options.signal });
}

/** POST /api/v1/clusters/{server}/rotate-auth */
export async function rotateClusterAuth(ref: ClusterRef, options: CallOptions = {}): Promise<void> {
  const { path, idType } = clusterPath(ref);
  await request(`${path}/rotate-auth`, { method: 'POST', query: { 'id.type': idType }, signal: options.signal });
}

/** POST /api/v1/clusters/{server}/invalidate-cache */
export function invalidateClusterCache(ref: ClusterRef, options: CallOptions = {}): Promise<RegisteredCluster> {
  const { path, idType } = clusterPath(ref);
  return request(`${path}/invalidate-cache`, { method: 'POST', query: { 'id.type': idType }, signal: options.signal });
}
