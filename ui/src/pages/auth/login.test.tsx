import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { settingsFixture } from '../../mocks/fixtures';
import { mockState } from '../../mocks/handlers';
import { server } from '../../mocks/server';
import { safeReturnPath } from '../../app/session';
import { renderApp } from '../../test/render';

function withSettings(overrides: Partial<typeof settingsFixture>) {
  server.use(http.get('*/api/v1/settings', () => HttpResponse.json({ ...settingsFixture, ...overrides })));
}

const origin = window.location.origin;

async function loginPage(path = '/login') {
  mockState.loggedIn = false;
  const result = renderApp(path);
  await screen.findByRole('heading', { name: 'Sign in to Patrol CD' });
  return result;
}

describe('login', () => {
  it('signs in with a local account, sets the cookie flag and returns to the requested page', async () => {
    const user = userEvent.setup();
    const posted: { url: URL; body: unknown; csrf: string | null }[] = [];
    server.use(
      http.post('*/api/v1/session', async ({ request }) => {
        posted.push({ url: new URL(request.url), body: await request.json(), csrf: request.headers.get('X-Requested-With') });
        mockState.loggedIn = true;
        return HttpResponse.json({ token: 'mock' });
      }),
    );
    mockState.loggedIn = false;
    const { router } = renderApp('/settings/projects');
    await screen.findByRole('heading', { name: 'Sign in to Patrol CD' });

    await user.type(screen.getByLabelText(/Username/), 'alice');
    await user.type(screen.getByLabelText(/Password/), 'correct horse');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));

    expect(await screen.findByRole('heading', { level: 1, name: 'Projects' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/settings/projects');
    expect(posted).toHaveLength(1);
    expect(posted[0]?.url.searchParams.get('cookie')).toBe('true');
    expect(posted[0]?.body).toEqual({ username: 'alice', password: 'correct horse' });
    expect(posted[0]?.csrf).toBe('patrol');
  });

  it('validates required fields before calling the server', async () => {
    const user = userEvent.setup();
    await loginPage();
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    const username = screen.getByLabelText(/Username/);
    expect(username).toHaveAttribute('aria-invalid', 'true');
    expect(username).toHaveAccessibleDescription('Enter your username.');
    expect(username).toHaveFocus();
    expect(screen.getByLabelText(/Password/)).toHaveAccessibleDescription('Enter your password.');
  });

  it('explains invalid credentials, clears the password and keeps the user on the page', async () => {
    const user = userEvent.setup();
    const { router } = await loginPage();
    await user.type(screen.getByLabelText(/Username/), 'alice');
    await user.type(screen.getByLabelText(/Password/), 'wrong');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Sign-in failed');
    expect(alert).toHaveTextContent('The username or password is incorrect.');
    expect(screen.getByLabelText(/Password/)).toHaveValue('');
    expect(screen.getByLabelText(/Password/)).toHaveFocus();
    expect(router.state.location.pathname).toBe('/login');
  });

  it('shows lockout and rate limiting clearly and blocks retries until the wait is over', async () => {
    const user = userEvent.setup();
    await loginPage();
    await user.type(screen.getByLabelText(/Username/), 'locked');
    await user.type(screen.getByLabelText(/Password/), 'whatever');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Too many sign-in attempts');
    expect(alert).toHaveTextContent('Try again in 30 seconds.');
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeDisabled();
  });

  it('reports an unreachable server', async () => {
    const user = userEvent.setup();
    server.use(http.post('*/api/v1/session', () => HttpResponse.error()));
    await loginPage();
    await user.type(screen.getByLabelText(/Username/), 'alice');
    await user.type(screen.getByLabelText(/Password/), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Patrol server unavailable');
  });

  it.each([
    [{ oidcConfig: { name: 'Okta' } }, 'Log in via Okta'],
    [{ dexConfig: { connectors: [{ name: 'GitHub', type: 'github' }] } }, 'Log in via GitHub'],
    [
      {
        dexConfig: {
          connectors: [
            { name: 'GitHub', type: 'github' },
            { name: 'LDAP', type: 'ldap' },
          ],
        },
      },
      'Log in with SSO',
    ],
  ])('offers SSO when configured (%j)', async (settings, label) => {
    withSettings(settings);
    await loginPage(`/login?return_url=${encodeURIComponent(`${origin}/applicationsets`)}`);
    const sso = screen.getByRole('link', { name: label });
    const href = new URL(sso.getAttribute('href') ?? '', origin);
    expect(href.pathname).toBe('/api/v1/auth/login');
    expect(href.searchParams.get('return_url')).toBe(`${origin}/applicationsets`);
    // Local login stays available alongside SSO.
    expect(screen.getByRole('form', { name: 'Local account sign-in' })).toBeInTheDocument();
  });

  it('explains when no sign-in method is available', async () => {
    withSettings({ userLoginsDisabled: true });
    await loginPage();
    expect(screen.queryByRole('form', { name: 'Local account sign-in' })).not.toBeInTheDocument();
    expect(screen.getByText('Sign-in is not available')).toBeInTheDocument();
  });

  it('hides the local form when only SSO is allowed', async () => {
    withSettings({ userLoginsDisabled: true, oidcConfig: { name: 'Okta' } });
    await loginPage();
    expect(screen.queryByRole('form', { name: 'Local account sign-in' })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Log in via Okta' })).toBeInTheDocument();
  });

  it('displays SSO errors passed back by the server', async () => {
    await loginPage('/login?has_sso_error=true&sso_error=state%20mismatch');
    expect(screen.getByRole('alert')).toHaveTextContent('Single sign-on failed');
    expect(screen.getByRole('alert')).toHaveTextContent('state mismatch');
  });

  it('redirects signed-in users to the return URL and ignores foreign return URLs', async () => {
    const { router, unmount } = renderApp(`/login?return_url=${encodeURIComponent(`${origin}/settings`)}`);
    await screen.findByRole('heading', { level: 1, name: 'Settings' });
    expect(router.state.location.pathname).toBe('/settings');
    unmount();

    const second = renderApp(`/login?return_url=${encodeURIComponent('https://evil.example/steal')}`);
    await screen.findByRole('heading', { level: 1, name: 'Applications' });
    expect(second.router.state.location.pathname).toBe('/applications');
  });

  it('rejects return URLs that resolve to protocol-relative paths', () => {
    expect(safeReturnPath('/.//evil.example/x')).toBe('/applications');
    expect(safeReturnPath(`${origin}//evil.example`)).toBe('/applications');
    expect(safeReturnPath(`${origin}/.//evil.example`)).toBe('/applications');
    expect(safeReturnPath('/\\evil.example')).toBe('/applications');
    expect(safeReturnPath(`${origin}/login?return_url=x`)).toBe('/applications');
    expect(safeReturnPath(`${origin}/settings/clusters?x=1#y`)).toBe('/settings/clusters?x=1#y');
    const base = document.createElement('base');
    base.href = '/patrol/';
    document.head.prepend(base);
    expect(safeReturnPath('/patrol/.//evil.example')).toBe('/applications');
    expect(safeReturnPath(`${origin}/patrol//evil.example`)).toBe('/applications');
    expect(safeReturnPath(`${origin}/patrol/settings`)).toBe('/settings');
  });

  it('keeps the anonymous "continue" link and post-login navigation inside the app for a hostile return URL', async () => {
    withSettings({ anonymousEnabled: true });
    const { router } = await loginPage(`/login?return_url=${encodeURIComponent('/.//evil.example/x')}`);
    expect(screen.getByRole('link', { name: 'Continue without signing in' })).toHaveAttribute('href', '/applications');

    const user = userEvent.setup();
    server.use(
      http.post('*/api/v1/session', () => {
        mockState.loggedIn = true;
        return HttpResponse.json({ token: 'mock' });
      }),
    );
    await user.type(screen.getByLabelText(/Username/), 'alice');
    await user.type(screen.getByLabelText(/Password/), 'correct horse');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/applications'));
    expect(screen.queryByText(/Sign-in failed/)).not.toBeInTheDocument();
  });

  it('lets anonymous users continue without signing in', async () => {
    withSettings({ anonymousEnabled: true });
    const user = userEvent.setup();
    const { router } = await loginPage();
    await user.click(screen.getByRole('link', { name: 'Continue without signing in' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/applications'));
  });
});

describe('user info', () => {
  it('shows username, issuer and groups', async () => {
    renderApp('/user-info');
    await screen.findByRole('heading', { level: 1, name: 'User info' });
    const main = within(screen.getByRole('main'));
    expect(main.getAllByRole('definition').map((d) => d.textContent)).toEqual(['mock-admin', 'patrol', 'platform-teamoncall']);
    expect(main.getByText('platform-team')).toBeInTheDocument();
    expect(screen.getByRole('form', { name: 'Change password' })).toBeInTheDocument();
  });

  it('rejects mismatched new passwords before calling the server', async () => {
    const user = userEvent.setup();
    renderApp('/user-info');
    await screen.findByRole('heading', { level: 1, name: 'User info' });
    await user.type(screen.getByLabelText(/^Current password/), 'old');
    await user.type(screen.getByLabelText(/^New password/), 'new-password-1');
    await user.type(screen.getByLabelText(/^Confirm new password/), 'new-password-2');
    await user.click(screen.getByRole('button', { name: 'Update password' }));
    expect(screen.getByLabelText(/^Confirm new password/)).toHaveAccessibleDescription('The new passwords do not match.');
  });
});
