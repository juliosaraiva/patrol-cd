/** Typed client for the published API (api/openapi/openapi.json); coverage is checked by ./contract.test.ts. */
export * from './endpoints/session';
export * from './endpoints/applications';
export * from './endpoints/applicationsets';
export * from './endpoints/projects';
export * from './endpoints/repositories';
export * from './endpoints/trust';
export * from './endpoints/clusters';
export * from './endpoints/misc';
export { ApiError, isApiError } from './errors';
export type { ErrorCode } from './errors';
export { request, buildURL, setUnauthorizedHandler, CSRF_HEADER, CSRF_VALUE } from './client';
export { openStream, FetchEventSource, createSSEParser } from './sse';
export type { StreamHandle, StreamState, StreamOptions, EventSourceConstructor, EventSourceLike } from './sse';
export type * from './types';
