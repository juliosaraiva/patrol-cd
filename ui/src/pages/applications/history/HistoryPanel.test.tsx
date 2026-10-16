import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Application, ApplicationRollbackRequest } from '../../../api/types';
import { GUESTBOOK_PREVIOUS_SHA, GUESTBOOK_SHA } from '../../../mocks/fixtures/apps-core';
import { problem } from '../../../mocks/handlers';
import { resetAppsCoreMockState } from '../../../mocks/handlers/apps-core';
import { server } from '../../../mocks/server';
import { renderApp } from '../../../test/render';

vi.mock('../resources/contract.ts', () => ({ ResourceViewsSlot: undefined, AppDiffPanel: undefined }));

// Whole-app integration tests (real routes, lazy pages, MSW): allow for a loaded CI machine.
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());

async function openHistory(name: string) {
  const result = renderApp(`/applications/patrol/${name}?panel=history`);
  const drawer = await screen.findByRole('dialog', { name: `History of ${name}` });
  return { ...result, drawer };
}

describe('history and rollback', () => {
  it('lists deployments newest first with revisions, compare links and commit details', async () => {
    const user = userEvent.setup({ delay: null });
    const { drawer } = await openHistory('guestbook');
    const items = within(within(drawer).getByRole('list', { name: 'Deployments' })).getAllByRole('heading', { level: 3 });
    expect(items.map((h) => h.textContent)).toEqual(['Deployment 2', 'Deployment 1']);
    expect(within(drawer).getByRole('button', { name: 'Redeploy to deployment 2' })).toBeInTheDocument();
    const older = within(drawer).getByRole('list', { name: 'Sources of deployment 1' });
    expect(within(older).getByRole('link', { name: /9a8b7c6/ })).toHaveAttribute('href', `https://github.com/example/guestbook/commit/${GUESTBOOK_PREVIOUS_SHA}`);
    expect(within(older).getByRole('link', { name: /Compare with current/ })).toHaveAttribute(
      'href',
      `https://github.com/example/guestbook/compare/${GUESTBOOK_PREVIOUS_SHA}...${GUESTBOOK_SHA}`,
    );
    const toggle = within(drawer).getAllByRole('button', { name: 'Show commit details' }).at(1);
    if (!toggle) throw new Error('toggle missing');
    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(await within(drawer).findByText('Dana Developer <dana@example.test>')).toBeInTheDocument();
  });

  it('disables auto-sync first when asked, then rolls back', async () => {
    const user = userEvent.setup({ delay: null });
    const calls: string[] = [];
    let saved: Application | undefined;
    let rollback: ApplicationRollbackRequest | undefined;
    server.use(
      http.put('*/api/v1/applications/guestbook', async ({ request }) => {
        calls.push('update');
        saved = (await request.clone().json()) as Application;
        return undefined;
      }),
      http.post('*/api/v1/applications/guestbook/rollback', async ({ request }) => {
        calls.push('rollback');
        rollback = (await request.clone().json()) as ApplicationRollbackRequest;
        return undefined;
      }),
    );
    const { drawer } = await openHistory('guestbook');
    await user.click(within(drawer).getByRole('button', { name: 'Roll back to deployment 1' }));
    const dialog = screen.getByRole('dialog', { name: 'Roll back to deployment 1?' });
    expect(within(dialog).getByText('Auto-sync is enabled')).toBeInTheDocument();
    const confirm = within(dialog).getByRole('button', { name: 'Roll back' });
    expect(confirm).toBeDisabled();
    await user.click(within(dialog).getByRole('checkbox', { name: 'Disable auto-sync, then roll back' }));
    await user.click(within(dialog).getByRole('checkbox', { name: /^Prune/ }));
    await user.click(confirm);
    expect(await screen.findByText('Rollback to deployment 1 started')).toBeInTheDocument();
    expect(calls).toEqual(['update', 'rollback']);
    expect(saved?.spec.syncPolicy?.automated).toEqual({ enabled: false, prune: true, selfHeal: true });
    expect(rollback).toEqual({ id: 1, prune: true, dryRun: false });
  });

  it('explains multi-source rollbacks and server refusals', async () => {
    const user = userEvent.setup({ delay: null });
    server.use(
      http.post('*/api/v1/applications/payments/rollback', () => problem(412, 'failed_precondition', 'application is deleting')),
    );
    const { drawer } = await openHistory('payments');
    const sources = within(drawer).getByRole('list', { name: 'Sources of deployment 4' });
    expect(within(sources).getAllByRole('listitem')).toHaveLength(2);
    await user.click(within(drawer).getByRole('button', { name: 'Redeploy to deployment 4' }));
    const dialog = screen.getByRole('dialog', { name: 'Redeploy to deployment 4?' });
    expect(within(dialog).getByText(/All 2 sources return to the revisions recorded/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Redeploy' }));
    const alert = await within(dialog).findByRole('alert');
    expect(alert).toHaveTextContent('Cannot redeploy right now');
    expect(alert).toHaveTextContent('application is deleting');
  });

  it('shows guidance when nothing was deployed yet', async () => {
    const { drawer } = await openHistory('search');
    expect(within(drawer).getByRole('heading', { name: 'No deployments recorded yet' })).toBeInTheDocument();
    expect(within(drawer).getByText(/newest 10 are kept/)).toBeInTheDocument();
    await waitFor(() => expect(within(drawer).queryByRole('button', { name: /Roll back/ })).not.toBeInTheDocument());
  });
});
