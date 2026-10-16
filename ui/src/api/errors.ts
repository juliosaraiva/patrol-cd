import type { ErrorCode } from './types';

export type { ErrorCode };

const STATUS_CODES: Record<number, ErrorCode> = {
  400: 'invalid_argument',
  401: 'unauthenticated',
  403: 'permission_denied',
  404: 'not_found',
  409: 'conflict',
  412: 'failed_precondition',
  422: 'invalid_argument',
  429: 'resource_exhausted',
  501: 'internal',
  502: 'unavailable',
  503: 'unavailable',
  504: 'unavailable',
};

/** Problem `code` values of the published document (Problem.code enum). */
export const ERROR_CODES = [
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
] as const satisfies readonly ErrorCode[];
/** Compile-time check that ERROR_CODES lists every code of the document. */
type Never<T extends never> = T;
export type MissingErrorCodes = Never<Exclude<ErrorCode, (typeof ERROR_CODES)[number]>>;

const KNOWN_CODES = new Set<string>(ERROR_CODES);

/** Fallback code for an HTTP status when the body carries none. */
export function codeForStatus(status: number): ErrorCode {
  if (status === 0) return 'unavailable';
  return STATUS_CODES[status] ?? (status >= 500 ? 'internal' : 'invalid_argument');
}

/** Typed error for every failed API call (RFC 9457 problem details or a synthesized one). */
export class ApiError extends Error {
  override readonly name = 'ApiError';
  readonly status: number;
  readonly code: ErrorCode;
  readonly detail: string;
  readonly title: string;
  readonly type: string | undefined;
  /** Server request ID (Problem.requestId), for correlating with server logs. */
  readonly requestId: string | undefined;
  /** Seconds from a Retry-After header (rate limiting / login lockout). */
  readonly retryAfterSeconds: number | undefined;

  constructor(init: {
    status: number;
    code?: ErrorCode | undefined;
    detail?: string | undefined;
    title?: string | undefined;
    type?: string | undefined;
    requestId?: string | undefined;
    retryAfterSeconds?: number | undefined;
  }) {
    const code = init.code ?? codeForStatus(init.status);
    const title = init.title ?? defaultTitle(init.status);
    const trimmed = init.detail?.trim() ?? '';
    const detail = trimmed === '' ? title : trimmed;
    super(detail);
    this.status = init.status;
    this.code = code;
    this.detail = detail;
    this.title = title;
    this.type = init.type;
    this.requestId = init.requestId;
    this.retryAfterSeconds = init.retryAfterSeconds;
  }
}

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError;
}

function defaultTitle(status: number): string {
  if (status === 0) return 'The server could not be reached';
  if (status === 401) return 'Authentication required';
  if (status === 403) return 'Permission denied';
  if (status === 404) return 'Not found';
  if (status === 409) return 'Conflict';
  if (status === 429) return 'Too many requests';
  if (status >= 500) return 'Server error';
  return 'Request failed';
}

function parseRetryAfter(value: string | null): number | undefined {
  if (!value) return undefined;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0) return Math.ceil(seconds);
  const date = Date.parse(value);
  if (Number.isNaN(date)) return undefined;
  return Math.max(0, Math.ceil((date - Date.now()) / 1000));
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function str(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/**
 * Maps a failed Response to ApiError. Understands application/problem+json, plain JSON error
 * bodies ({error|message}) and text bodies.
 */
export async function toApiError(response: Response): Promise<ApiError> {
  const retryAfterSeconds = parseRetryAfter(response.headers.get('Retry-After'));
  const contentType = response.headers.get('Content-Type') ?? '';
  let text = '';
  try {
    text = await response.text();
  } catch {
    // Body unavailable (aborted or already consumed): fall back to the status line.
  }
  if (contentType.includes('json') && text) {
    try {
      const body = asRecord(JSON.parse(text));
      if (body) {
        const rawCode = str(body.code);
        return new ApiError({
          status: response.status,
          code: rawCode && KNOWN_CODES.has(rawCode) ? (rawCode as ErrorCode) : undefined,
          detail: str(body.detail) ?? str(body.message) ?? str(body.error),
          title: str(body.title),
          type: str(body.type),
          requestId: str(body.requestId),
          retryAfterSeconds,
        });
      }
    } catch {
      // Not JSON after all; treat as text below.
    }
  }
  const detail = text && !contentType.includes('html') ? text.slice(0, 500) : response.statusText;
  return new ApiError({ status: response.status, detail, retryAfterSeconds });
}

/**
 * Maps a problem document received outside an HTTP error response (an SSE `event: error`) to
 * ApiError. Returns undefined when `data` is not a problem document.
 */
export function problemToApiError(data: string): ApiError | undefined {
  let body: Record<string, unknown> | null;
  try {
    body = asRecord(JSON.parse(data));
  } catch {
    return undefined;
  }
  if (!body || typeof body.status !== 'number') return undefined;
  const rawCode = str(body.code);
  return new ApiError({
    status: body.status,
    code: rawCode && KNOWN_CODES.has(rawCode) ? (rawCode as ErrorCode) : undefined,
    detail: str(body.detail),
    title: str(body.title),
    type: str(body.type),
    requestId: str(body.requestId),
  });
}

/** Error for a request that never produced a response (DNS, TLS, offline, CORS). */
export function networkError(cause: unknown): ApiError {
  const reason = cause instanceof Error ? cause.message : String(cause);
  return new ApiError({ status: 0, code: 'unavailable', detail: `Network error: ${reason}` });
}
