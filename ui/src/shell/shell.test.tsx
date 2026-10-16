import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { request } from '../api/client';
import { installExtensionGlobals, resetExtensionRegistry } from '../extensions';
import { browser } from '../lib/browser';
import { settingsFixture } from '../mocks/fixtures';
import { helpSettings } from '../mocks/fixtures/help';
import { mockState, problem } from '../mocks/handlers';
import { server } from '../mocks/server';
import { renderApp } from '../test/render';
import { HIDDEN_BANNER_KEY } from './SiteBanner';

function useSettings(overrides: Partial<typeof settingsFixture>) {
  server.use(http.get('*/api/v1/settings', () => HttpResponse.json({ ...settingsFixture, ...overrides })));
}

function only<T>(items: T[], index = 0): T {
  const item = items[index];
  if (item === undefined) throw new Error(`expected an element at index ${index}`);
  return item;
}

async function shellReady(heading: string) {
  return screen.findByRole('heading', { level: 1, name: heading });
}

describe('app shell', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the main navigation, marks the active section and shows breadcrumbs', async () => {
    renderApp('/applications');
    await shellReady('Applications');
    const nav = only(screen.getAllByRole('navigation', { name: 'Main' }));
    const links = within(nav).getAllByRole('link').map((l) => l.textContent);
    expect(links).toEqual(['Applications', 'ApplicationSets', 'Settings', 'User info', 'Docs & help']);
    expect(within(nav).getByRole('link', { name: 'Applications' })).toHaveAttribute('aria-current', 'page');
    const crumbs = screen.getByRole('navigation', { name: 'Breadcrumb' });
    expect(within(crumbs).getByText('Applications')).toHaveAttribute('aria-current', 'page');
    expect(document.title).toBe('Applications · Patrol CD');
  });

  it('navigates between sections and builds nested breadcrumbs', async () => {
    const user = userEvent.setup();
    const { router } = renderApp('/applications');
    await shellReady('Applications');
    const nav = only(screen.getAllByRole('navigation', { name: 'Main' }));
    await user.click(within(nav).getByRole('link', { name: 'Settings' }));
    await shellReady('Settings');
    await user.click(screen.getByRole('link', { name: /^Clusters/ }));
    await shellReady('Clusters');
    expect(router.state.location.pathname).toBe('/settings/clusters');
    const crumbs = within(screen.getByRole('navigation', { name: 'Breadcrumb' }));
    expect(crumbs.getByRole('link', { name: 'Settings' })).toHaveAttribute('href', '/settings');
    expect(crumbs.getByText('Clusters')).toHaveAttribute('aria-current', 'page');
  });

  // One test per route: each gets its own timeout and failure message. The heading is the page's
  // h1 as rendered before any data arrives (no API mock here), so this checks routing and the shell.
  it.each<[string, string]>([
    ['/applications/guestbook', 'guestbook'],
    ['/applications/team-a/guestbook', 'guestbook'],
    ['/applications/guestbook/default/app/logs', 'Logs'],
    ['/applicationsets', 'ApplicationSets'],
    ['/applicationsets/team-a/previews', 'previews'],
    ['/settings/repos', 'Repositories'],
    ['/settings/repocreds', 'Repository credentials'],
    ['/settings/certs', 'Certificates and known hosts'],
    ['/settings/gpgkeys', 'GnuPG keys'],
    ['/settings/clusters/https%3A%2F%2Fkubernetes.default.svc', 'Cluster'],
    ['/settings/projects', 'Projects'],
    ['/settings/projects/default', 'default'],
    ['/settings/accounts', 'Accounts'],
    ['/settings/accounts/admin', 'admin'],
    ['/settings/scmproviders', 'SCM providers'],
    ['/settings/notifications', 'Notifications'],
    ['/settings/appearance', 'Appearance'],
    ['/help', 'Docs & help'],
  ])('renders the page at %s', async (path, heading) => {
    renderApp(path);
    expect(await shellReady(heading)).toBeInTheDocument();
  });

  it('moves focus to main and announces the page after client-side navigation', async () => {
    const user = userEvent.setup();
    renderApp('/applications');
    await shellReady('Applications');
    const main = screen.getByRole('main');
    const announcer = document.querySelector('[data-route-announcer]');
    expect(announcer).toHaveAttribute('aria-live', 'polite');
    expect(announcer).toHaveTextContent(/^$/); // nothing announced on the initial load
    expect(main).not.toHaveFocus();

    const nav = only(screen.getAllByRole('navigation', { name: 'Main' }));
    await user.click(within(nav).getByRole('link', { name: 'Settings' }));
    await shellReady('Settings');
    expect(main).toHaveFocus();
    expect(announcer).toHaveTextContent('Settings · Patrol CD');

    // Breadcrumb links (top bar) behave the same.
    await user.click(screen.getByRole('link', { name: /^Clusters/ }));
    await shellReady('Clusters');
    await user.click(within(screen.getByRole('navigation', { name: 'Breadcrumb' })).getByRole('link', { name: 'Settings' }));
    await shellReady('Settings');
    expect(main).toHaveFocus();
    expect(announcer).toHaveTextContent('Settings · Patrol CD');
  });

  it('keeps focus where it is when only the search params change', async () => {
    const user = userEvent.setup();
    const { router } = renderApp('/applications');
    await shellReady('Applications');
    const newApp = screen.getByRole('button', { name: 'New application' });
    await user.click(newApp);
    expect(router.state.location.search).toBe('?new=1');
    expect(screen.getByRole('dialog', { name: 'New application' })).toContainElement(document.activeElement as HTMLElement);
    expect(document.querySelector('[data-route-announcer]')).toHaveTextContent(/^$/);
  });

  it('leaves focus in a dialog that stays open while a route change commits', async () => {
    const user = userEvent.setup();
    const { router } = renderApp('/settings');
    await shellReady('Settings');
    await user.keyboard('?');
    const dialog = screen.getByRole('dialog', { name: 'Keyboard shortcuts' });
    expect(dialog).toContainElement(document.activeElement as HTMLElement);

    // A navigation started before the dialog opened can commit after it (lazy route modules).
    await act(() => router.navigate('/applications'));
    await shellReady('Applications');
    expect(dialog).toContainElement(document.activeElement as HTMLElement);
    expect(document.querySelector('[data-route-announcer]')).toHaveTextContent('Applications · Patrol CD');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('opens the application creation panel from the list', async () => {
    const user = userEvent.setup();
    const { router } = renderApp('/applications');
    await shellReady('Applications');
    await user.click(screen.getByRole('button', { name: 'New application' }));
    expect(screen.getByRole('dialog', { name: 'New application' })).toBeInTheDocument();
    expect(router.state.location.search).toBe('?new=1');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(router.state.location.search).toBe('');
  });

  it('shows a 404 page inside the shell for unknown paths', async () => {
    renderApp('/does/not/exist');
    expect(await shellReady('Page not found')).toBeInTheDocument();
    expect(screen.getAllByRole('navigation', { name: 'Main' }).length).toBeGreaterThan(0);
    expect(screen.getByRole('link', { name: 'Go to applications' })).toHaveAttribute('href', '/applications');
  });

  it('supports the skip link and the / and ? shortcuts', async () => {
    const user = userEvent.setup();
    const { router } = renderApp('/settings');
    await shellReady('Settings');

    await user.tab();
    const skip = screen.getByRole('link', { name: 'Skip to main content' });
    expect(skip).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(screen.getByRole('main')).toHaveFocus();

    await user.keyboard('/');
    const search = screen.getByRole('searchbox', { name: 'Search applications' });
    expect(search).toHaveFocus();
    await user.type(search, 'guest{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/applications'));
    expect(router.state.location.search).toBe('?search=guest');

    search.blur();
    await user.keyboard('?');
    const dialog = screen.getByRole('dialog', { name: 'Keyboard shortcuts' });
    expect(within(dialog).getByText('Focus search')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('opens the navigation drawer from the menu button on small screens', async () => {
    const user = userEvent.setup();
    const { router } = renderApp('/applications');
    await shellReady('Applications');
    await user.click(screen.getByRole('button', { name: 'Open navigation' }));
    const drawer = screen.getByRole('dialog', { name: 'Patrol CD' });
    await user.click(within(drawer).getByRole('link', { name: 'ApplicationSets' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/applicationsets');
  });

  it('switches the theme from the top bar', async () => {
    const user = userEvent.setup();
    renderApp('/applications');
    await shellReady('Applications');
    await user.click(screen.getByRole('button', { name: /^Theme:/ }));
    await user.click(screen.getByRole('menuitem', { name: 'Dark theme' }));
    expect(document.documentElement.dataset.theme).toBe('dark');
    await user.click(screen.getByRole('button', { name: /^Theme:/ }));
    await user.click(screen.getByRole('menuitem', { name: 'System theme' }));
    expect(document.documentElement.dataset.theme).toBeUndefined();
  });

  it('shows version details from the sidebar', async () => {
    const user = userEvent.setup();
    renderApp('/applications');
    await shellReady('Applications');
    await user.click(await screen.findByRole('button', { name: 'Version v0.0.0-mock' }));
    const dialog = screen.getByRole('dialog', { name: 'Version information' });
    expect(within(dialog).getByText('linux/amd64')).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Copy version information' })).toBeInTheDocument();
  });

  it('logs out through the user menu', async () => {
    const user = userEvent.setup();
    const { router } = renderApp('/applications');
    await shellReady('Applications');
    await user.click(screen.getByRole('button', { name: 'Account: mock-admin' }));
    expect(screen.getByText('Signed in as mock-admin')).toBeInTheDocument();
    await user.click(screen.getByRole('menuitem', { name: 'Log out' }));
    expect(await screen.findByRole('heading', { name: 'Sign in to Patrol CD' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/login');
    expect(mockState.loggedIn).toBe(false);
  });

  it('follows the OIDC end-session URL on logout when the server returns one', async () => {
    const assign = vi.spyOn(browser, 'assign').mockImplementation(() => undefined);
    server.use(http.delete('*/api/v1/session', () => HttpResponse.json({ logoutURL: 'https://idp.example/logout' })));
    renderApp('/logout');
    await waitFor(() => expect(assign).toHaveBeenCalledWith('https://idp.example/logout'));
  });
});

describe('session gating', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('sends logged-out users to the login page with a return URL', async () => {
    mockState.loggedIn = false;
    const { router } = renderApp('/settings/clusters?x=1');
    expect(await screen.findByRole('heading', { name: 'Sign in to Patrol CD' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/login');
    const returnURL = new URLSearchParams(router.state.location.search).get('return_url');
    expect(returnURL).toBe(`${window.location.origin}/settings/clusters?x=1`);
  });

  it('goes straight to SSO when an identity provider is configured', async () => {
    mockState.loggedIn = false;
    useSettings({ oidcConfig: { name: 'Okta' } });
    const assign = vi.spyOn(browser, 'assign').mockImplementation(() => undefined);
    renderApp('/applications');
    await waitFor(() => expect(assign).toHaveBeenCalledTimes(1));
    const target = new URL(assign.mock.calls[0]?.[0] ?? '');
    expect(target.pathname).toBe('/api/v1/auth/login');
    expect(target.searchParams.get('return_url')).toBe(`${window.location.origin}/applications`);
  });

  it('lets anonymous sessions in without redirecting', async () => {
    mockState.loggedIn = false;
    useSettings({ anonymousEnabled: true });
    renderApp('/applications');
    expect(await shellReady('Applications')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Account: Anonymous' })).toBeInTheDocument();
  });

  it('redirects to login when any API call returns 401', async () => {
    const { router } = renderApp('/applications');
    await shellReady('Applications');
    server.use(http.get('*/api/v1/applications', () => problem(401, 'unauthenticated', 'session expired')));
    await act(async () => {
      await request('/api/v1/applications').catch(() => undefined);
    });
    expect(await screen.findByRole('heading', { name: 'Sign in to Patrol CD' })).toBeInTheDocument();
    expect(new URLSearchParams(router.state.location.search).get('return_url')).toBe(`${window.location.origin}/applications`);
  });

  it('shows recovery guidance when the server is unreachable', async () => {
    server.use(http.get('*/api/v1/session/userinfo', () => HttpResponse.error()));
    renderApp('/applications');
    expect(await screen.findByRole('heading', { name: 'Patrol server unavailable' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });
});

describe('instance settings', () => {
  it('shows the banner at the configured positions and can hide it permanently', async () => {
    const user = userEvent.setup();
    useSettings({ uiBannerContent: 'Maintenance at 18:00 UTC', uiBannerURL: 'https://status.example/maint', uiBannerPosition: 'both' });
    renderApp('/applications');
    await shellReady('Applications');
    const banners = screen.getAllByRole('complementary', { name: 'Announcement' });
    expect(banners).toHaveLength(2);
    expect(within(only(banners)).getByRole('link', { name: 'Maintenance at 18:00 UTC' })).toHaveAttribute(
      'href',
      'https://status.example/maint',
    );
    await user.click(within(only(banners)).getByRole('button', { name: "Don't show again" }));
    expect(screen.queryByRole('complementary', { name: 'Announcement' })).not.toBeInTheDocument();
    expect(window.localStorage.getItem(HIDDEN_BANNER_KEY)).toBe('Maintenance at 18:00 UTC');
  });

  it('renders permanent banners without dismiss actions and ignores unsafe banner URLs', async () => {
    useSettings({ uiBannerContent: 'Production', uiBannerPermanent: true, uiBannerURL: 'javascript:alert(1)' });
    renderApp('/applications');
    await shellReady('Applications');
    const banner = screen.getByRole('complementary', { name: 'Announcement' });
    expect(within(banner).queryByRole('button')).not.toBeInTheDocument();
    expect(within(banner).queryByRole('link')).not.toBeInTheDocument();
  });

  it('injects the custom stylesheet from ui.cssurl', async () => {
    useSettings({ uiCssURL: './custom/brand.css' });
    renderApp('/applications');
    await shellReady('Applications');
    const link = document.head.querySelector<HTMLLinkElement>('link[data-patrol-custom-css]');
    expect(link?.href).toBe(`${window.location.origin}/custom/brand.css`);
  });

  it('refuses non-http stylesheet URLs', async () => {
    useSettings({ uiCssURL: 'javascript:alert(1)' });
    renderApp('/applications');
    await shellReady('Applications');
    expect(document.head.querySelector('link[data-patrol-custom-css]')).toBeNull();
  });
});

describe('system-level UI extensions (API-122)', () => {
  afterEach(() => {
    resetExtensionRegistry();
    delete window.extensionsAPI;
    delete window.extensions;
  });

  function HelloExtension() {
    return <p>Hello from an extension</p>;
  }

  const BUILT_IN_LINKS = ['Applications', 'ApplicationSets', 'Settings', 'User info', 'Docs & help'];

  it('adds an extension registered through window.extensionsAPI to the navigation and renders it at its path', async () => {
    const user = userEvent.setup();
    installExtensionGlobals();
    window.extensionsAPI?.registerSystemLevelExtension(HelloExtension, 'Hello', '/hello', 'fa-hand');
    const { router } = renderApp('/applications');
    await shellReady('Applications');
    const nav = only(screen.getAllByRole('navigation', { name: 'Main' }));
    expect(within(nav).getAllByRole('link').map((l) => l.textContent)).toEqual([...BUILT_IN_LINKS, 'Hello']);
    expect(within(nav).getByRole('link', { name: 'Hello' })).toHaveAttribute('href', '/hello');

    await user.click(within(nav).getByRole('link', { name: 'Hello' }));
    await shellReady('Hello');
    expect(router.state.location.pathname).toBe('/hello');
    expect(screen.getByText('Hello from an extension')).toBeInTheDocument();
    expect(within(nav).getByRole('link', { name: 'Hello' })).toHaveAttribute('aria-current', 'page');
    expect(within(screen.getByRole('navigation', { name: 'Breadcrumb' })).getByText('Hello')).toHaveAttribute('aria-current', 'page');
    expect(document.title).toBe('Hello · Patrol CD');
    // Nested paths under the extension path render the same page.
    act(() => void router.navigate('/hello/details'));
    expect(await shellReady('Hello')).toBeInTheDocument();
  });

  it('shows a page registered after the first render and keeps the 404 for other paths', async () => {
    installExtensionGlobals();
    const { router } = renderApp('/hello');
    await shellReady('Page not found');
    const nav = only(screen.getAllByRole('navigation', { name: 'Main' }));
    expect(within(nav).getAllByRole('link').map((l) => l.textContent)).toEqual(BUILT_IN_LINKS);

    // An extension bundle that loads late registers while the 404 is shown.
    act(() => window.extensionsAPI?.registerSystemLevelExtension(HelloExtension, 'Hello', 'hello/'));
    expect(await shellReady('Hello')).toBeInTheDocument();
    expect(screen.getByText('Hello from an extension')).toBeInTheDocument();
    expect(within(nav).getByRole('link', { name: 'Hello' })).toHaveAttribute('aria-current', 'page');
    expect(within(screen.getByRole('navigation', { name: 'Breadcrumb' })).getByText('Hello')).toHaveAttribute('aria-current', 'page');

    act(() => void router.navigate('/does/not/exist'));
    expect(await shellReady('Page not found')).toBeInTheDocument();
    expect(screen.queryByText('Hello from an extension')).not.toBeInTheDocument();
  });

  it('never replaces a built-in page', async () => {
    installExtensionGlobals();
    window.extensionsAPI?.registerSystemLevelExtension(HelloExtension, 'Fake settings', '/settings');
    renderApp('/settings');
    expect(await shellReady('Settings')).toBeInTheDocument();
    expect(screen.queryByText('Hello from an extension')).not.toBeInTheDocument();
  });
});

describe('floating chat button (API-111)', () => {
  it('renders nothing when help.chatUrl is not configured', async () => {
    useSettings({ help: { chatUrl: '', chatText: '', binaryUrls: {} } });
    renderApp('/applications');
    await shellReady('Applications');
    expect(screen.queryByRole('link', { name: /Chat now!|opens in a new tab/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Chat now!/ })).not.toBeInTheDocument();
  });

  it('links to help.chatUrl once on every page, and leaves the help page its own link', async () => {
    const user = userEvent.setup();
    useSettings({ help: helpSettings });
    renderApp('/applications');
    await shellReady('Applications');
    const link = await screen.findByRole('link', { name: /Ask the platform team/ });
    expect(link).toHaveAttribute('href', 'https://chat.example.test/patrol');
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    expect(link).toHaveClass('floatingChat');

    const nav = only(screen.getAllByRole('navigation', { name: 'Main' }));
    await user.click(within(nav).getByRole('link', { name: 'Settings' }));
    await shellReady('Settings');
    expect(screen.getAllByRole('link', { name: /Ask the platform team/ })).toHaveLength(1);

    await user.click(within(nav).getByRole('link', { name: 'Docs & help' }));
    await shellReady('Docs & help');
    const onHelp = await screen.findAllByRole('link', { name: /Ask the platform team/ });
    expect(onHelp).toHaveLength(1);
    expect(onHelp[0]).not.toHaveClass('floatingChat');
  });

  it('shows a disabled control for an invalid chat URL', async () => {
    useSettings({ help: { chatUrl: 'javascript:alert(1)', chatText: '', binaryUrls: {} } });
    renderApp('/applications');
    await shellReady('Applications');
    const button = await screen.findByRole('button', { name: /Chat now!/ });
    expect(button).toHaveAttribute('aria-disabled', 'true');
    expect(screen.queryByRole('link', { name: /Chat now!/ })).not.toBeInTheDocument();
  });
});
