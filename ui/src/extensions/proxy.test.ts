import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { applicationFixture } from '../mocks/fixtures';
import { server } from '../mocks/server';
import { callProxyExtension, extensionProxyHeaders } from './proxy';

describe('proxy extension calls', () => {
  it('builds the Patrol application and project headers', () => {
    const app = applicationFixture('guestbook', { namespace: 'team-a', project: 'payments' });
    expect(extensionProxyHeaders(app)).toEqual({ 'Patrol-Application-Name': 'team-a:guestbook', 'Patrol-Project-Name': 'payments' });
    const noNamespace = applicationFixture('guestbook');
    delete noNamespace.metadata.namespace;
    expect(extensionProxyHeaders(noNamespace, 'patrol')['Patrol-Application-Name']).toBe('patrol:guestbook');
  });

  it('calls /extensions/<name>/<path> under the root path with the headers and CSRF marker', async () => {
    let seen: { url: string; app: string | null; project: string | null; csrf: string | null } | undefined;
    server.use(
      http.post('*/extensions/metrics/api/v1/query', ({ request }) => {
        seen = {
          url: request.url,
          app: request.headers.get('Patrol-Application-Name'),
          project: request.headers.get('Patrol-Project-Name'),
          csrf: request.headers.get('X-Requested-With'),
        };
        return HttpResponse.json({ ok: true });
      }),
    );
    const app = applicationFixture('guestbook', { project: 'default' });
    const body = await callProxyExtension<{ ok: boolean }>('metrics', '/api/v1/query', app, { method: 'POST', body: { q: 'up' } });
    expect(body).toEqual({ ok: true });
    expect(new URL(seen?.url ?? '').pathname).toBe('/extensions/metrics/api/v1/query');
    expect(seen).toMatchObject({ app: 'patrol:guestbook', project: 'default', csrf: 'patrol' });
  });

  it('surfaces authorization failures as ApiErrors', async () => {
    server.use(http.get('*/extensions/metrics/*', () => new HttpResponse('Unauthorized extension request', { status: 401 })));
    await expect(callProxyExtension('metrics', 'x', applicationFixture('guestbook'), { authRedirect: false })).rejects.toMatchObject({
      status: 401,
      code: 'unauthenticated',
    });
  });
});
