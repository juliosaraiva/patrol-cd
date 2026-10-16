import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { SCMProviderWriteRequest } from '../../../api/types';
import { settingsStore } from '../../../mocks/handlers/settings';
import { server } from '../../../mocks/server';
import { emptyGet, expectToast, failGet, findDialog, renderSettings, tableRows } from '../testUtils';
import { webhookProviderName } from './model';

function capture(method: 'post' | 'put') {
  const bodies: SCMProviderWriteRequest[] = [];
  const route = method === 'post' ? '*/api/v1/scmproviders' : '*/api/v1/scmproviders/:name';
  server.use(
    http[method](route, async ({ request }) => {
      const body = (await request.json()) as SCMProviderWriteRequest;
      bodies.push(body);
      return HttpResponse.json({ apiVersion: 'cd.patrol.example/v1alpha1', kind: 'SCMProvider', ...body });
    }),
  );
  return bodies;
}

describe('Settings → SCM providers', () => {
  it('lists providers with type, authentication, connection and rate limit', async () => {
    await renderSettings('/settings/scmproviders', 'SCM providers');
    await screen.findByRole('table', { name: 'SCM providers' });
    const rows = tableRows('SCM providers');
    expect(rows[0]).toHaveTextContent('GitHub App 4242');
    expect(rows[0]).toHaveTextContent('Check runs');
    expect(rows[0]).toHaveTextContent('4875 / 5000');
    expect(rows[0]?.querySelector('[data-status-family="connection"][data-status="Successful"]')).not.toBeNull();
    expect(rows[1]).toHaveTextContent('Token (Secret scm-gitlab)');
    expect(rows[1]?.querySelector('[data-status="Failed"]')).not.toBeNull();
  });

  it('shows details with capabilities, rate limit and the webhook URL', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/scmproviders', 'SCM providers');
    await screen.findByRole('table', { name: 'SCM providers' });
    await user.click(screen.getByRole('button', { name: 'Details of github-main' }));
    const drawer = await findDialog('github-main');
    expect(within(drawer).getByRole('list', { name: 'Capabilities' })).toHaveTextContent('pullRequests');
    expect(drawer).toHaveTextContent('4875 of 5000 left');
    // The receiver path names the provider type (as the server routes it), not the object.
    expect(drawer).toHaveTextContent('/api/webhook/github');
    expect(drawer).not.toHaveTextContent('/api/webhook/github-main');
    expect(within(drawer).getByRole('button', { name: 'Copy webhook URL' })).toBeInTheDocument();
  });

  it('maps provider types to the server webhook receivers', () => {
    expect(webhookProviderName('github')).toBe('github');
    expect(webhookProviderName('bitbucketCloud')).toBe('bitbucket');
    expect(webhookProviderName('bitbucketServer')).toBe('bitbucketserver');
    expect(webhookProviderName('azureDevOps')).toBe('azuredevops');
    expect(webhookProviderName('awsCodeCommit')).toBeUndefined();
  });

  it('shows the empty state and load errors', async () => {
    emptyGet('*/api/v1/scmproviders');
    await renderSettings('/settings/scmproviders', 'SCM providers');
    expect(await screen.findByRole('heading', { name: 'No SCM providers' })).toBeInTheDocument();
  });

  it('reports load errors', async () => {
    failGet('*/api/v1/scmproviders');
    await renderSettings('/settings/scmproviders', 'SCM providers');
    expect(await screen.findByRole('alert')).toHaveTextContent('Server error');
  });

  it('creates a token provider that references an existing Secret, without secret values', async () => {
    const user = userEvent.setup();
    const posted = capture('post');
    await renderSettings('/settings/scmproviders', 'SCM providers');
    await screen.findByRole('table', { name: 'SCM providers' });
    await user.click(screen.getByRole('button', { name: 'New SCM provider' }));
    const dialog = await findDialog('New SCM provider');
    await user.click(within(dialog).getByRole('button', { name: 'Create provider' }));
    const summary = within(dialog).getByRole('alert');
    expect(summary).toHaveTextContent('Name is required.');
    expect(summary).toHaveTextContent('Token Secret name is required.');
    await user.type(within(dialog).getByRole('textbox', { name: 'Name' }), 'gitea-lab');
    await user.selectOptions(within(dialog).getByRole('combobox', { name: 'Type' }), 'gitea');
    expect(within(dialog).getByRole('alert')).toHaveTextContent('API URL is required.');
    await user.type(within(dialog).getByRole('textbox', { name: 'API URL' }), 'https://gitea.example.test/api/v1');
    await user.type(within(dialog).getByRole('textbox', { name: 'Secret name' }), 'scm-gitea');
    await user.click(within(dialog).getByRole('switch', { name: 'Report sync status to commits' }));
    await user.selectOptions(within(dialog).getByLabelText('Reporting mode'), 'checks');
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Check runs need a GitHub provider');
    await user.selectOptions(within(dialog).getByLabelText('Reporting mode'), 'statuses');
    await user.click(within(dialog).getByRole('button', { name: 'Create provider' }));
    await expectToast('SCM provider created');
    expect(posted[0]).toEqual({
      metadata: { name: 'gitea-lab' },
      spec: {
        type: 'gitea',
        apiURL: 'https://gitea.example.test/api/v1',
        auth: { tokenSecretRef: { name: 'scm-gitea', key: 'token' } },
        statusReporting: { enabled: true, mode: 'statuses' },
      },
    });
  });

  it('references a webhook Secret by name and key; secret values are never entered or sent', async () => {
    const user = userEvent.setup();
    const puts = capture('put');
    await renderSettings('/settings/scmproviders', 'SCM providers');
    await screen.findByRole('table', { name: 'SCM providers' });
    await user.click(screen.getByRole('button', { name: 'Details of gitlab-internal' }));
    await user.click(within(await findDialog('gitlab-internal')).getByRole('button', { name: 'Edit' }));
    const dialog = await findDialog('Edit gitlab-internal');
    expect(within(dialog).getByRole('textbox', { name: 'Name' })).toBeDisabled();
    expect(dialog.querySelector('input[type="password"], textarea')).toBeNull();
    await user.click(within(dialog).getByRole('switch', { name: 'Verify webhook deliveries' }));
    const webhook = within(dialog).getByRole('group', { name: 'Webhook secret' });
    expect(webhook).toHaveTextContent('Reference a key of an existing Secret in patrol');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Webhook secret Secret name is required.');
    await user.type(within(webhook).getByRole('textbox', { name: 'Secret name' }), 'scm-gitlab-webhook');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('SCM provider updated');
    expect(puts[0]?.spec.webhook).toEqual({ secretRef: { name: 'scm-gitlab-webhook', key: 'secret' } });
    expect(Object.keys(puts[0] ?? {}).sort()).toEqual(['metadata', 'spec']);
    expect(puts[0]?.metadata.resourceVersion).toBe('10');
  });

  it('recovers from a concurrent change (409) by reloading the latest version', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/scmproviders', 'SCM providers');
    await screen.findByRole('table', { name: 'SCM providers' });
    const stored = settingsStore.scmProviders.find((p) => p.metadata.name === 'gitlab-internal');
    if (stored) stored.metadata.resourceVersion = '42';
    await user.click(screen.getByRole('button', { name: 'Details of gitlab-internal' }));
    await user.click(within(await findDialog('gitlab-internal')).getByRole('button', { name: 'Edit' }));
    const dialog = await findDialog('Edit gitlab-internal');
    await user.type(within(dialog).getByRole('textbox', { name: 'Web URL' }), 'https://gitlab.example.test');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(await within(dialog).findByText('Changed by someone else')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Reload latest' }));
    await expectToast('Loaded the latest version');
    expect(within(dialog).getByRole('textbox', { name: 'Web URL' })).toHaveValue('https://gitlab.example.test');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('SCM provider updated');
    expect(settingsStore.scmProviders.find((p) => p.metadata.name === 'gitlab-internal')?.spec.webURL).toBe('https://gitlab.example.test');
  });

  it('configures GitHub App authentication', async () => {
    const user = userEvent.setup();
    const posted = capture('post');
    await renderSettings('/settings/scmproviders', 'SCM providers');
    await screen.findByRole('table', { name: 'SCM providers' });
    await user.click(screen.getByRole('button', { name: 'New SCM provider' }));
    const dialog = await findDialog('New SCM provider');
    await user.type(within(dialog).getByRole('textbox', { name: 'Name' }), 'github-two');
    await user.click(within(dialog).getByRole('radio', { name: /GitHub App/ }));
    await user.type(within(dialog).getByLabelText(/App ID/), '77');
    const key = within(dialog).getByRole('group', { name: 'Private key' });
    await user.type(within(key).getByRole('textbox', { name: 'Secret name' }), 'scm-github-two');
    await user.click(within(dialog).getByRole('button', { name: 'Create provider' }));
    await expectToast('SCM provider created');
    expect(posted[0]?.spec.auth).toEqual({ githubApp: { appID: 77, privateKeySecretRef: { name: 'scm-github-two', key: 'privateKey' } } });
    expect(Object.keys(posted[0] ?? {}).sort()).toEqual(['metadata', 'spec']);
  });

  it('deletes a provider after typing its name', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/scmproviders', 'SCM providers');
    await screen.findByRole('table', { name: 'SCM providers' });
    await user.click(screen.getByRole('button', { name: 'Details of gitlab-internal' }));
    await user.click(within(await findDialog('gitlab-internal')).getByRole('button', { name: 'Delete' }));
    const dialog = await findDialog('Delete SCM provider?');
    await user.type(within(dialog).getByRole('textbox'), 'gitlab-internal');
    await user.click(within(dialog).getByRole('button', { name: 'Delete provider' }));
    await expectToast('SCM provider deleted');
    await waitFor(() => expect(settingsStore.scmProviders).toHaveLength(1));
  });
});
