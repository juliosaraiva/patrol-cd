import { http, HttpResponse } from 'msw';
import type { SessionCreateRequest } from '../../api/types';
import { loggedInUser, loggedOutUser, settingsFixture, versionFixture } from '../fixtures';
import { emptyObject, mockState, problem } from './util';

/**
 * Mock session rules (dev mock mode and tests): any username with a non-empty password signs
 * in, except password "wrong" (401) and username "locked" (429 with Retry-After).
 */
export const sessionHandlers = [
  http.get('*/api/v1/settings', () => HttpResponse.json(settingsFixture)),
  http.get('*/api/v1/version', () => HttpResponse.json(versionFixture)),
  http.get('*/api/version', () => HttpResponse.json(versionFixture)),
  http.get('*/api/v1/session/userinfo', () =>
    HttpResponse.json(mockState.loggedIn ? { ...loggedInUser, username: mockState.username } : loggedOutUser),
  ),
  http.post('*/api/v1/session', async ({ request }) => {
    const body = (await request.json()) as SessionCreateRequest;
    if (body.username === 'locked') {
      return problem(429, 'resource_exhausted', 'Too many failed login attempts', { 'Retry-After': '30' });
    }
    if (!body.password || body.password === 'wrong') {
      return problem(401, 'unauthenticated', 'Invalid username or password');
    }
    mockState.loggedIn = true;
    mockState.username = body.username;
    return HttpResponse.json({ token: 'mock-session' }, { headers: { 'Set-Cookie': 'patrol.token=mock-session; Path=/' } });
  }),
  http.delete('*/api/v1/session', () => {
    mockState.loggedIn = false;
    return HttpResponse.json(emptyObject);
  }),
  http.put('*/api/v1/account/password', () => HttpResponse.json(emptyObject)),
  http.get('*/api/v1/account/can-i/*', () => HttpResponse.json({ value: 'yes' })),
];
