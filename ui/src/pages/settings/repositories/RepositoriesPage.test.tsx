import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { RepositoryCredentialWriteRequest, RepositoryWriteRequest } from '../../../api/types';
import { repositoriesFixture } from '../../../mocks/fixtures/settings';
import { problem } from '../../../mocks/handlers';
import { settingsStore } from '../../../mocks/handlers/settings';
import { server } from '../../../mocks/server';
import { emptyGet, expectToast, failGet, findDialog, renderSettings, tableRows, withHydrator } from '../testUtils';

describe('Settings → Repositories', () => {
  it('shows a loading state, then repositories with type, project and connection status', async () => {
    server.use(
      http.get('*/api/v1/repositories', async () => {
        await delay(150);
        return HttpResponse.json({ metadata: { resourceVersion: '10' }, items: repositoriesFixture() });
      }),
    );
    await renderSettings('/settings/repos', 'Repositories');
    expect(screen.getByRole('status', { name: 'Loading repositories' })).toHaveAttribute('aria-busy', 'true');
    const table = await screen.findByRole('table', { name: 'Repositories' });
    const rows = tableRows('Repositories');
    expect(rows).toHaveLength(5);
    const infra = rows.find((r) => r.textContent?.includes('git@git.example.test:platform/infra.git'));
    expect(infra).toBeDefined();
    if (!infra) return;
    // Connection state uses the connection glyph family plus a text label and the server message.
    expect(within(infra).getByText('Failed')).toBeInTheDocument();
    expect(within(infra).getByText('ssh: handshake failed: knownhosts: key is unknown')).toBeInTheDocument();
    expect(infra.querySelector('[data-status-family="connection"]')).not.toBeNull();
    expect(within(table).getAllByText('All projects').length).toBeGreaterThan(0);
    // Credential flags of RepositoryInfo: own credentials, a matching template, or none.
    const row = (url: string) => rows.find((r) => r.textContent?.includes(url));
    expect(row('https://git.example.test/platform/apps.git')).toHaveTextContent('Stored');
    expect(row('https://git.example.test/platform/tools.git')).toHaveTextContent('From template');
    expect(row('https://charts.example.test/stable')).toHaveTextContent('None');
  });

  it('filters by type, connection status and search text', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.selectOptions(screen.getByLabelText('Type'), 'helm');
    expect(tableRows('Repositories')).toHaveLength(1);
    expect(screen.getByText('1 of 5 shown')).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText('Type'), '');
    await user.selectOptions(screen.getByLabelText('Connection'), 'Failed');
    expect(tableRows('Repositories')).toHaveLength(1);
    await user.selectOptions(screen.getByLabelText('Connection'), '');
    await user.type(screen.getByRole('searchbox', { name: 'Search repositories' }), 'nothing-matches');
    expect(screen.getByRole('heading', { name: 'No repositories match the filters' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    expect(tableRows('Repositories')).toHaveLength(5);
  });

  it('shows an empty state with a connect action', async () => {
    emptyGet('*/api/v1/repositories');
    await renderSettings('/settings/repos', 'Repositories');
    expect(await screen.findByRole('heading', { name: 'No repositories connected' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Connect repository' }).length).toBeGreaterThan(0);
  });

  it('explains load failures and retries', async () => {
    const user = userEvent.setup();
    failGet('*/api/v1/repositories', 403, 'permission_denied', 'permission denied: repositories, get');
    await renderSettings('/settings/repos', 'Repositories');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('You do not have access');
    expect(alert).toHaveTextContent('Ask an administrator');
    server.resetHandlers();
    await user.click(within(alert).getByRole('button', { name: 'Retry' }));
    expect(await screen.findByRole('table', { name: 'Repositories' })).toBeInTheDocument();
  });

  it('connects an HTTPS repository after a successful connection test', async () => {
    const user = userEvent.setup();
    const validated: RepositoryWriteRequest[] = [];
    const created: RepositoryWriteRequest[] = [];
    server.use(
      http.post('*/api/v1/repositories/:repo/validate', async ({ request }) => {
        validated.push((await request.json()) as RepositoryWriteRequest);
        return HttpResponse.json({ status: 'Successful' });
      }),
      http.post('*/api/v1/repositories', async ({ request }) => {
        const body = (await request.json()) as RepositoryWriteRequest;
        created.push(body);
        return HttpResponse.json({ metadata: { name: 'new' }, spec: body.spec });
      }),
    );
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Connect repository' }));
    const dialog = await findDialog('Connect repository');
    expect(within(dialog).getByRole('tab', { name: 'Via HTTPS' })).toHaveAttribute('aria-selected', 'true');
    await user.type(within(dialog).getByLabelText(/Repository URL/), 'https://git.example.test/team/new.git');
    await user.type(within(dialog).getByLabelText('Username'), 'robot');
    await user.type(within(dialog).getByLabelText(/Password or access token/), 's3cret-value');
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    await expectToast('Repository connected');
    expect(validated).toHaveLength(1);
    expect(created).toHaveLength(1);
    expect(created[0]?.spec).toMatchObject({ url: 'https://git.example.test/team/new.git', type: 'git' });
    expect(created[0]?.credentials).toEqual({ username: 'robot', password: 's3cret-value' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Connect repository' })).not.toBeInTheDocument());
  });

  it('validates required fields with an error summary that links to the fields', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Connect repository' }));
    const dialog = await findDialog('Connect repository');
    await user.click(within(dialog).getByRole('tab', { name: 'Via SSH' }));
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    const summary = within(dialog).getByRole('alert');
    expect(summary).toHaveTextContent('Fix 2 problems before saving');
    expect(summary).toHaveFocus();
    await user.click(within(summary).getByRole('link', { name: 'Repository URL is required.' }));
    expect(within(dialog).getByLabelText(/Repository URL/)).toHaveFocus();
    expect(within(dialog).getByLabelText(/SSH private key/)).toHaveAttribute('aria-invalid', 'true');
  });

  it('expands collapsed optional fields that hold an error so the summary link reaches them', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Connect repository' }));
    const dialog = await findDialog('Connect repository');
    await user.type(within(dialog).getByLabelText(/Repository URL/), 'https://git.example.test/team/new.git');
    const toggle = within(dialog).getByText('Network and clone options');
    const details = toggle.closest('details');
    if (!details) throw new Error('expected the network options in a <details> element');
    await user.click(toggle);
    expect(details).toHaveAttribute('open');
    await user.type(within(dialog).getByLabelText('Proxy'), 'proxy.internal:3128');
    await user.click(toggle);
    expect(details).not.toHaveAttribute('open');
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    const summary = within(dialog).getByRole('alert');
    expect(summary).toHaveTextContent('Fix 1 problem before saving');
    // The submit attempt expands the section with the invalid field and shows its message.
    expect(details).toHaveAttribute('open');
    expect(within(dialog).getByLabelText('Proxy')).toHaveAttribute('aria-invalid', 'true');
    expect(within(dialog).getByLabelText('Proxy')).toHaveAccessibleDescription(/Proxy must use http/);
    // Collapsed again by the user: the summary link expands it before moving focus.
    await user.click(toggle);
    expect(details).not.toHaveAttribute('open');
    await user.click(within(summary).getByRole('link', { name: /Proxy must use http/ }));
    expect(details).toHaveAttribute('open');
    expect(within(dialog).getByLabelText('Proxy')).toHaveFocus();
  });

  it('explains a failed connection test and allows saving anyway', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Connect repository' }));
    const dialog = await findDialog('Connect repository');
    await user.type(within(dialog).getByLabelText(/Repository URL/), 'https://unreachable.example.test/a.git');
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    const failure = await within(dialog).findByText('Connection failed');
    const banner = failure.closest('[data-tone]');
    expect(banner).toHaveTextContent('i/o timeout');
    expect(banner).toHaveTextContent('Patrol could not reach the server');
    await user.click(within(dialog).getByRole('button', { name: 'Connect anyway' }));
    await expectToast('Repository connected');
    expect(settingsStore.repos.read.some((r) => r.spec.url === 'https://unreachable.example.test/a.git')).toBe(true);
  });

  it('tests a connection without saving', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Connect repository' }));
    const dialog = await findDialog('Connect repository');
    await user.type(within(dialog).getByLabelText(/Repository URL/), 'https://git.example.test/team/x.git');
    await user.type(within(dialog).getByLabelText('Username'), 'u');
    await user.type(within(dialog).getByLabelText(/Password or access token/), 'wrong');
    await user.click(within(dialog).getByRole('button', { name: 'Test connection' }));
    expect(await within(dialog).findByText('Connection failed')).toBeInTheDocument();
    expect(within(dialog).getByText(/Check the username, password, token or key/)).toBeInTheDocument();
    expect(within(dialog).queryByRole('button', { name: 'Connect anyway' })).not.toBeInTheDocument();
    expect(settingsStore.repos.read).toHaveLength(5);
  });

  it('maps a duplicate repository to guidance', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Connect repository' }));
    const dialog = await findDialog('Connect repository');
    await user.selectOptions(within(dialog).getByLabelText('Project'), 'default');
    await user.type(within(dialog).getByLabelText(/Repository URL/), 'https://git.example.test/platform/apps.git');
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    const alert = await within(dialog).findByText('Already exists');
    expect(alert.closest('[data-tone]')).toHaveTextContent('Choose a different name, or edit the existing entry instead.');
  });

  it('connects via GitHub App with an SCM provider reference', async () => {
    const user = userEvent.setup();
    const created: RepositoryWriteRequest[] = [];
    server.use(
      http.post('*/api/v1/repositories', async ({ request }) => {
        const body = (await request.json()) as RepositoryWriteRequest;
        created.push(body);
        return HttpResponse.json({ metadata: { name: 'gh' }, spec: body.spec });
      }),
    );
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Connect repository' }));
    const dialog = await findDialog('Connect repository');
    const tab = within(dialog).getByRole('tab', { name: 'Via HTTPS' });
    tab.focus();
    await user.keyboard('{ArrowRight}{ArrowRight}');
    expect(within(dialog).getByRole('tab', { name: 'Via GitHub App' })).toHaveAttribute('aria-selected', 'true');
    expect(within(dialog).getByLabelText(/Installation ID/)).toHaveAccessibleDescription(/looks up the installation/);
    await user.type(within(dialog).getByLabelText(/Repository URL/), 'https://github.example.test/acme/app.git');
    await user.click(within(dialog).getByRole('radio', { name: /Use an SCM provider/ }));
    await user.selectOptions(within(dialog).getByRole('combobox', { name: 'SCM provider' }), 'github-main');
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    await expectToast('Repository connected');
    expect(created[0]?.spec.scmProviderRef).toEqual({ name: 'github-main' });
    expect(created[0]?.credentials).toBeUndefined();
  });

  it('saves the credentials as a template for the URL prefix', async () => {
    const user = userEvent.setup();
    const created: RepositoryCredentialWriteRequest[] = [];
    server.use(
      http.post('*/api/v1/repocreds', async ({ request }) => {
        const body = (await request.json()) as RepositoryCredentialWriteRequest;
        created.push(body);
        return HttpResponse.json({ metadata: {}, spec: body.spec });
      }),
    );
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Connect repository' }));
    const dialog = await findDialog('Connect repository');
    await user.type(within(dialog).getByLabelText(/Repository URL/), 'https://git.example.test/team/one.git');
    await user.type(within(dialog).getByLabelText('Username'), 'robot');
    await user.type(within(dialog).getByLabelText(/Password or access token/), 'pw');
    await user.click(within(dialog).getByRole('button', { name: 'Save as credential template' }));
    await expectToast('Credential template saved');
    expect(created[0]?.spec.urlPrefix).toBe('https://git.example.test/team');
    expect(created[0]?.credentials).toEqual({ username: 'robot', password: 'pw' });
  });

  it('edits a repository keeping stored secrets when the fields stay empty', async () => {
    const user = userEvent.setup();
    const updated: RepositoryWriteRequest[] = [];
    server.use(
      http.put('*/api/v1/repositories/:repo', async ({ request }) => {
        const body = (await request.json()) as RepositoryWriteRequest;
        updated.push(body);
        return HttpResponse.json(body);
      }),
    );
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Actions for https://git.example.test/platform/apps.git' }));
    await user.click(screen.getByRole('menuitem', { name: 'Edit' }));
    const dialog = await findDialog('Edit repository');
    expect(within(dialog).getByLabelText(/Repository URL/)).toBeDisabled();
    expect(within(dialog).getByRole('tab', { name: 'Via SSH' })).toBeDisabled();
    expect(within(dialog).getByLabelText(/Password or access token/)).toHaveAccessibleDescription('Leave empty to keep the stored value.');
    await user.click(within(dialog).getByRole('checkbox', { name: /Enable Git LFS/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Repository updated');
    expect(updated[0]?.spec.enableLFS).toBe(true);
    expect(updated[0]?.spec.credentialsRef).toEqual({ name: 'repo-apps' });
    expect(updated[0]?.credentials).toBeUndefined();
  });

  it('disconnects a repository after confirmation', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('table', { name: 'Repositories' });
    await user.click(screen.getByRole('button', { name: 'Actions for oci://registry.example.test/charts' }));
    await user.click(screen.getByRole('menuitem', { name: 'Disconnect' }));
    const dialog = await findDialog('Disconnect repository?');
    await user.click(within(dialog).getByRole('button', { name: 'Disconnect' }));
    await expectToast('Repository disconnected');
    await waitFor(() => expect(tableRows('Repositories')).toHaveLength(4));
  });

  it('refreshes connection status with forceRefresh', async () => {
    const user = userEvent.setup();
    const seen: string[] = [];
    server.use(
      http.get('*/api/v1/repositories', ({ request }) => {
        seen.push(new URL(request.url).searchParams.get('forceRefresh') ?? '');
        return HttpResponse.json({ items: [] });
      }),
    );
    await renderSettings('/settings/repos', 'Repositories');
    await screen.findByRole('heading', { name: 'No repositories connected' });
    await user.click(screen.getByRole('button', { name: 'Refresh status' }));
    await expectToast('Connection status refreshed');
    expect(seen).toContain('true');
  });

  it('explains write repositories and hides them when the hydrator is disabled', async () => {
    const user = userEvent.setup();
    const { router } = await renderSettings('/settings/repos', 'Repositories');
    await user.click(screen.getByRole('tab', { name: 'Write repositories' }));
    expect(router.state.location.search).toBe('?purpose=write');
    expect(screen.getByText('The source hydrator is disabled')).toBeInTheDocument();
  });

  it('lists and connects write repositories when the hydrator is enabled', async () => {
    const user = userEvent.setup();
    withHydrator();
    await renderSettings('/settings/repos?purpose=write', 'Repositories');
    expect(await screen.findByRole('table', { name: 'Write repositories' })).toBeInTheDocument();
    expect(screen.getByText(/needs push access/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Connect write repository' }));
    const dialog = await findDialog('Connect write repository');
    // Only git methods can push.
    expect(within(dialog).queryByRole('tab', { name: 'Via Helm' })).not.toBeInTheDocument();
    server.use(http.post('*/api/v1/write-repositories/:repo/validate', () => problem(501, 'internal', 'not implemented')));
    await user.type(within(dialog).getByLabelText(/Repository URL/), 'https://git.example.test/platform/hydrated.git');
    await user.click(within(dialog).getByRole('button', { name: 'Test connection' }));
    expect(await within(dialog).findByText('Unable to test the connection')).toBeInTheDocument();
  });
});
