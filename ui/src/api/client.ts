import { resolveServerURL } from './basePath';
import { ApiError, networkError, toApiError } from './errors';

export type QueryScalar = string | number | boolean;
export type QueryValue = QueryScalar | readonly QueryScalar[] | null | undefined;
export type QueryParams = Readonly<Record<string, QueryValue>>;

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  query?: QueryParams | undefined;
  /** JSON-serialized request body. */
  body?: unknown;
  /** Pre-encoded body (e.g. a tar.gz Blob); sent as-is with `contentType`. */
  rawBody?: BodyInit;
  contentType?: string;
  headers?: Readonly<Record<string, string>>;
  signal?: AbortSignal | undefined;
  /** Redirect to login on 401 (default true). Off for session bootstrap and login calls. */
  authRedirect?: boolean;
  /** How to read a successful body (default: JSON when the response says so, else text). */
  responseType?: 'json' | 'text' | 'response';
}

/** Per-call options that endpoint functions forward. */
export interface CallOptions {
  signal?: AbortSignal | undefined;
}

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);

/** Header required on cookie-authenticated state-changing requests (ADR-0009). */
export const CSRF_HEADER = 'X-Requested-With';
export const CSRF_VALUE = 'patrol';

/** Appends query parameters; arrays repeat the key, null/undefined/"" are skipped. */
export function buildURL(path: string, query?: QueryParams): URL {
  const url = resolveServerURL(path);
  if (query) {
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === null || value === '') continue;
      const values: readonly QueryScalar[] = Array.isArray(value) ? value : [value as QueryScalar];
      for (const v of values) url.searchParams.append(key, String(v));
    }
  }
  return url;
}

/** Encodes one path segment; use for every user-controlled value placed in a path. */
export function seg(value: string): string {
  return encodeURIComponent(value);
}

/** Encodes a value that may contain "/" separators, keeping the separators (can-i, extensions). */
export function segs(value: string): string {
  return value.split('/').map(encodeURIComponent).join('/');
}

type UnauthorizedHandler = (returnURL: string) => void;

let unauthorizedHandler: UnauthorizedHandler = (returnURL) => {
  const login = resolveServerURL('login');
  login.searchParams.set('return_url', returnURL);
  window.location.assign(login.href);
};

/**
 * Installs the 401 handler. The app shell installs one that knows whether SSO is configured;
 * the default sends the browser to the login page.
 */
export function setUnauthorizedHandler(handler: UnauthorizedHandler): () => void {
  const previous = unauthorizedHandler;
  unauthorizedHandler = handler;
  return () => {
    unauthorizedHandler = previous;
  };
}

/** Invokes the 401 handler with the current location as the return URL. */
export function notifyUnauthorized(): void {
  unauthorizedHandler(window.location.href);
}

/**
 * Fetch wrapper for the Patrol API. Same-origin cookie auth, JSON in and out, CSRF header on
 * mutations, problem+json errors mapped to ApiError, 401 → login redirect.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? 'GET';
  const headers = new Headers(options.headers);
  headers.set('Accept', 'application/json, application/problem+json;q=0.9, */*;q=0.1');
  if (!SAFE_METHODS.has(method)) headers.set(CSRF_HEADER, CSRF_VALUE);

  let body: BodyInit | undefined;
  if (options.rawBody !== undefined) {
    body = options.rawBody;
    if (options.contentType) headers.set('Content-Type', options.contentType);
  } else if (options.body !== undefined) {
    body = JSON.stringify(options.body);
    headers.set('Content-Type', options.contentType ?? 'application/json');
  }

  let response: Response;
  try {
    response = await fetch(buildURL(path, options.query), {
      method,
      headers,
      body,
      credentials: 'same-origin',
      signal: options.signal,
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err;
    throw networkError(err);
  }

  if (!response.ok) {
    const error = await toApiError(response);
    if (response.status === 401 && options.authRedirect !== false) notifyUnauthorized();
    throw error;
  }

  if (options.responseType === 'response') return response as T;
  if (response.status === 204 || response.headers.get('Content-Length') === '0') {
    return undefined as T;
  }
  const contentType = response.headers.get('Content-Type') ?? '';
  if (options.responseType === 'text' || !contentType.includes('json')) {
    return (await response.text()) as T;
  }
  const text = await response.text();
  if (text === '') return undefined as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new ApiError({ status: response.status, code: 'internal', detail: 'The server returned malformed JSON' });
  }
}
