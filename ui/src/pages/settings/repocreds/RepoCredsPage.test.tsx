import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { RepositoryCredentialWriteRequest } from '../../../api/types';
import { server } from '../../../mocks/server';
import { emptyGet, expectToast, failGet, findDialog, renderSettings, tableRows, withHydrator } from '../testUtils';

describe('Settings → Repository credentials', () => {
  it('lists templates with their prefix, authentication and whether credentials are stored', async () => {
    await renderSettings('/settings/repocreds', 'Repository credentials');
    await screen.findByRole('table', { name: 'Credential templates' });
    const rows = tableRows('Credential templates');
    expect(rows).toHaveLength(2);
    const app = rows.find((r) => r.textContent?.includes('https://github.example.test/acme'));
    expect(app).toHaveTextContent('GitHub App');
    expect(app).toHaveTextContent('Stored');
    // RepositoryCredentialInfo carries hasCredentials and the (non-secret) username only.
    const https = rows.find((r) => r.textContent?.includes('https://git.example.test/platform'));
    expect(https).toHaveTextContent('Stored (user deploy-bot)');
  });

  it('shows an empty state explaining templates', async () => {
    emptyGet('*/api/v1/repocreds');
    await renderSettings('/settings/repocreds', 'Repository credentials');
    expect(await screen.findByRole('heading', { name: 'No credential templates' })).toBeInTheDocument();
    expect(screen.getByText(/any repository whose URL starts with the template prefix/)).toBeInTheDocument();
  });

  it('reports load errors with a retry', async () => {
    failGet('*/api/v1/repocreds', 503, 'unavailable', 'repo-server unavailable');
    await renderSettings('/settings/repocreds', 'Repository credentials');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Patrol server unavailable');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('creates an SSH template with a URL prefix', async () => {
    const user = userEvent.setup();
    const created: RepositoryCredentialWriteRequest[] = [];
    server.use(
      http.post('*/api/v1/repocreds', async ({ request }) => {
        const body = (await request.json()) as RepositoryCredentialWriteRequest;
        created.push(body);
        return HttpResponse.json({ metadata: { name: 'x' }, spec: body.spec });
      }),
    );
    await renderSettings('/settings/repocreds', 'Repository credentials');
    await screen.findByRole('table', { name: 'Credential templates' });
    await user.click(screen.getByRole('button', { name: 'New template' }));
    const dialog = await findDialog('New credential template');
    await user.click(within(dialog).getByRole('tab', { name: 'Via SSH' }));
    // Templates have no repository-only fields.
    expect(within(dialog).queryByLabelText('Project')).not.toBeInTheDocument();
    await user.type(within(dialog).getByLabelText(/URL prefix/), 'git@git.example.test:platform');
    await user.type(within(dialog).getByLabelText(/SSH private key/), '-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n');
    await user.click(within(dialog).getByRole('button', { name: 'Save template' }));
    await expectToast('Credential template saved');
    expect(created[0]?.spec).toMatchObject({ urlPrefix: 'git@git.example.test:platform', type: 'git' });
    expect(created[0]?.credentials?.sshPrivateKey).toContain('BEGIN OPENSSH PRIVATE KEY');
  });

  it('deletes a template after confirmation', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/repocreds', 'Repository credentials');
    await screen.findByRole('table', { name: 'Credential templates' });
    await user.click(screen.getByRole('button', { name: 'Delete template https://git.example.test/platform' }));
    const dialog = await findDialog('Delete credential template?');
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(tableRows('Credential templates')).toHaveLength(2);
    await user.click(screen.getByRole('button', { name: 'Delete template https://git.example.test/platform' }));
    await user.click(within(await findDialog('Delete credential template?')).getByRole('button', { name: 'Delete' }));
    await expectToast('Credential template deleted');
    await waitFor(() => expect(tableRows('Credential templates')).toHaveLength(1));
  });

  it('separates read and write templates when the hydrator is enabled', async () => {
    const user = userEvent.setup();
    withHydrator();
    await renderSettings('/settings/repocreds', 'Repository credentials');
    await user.click(await screen.findByRole('tab', { name: 'Write (source hydrator)' }));
    expect(await screen.findByRole('heading', { name: 'No write credential templates' })).toBeInTheDocument();
  });
});
