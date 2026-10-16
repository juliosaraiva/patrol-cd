import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Application } from '../../api/types';
import { searchApp } from '../../mocks/fixtures/apps-core';
import { problem } from '../../mocks/handlers';
import { resetAppsCoreMockState } from '../../mocks/handlers/apps-core';
import { server } from '../../mocks/server';
import { renderApp } from '../../test/render';
import { FAVORITES_KEY } from './shared/favorites';

// Whole-app integration tests (real routes, lazy pages, MSW): allow for a loaded CI machine.
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());

async function ready() {
  await screen.findByRole('heading', { level: 1, name: 'Applications' });
  return screen.findByRole('article', { name: 'guestbook' });
}

function tileNames(): string[] {
  return within(screen.getByRole('list', { name: 'Applications' }))
    .getAllByRole('article')
    .map((a) => a.getAttribute('aria-label') ?? '');
}

function generated(count: number): Application[] {
  return Array.from({ length: count }, (_, i) => {
    const app = searchApp();
    app.metadata = { ...app.metadata, name: `app-${String(i).padStart(4, '0')}`, uid: `uid-${i}` };
    return app;
  });
}

function sse(events: unknown[]) {
  return http.get('*/api/v1/stream/applications', ({ request }) => {
    const encoder = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        events.forEach((e, i) => controller.enqueue(encoder.encode(`id: ${i + 1}\ndata: ${JSON.stringify(e)}\n\n`)));
        request.signal.addEventListener('abort', () => controller.close());
      },
    });
    return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream' } });
  });
}

