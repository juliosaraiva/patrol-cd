import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { RepositoryCertificateList } from '../../../api/types';
import { server } from '../../../mocks/server';
import { emptyGet, expectToast, failGet, findDialog, renderSettings, tableRows } from '../testUtils';

const PEM = '-----BEGIN CERTIFICATE-----\nTUlJQmZha2U=\n-----END CERTIFICATE-----';
const ED25519 = 'AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl';

describe('Settings → Certificates and known hosts', () => {
  it('lists TLS certificates and switches to SSH known hosts with fingerprints', async () => {
    const user = userEvent.setup();
    const { router } = await renderSettings('/settings/certs', 'Certificates and known hosts');
    await screen.findByRole('table', { name: 'TLS certificates' });
    expect(tableRows('TLS certificates')).toHaveLength(1);
    const tab = screen.getByRole('tab', { name: 'TLS certificates' });
    tab.focus();
    await user.keyboard('{ArrowRight}');
    expect(screen.getByRole('tab', { name: 'SSH known hosts' })).toHaveAttribute('aria-selected', 'true');
    expect(router.state.location.search).toBe('?tab=ssh');
    const rows = tableRows('SSH known hosts');
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent(/SHA256:/);
  });

  it('shows empty states with guidance', async () => {
    emptyGet('*/api/v1/certificates');
    await renderSettings('/settings/certs?tab=ssh', 'Certificates and known hosts');
    expect(await screen.findByRole('heading', { name: 'No SSH known hosts' })).toBeInTheDocument();
    expect(screen.getByText(/only reachable once their host keys are trusted/)).toBeInTheDocument();
  });

  it('reports load errors', async () => {
    failGet('*/api/v1/certificates');
    await renderSettings('/settings/certs', 'Certificates and known hosts');
    expect(await screen.findByRole('alert')).toHaveTextContent('Server error');
  });

  it('validates and adds a TLS certificate', async () => {
    const user = userEvent.setup();
    const posted: RepositoryCertificateList[] = [];
    server.use(
      http.post('*/api/v1/certificates', async ({ request }) => {
        const body = (await request.json()) as RepositoryCertificateList;
        posted.push(body);
        return HttpResponse.json(body);
      }),
    );
    await renderSettings('/settings/certs', 'Certificates and known hosts');
    await screen.findByRole('table', { name: 'TLS certificates' });
    await user.click(screen.getByRole('button', { name: 'Add TLS certificate' }));
    const dialog = await findDialog('Add TLS certificate');
    await user.type(within(dialog).getByLabelText(/Repository server name/), 'https://git.internal.test/');
    await user.type(within(dialog).getByLabelText(/Certificate \(PEM\)/), '-----BEGIN PRIVATE KEY-----');
    await user.click(within(dialog).getByRole('button', { name: 'Add certificate' }));
    const summary = within(dialog).getByRole('alert');
    expect(summary).toHaveTextContent('Enter only the host name');
    expect(summary).toHaveTextContent('looks like a private key');
    expect(posted).toHaveLength(0);

    await user.clear(within(dialog).getByLabelText(/Repository server name/));
    await user.type(within(dialog).getByLabelText(/Repository server name/), 'git.internal.test');
    await user.clear(within(dialog).getByLabelText(/Certificate \(PEM\)/));
    await user.click(within(dialog).getByLabelText(/Certificate \(PEM\)/));
    await user.paste(PEM);
    expect(within(dialog).getByLabelText(/Certificate \(PEM\)/)).toHaveAccessibleDescription('1 certificate detected.');
    await user.click(within(dialog).getByRole('button', { name: 'Add certificate' }));
    await expectToast('TLS certificate added');
    expect(posted[0]?.items[0]).toEqual({ serverName: 'git.internal.test', certType: 'https', certData: PEM });
  });

  it('parses known hosts, previews fingerprints and adds one entry per host', async () => {
    const user = userEvent.setup();
    const posted: RepositoryCertificateList[] = [];
    server.use(
      http.post('*/api/v1/certificates', async ({ request }) => {
        const body = (await request.json()) as RepositoryCertificateList;
        posted.push(body);
        return HttpResponse.json(body);
      }),
    );
    await renderSettings('/settings/certs?tab=ssh', 'Certificates and known hosts');
    await screen.findByRole('table', { name: 'SSH known hosts' });
    await user.click(screen.getByRole('button', { name: 'Add known hosts' }));
    const dialog = await findDialog('Add SSH known hosts');
    const field = within(dialog).getByLabelText(/Known hosts data/);
    await user.click(field);
    await user.paste(`# scanned\nbitbucket.example.test,10.0.0.5 ssh-ed25519 ${ED25519}\n|1|hashed ssh-rsa AAAA`);
    expect(within(dialog).getByRole('region', { name: 'Entries to add' })).toHaveTextContent('bitbucket.example.test, 10.0.0.5 · ssh-ed25519');
    expect(await within(dialog).findByText(/SHA256:/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Add known hosts' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Line 3: Hashed host names are not supported');
    await user.clear(field);
    await user.click(field);
    await user.paste(`bitbucket.example.test,10.0.0.5 ssh-ed25519 ${ED25519}`);
    await user.click(within(dialog).getByRole('button', { name: 'Add known hosts' }));
    await expectToast('2 known host entries added');
    expect(posted[0]?.items.map((i) => i.serverName)).toEqual(['bitbucket.example.test', '10.0.0.5']);
    expect(posted[0]?.items[0]).toMatchObject({ certType: 'ssh', certSubType: 'ssh-ed25519', certData: ED25519 });
  });

  it('removes an entry after confirmation', async () => {
    const user = userEvent.setup();
    const deleted: URLSearchParams[] = [];
    server.use(
      http.delete('*/api/v1/certificates', ({ request }) => {
        deleted.push(new URL(request.url).searchParams);
        return HttpResponse.json({ items: [] });
      }),
    );
    await renderSettings('/settings/certs?tab=ssh', 'Certificates and known hosts');
    await screen.findByRole('table', { name: 'SSH known hosts' });
    await user.click(screen.getByRole('button', { name: 'Remove ssh-ed25519 entry for git.example.test' }));
    const dialog = await findDialog('Remove known host entry?');
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    await expectToast('Entry removed');
    await waitFor(() => expect(deleted).toHaveLength(1));
    expect(Object.fromEntries(deleted[0] ?? [])).toEqual({ hostNamePattern: 'git.example.test', certType: 'ssh', certSubType: 'ssh-ed25519' });
  });
});
