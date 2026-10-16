import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { loggedInUser } from '../../../mocks/fixtures';
import { mockState } from '../../../mocks/handlers';
import { server } from '../../../mocks/server';
import { expectToast, failGet, findDialog, renderSettings, tableRows } from '../testUtils';

describe('Settings → Accounts', () => {
  it('lists accounts with status, capabilities and token counts', async () => {
    mockState.username = 'admin';
    await renderSettings('/settings/accounts', 'Accounts');
    await screen.findByRole('table', { name: 'Local accounts' });
    const rows = tableRows('Local accounts');
    expect(rows.map((r) => within(r).getAllByRole('cell').map((c) => c.textContent))).toEqual([
      ['admin (you)', 'Enabled', 'Login, API key', '1'],
      ['ci-bot', 'Enabled', 'API key', '1'],
      ['viewer', 'Disabled', 'Login', '0'],
    ]);
    expect(within(screen.getByRole('main')).getByRole('link', { name: 'User info' })).toHaveAttribute('href', '/user-info');
  });

  it('shows an empty state and errors', async () => {
    server.use(http.get('*/api/v1/account', () => HttpResponse.json({ items: [] })));
    await renderSettings('/settings/accounts', 'Accounts');
    expect(await screen.findByRole('heading', { name: 'No local accounts' })).toBeInTheDocument();
  });

  it('reports load errors', async () => {
    failGet('*/api/v1/account', 403, 'permission_denied', 'permission denied: accounts, get');
    await renderSettings('/settings/accounts', 'Accounts');
    expect(await screen.findByRole('alert')).toHaveTextContent('You do not have access');
  });

  it('generates a token shown once and lists it', async () => {
    const user = userEvent.setup();
    const { router } = await renderSettings('/settings/accounts', 'Accounts');
    await user.click(await screen.findByRole('link', { name: 'ci-bot' }));
    await screen.findByRole('heading', { level: 1, name: 'ci-bot' });
    expect(router.state.location.pathname).toBe('/settings/accounts/ci-bot');
    await screen.findByRole('table', { name: 'API tokens' });
    await user.type(screen.getByLabelText('Token ID'), 'pipeline');
    await user.click(screen.getByRole('button', { name: 'Generate token' }));
    expect(screen.getByRole('alert')).toHaveTextContent('A token with ID "pipeline" already exists.');
    await user.clear(screen.getByLabelText('Token ID'));
    await user.type(screen.getByLabelText('Token ID'), 'nightly');
    await user.type(screen.getByLabelText('Expires in'), '30d');
    await user.click(screen.getByRole('button', { name: 'Generate token' }));
    const dialog = await findDialog('New token for ci-bot');
    expect(within(dialog).getByRole('textbox', { name: 'Token' })).toHaveValue('mock.account.token.1');
    expect(within(dialog).getByRole('button', { name: 'Copy token' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(tableRows('API tokens')).toHaveLength(2));
    // The token value is gone once the dialog is closed.
    expect(screen.queryByDisplayValue('mock.account.token.1')).not.toBeInTheDocument();
  });

  it('deletes a token after confirmation', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/accounts/admin', 'admin');
    await screen.findByRole('table', { name: 'API tokens' });
    await user.click(screen.getByRole('button', { name: 'Delete token laptop' }));
    await user.click(within(await findDialog('Delete token?')).getByRole('button', { name: 'Delete token' }));
    await expectToast('Token deleted');
    expect(await screen.findByText('No tokens.')).toBeInTheDocument();
  });

  it('explains accounts without the apiKey capability and links the own password change', async () => {
    mockState.username = 'viewer';
    server.use(http.get('*/api/v1/session/userinfo', () => HttpResponse.json({ ...loggedInUser, username: 'viewer' })));
    await renderSettings('/settings/accounts/viewer', 'viewer');
    expect(await screen.findByText('Tokens are not enabled for this account')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Generate token' })).not.toBeInTheDocument();
    expect(screen.getByText('This account is disabled')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Change your password' })).toHaveAttribute('href', '/user-info');
  });

  it('shows guidance for an unknown account', async () => {
    await renderSettings('/settings/accounts/ghost', 'ghost');
    expect(await screen.findByRole('heading', { name: 'Account not found' })).toBeInTheDocument();
  });
});