describe('applications list states', () => {
  it('marks the results busy while loading', async () => {
    server.use(http.get('*/api/v1/applications', async () => {
      await delay('infinite');
      return HttpResponse.json({ items: [] });
    }));
    renderApp('/applications');
    expect(await screen.findByLabelText('Loading applications')).toHaveAttribute('aria-busy', 'true');
  });

  it('renders tiles with the distinct status families and details links', async () => {
    renderApp('/applications');
    const tile = await ready();
    expect(tileNames()).toEqual(['guestbook', 'payments', 'search']);
    expect(within(tile).getByRole('link', { name: 'guestbook' })).toHaveAttribute('href', '/applications/patrol/guestbook');
    expect(tile.querySelector('[data-status-family="health"]')).toHaveTextContent('Healthy');
    expect(tile.querySelector('[data-status-family="sync"]')).toHaveTextContent('Synced');
    const search = screen.getByRole('article', { name: 'search' });
    expect(search.querySelector('[data-status-family="operation"]')).toHaveTextContent('Running');
    const payments = screen.getByRole('article', { name: 'payments' });
    expect(within(payments).getByText(/Notice \(warning\): Payments freeze/)).toBeInTheDocument();
    expect(screen.getByText('3 applications')).toBeInTheDocument();
    const bar = screen.getByRole('group', { name: 'Health of the listed applications' });
    expect(within(bar).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['Healthy 1', 'Progressing 1', 'Degraded 1']);
  });

  it('toggles the health bar and remembers the choice', async () => {
    const user = userEvent.setup({ delay: null });
    renderApp('/applications');
    await ready();
    const toggle = screen.getByRole('button', { name: 'Health bar' });
    expect(toggle).toHaveAttribute('aria-pressed', 'true');
    await user.click(toggle);
    expect(screen.queryByRole('group', { name: 'Health of the listed applications' })).not.toBeInTheDocument();
    expect(window.localStorage.getItem('patrol.apps.healthBar')).toBe('false');
  });

  it('guides first-time users from the empty state', async () => {
    const user = userEvent.setup({ delay: null });
    server.use(http.get('*/api/v1/applications', () => HttpResponse.json({ items: [], metadata: { resourceVersion: '1' } })));
    const { router } = renderApp('/applications');
    expect(await screen.findByRole('heading', { name: 'No applications yet' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Docs and help' })).toHaveAttribute('href', '/help');
    const emptyState = screen.getByRole('heading', { name: 'No applications yet' }).parentElement;
    if (!emptyState) throw new Error('empty state container missing');
    await user.click(within(emptyState).getByRole('button', { name: 'New application' }));
    expect(new URLSearchParams(router.state.location.search).get('new')).toBe('1');
  });

  it('explains load failures and retries', async () => {
    const user = userEvent.setup({ delay: null });
    let calls = 0;
    server.use(
      http.get('*/api/v1/applications', () => {
        calls += 1;
        return calls === 1 ? problem(403, 'permission_denied', 'permission denied: applications, get') : HttpResponse.json({ items: [searchApp()], metadata: { resourceVersion: '5' } });
      }),
    );
    renderApp('/applications');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Applications could not be loaded');
    expect(alert).toHaveTextContent('Ask an administrator for the required RBAC permission');
    await user.click(within(alert).getByRole('button', { name: 'Retry' }));
    expect(await screen.findByRole('article', { name: 'search' })).toBeInTheDocument();
  });
});

describe('search and filters', () => {
  it('focuses the list search with "/" and keeps the search in the URL', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp('/applications');
    await ready();
    await user.keyboard('/');
    const search = screen.getByRole('searchbox', { name: 'Filter applications by name or namespace' });
    expect(search).toHaveFocus();
    await user.type(search, 'pay');
    await waitFor(() => expect(tileNames()).toEqual(['payments']));
    expect(new URLSearchParams(router.state.location.search).get('search')).toBe('pay');
    await user.keyboard('{Escape}');
    await waitFor(() => expect(tileNames()).toHaveLength(3));
  });

  it('filters with counts, shows chips and clears them', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp('/applications');
    await ready();
    const toggle = screen.getByRole('button', { name: 'Filters' });
    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const panel = screen.getByRole('region', { name: 'Filter options' });
    await user.click(within(panel).getByRole('checkbox', { name: 'Out of sync (1)' }));
    await waitFor(() => expect(tileNames()).toEqual(['payments']));
    expect(new URLSearchParams(router.state.location.search).get('sync')).toBe('OutOfSync');
    expect(within(screen.getByRole('list', { name: 'Active filters' })).getByText('Sync: Out of sync')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Remove filter Sync: Out of sync' }));
    await waitFor(() => expect(tileNames()).toHaveLength(3));

    await user.type(within(panel).getByRole('textbox', { name: 'Label selector' }), 'team=web{Enter}');
    await waitFor(() => expect(tileNames()).toEqual(['guestbook']));
    await user.type(within(panel).getByRole('textbox', { name: 'Namespaces pattern' }), 'pay*{Enter}');
    expect(await screen.findByRole('heading', { name: 'No applications match the filters' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    await waitFor(() => expect(tileNames()).toHaveLength(3));
    expect(router.state.location.search).toBe('');
  });

  it('restores filters and the view from the URL', async () => {
    renderApp('/applications?health=Progressing&view=list');
    const table = await screen.findByRole('table', { name: 'Applications' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent('search');
  });

  it('switches to the summary view and filters from a count', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp('/applications');
    await ready();
    await user.click(screen.getByRole('button', { name: 'Summary' }));
    expect(screen.getByRole('button', { name: 'Summary' })).toHaveAttribute('aria-pressed', 'true');
    const bySync = screen.getByRole('table', { name: 'Applications by sync status' });
    expect(within(bySync).getByRole('row', { name: /Out of sync.*1/ })).toBeInTheDocument();
    await user.click(within(bySync).getByRole('button', { name: /Out of sync/ }));
    await waitFor(() => expect(tileNames()).toEqual(['payments']));
    const search = new URLSearchParams(router.state.location.search);
    expect(search.get('view')).toBe('tiles');
    expect(search.get('sync')).toBe('OutOfSync');
  });

  it('marks favorites and filters by them', async () => {
    const user = userEvent.setup({ delay: null });
    renderApp('/applications');
    await ready();
    const star = screen.getByRole('button', { name: 'Add search to favorites' });
    await user.click(star);
    expect(screen.getByRole('button', { name: 'Remove search from favorites' })).toHaveAttribute('aria-pressed', 'true');
    expect(JSON.parse(window.localStorage.getItem(FAVORITES_KEY) ?? '[]')).toEqual(['patrol/search']);
    await user.click(screen.getByRole('button', { name: 'Filters' }));
    await user.click(screen.getByRole('checkbox', { name: 'Favorites only' }));
    await waitFor(() => expect(tileNames()).toEqual(['search']));
  });
});

describe('keyboard navigation', () => {
  it('moves a cursor over tiles with arrows and digits, opens with Enter and clears with Esc', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp('/applications');
    await ready();
    document.body.focus();
    await user.keyboard('{ArrowRight}');
    await waitFor(() => expect(screen.getByRole('link', { name: 'guestbook' })).toHaveFocus());
    expect(screen.getByRole('article', { name: 'guestbook' })).toHaveAttribute('aria-current', 'true');
    await user.keyboard('{ArrowRight}');
    await waitFor(() => expect(screen.getByRole('link', { name: 'payments' })).toHaveFocus());
    await user.keyboard('{ArrowLeft}{ArrowLeft}');
    await waitFor(() => expect(screen.getByRole('link', { name: 'guestbook' })).toHaveFocus());
    await user.keyboard('3');
    await waitFor(() => expect(screen.getByRole('article', { name: 'search' })).toHaveAttribute('aria-current', 'true'));
    await user.keyboard('{Escape}');
    expect(screen.getByRole('article', { name: 'search' })).not.toHaveAttribute('aria-current');
    await user.keyboard('2');
    await waitFor(() => expect(screen.getByRole('link', { name: 'payments' })).toHaveFocus());
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/applications/patrol/payments'));
  });

  it('enters the table with ArrowDown, moves by row, returns to search with Esc and opens with Enter', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp('/applications?view=list');
    const table = await screen.findByRole('table', { name: 'Applications' });
    document.body.focus();
    await user.keyboard('{ArrowDown}');
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows[0]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(rows[1]).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.getByRole('searchbox', { name: 'Filter applications by name or namespace' })).toHaveFocus();
    rows[1]?.focus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/applications/patrol/payments'));
  });
});

describe('bulk and per-application actions', () => {
  it('hard-refreshes the selected applications and reports each result', async () => {
    const user = userEvent.setup({ delay: null });
    const refreshed: string[] = [];
    server.use(
      http.get('*/api/v1/applications/:name', ({ params, request }) => {
        const url = new URL(request.url);
        if (url.searchParams.get('refresh')) refreshed.push(`${String(params.name)}:${url.searchParams.get('refresh') ?? ''}`);
        if (params.name === 'payments') return problem(500, 'internal', 'repo server timed out');
        return undefined;
      }),
    );
    renderApp('/applications');
    await ready();
    await user.click(screen.getByRole('checkbox', { name: 'Select guestbook' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select payments' }));
    const bar = screen.getByRole('region', { name: 'Bulk actions' });
    expect(bar).toHaveTextContent('2 selected');
    await user.click(within(bar).getByRole('button', { name: 'Refresh selected' }));
    const dialog = screen.getByRole('dialog', { name: 'Refresh 2 applications' });
    await user.click(within(dialog).getByRole('radio', { name: /Hard/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Refresh 2 applications' }));
    const results = await within(dialog).findByRole('table', { name: 'Results' });
    await waitFor(() => expect(within(dialog).getByText('Finished: 1 succeeded, 1 failed.')).toBeInTheDocument());
    expect(within(results).getByRole('row', { name: /guestbook.*Succeeded/ })).toBeInTheDocument();
    expect(within(results).getByRole('row', { name: /payments.*Failed.*repo server timed out/ })).toBeInTheDocument();
    expect(refreshed.sort()).toEqual(['guestbook:hard', 'payments:hard']);
  });

  it('syncs every selected application with the chosen flags', async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: Record<string, unknown> = {};
    server.use(
      http.post('*/api/v1/applications/:name/sync', async ({ params, request }) => {
        bodies[String(params.name)] = await request.clone().json();
        return undefined;
      }),
    );
    renderApp('/applications');
    await ready();
    await user.click(screen.getByRole('button', { name: 'Select all 3' }));
    await user.click(screen.getByRole('button', { name: 'Sync selected' }));
    const dialog = screen.getByRole('dialog', { name: 'Sync 3 applications' });
    await user.click(within(dialog).getByRole('checkbox', { name: /^Prune Delete live/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Sync 3 applications' }));
    await waitFor(() => expect(within(dialog).getByText('Finished: 3 succeeded, 0 failed.')).toBeInTheDocument());
    expect(Object.keys(bodies).sort()).toEqual(['guestbook', 'payments', 'search']);
    expect(bodies.guestbook).toMatchObject({ prune: true, strategy: { hook: { force: false } }, syncOptions: { items: ['CreateNamespace=true'] } });
    expect(within(dialog).getAllByRole('link', { name: 'View operation' })).toHaveLength(3);
  });

  it('requires typing delete before deleting several applications', async () => {
    const user = userEvent.setup({ delay: null });
    const deleted: string[] = [];
    server.use(
      http.delete('*/api/v1/applications/:name', ({ params, request }) => {
        deleted.push(`${String(params.name)}?${new URL(request.url).searchParams.toString()}`);
        return undefined;
      }),
    );
    renderApp('/applications');
    await ready();
    await user.click(screen.getByRole('checkbox', { name: 'Select search' }));
    await user.click(screen.getByRole('button', { name: 'Delete selected' }));
    const dialog = screen.getByRole('dialog', { name: 'Delete 1 application' });
    await user.click(within(dialog).getByRole('button', { name: 'Delete 1 application' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent("Type 'delete' to confirm.");
    await user.type(within(dialog).getByRole('textbox', { name: "Type 'delete' to confirm" }), 'delete');
    await user.click(within(dialog).getByRole('button', { name: 'Delete 1 application' }));
    await waitFor(() => expect(within(dialog).getByText('Finished: 1 succeeded, 0 failed.')).toBeInTheDocument());
    expect(deleted[0]).toMatch(/^search\?cascade=true&propagationPolicy=foreground/);
  });

  it('refreshes and deletes from a tile', async () => {
    const user = userEvent.setup({ delay: null });
    renderApp('/applications');
    await ready();
    await user.click(screen.getByRole('button', { name: 'Refresh guestbook' }));
    expect(await screen.findByText('Refreshed guestbook')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Delete payments' }));
    const dialog = screen.getByRole('dialog', { name: 'Delete payments?' });
    await user.type(within(dialog).getByRole('textbox', { name: /to confirm/ }), 'payments');
    await user.click(within(dialog).getByRole('button', { name: 'Delete application' }));
    await waitFor(() => expect(screen.queryByRole('article', { name: 'payments' })).not.toBeInTheDocument());
  });
});

describe('live updates and large lists', () => {
  it('merges watch events into the list', async () => {
    const billing = { ...searchApp(), metadata: { ...searchApp().metadata, name: 'billing', uid: 'uid-billing' } };
    server.use(sse([{ type: 'ADDED', object: billing }]));
    renderApp('/applications');
    await ready();
    await waitFor(() => expect(tileNames()).toEqual(['billing', 'guestbook', 'payments', 'search']), { timeout: 3000 });
  });

  it('paginates and virtualizes thousands of applications', async () => {
    const user = userEvent.setup({ delay: null });
    server.use(http.get('*/api/v1/applications', () => HttpResponse.json({ items: generated(1200), metadata: { resourceVersion: '9' } })));
    renderApp('/applications');
    await screen.findByRole('article', { name: 'app-0000' });
    expect(tileNames()).toHaveLength(25);
    const pagination = screen.getByRole('navigation', { name: 'Pagination' });
    expect(pagination).toHaveTextContent('Page 1 of 48 (1200 items)');
    await user.click(within(pagination).getByRole('button', { name: 'Next page' }));
    expect(await screen.findByRole('article', { name: 'app-0025' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'List' }));
    await user.selectOptions(within(screen.getByRole('navigation', { name: 'Pagination' })).getByRole('combobox', { name: 'Rows per page' }), '500');
    const table = await screen.findByRole('table', { name: 'Applications' });
    expect(table).toHaveAttribute('aria-rowcount', '501');
    expect(within(table).getAllByRole('row').length).toBeLessThan(100);
    await act(async () => {
      await Promise.resolve();
    });
  });
});
