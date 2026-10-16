import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Application } from '../../../api/types';
import { registerResourceExtension, registerStatusPanelExtension, registerTopBarActionMenuExt, resetExtensionRegistry } from '../../../extensions';
import { resetAppsCoreMockState } from '../../../mocks/handlers/apps-core';
import { renderApp } from '../../../test/render';

vi.mock('../resources/contract.ts', () => ({ ResourceViewsSlot: undefined, AppDiffPanel: undefined }));
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());
afterEach(() => resetExtensionRegistry());

function CostTab({ application }: { application: Application }) {
  return <p>Cost of {application.metadata.name}</p>;
}

function Budget() {
  return <span>Budget OK</span>;
}

function Runbook() {
  return <span>Runbook</span>;
}

describe('details page integrations', () => {
  it('mounts status-panel and top-bar extensions', async () => {
    registerStatusPanelExtension(Budget, 'Budget', 'budget');
    registerTopBarActionMenuExt(Runbook, 'Open runbook', 'runbook');
    renderApp('/applications/patrol/guestbook');
    expect(await screen.findByRole('group', { name: 'Budget' })).toHaveTextContent('Budget OK');
    const actions = screen.getByRole('group', { name: 'Application actions' });
    expect(within(actions).getByRole('button', { name: 'Open runbook' })).toBeInTheDocument();
  });

  it('adds Application resource-tab extensions and the notification subscriptions editor to the details panel', async () => {
    const user = userEvent.setup({ delay: null });
    registerResourceExtension(CostTab, 'argoproj.io', 'Application', 'Cost');
    const { router } = renderApp('/applications/patrol/guestbook?panel=details');
    const drawer = await screen.findByRole('dialog', { name: 'guestbook details' });
    expect(await within(drawer).findByRole('heading', { name: 'Notification subscriptions' })).toBeInTheDocument();
    await user.click(within(drawer).getByRole('tab', { name: 'Cost' }));
    expect(within(drawer).getByText('Cost of guestbook')).toBeInTheDocument();
    expect(new URLSearchParams(router.state.location.search).get('appTab')).toMatch(/^extension-0-Cost$/);
  });

  it('opens the application creation wizard from the list', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp('/applications');
    await screen.findByRole('article', { name: 'guestbook' });
    const header = screen.getByRole('heading', { level: 1, name: 'Applications' }).closest('header');
    if (!header) throw new Error('page header missing');
    await user.click(within(header).getByRole('button', { name: 'New application' }));
    expect(await screen.findByRole('dialog', { name: 'New application' })).toBeInTheDocument();
    expect(new URLSearchParams(router.state.location.search).get('new')).toBe('1');
  });
});
