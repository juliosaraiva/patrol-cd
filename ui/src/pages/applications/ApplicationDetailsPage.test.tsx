import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Application } from '../../api/types';
import { GUESTBOOK_SHA } from '../../mocks/fixtures/apps-core';
import { problem } from '../../mocks/handlers';
import { appsCoreMockStore, resetAppsCoreMockState } from '../../mocks/handlers/apps-core';
import { server } from '../../mocks/server';
import { renderApp } from '../../test/render';
import { DISMISSED_NOTICES_KEY } from './shared/notice';

// Resource views come from another workstream; the shell's built-in fallback is tested here.
vi.mock('./resources/contract.ts', () => ({ ResourceViewsSlot: undefined, AppDiffPanel: undefined }));

// Whole-app integration tests (real routes, lazy pages, MSW): allow for a loaded CI machine.
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());

async function details(path: string, name: string) {
  const result = renderApp(path);
  await screen.findByRole('heading', { level: 1, name });
  await screen.findByRole('group', { name: 'Application actions' });
  return result;
}

describe('application details states', () => {
  it('shows the name while loading', async () => {
    server.use(
      http.get('*/api/v1/applications/:name', async () => {
        await delay('infinite');
        return HttpResponse.json({});
      }),
    );
    renderApp('/applications/guestbook');
    expect(await screen.findByRole('heading', { level: 1, name: 'guestbook' })).toBeInTheDocument();
    expect(screen.getByText('Loading application…')).toBeInTheDocument();
  });

  it('explains a missing application and links back to the list', async () => {
    renderApp('/applications/patrol/nope');
    expect(await screen.findByRole('heading', { name: 'Application not found' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to applications' })).toHaveAttribute('href', '/applications');
  });

  it('offers a retry for server errors', async () => {
    const user = userEvent.setup({ delay: null });
    let calls = 0;
    server.use(
      http.get('*/api/v1/applications/guestbook', () => {
        calls += 1;
        return calls === 1 ? problem(503, 'unavailable', 'patrol-server is restarting') : undefined;
      }),
    );
    renderApp('/applications/guestbook');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Patrol server unavailable');
    await user.click(within(alert).getByRole('button', { name: 'Retry' }));
    expect(await screen.findByRole('group', { name: 'Application actions' })).toBeInTheDocument();
  });
});

describe('header and status panel', () => {
  it('shows health, sync with revision link and commit, auto-sync, last operation and source', async () => {
    await details('/applications/patrol/guestbook', 'guestbook');
    const health = screen.getByRole('region', { name: 'App health' });
    expect(health.querySelector('[data-status-family="health"]')).toHaveTextContent('Healthy');
    const sync = screen.getByRole('region', { name: 'Sync status' });
    expect(sync.querySelector('[data-status-family="sync"]')).toHaveTextContent('Synced');
    expect(within(sync).getByRole('link', { name: /4f2c1a9/ })).toHaveAttribute('href', `https://github.com/example/guestbook/commit/${GUESTBOOK_SHA}`);
    expect(await within(sync).findByText(/Update guestbook to 4f2c1a9/)).toBeInTheDocument();
    expect(within(sync).getByText('Auto-sync enabled (prune, self-heal)')).toBeInTheDocument();
    const op = screen.getByRole('region', { name: 'Last operation' });
    expect(op.querySelector('[data-status-family="operation"]')).toHaveTextContent('Succeeded');
    expect(within(op).getByText(/mock-admin/)).toBeInTheDocument();
    const source = screen.getByRole('region', { name: 'Source' });
    expect(within(source).getByText('https://github.com/example/guestbook.git')).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: /Conditions/ })).not.toBeInTheDocument();
  });

  it('lists conditions with recovery guidance and a dismissible notice', async () => {
    const user = userEvent.setup({ delay: null });
    await details('/applications/patrol/payments', 'payments');
    const conditions = screen.getByRole('region', { name: 'Conditions (2)' });
    expect(within(conditions).getByText('ComparisonError')).toBeInTheDocument();
    expect(within(conditions).getByText(/use Hard refresh/)).toBeInTheDocument();
    expect(within(conditions).getByText(/Keep each resource in one application/)).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Sources (2)' })).toBeInTheDocument();
    expect(screen.getByText('Payments freeze until Friday: only hotfixes.')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /More information/ })).toHaveAttribute('href', 'https://status.example.test/freeze');
    await user.click(screen.getByRole('button', { name: 'Dismiss notice' }));
    expect(screen.queryByText('Payments freeze until Friday: only hotfixes.')).not.toBeInTheDocument();
    expect(window.localStorage.getItem(DISMISSED_NOTICES_KEY)).toMatch(/patrol\/payments:/);
  });
});

