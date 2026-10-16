import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ApplicationSyncRequest } from '../../../api/types';
import { settingsFixture } from '../../../mocks/fixtures';
import { problem } from '../../../mocks/handlers';
import { appsCoreMockStore, resetAppsCoreMockState } from '../../../mocks/handlers/apps-core';
import { server } from '../../../mocks/server';
import { renderApp } from '../../../test/render';

vi.mock('../resources/contract.ts', () => ({ ResourceViewsSlot: undefined, AppDiffPanel: undefined }));

// Whole-app integration tests (real routes, lazy pages, MSW): allow for a loaded CI machine.
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());

function captureSync(name: string) {
  const bodies: ApplicationSyncRequest[] = [];
  server.use(
    http.post(`*/api/v1/applications/${name}/sync`, async ({ request }) => {
      bodies.push((await request.clone().json()) as ApplicationSyncRequest);
      return undefined;
    }),
  );
  return bodies;
}

async function openSync(name: string) {
  const result = renderApp(`/applications/patrol/${name}?panel=sync`);
  const drawer = await screen.findByRole('dialog', { name: `Sync ${name}` });
  return { ...result, drawer };
}

describe('sync panel', () => {
  it('starts a full sync and links to the created operation', async () => {
    const user = userEvent.setup({ delay: null });
    const bodies = captureSync('guestbook');
    const { drawer, router } = await openSync('guestbook');
    expect(within(drawer).getByRole('textbox', { name: 'Revision' })).toHaveValue('main');
    expect(within(drawer).getByText('All 2 resources are synced.', { exact: false })).toBeInTheDocument();
    expect(within(drawer).getByRole('checkbox', { name: /^Auto-create namespace/ })).toBeChecked();
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(bodies[0]).toEqual({ prune: false, dryRun: false, strategy: { hook: { force: false } }, syncOptions: { items: ['CreateNamespace=true'] } });
    expect(await screen.findByText('Sync started for guestbook')).toBeInTheDocument();
    const op = appsCoreMockStore().operations.guestbook?.[0]?.metadata.name ?? '';
    await user.click(screen.getByRole('link', { name: 'View operation' }));
    await waitFor(() => expect(new URLSearchParams(router.state.location.search).get('op')).toBe(op));
    expect(await screen.findByRole('dialog', { name: 'Operations of guestbook' })).toBeInTheDocument();
  });

  it('sends a revision override, apply-only strategy, retry and a selective resource list', async () => {
    const user = userEvent.setup({ delay: null });
    const bodies = captureSync('guestbook');
    const { drawer } = await openSync('guestbook');
    const revision = within(drawer).getByRole('textbox', { name: 'Revision' });
    await user.clear(revision);
    await user.type(revision, 'v2.0.0');
    await user.click(within(drawer).getByRole('checkbox', { name: /^Apply only/ }));
    await user.click(within(drawer).getByRole('checkbox', { name: /^Retry failed syncs/ }));
    await user.click(within(drawer).getByRole('button', { name: 'Select none' }));
    await user.click(within(drawer).getByRole('checkbox', { name: 'Sync Deployment guestbook/guestbook-ui' }));
    expect(within(drawer).getByText(/Selective sync of 1 of 2 resources/)).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({
      revision: 'v2.0.0',
      strategy: { apply: { force: false } },
      resources: [{ group: 'apps', kind: 'Deployment', namespace: 'guestbook', name: 'guestbook-ui' }],
      retryStrategy: { limit: 5, backoff: { duration: '5s', factor: 2, maxDuration: '3m' } },
    });
  });

  it('filters the resource picker and refuses an empty selection', async () => {
    const user = userEvent.setup({ delay: null });
    const bodies = captureSync('guestbook');
    const { drawer } = await openSync('guestbook');
    await user.type(within(drawer).getByRole('searchbox', { name: 'Filter resources' }), 'service');
    const list = within(drawer).getByRole('list', { name: 'Resources to sync' });
    expect(within(list).getAllByRole('checkbox')).toHaveLength(1);
    await user.click(within(drawer).getByRole('button', { name: 'Select none' }));
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    expect(within(drawer).getByRole('alert')).toHaveTextContent('Select at least one resource to sync.');
    expect(bodies).toHaveLength(0);
  });

  it('lists every source revision of a multi-source app, excludes hooks and confirms pruning', async () => {
    const user = userEvent.setup({ delay: null });
    const bodies = captureSync('payments');
    const { drawer } = await openSync('payments');
    expect(within(drawer).getByRole('textbox', { name: 'Revision of source 1' })).toHaveValue('1.4.2');
    expect(within(drawer).getByRole('textbox', { name: 'Revision of source 2' })).toHaveValue('main');
    expect(within(drawer).queryByRole('checkbox', { name: /payments-migrate/ })).not.toBeInTheDocument();
    await user.click(within(drawer).getByRole('checkbox', { name: /^Prune Delete live/ }));
    expect(within(drawer).getByText('1 resource(s) will be deleted')).toBeInTheDocument();
    // Listed both in the resource picker and in the prune warning.
    expect(within(drawer).getAllByText('ConfigMap payments/payments-legacy')).toHaveLength(2);
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    expect(within(drawer).getByRole('alert')).toHaveTextContent("Type 'prune' to confirm deleting resources.");
    expect(bodies).toHaveLength(0);
    await user.type(within(drawer).getByRole('textbox', { name: "Type 'prune' to confirm" }), 'prune');
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]?.prune).toBe(true);
    expect(bodies[0]?.revisions).toBeUndefined();
  });

  it('requires confirming Force, and explains deletion approval for Prune=confirm', async () => {
    const user = userEvent.setup({ delay: null });
    const bodies = captureSync('guestbook');
    const { drawer } = await openSync('guestbook');
    await user.click(within(drawer).getByRole('checkbox', { name: /^Force/ }));
    await user.selectOptions(within(drawer).getByRole('combobox', { name: 'Prune' }), 'confirm');
    expect(within(drawer).getByText(/Prune=confirm holds deletions until approved/)).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    expect(within(drawer).getByRole('alert')).toHaveTextContent('Confirm that you understand the Force option.');
    await user.click(within(drawer).getByRole('checkbox', { name: 'I understand that Force deletes and recreates resources' }));
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ strategy: { hook: { force: true } }, syncOptions: { items: ['CreateNamespace=true', 'Prune=confirm'] } });
  });

  it('offers Replace only when the server allows it, with a typed confirmation', async () => {
    const user = userEvent.setup({ delay: null });
    const first = await openSync('guestbook');
    expect(within(first.drawer).queryByRole('checkbox', { name: /^Replace/ })).not.toBeInTheDocument();
    first.unmount();

    server.use(http.get('*/api/v1/settings', () => HttpResponse.json({ ...settingsFixture, syncWithReplaceAllowed: true })));
    const bodies = captureSync('guestbook');
    const { drawer } = await openSync('guestbook');
    await user.click(within(drawer).getByRole('checkbox', { name: /^Replace/ }));
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    expect(within(drawer).getByRole('alert')).toHaveTextContent("Type 'replace' to confirm the Replace option.");
    await user.type(within(drawer).getByRole('textbox', { name: "Type 'replace' to confirm" }), 'replace');
    await user.click(within(drawer).getByRole('button', { name: 'Synchronize' }));
    await waitFor(() => expect(bodies[0]?.syncOptions?.items).toContain('Replace=true'));
  });

  it('preselects one resource from the upstream deploy deep link', async () => {
    renderApp('/applications/patrol/guestbook?deploy=apps%2FDeployment%2Fguestbook%2Fguestbook-ui');
    const drawer = await screen.findByRole('dialog', { name: 'Sync guestbook' });
    expect(within(drawer).getByText(/Selective sync of 1 of 2 resources/)).toBeInTheDocument();
    expect(within(drawer).getByRole('checkbox', { name: 'Sync Deployment guestbook/guestbook-ui' })).toBeChecked();
    expect(within(drawer).getByRole('checkbox', { name: 'Sync Service guestbook/guestbook-ui' })).not.toBeChecked();
  });

  it('explains a sync refused by a sync window', async () => {
    const user = userEvent.setup({ delay: null });
    server.use(http.post('*/api/v1/applications/guestbook/sync', () => problem(412, 'failed_precondition', 'cannot sync: blocked by sync window')));
    const { drawer } = await openSync('guestbook');
    await user.click(within(drawer).getByRole('checkbox', { name: /^Dry run/ }));
    await user.click(within(drawer).getByRole('button', { name: 'Start dry run' }));
    const alert = await within(drawer).findByRole('alert');
    expect(alert).toHaveTextContent('Cannot sync right now');
    expect(alert).toHaveTextContent('blocked by sync window');
    expect(alert).toHaveTextContent(/active sync window that denies manual syncs/);
    expect(screen.getByRole('dialog', { name: 'Sync guestbook' })).toBeInTheDocument();
  });
});
