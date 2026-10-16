import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { server } from '../mocks/server';
import { problem } from '../mocks/handlers';
import { appBaseURL, resolveServerURL, routerBasename, toRouterPath } from './basePath';
import { buildURL, request, setUnauthorizedHandler } from './client';
import { terminalURL, watchApplications } from './endpoints/applications';
import { badgeURL } from './endpoints/misc';
import { getVersion, ssoLoginURL } from './endpoints/session';
import { ApiError, codeForStatus } from './errors';

function setBase(href: string) {
  const base = document.createElement('base');
  base.href = href;
  document.head.prepend(base);
}

describe('base path', () => {
  it('resolves API paths against <base href> so one build serves any root path', () => {
    setBase('/patrol/');
    expect(appBaseURL().pathname).toBe('/patrol/');
    expect(routerBasename()).toBe('/patrol');
    expect(resolveServerURL('/api/v1/applications').pathname).toBe('/patrol/api/v1/applications');
  });

  it('uses the origin root without a base element', () => {
    expect(routerBasename()).toBe('');
    expect(resolveServerURL('/api/v1/settings').pathname).toBe('/api/v1/settings');
  });

  it('maps same-origin URLs under the base to router paths and rejects others', () => {
    setBase('/patrol/');
    expect(toRouterPath(`${window.location.origin}/patrol/applications?x=1`)).toBe('/applications?x=1');
    expect(toRouterPath('https://evil.example/patrol/applications')).toBeNull();
    expect(toRouterPath(`${window.location.origin}/other/applications`)).toBeNull();
  });

  it('never returns a protocol-relative path', () => {
    const origin = window.location.origin;
    expect(toRouterPath('/.//evil.example/x')).toBeNull();
    expect(toRouterPath(`${origin}//evil.example`)).toBeNull();
    expect(toRouterPath('/.\\/evil.example')).toBeNull();
    setBase('/patrol/');
    expect(toRouterPath('/patrol/.//evil.example')).toBeNull();
    expect(toRouterPath(`${origin}/patrol//evil.example`)).toBeNull();
    expect(toRouterPath(`${origin}/patrol/applications//x`)).toBe('/applications//x');
  });
});

describe('URLs under a non-root base path (server --rootpath)', () => {
  it('sends requests, streams and builds navigation URLs relative to document.baseURI', async () => {
    setBase('/team/patrol/');
    const seen: string[] = [];
    server.use(
      http.get('*/api/v1/version', ({ request: req }) => {
        seen.push(new URL(req.url).pathname);
        return HttpResponse.json({ version: 'v1' });
      }),
      http.get('*/api/v1/stream/applications', ({ request: req }) => {
        seen.push(new URL(req.url).pathname);
        return new HttpResponse('', { headers: { 'Content-Type': 'text/event-stream' } });
      }),
    );
    await getVersion();
    const stream = watchApplications({}, { onEvent: () => undefined, maxRetries: 0 });
    await vi.waitFor(() => expect(seen).toHaveLength(2));
    stream.close();
    expect(seen).toEqual(['/team/patrol/api/v1/version', '/team/patrol/api/v1/stream/applications']);
    expect(new URL(ssoLoginURL('x')).pathname).toBe('/team/patrol/api/v1/auth/login');
    expect(new URL(terminalURL({ appName: 'a', projectName: 'p', namespace: 'n', pod: 'p', container: 'c' })).pathname).toBe('/team/patrol/terminal');
    expect(new URL(badgeURL({ name: 'a' })).pathname).toBe('/team/patrol/api/badge');
    expect(new URL(resolveServerURL('/extensions/metrics/x').href).pathname).toBe('/team/patrol/extensions/metrics/x');
  });
});

describe('buildURL', () => {
  it('repeats array parameters and skips empty values', () => {
    const url = buildURL('/api/v1/applications', { projects: ['a', 'b'], selector: '', name: undefined, limit: 5, refresh: false });
    expect(url.search).toBe('?projects=a&projects=b&limit=5&refresh=false');
  });
});

