import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';

/*
 * Settings journeys against a running Patrol UI (PATROL_UI_URL). Not run by `pnpm test`.
 *
 * Required: PATROL_E2E_USERNAME / PATROL_E2E_PASSWORD (an admin account of the disposable test
 * environment, never a real one). Optional:
 *   PATROL_E2E_REPO_URL       public HTTPS Git repository reachable from the cluster
 *   PATROL_E2E_TOKEN_ACCOUNT  local account with the apiKey capability
 *
 * Every object created here carries a unique "e2e-<suffix>" name and is deleted at the end of
 * its test.
 */

const username = process.env.PATROL_E2E_USERNAME;
const password = process.env.PATROL_E2E_PASSWORD;
const suffix = `${Date.now().toString(36)}${randomBytes(2).toString('hex')}`;

test.skip(!username || !password, 'PATROL_E2E_USERNAME/PATROL_E2E_PASSWORD not set');

async function login(page: Page) {
  await page.goto('login');
  await page.getByLabel('Username').fill(username ?? '');
  await page.getByLabel('Password').fill(password ?? '');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Applications', level: 1 })).toBeVisible();
}

/** A syntactically valid ssh-ed25519 public key blob (random key, never used to connect). */
function randomEd25519Key(): string {
  const type = Buffer.from('ssh-ed25519');
  const len = (n: number) => {
    const b = Buffer.alloc(4);
    b.writeUInt32BE(n);
    return b;
  };
  return Buffer.concat([len(type.length), type, len(32), randomBytes(32)]).toString('base64');
}

test.beforeEach(async ({ page }) => {
  await login(page);
});

test('settings index links every section', async ({ page }) => {
  await page.goto('settings');
  await expect(page.getByRole('heading', { name: 'Settings', level: 1 })).toBeVisible();
  for (const [link, heading] of [
    ['Repositories', 'Repositories'],
    ['Certificates and known hosts', 'Certificates and known hosts'],
    ['GnuPG keys', 'GnuPG keys'],
    ['Clusters', 'Clusters'],
    ['Projects', 'Projects'],
    ['Accounts', 'Accounts'],
    ['SCM providers', 'SCM providers'],
    ['Appearance', 'Appearance'],
  ] as const) {
    await page.goto('settings');
    await page.getByRole('main').getByRole('link', { name: new RegExp(`^${link}`) }).click();
    await expect(page.getByRole('heading', { name: heading, level: 1 })).toBeVisible();
  }
});

test('project lifecycle: create, destination, role token, sync window, delete', async ({ page }) => {
  const name = `e2e-${suffix}`;
  await page.goto('settings/projects');
  await page.getByRole('button', { name: 'New project' }).click();
  const create = page.getByRole('dialog', { name: 'New project' });
  await create.getByLabel(/Name/).fill(name);
  await create.getByRole('button', { name: 'Create project' }).click();
  await expect(page.getByRole('heading', { name, level: 1 })).toBeVisible();

  await page.getByRole('button', { name: 'Edit destinations' }).click();
  const dest = page.getByRole('dialog', { name: 'Edit destinations' });
  await dest.getByRole('button', { name: 'Add destination' }).click();
  await dest.getByRole('textbox', { name: 'Server, destination 1' }).fill('https://kubernetes.default.svc');
  await dest.getByRole('textbox', { name: 'Namespace, destination 1' }).fill(name);
  await dest.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByRole('table', { name: 'Destinations' })).toContainText(name);

  await page.getByRole('tab', { name: 'Roles' }).click();
  await page.getByRole('button', { name: 'Add role' }).click();
  const role = page.getByRole('dialog', { name: 'Add role' });
  await role.getByLabel(/Role name/).fill('ci');
  await role.getByRole('button', { name: 'Insert read-only policy' }).click();
  await role.getByRole('button', { name: 'Create role' }).click();
  await page.getByRole('button', { name: 'Edit role ci' }).click();
  const roleDrawer = page.getByRole('dialog', { name: 'Role ci' });
  await roleDrawer.getByLabel('Expires in').fill('1h');
  await roleDrawer.getByRole('button', { name: 'Create token' }).click();
  const token = page.getByRole('dialog', { name: 'Token for role ci' });
  await expect(token.getByRole('textbox', { name: 'Token' })).not.toHaveValue('');
  await token.getByRole('button', { name: 'Done' }).click();
  await expect(page.getByRole('table', { name: 'Tokens of role ci' }).getByRole('row')).toHaveCount(2);
  await roleDrawer.getByRole('button', { name: 'Cancel' }).click();

  await page.getByRole('tab', { name: 'Sync windows' }).click();
  await page.getByRole('button', { name: 'Add sync window' }).click();
  const win = page.getByRole('dialog', { name: 'Add sync window' });
  await win.getByLabel(/Schedule/).fill('0 22 * * *');
  await win.getByLabel(/Duration/).fill('1h');
  await win.getByRole('button', { name: 'Add application' }).click();
  await win.getByRole('textbox', { name: 'Application name or pattern 1' }).fill('*');
  await expect(win.getByRole('list', { name: 'Next windows' }).getByRole('listitem')).toHaveCount(3);
  await win.getByRole('button', { name: 'Add window' }).click();
  await expect(page.getByRole('table', { name: 'Sync windows' })).toContainText('0 22 * * *');

  await page.getByRole('button', { name: 'Delete project' }).click();
  const del = page.getByRole('dialog', { name: 'Delete project?' });
  await del.getByRole('textbox').fill(name);
  await del.getByRole('button', { name: 'Delete project' }).click();
  await expect(page).toHaveURL(/settings\/projects$/);
  await expect(page.getByRole('link', { name })).toHaveCount(0);
});