describe('action bar', () => {
  it('refreshes normally and hard', async () => {
    const user = userEvent.setup({ delay: null });
    const refreshes: string[] = [];
    server.use(
      http.get('*/api/v1/applications/guestbook', ({ request }) => {
        const refresh = new URL(request.url).searchParams.get('refresh');
        if (refresh) refreshes.push(refresh);
        return undefined;
      }),
    );
    await details('/applications/patrol/guestbook', 'guestbook');
    await user.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(await screen.findByText('Refresh complete')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Hard refresh' }));
    expect(await screen.findByText('Hard refresh complete')).toBeInTheDocument();
    expect(refreshes).toEqual(['normal', 'hard']);
  });

  it('explains a failed refresh', async () => {
    const user = userEvent.setup({ delay: null });
    server.use(
      http.get('*/api/v1/applications/guestbook', ({ request }) =>
        new URL(request.url).searchParams.get('refresh') ? problem(403, 'permission_denied', 'permission denied: applications, get') : undefined,
      ),
    );
    await details('/applications/patrol/guestbook', 'guestbook');
    await user.click(screen.getByRole('button', { name: 'Refresh' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Permission denied');
    expect(alert).toHaveTextContent('Ask an administrator');
  });

  it('disables Diff when there is nothing to compare', async () => {
    await details('/applications/patrol/guestbook', 'guestbook');
    expect(screen.getByRole('button', { name: 'Diff' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Diff' })).toHaveAccessibleDescription(/nothing to compare/);
  });

  it('opens the diff drawer for an out-of-sync application', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = await details('/applications/patrol/payments', 'payments');
    await user.click(screen.getByRole('button', { name: 'Diff' }));
    const drawer = screen.getByRole('dialog', { name: 'Differences in payments' });
    expect(await within(drawer).findByRole('region', { name: 'Deployment payments/payments-api' })).toBeInTheDocument();
    expect(within(drawer).getByLabelText('Live state of Deployment payments/payments-api')).toHaveTextContent('"replicas": 1');
    expect(within(drawer).getByLabelText('Desired state of Deployment payments/payments-api')).toHaveTextContent('"replicas": 3');
    expect(within(drawer).getByText('Not in the source (would be pruned).')).toBeInTheDocument();
    expect(new URLSearchParams(router.state.location.search).get('panel')).toBe('diff');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(router.state.location.search).toBe('');
  });

  it('opens deep links and external URLs from the Links menu', async () => {
    const user = userEvent.setup({ delay: null });
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    await details('/applications/patrol/guestbook', 'guestbook');
    await user.click(await screen.findByRole('button', { name: /^Links/ }));
    const menu = screen.getByRole('menu', { name: 'Links' });
    expect(within(menu).getAllByRole('menuitem').map((i) => i.textContent)).toEqual(['Runbook', 'Dashboard', 'https://guestbook.example.test']);
    await user.click(within(menu).getByRole('menuitem', { name: 'Runbook' }));
    expect(open).toHaveBeenCalledWith('https://runbooks.example.test/guestbook', '_blank', 'noopener,noreferrer');
  });

  it('terminates a running operation after confirmation', async () => {
    const user = userEvent.setup({ delay: null });
    let terminated = false;
    server.use(
      http.delete('*/api/v1/applications/search/operation', () => {
        terminated = true;
        return undefined;
      }),
    );
    await details('/applications/patrol/search', 'search');
    await user.click(screen.getByRole('button', { name: 'Terminate operation' }));
    const dialog = screen.getByRole('dialog', { name: 'Terminate the running operation?' });
    await user.click(within(dialog).getByRole('button', { name: 'Terminate' }));
    expect(await screen.findByText('Termination requested')).toBeInTheDocument();
    expect(terminated).toBe(true);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });
});

describe('auto-sync and deletion approval', () => {
  it('toggles auto-sync after confirmation, keeping prune and self-heal', async () => {
    const user = userEvent.setup({ delay: null });
    let saved: Application | undefined;
    server.use(
      http.put('*/api/v1/applications/guestbook', async ({ request }) => {
        saved = (await request.clone().json()) as Application;
        expect(new URL(request.url).searchParams.get('validate')).toBe('false');
        return undefined;
      }),
    );
    await details('/applications/patrol/guestbook', 'guestbook');
    await user.click(screen.getByRole('button', { name: 'Disable auto-sync' }));
    const dialog = screen.getByRole('dialog', { name: 'Disable auto-sync?' });
    await user.click(within(dialog).getByRole('button', { name: 'Disable auto-sync' }));
    expect(await screen.findByText('Auto-sync disabled for guestbook')).toBeInTheDocument();
    expect(saved?.spec.syncPolicy?.automated).toEqual({ enabled: false, prune: true, selfHeal: true });
    expect(await screen.findByRole('button', { name: 'Enable auto-sync' })).toBeInTheDocument();
  });

  it('replaces Delete with Confirm deletion while a deletion waits for approval', async () => {
    const user = userEvent.setup({ delay: null });
    const app = appsCoreMockStore().apps.find((a) => a.metadata.name === 'payments');
    if (app) {
      app.metadata.deletionTimestamp = '2026-10-16T10:00:00Z';
      const legacy = app.status?.resources?.[1];
      if (legacy) legacy.requiresDeletionConfirmation = true;
    }
    await details('/applications/patrol/payments', 'payments');
    expect(screen.queryByRole('button', { name: 'Delete' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Confirm deletion' }));
    const dialog = screen.getByRole('dialog', { name: 'Approve deletion?' });
    await user.click(within(dialog).getByRole('button', { name: 'Approve deletion' }));
    expect(await screen.findByText('Deletion approved')).toBeInTheDocument();
    expect(appsCoreMockStore().apps.find((a) => a.metadata.name === 'payments')?.metadata.annotations?.['cd.patrol.example/deletion-approved']).toMatch(/^2\d{3}-/);
  });
});

describe('delete dialog', () => {
  it('requires the name, explains finalizers and deletes without cascading', async () => {
    const user = userEvent.setup({ delay: null });
    let query = '';
    server.use(
      http.delete('*/api/v1/applications/guestbook', ({ request }) => {
        query = new URL(request.url).searchParams.toString();
        return undefined;
      }),
    );
    const { router } = await details('/applications/patrol/guestbook', 'guestbook');
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    const dialog = screen.getByRole('dialog', { name: 'Delete guestbook?' });
    expect(within(dialog).getByText('resources-finalizer.cd.patrol.example')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: /Non-cascading/ }));
    expect(within(dialog).getByText(/removes the resources finalizer first/)).toBeInTheDocument();
    await user.type(within(dialog).getByRole('textbox', { name: /to confirm/ }), 'guestbok');
    await user.click(within(dialog).getByRole('button', { name: 'Delete application' }));
    expect(within(dialog).getByText('The name does not match.')).toBeInTheDocument();
    await user.type(within(dialog).getByRole('textbox', { name: /to confirm/ }), '{Backspace}ok');
    await user.click(within(dialog).getByRole('button', { name: 'Delete application' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/applications'));
    expect(query).toBe('cascade=false&appNamespace=patrol&project=default');
    expect(await screen.findByText('Deleting guestbook')).toBeInTheDocument();
  });

  it('shows a permission error with guidance', async () => {
    const user = userEvent.setup({ delay: null });
    server.use(http.delete('*/api/v1/applications/guestbook', () => problem(403, 'permission_denied', 'permission denied: applications, delete')));
    await details('/applications/patrol/guestbook', 'guestbook');
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    const dialog = screen.getByRole('dialog', { name: 'Delete guestbook?' });
    await user.type(within(dialog).getByRole('textbox', { name: /to confirm/ }), 'guestbook');
    await user.click(within(dialog).getByRole('button', { name: 'Delete application' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/lacks the RBAC permission to delete/);
  });
});

describe('resource views slot (built-in fallback)', () => {
  it('lists resources, selects one through the URL and starts a selective sync', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = await details('/applications/patrol/guestbook', 'guestbook');
    const table = screen.getByRole('table', { name: 'Managed resources' });
    const row = within(table).getByRole('row', { name: /Deployment.*guestbook-ui/ });
    row.focus();
    await user.keyboard('{Enter}');
    expect(new URLSearchParams(router.state.location.search).get('node')).toBe('apps/Deployment/guestbook/guestbook-ui');
    const selected = screen.getByRole('region', { name: 'Resource Deployment guestbook-ui' });
    await user.click(within(selected).getByRole('button', { name: 'Sync this resource' }));
    const drawer = screen.getByRole('dialog', { name: 'Sync guestbook' });
    expect(within(drawer).getByText(/Selective sync of 1 of 2 resources/)).toBeInTheDocument();
    expect(appsCoreMockStore().apps).toHaveLength(3);
  });
});
