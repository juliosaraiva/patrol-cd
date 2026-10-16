import { request, seg, type CallOptions } from '../client';
import { openStream, type StreamHandle } from '../sse';
import type {
  ApplicationSet,
  ApplicationSetGenerateRequest,
  ApplicationSetGenerateResponse,
  ApplicationSetInput,
  ApplicationSetList,
  ApplicationSetTree,
  ApplicationSetWatchEvent,
  EventList,
  QueryOf,
} from '../types';
import type { StreamCallbacks } from './applications';

export type ListApplicationSetsQuery = QueryOf<'listApplicationSets'>;
/** `appSetNamespace` is the upstream spelling of `appsetNamespace`; the UI sends the latter. */
export type WatchApplicationSetsQuery = Omit<QueryOf<'watchApplicationSets'>, 'appSetNamespace'>;
export type DeleteApplicationSetQuery = Pick<QueryOf<'deleteApplicationSet'>, 'cascade' | 'propagationPolicy'>;

const base = (name: string) => `/api/v1/applicationsets/${seg(name)}`;

/** GET /api/v1/applicationsets */
export function listApplicationSets(query: ListApplicationSetsQuery = {}, options: CallOptions = {}): Promise<ApplicationSetList> {
  return request('/api/v1/applicationsets', { query, signal: options.signal });
}

/** GET /api/v1/stream/applicationsets (SSE) */
export function watchApplicationSets(
  query: WatchApplicationSetsQuery,
  callbacks: StreamCallbacks<ApplicationSetWatchEvent>,
): StreamHandle {
  return openStream({ ...callbacks, path: '/api/v1/stream/applicationsets', query });
}

/** POST /api/v1/applicationsets */
export function createApplicationSet(
  appset: ApplicationSetInput,
  query: { upsert?: boolean; dryRun?: boolean } = {},
  options: CallOptions = {},
): Promise<ApplicationSet> {
  return request('/api/v1/applicationsets', { method: 'POST', query, body: appset, signal: options.signal });
}

/** GET /api/v1/applicationsets/{name} */
export function getApplicationSet(name: string, appsetNamespace?: string, options: CallOptions = {}): Promise<ApplicationSet> {
  return request(base(name), { query: { appsetNamespace }, signal: options.signal });
}

/**
 * DELETE /api/v1/applicationsets/{name}. `cascade` (default true) deletes the generated
 * Applications; false orphans them.
 */
export async function deleteApplicationSet(
  name: string,
  appsetNamespace?: string,
  query: DeleteApplicationSetQuery = {},
  options: CallOptions = {},
): Promise<void> {
  await request(base(name), {
    method: 'DELETE',
    query: { appsetNamespace, cascade: query.cascade, propagationPolicy: query.propagationPolicy },
    signal: options.signal,
  });
}

/** POST /api/v1/applicationsets/generate — dry-run render; never persisted. */
export function generateApplicationSet(
  body: ApplicationSetGenerateRequest,
  options: CallOptions = {},
): Promise<ApplicationSetGenerateResponse> {
  return request('/api/v1/applicationsets/generate', { method: 'POST', body, signal: options.signal });
}

/** GET /api/v1/applicationsets/{name}/resource-tree — the ApplicationSet and its generated Applications. */
export function getApplicationSetResourceTree(name: string, appsetNamespace?: string, options: CallOptions = {}): Promise<ApplicationSetTree> {
  return request(`${base(name)}/resource-tree`, { query: { appsetNamespace }, signal: options.signal });
}

/** GET /api/v1/applicationsets/{name}/events */
export function listApplicationSetEvents(name: string, appsetNamespace?: string, options: CallOptions = {}): Promise<EventList> {
  return request(`${base(name)}/events`, { query: { appsetNamespace }, signal: options.signal });
}
