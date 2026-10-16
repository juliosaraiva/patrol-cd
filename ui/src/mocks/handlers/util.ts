import { HttpResponse } from 'msw';
import type { ErrorCode, Problem } from '../../api/types';
import { loggedInUser } from '../fixtures';

/** Mutable mock server state; reset between tests with resetMockState(). */
export const mockState = {
  loggedIn: true,
  username: loggedInUser.username ?? 'mock-admin',
};

export function resetMockState(): void {
  mockState.loggedIn = true;
  mockState.username = loggedInUser.username ?? 'mock-admin';
}

/** HTTP reason phrases patrol-server puts in Problem.title (Go's http.StatusText). */
const REASONS: Record<number, string> = {
  400: 'Bad Request',
  401: 'Unauthorized',
  403: 'Forbidden',
  404: 'Not Found',
  409: 'Conflict',
  412: 'Precondition Failed',
  429: 'Too Many Requests',
  500: 'Internal Server Error',
  501: 'Not Implemented',
  502: 'Bad Gateway',
  503: 'Service Unavailable',
  504: 'Gateway Timeout',
};

/**
 * RFC 9457 problem document as patrol-server writes it: `type` urn:patrol-cd:problem:<code>,
 * `title` the reason phrase of the status, the stable `code`, the `detail` and a request ID.
 */
export function problemBody(status: number, code: ErrorCode, detail: string): Problem {
  return { type: `urn:patrol-cd:problem:${code}`, title: REASONS[status] ?? 'Error', status, detail, code, requestId: 'mock-request' };
}

/** RFC 9457 problem response (application/problem+json) like patrol-server returns. */
export function problem(status: number, code: ErrorCode, detail: string, headers: Record<string, string> = {}) {
  return HttpResponse.json(problemBody(status, code, detail), {
    status,
    headers: { 'Content-Type': 'application/problem+json', ...headers },
  });
}

/** Body of the operations whose response is `Empty` or `ApplicationResponse` (`{}`). */
export const emptyObject: Record<string, never> = {};

/** Empty list of a CRD-backed list route: `{metadata: {resourceVersion}, items: []}`. */
export const emptyList = { items: [], metadata: { resourceVersion: '1' } };
