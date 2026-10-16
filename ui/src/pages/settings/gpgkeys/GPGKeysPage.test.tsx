import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { emptyGet, expectToast, failGet, findDialog, renderSettings, tableRows } from '../testUtils';

const KEY = '-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQENBFakeKeyForTests\n-----END PGP PUBLIC KEY BLOCK-----';

describe('Settings → GnuPG keys', () => {
  it('lists keys with ID, fingerprint, owner and trust', async () => {
    await renderSettings('/settings/gpgkeys', 'GnuPG keys');
    await screen.findByRole('table', { name: 'GnuPG public keys' });
    const rows = tableRows('GnuPG public keys');
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent('4AEE18F83AFDEB23');
    expect(rows[0]).toHaveTextContent('Release Bot');
    expect(rows[0]).toHaveTextContent('ultimate');
  });

  it('shows an empty state that explains signature verification', async () => {
    emptyGet('*/api/v1/gpgkeys');
    await renderSettings('/settings/gpgkeys', 'GnuPG keys');
    expect(await screen.findByRole('heading', { name: 'No GnuPG keys' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'projects' })).toHaveAttribute('href', '/settings/projects');
  });

  it('reports load errors', async () => {
    failGet('*/api/v1/gpgkeys', 403, 'permission_denied', 'permission denied: gpgkeys, get');
    await renderSettings('/settings/gpgkeys', 'GnuPG keys');
    expect(await screen.findByRole('alert')).toHaveTextContent('You do not have access');
  });

  it('rejects private keys and adds an armored public key', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/gpgkeys', 'GnuPG keys');
    await screen.findByRole('table', { name: 'GnuPG public keys' });
    await user.click(screen.getByRole('button', { name: 'Add GnuPG key' }));
    const dialog = await findDialog('Add GnuPG public key');
    const field = within(dialog).getByLabelText(/ASCII-armored public key/);
    await user.click(field);
    await user.paste('-----BEGIN PGP PRIVATE KEY BLOCK-----');
    await user.click(within(dialog).getByRole('button', { name: 'Add key' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('This is a private key');
    await user.clear(field);
    await user.paste(KEY);
    await user.click(within(dialog).getByRole('button', { name: 'Add key' }));
    await expectToast('GnuPG key added');
    await waitFor(() => expect(tableRows('GnuPG public keys')).toHaveLength(3));
  });

  it('removes a key after confirmation', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/gpgkeys', 'GnuPG keys');
    await screen.findByRole('table', { name: 'GnuPG public keys' });
    await user.click(screen.getByRole('button', { name: 'Remove key 7C1F2A6B9D0E3F44' }));
    const dialog = await findDialog('Remove GnuPG key?');
    expect(dialog).toHaveTextContent('will fail verification');
    await user.click(within(dialog).getByRole('button', { name: 'Remove key' }));
    await expectToast('GnuPG key removed');
    await waitFor(() => expect(tableRows('GnuPG public keys')).toHaveLength(1));
  });
});
