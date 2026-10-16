import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { problem } from '../../../mocks/handlers';
import { appsCoreMockStore, resetAppsCoreMockState } from '../../../mocks/handlers/apps-core';
import { server } from '../../../mocks/server';
import { renderApp } from '../../../test/render';

vi.mock('../resources/contract.ts', () => ({ ResourceViewsSlot: undefined, AppDiffPanel: undefined }));

// Whole-app integration tests (real routes, lazy pages, MSW): allow for a loaded CI machine.
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());

async function openOperations(path: string, name: string) {
  const result = renderApp(path);
  const drawer = await screen.findByRole('dialog', { name: `Operations of ${name}` });
  return { ...result, drawer };
}

describe('operation panel', () => {
  it('shows the running operation with progress and per-resource results, and terminates it', async () => {
    const user = userEvent.setup({ delay: null });
    let terminated = 0;
    server.use(
      http.delete('*/api/v1/applications/search/operation', () => {
        terminated += 1;
        return undefined;
      }),
    );
    const { drawer } = await openOperations('/applications/patrol/search?panel=operation', 'search');
    const summary = await within(drawer).findByRole('region', { name: 'Operation summary' });
    expect(summary.querySelector('[data-status-family="operation"]')).toHaveTextContent('Running');
    expect(within(summary).getByText('search-sync-7')).toBeInTheDocument();
    expect(await within(drawer).findByText('Phase Sync, wave 1: 2 of 5 tasks completed')).toBeInTheDocument();
    expect(within(drawer).getByRole('progressbar', { name: '2 of 5 tasks completed' })).toHaveAttribute('value', '2');
    const results = within(drawer).getByRole('table', { name: 'Resource results' });
    expect(within(results).getByRole('row', { name: /StatefulSet.*prod-search.*Synced/ })).toBeInTheDocument();

    await user.click(within(drawer).getByRole('button', { name: 'Terminate operation' }));
    const confirm = screen.getByRole('dialog', { name: 'Terminate the running operation?' });
    await user.click(within(confirm).getByRole('button', { name: 'Terminate' }));
    await waitFor(() => expect(terminated).toBe(1));
    expect(await screen.findByText('Termination requested')).toBeInTheDocument();
  });

  it('pages through older operations with the continue token', async () => {
    const user = userEvent.setup({ delay: null });
    const requests: URL[] = [];
    server.use(
      http.get('*/api/v1/applications/guestbook/operations', ({ request }) => {
        requests.push(new URL(request.url));
        const [newest, ...rest] = appsCoreMockStore().operations.guestbook ?? [];
        const token = new URL(request.url).searchParams.get('continue');
        return token === 'page-2'
          ? HttpResponse.json({ metadata: {}, items: rest })
          : HttpResponse.json({ metadata: { continue: 'page-2' }, items: newest ? [newest] : [] });
      }),
    );
    const { drawer } = await openOperations('/applications/patrol/guestbook?panel=operation', 'guestbook');
    const history = await within(drawer).findByRole('table', { name: 'Operation history' });
    expect(within(history).getAllByRole('row').slice(1)).toHaveLength(1);
    await user.click(within(drawer).getByRole('button', { name: 'Load older operations' }));
    await waitFor(() => expect(within(history).getAllByRole('row').slice(1).length).toBeGreaterThan(1));
    expect(requests.at(-1)?.searchParams.get('continue')).toBe('page-2');
    expect(within(drawer).queryByRole('button', { name: 'Load older operations' })).not.toBeInTheDocument();
  });

  it('filters results, shows warnings and hooks', async () => {
    const user = userEvent.setup({ delay: null });
    const { drawer } = await openOperations('/applications/patrol/payments?panel=operation', 'payments');
    const results = await within(drawer).findByRole('table', { name: 'Resource results' });
    expect(within(drawer).getByText('ConfigMap payments/shared-config is managed by another application')).toBeInTheDocument();
    await user.selectOptions(within(drawer).getByRole('combobox', { name: 'Show' }), 'failed');
    const rows = within(results).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent('payments-api');
    expect(rows[0]).toHaveTextContent('Sync failed');
    expect(rows[0]).toHaveTextContent('missing resource limits');
  });

  it('lists operation history and shows another operation through the URL', async () => {
    const user = userEvent.setup({ delay: null });
    const { drawer, router } = await openOperations('/applications/patrol/guestbook?panel=operation', 'guestbook');
    const results = await within(drawer).findByRole('table', { name: 'Resource results' });
    expect(within(results).getByRole('row', { name: /Job.*smoke-test.*PostSync/ })).toBeInTheDocument();
    expect(within(drawer).getByText(/1 hook: PostSync Job\/smoke-test \(Succeeded\)/)).toBeInTheDocument();
    const history = within(drawer).getByRole('table', { name: 'Operation history' });
    const rows = within(history).getAllByRole('row').slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([expect.stringContaining('guestbook-sync-3'), expect.stringContaining('guestbook-sync-2')]);
    rows[1]?.focus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(new URLSearchParams(router.state.location.search).get('op')).toBe('guestbook-sync-2'));
    const summary = within(drawer).getByRole('region', { name: 'Operation summary' });
    expect(within(summary).getByText('guestbook-sync-2')).toBeInTheDocument();
    expect(within(summary).getByText('Automated sync')).toBeInTheDocument();
  });

  it('approves deletions waiting for confirmation', async () => {
    const user = userEvent.setup({ delay: null });
    const legacy = appsCoreMockStore().apps.find((a) => a.metadata.name === 'payments')?.status?.resources?.[1];
    if (legacy) legacy.requiresDeletionConfirmation = true;
    const patches: unknown[] = [];
    server.use(
      http.patch('*/api/v1/applications/payments', async ({ request }) => {
        patches.push(await request.clone().json());
        return undefined;
      }),
    );
    const { drawer } = await openOperations('/applications/patrol/payments?panel=operation', 'payments');
    expect(within(drawer).getByText('1 resource(s) wait for deletion approval')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Confirm pruning' }));
    const dialog = screen.getByRole('dialog', { name: 'Approve deletion?' });
    expect(within(dialog).getByText('ConfigMap payments/payments-legacy')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Approve deletion' }));
    expect(await screen.findByText('Deletion approved')).toBeInTheDocument();
    const patch = patches[0] as { patchType: string; patch: string };
    expect(patch.patchType).toBe('merge');
    expect(JSON.parse(patch.patch)).toEqual({ metadata: { annotations: { 'cd.patrol.example/deletion-approved': expect.stringMatching(/^\d{4}-\d{2}-\d{2}T/) as unknown } } });
  });

  it('falls back to the application operation state when Operations cannot be listed', async () => {
    server.use(http.get('*/api/v1/applications/search/operations', () => problem(403, 'permission_denied', 'permission denied')));
    const { drawer } = await openOperations('/applications/patrol/search?panel=operation', 'search');
    expect(await within(drawer).findByText('Could not load Operation objects')).toBeInTheDocument();
    const summary = within(drawer).getByRole('region', { name: 'Operation summary' });
    expect(within(summary).getByText('search-sync-7')).toBeInTheDocument();
    expect(within(drawer).getByText(/Per-resource results are recorded on the Operation object/)).toBeInTheDocument();
  });

  it('explains when nothing has run yet', async () => {
    const app = appsCoreMockStore().apps.find((a) => a.metadata.name === 'search');
    if (app?.status) delete app.status.operationState;
    appsCoreMockStore().operations.search = [];
    const { drawer } = await openOperations('/applications/patrol/search?panel=operation', 'search');
    expect(await within(drawer).findByRole('heading', { name: 'No operation yet' })).toBeInTheDocument();
  });
});