describe('request', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('sends cookies same-origin and the CSRF header only on mutations', async () => {
    const seen: { method: string; csrf: string | null; credentials: RequestCredentials }[] = [];
    server.use(
      http.all('*/api/v1/echo', ({ request: req }) => {
        seen.push({ method: req.method, csrf: req.headers.get('X-Requested-With'), credentials: req.credentials });
        return HttpResponse.json({ ok: true });
      }),
    );
    await request('/api/v1/echo');
    await request('/api/v1/echo', { method: 'POST', body: { a: 1 } });
    await request('/api/v1/echo', { method: 'DELETE' });
    expect(seen).toEqual([
      { method: 'GET', csrf: null, credentials: 'same-origin' },
      { method: 'POST', csrf: 'patrol', credentials: 'same-origin' },
      { method: 'DELETE', csrf: 'patrol', credentials: 'same-origin' },
    ]);
  });

  it('serializes JSON bodies and parses JSON responses', async () => {
    server.use(
      http.post('*/api/v1/echo', async ({ request: req }) => {
        expect(req.headers.get('Content-Type')).toBe('application/json');
        return HttpResponse.json({ received: await req.json() });
      }),
    );
    await expect(request('/api/v1/echo', { method: 'POST', body: { name: 'guestbook' } })).resolves.toEqual({
      received: { name: 'guestbook' },
    });
  });

  it('returns undefined for 204 responses', async () => {
    server.use(http.delete('*/api/v1/echo', () => new HttpResponse(null, { status: 204 })));
    await expect(request('/api/v1/echo', { method: 'DELETE' })).resolves.toBeUndefined();
  });

  it('maps problem+json errors to ApiError with status, code and detail', async () => {
    server.use(http.get('*/api/v1/echo', () => problem(409, 'conflict', 'object was modified')));
    const err = await request('/api/v1/echo').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 409, code: 'conflict', detail: 'object was modified' });
  });

  it('derives the code from the status when the body has none', async () => {
    server.use(http.get('*/api/v1/echo', () => HttpResponse.json({ message: 'nope' }, { status: 403 })));
    await expect(request('/api/v1/echo')).rejects.toMatchObject({ status: 403, code: 'permission_denied', detail: 'nope' });
  });

  it('keeps short text bodies as detail and ignores unknown codes', async () => {
    server.use(
      http.get('*/api/v1/text', () => new HttpResponse('backend exploded', { status: 502 })),
      http.get('*/api/v1/weird', () => HttpResponse.json({ code: 'teapot', detail: 'short and stout' }, { status: 418 })),
    );
    await expect(request('/api/v1/text')).rejects.toMatchObject({ status: 502, code: 'unavailable', detail: 'backend exploded' });
    await expect(request('/api/v1/weird')).rejects.toMatchObject({ status: 418, code: 'invalid_argument', detail: 'short and stout' });
  });

  it('exposes Retry-After on rate limiting', async () => {
    server.use(http.post('*/api/v1/session', () => problem(429, 'resource_exhausted', 'slow down', { 'Retry-After': '42' })));
    await expect(request('/api/v1/session', { method: 'POST', body: {}, authRedirect: false })).rejects.toMatchObject({
      status: 429,
      code: 'resource_exhausted',
      retryAfterSeconds: 42,
    });
  });

  it('maps network failures to status 0 / unavailable', async () => {
    server.use(http.get('*/api/v1/echo', () => HttpResponse.error()));
    await expect(request('/api/v1/echo')).rejects.toMatchObject({ status: 0, code: 'unavailable' });
  });

  it('calls the unauthorized handler with the current URL on 401', async () => {
    const handler = vi.fn();
    const restore = setUnauthorizedHandler(handler);
    server.use(http.get('*/api/v1/applications', () => problem(401, 'unauthenticated', 'token expired')));
    await expect(request('/api/v1/applications')).rejects.toMatchObject({ status: 401, code: 'unauthenticated' });
    expect(handler).toHaveBeenCalledWith(window.location.href);
    restore();
  });

  it('does not redirect when authRedirect is false', async () => {
    const handler = vi.fn();
    const restore = setUnauthorizedHandler(handler);
    server.use(http.get('*/api/v1/session/userinfo', () => problem(401, 'unauthenticated', 'no session')));
    await expect(request('/api/v1/session/userinfo', { authRedirect: false })).rejects.toMatchObject({ status: 401 });
    expect(handler).not.toHaveBeenCalled();
    restore();
  });
});

describe('codeForStatus', () => {
  it.each([
    [400, 'invalid_argument'],
    [401, 'unauthenticated'],
    [404, 'not_found'],
    [412, 'failed_precondition'],
    [500, 'internal'],
    [503, 'unavailable'],
    [0, 'unavailable'],
  ] as const)('%i → %s', (status, code) => {
    expect(codeForStatus(status)).toBe(code);
  });
});
