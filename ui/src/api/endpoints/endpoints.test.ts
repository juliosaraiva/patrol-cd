import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '../../mocks/server';
import { getApplication, parseTreeEvent, terminalURL } from './applications';
import { getCluster } from './clusters';
import { badgeURL, invokeExtension, webhookURL } from './misc';
import { getRepository } from './repositories';
import { canI, ssoLoginURL } from './session';

function capture(method: 'get' | 'post' | 'put' | 'delete', pattern: string) {
  const urls: URL[] = [];
  const headers: Headers[] = [];
  server.use(
    http[method](pattern, ({ request }) => {
      urls.push(new URL(request.url));
      headers.push(request.headers);
      return HttpResponse.json({ value: 'yes', metadata: {} });
    }),
  );
  return { urls, headers };
}

describe('endpoint URL construction', () => {
  it('URL-encodes repository URLs as one path segment', async () => {
    const { urls } = capture('get', '*/api/v1/repositories/*');
    await getRepository('https://github.com/org/repo.git', { appProject: 'team-a' });
    expect(urls[0]?.pathname).toBe('/api/v1/repositories/https%3A%2F%2Fgithub.com%2Forg%2Frepo.git');
    expect(urls[0]?.searchParams.get('appProject')).toBe('team-a');
  });

  it('keeps slashes of the can-i subresource as separators', async () => {
    const { urls } = capture('get', '*/api/v1/account/can-i/*');
    await canI('applications', 'sync', 'default/guest book');
    expect(urls[0]?.pathname).toBe('/api/v1/account/can-i/applications/sync/default/guest%20book');
  });

  it('addresses clusters by name with id.type=name', async () => {
    const { urls } = capture('get', '*/api/v1/clusters/*');
    await getCluster({ name: 'in-cluster' });
    expect(urls[0]?.pathname).toBe('/api/v1/clusters/in-cluster');
    expect(urls[0]?.searchParams.get('id.type')).toBe('name');
  });

  it('passes refresh and app scope query parameters', async () => {
    const { urls } = capture('get', '*/api/v1/applications/:name');
    await getApplication('guestbook', { appNamespace: 'team-a', refresh: 'hard' });
    expect(urls[0]?.searchParams.get('refresh')).toBe('hard');
    expect(urls[0]?.searchParams.get('appNamespace')).toBe('team-a');
  });

  it('sends extension headers and the CSRF header on extension calls', async () => {
    const { urls, headers } = capture('post', '*/extensions/:ext/*');
    await invokeExtension('metrics', '/api/v1/query', { method: 'POST', body: {}, application: { namespace: 'patrol', name: 'guestbook' }, project: 'default' });
    expect(urls[0]?.pathname).toBe('/extensions/metrics/api/v1/query');
    expect(headers[0]?.get('Patrol-Application-Name')).toBe('patrol:guestbook');
    expect(headers[0]?.get('Patrol-Project-Name')).toBe('default');
    expect(headers[0]?.get('X-Requested-With')).toBe('patrol');
  });

  it('builds browser-navigation URLs', () => {
    expect(new URL(ssoLoginURL('http://localhost:3000/applications')).pathname).toBe('/api/v1/auth/login');
    expect(new URL(ssoLoginURL('http://localhost:3000/applications')).searchParams.get('return_url')).toBe(
      'http://localhost:3000/applications',
    );
    expect(webhookURL('github')).toMatch(/\/api\/webhook\/github$/);
    expect(badgeURL({ name: 'guestbook', revision: true })).toMatch(/\/api\/badge\?name=guestbook&revision=true$/);
    expect(badgeURL({ project: 'p' }, 'https://badges.example')).toBe('https://badges.example/api/badge?project=p');
    const ws = new URL(terminalURL({ appName: 'a', projectName: 'p', namespace: 'n', pod: 'web-0', container: 'app' }));
    expect(ws.protocol).toBe('ws:');
    expect(ws.pathname).toBe('/terminal');
    expect(ws.searchParams.get('pod')).toBe('web-0');
  });
});

describe('parseTreeEvent', () => {
  it('decodes the {type, object} watch envelope (WatchEvent_ApplicationTree)', () => {
    expect(parseTreeEvent('{"type":"ADDED","object":{"nodes":[{"kind":"Pod","name":"a"}]}}')).toEqual({
      type: 'ADDED',
      object: { nodes: [{ kind: 'Pod', name: 'a' }] },
    });
    expect(parseTreeEvent('{"type":"DELETED","object":{}}')).toEqual({ type: 'DELETED', object: {} });
  });

  it('rejects payloads that are not envelopes (openStream skips them)', () => {
    expect(() => parseTreeEvent('{"nodes":[]}')).toThrow();
    expect(() => parseTreeEvent('null')).toThrow();
    expect(() => parseTreeEvent('{"type":"MODIFIED"}')).toThrow();
  });
});