test('repository: connect after a connection test, then disconnect', async ({ page }) => {
  const url = process.env.PATROL_E2E_REPO_URL;
  test.skip(!url, 'PATROL_E2E_REPO_URL not set');
  await page.goto('settings/repos');
  await page.getByRole('button', { name: 'Connect repository' }).first().click();
  const dialog = page.getByRole('dialog', { name: 'Connect repository' });
  await dialog.getByRole('radio', { name: 'None' }).check();
  await dialog.getByLabel(/Repository URL/).fill(url ?? '');
  await dialog.getByRole('button', { name: 'Test connection' }).click();
  await expect(dialog.getByText('Connection successful')).toBeVisible();
  await dialog.getByRole('button', { name: 'Connect' }).click();
  const row = page.getByRole('row').filter({ hasText: url ?? '' });
  await expect(row).toContainText('Successful');
  await row.getByRole('button', { name: `Actions for ${url ?? ''}` }).click();
  await page.getByRole('menuitem', { name: 'Disconnect' }).click();
  await page.getByRole('dialog', { name: 'Disconnect repository?' }).getByRole('button', { name: 'Disconnect' }).click();
  await expect(page.getByRole('row').filter({ hasText: url ?? '' })).toHaveCount(0);
});

test('known hosts: add an entry with its fingerprint, then remove it', async ({ page }) => {
  const host = `e2e-${suffix}.example.test`;
  await page.goto('settings/certs?tab=ssh');
  await page.getByRole('button', { name: 'Add known hosts' }).first().click();
  const dialog = page.getByRole('dialog', { name: 'Add SSH known hosts' });
  await dialog.getByLabel(/Known hosts data/).fill(`${host} ssh-ed25519 ${randomEd25519Key()}`);
  await expect(dialog.getByRole('region', { name: 'Entries to add' })).toContainText('SHA256:');
  await dialog.getByRole('button', { name: 'Add known hosts' }).click();
  const row = page.getByRole('row').filter({ hasText: host });
  await expect(row).toContainText('ssh-ed25519');
  await row.getByRole('button', { name: `Remove ssh-ed25519 entry for ${host}` }).click();
  await page.getByRole('dialog', { name: 'Remove known host entry?' }).getByRole('button', { name: 'Remove' }).click();
  await expect(page.getByRole('row').filter({ hasText: host })).toHaveCount(0);
});

test('clusters: details and cache invalidation', async ({ page }) => {
  await page.goto('settings/clusters');
  const table = page.getByRole('table', { name: 'Clusters' });
  await expect(table).toBeVisible();
  await table.getByRole('link').first().click();
  await expect(page.getByRole('heading', { name: 'Connection' })).toBeVisible();
  await page.getByRole('button', { name: 'Invalidate cache' }).click();
  await expect(page.getByRole('region', { name: 'Notifications' })).toContainText('Cache invalidated');
});

test('SCM provider referencing a Secret by name, then deleted', async ({ page }) => {
  const name = `e2e-${suffix}`;
  await page.goto('settings/scmproviders');
  await page.getByRole('button', { name: 'New SCM provider' }).first().click();
  const dialog = page.getByRole('dialog', { name: 'New SCM provider' });
  await dialog.getByRole('textbox', { name: 'Name' }).fill(name);
  await dialog.getByRole('group', { name: 'Token' }).getByRole('textbox', { name: 'Secret name' }).fill(`${name}-token`);
  await dialog.getByRole('button', { name: 'Create provider' }).click();
  await page.getByRole('button', { name: `Details of ${name}` }).click();
  await page.getByRole('dialog', { name }).getByRole('button', { name: 'Delete' }).click();
  const del = page.getByRole('dialog', { name: 'Delete SCM provider?' });
  await del.getByRole('textbox').fill(name);
  await del.getByRole('button', { name: 'Delete provider' }).click();
  await expect(page.getByRole('button', { name: `Details of ${name}` })).toHaveCount(0);
});

test('account token is shown once and can be deleted', async ({ page }) => {
  const account = process.env.PATROL_E2E_TOKEN_ACCOUNT;
  test.skip(!account, 'PATROL_E2E_TOKEN_ACCOUNT not set');
  const id = `e2e-${suffix}`;
  await page.goto(`settings/accounts/${encodeURIComponent(account ?? '')}`);
  await page.getByLabel('Token ID').fill(id);
  await page.getByLabel('Expires in').fill('1h');
  await page.getByRole('button', { name: 'Generate token' }).click();
  const dialog = page.getByRole('dialog', { name: `New token for ${account ?? ''}` });
  await expect(dialog.getByRole('textbox', { name: 'Token' })).not.toHaveValue('');
  await dialog.getByRole('button', { name: 'Done' }).click();
  await page.getByRole('button', { name: `Delete token ${id}` }).click();
  await page.getByRole('dialog', { name: 'Delete token?' }).getByRole('button', { name: 'Delete token' }).click();
  await expect(page.getByRole('button', { name: `Delete token ${id}` })).toHaveCount(0);
});

test('appearance: theme choice persists across reloads', async ({ page }) => {
  await page.goto('settings/appearance');
  await page.getByRole('radio', { name: /Dark/ }).check();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  await page.getByRole('radio', { name: /System/ }).check();
});
