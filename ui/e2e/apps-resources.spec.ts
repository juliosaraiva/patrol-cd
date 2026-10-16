import { expect, test, type Page } from '@playwright/test';

/*
 * Application resource views against a running Patrol server (not run by `pnpm test`).
 *
 *   PATROL_UI_URL=https://localhost:8080/ PATROL_E2E_USERNAME=… PATROL_E2E_PASSWORD=… \
 *   PATROL_E2E_APP=guestbook pnpm test:e2e e2e/apps-resources.spec.ts
 *
 * PATROL_E2E_APP names a synced application that owns at least one Deployment (and therefore
 * Pods), e.g. the guestbook example deployed into the disposable kind environment. Optional:
 * PATROL_E2E_APP_NAMESPACE when the application lives outside the control-plane namespace.
 */

const username = process.env.PATROL_E2E_USERNAME;
const password = process.env.PATROL_E2E_PASSWORD;
const appName = process.env.PATROL_E2E_APP;
const appNamespace = process.env.PATROL_E2E_APP_NAMESPACE;

test.skip(!username || !password || !appName, 'PATROL_E2E_USERNAME, PATROL_E2E_PASSWORD and PATROL_E2E_APP are required');

const appPath = () => `applications/${appNamespace ? `${encodeURIComponent(appNamespace)}/` : ''}${encodeURIComponent(appName ?? '')}`;

async function signIn(page: Page) {
  await page.goto('login');
  await page.getByLabel('Username').fill(username ?? '');
  await page.getByLabel('Password').fill(password ?? '');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Applications', level: 1 })).toBeVisible();
}

test.beforeEach(async ({ page }) => {
  await signIn(page);
});

test('tree view: keyboard navigation opens the resource details drawer', async ({ page }) => {
  await page.goto(appPath());
  const tree = page.getByRole('tree', { name: `Resource tree of ${appName ?? ''}` });
  await expect(tree).toBeVisible();
  const root = tree.getByRole('treeitem', { name: new RegExp(`^Application ${appName ?? ''}`) });
  await root.focus();
  await page.keyboard.press('ArrowRight');
  await page.keyboard.press('Enter');
  const drawer = page.getByRole('dialog');
  await expect(drawer).toBeVisible();
  await expect(drawer.getByRole('tab', { name: 'Summary' })).toHaveAttribute('aria-selected', 'true');
  await expect(page).toHaveURL(/[?&]node=/);
  await page.keyboard.press('Escape');
  await expect(drawer).toBeHidden();
  await expect(page).not.toHaveURL(/[?&]node=/);
});

test('zoom controls and outline representation', async ({ page }) => {
  await page.goto(appPath());
  await expect(page.getByRole('tree', { name: `Resource tree of ${appName ?? ''}` })).toBeVisible();
  const readout = page.getByRole('button', { name: /^Zoom \d+%, reset to 100%$/ });
  await page.getByRole('button', { name: 'Zoom out' }).click();
  await expect(readout).toHaveText('80%');
  await page.getByRole('button', { name: 'Fit to screen' }).click();
  await page.getByRole('switch', { name: 'Show as outline' }).click();
  const outline = page.getByRole('tree', { name: `Resource tree of ${appName ?? ''}` });
  await expect(outline.getByRole('treeitem').first()).toContainText(appName ?? '');
});

test('list view with URL filters and a deep link to a Deployment', async ({ page }) => {
  await page.goto(`${appPath()}?view=list&kind=Deployment`);
  const table = page.getByRole('table', { name: /^Resources \(\d+\)$/ });
  await expect(table).toBeVisible();
  await expect(page.getByRole('list', { name: 'Active resource filters' })).toContainText('Kind: Deployment');
  await table.getByRole('row').nth(1).press('Enter');
  const drawer = page.getByRole('dialog');
  await expect(drawer).toContainText('Deployment');
  await drawer.getByRole('tab', { name: 'Live manifest' }).click();
  await expect(drawer.getByLabel(/^Live manifest of Deployment /)).toContainText('kind: Deployment');
  await expect(page).toHaveURL(/[?&]tab=live/);
  // Reloading the deep link restores the drawer and tab.
  await page.reload();
  await expect(page.getByRole('dialog').getByRole('tab', { name: 'Live manifest' })).toHaveAttribute('aria-selected', 'true');
});

test('pods view groups by node and the logs tab streams lines', async ({ page }) => {
  await page.goto(`${appPath()}?view=pods`);
  await expect(page.getByRole('radiogroup', { name: 'Group pods by' }).or(page.getByRole('group', { name: 'Group pods by' }))).toBeVisible();
  const podTable = page.getByRole('table', { name: /^Pods on / }).first();
  await expect(podTable).toBeVisible();
  await podTable.getByRole('row').nth(1).press('Enter');
  const drawer = page.getByRole('dialog');
  await drawer.getByRole('tab', { name: 'Logs' }).click();
  const output = drawer.getByRole('log');
  await expect(output).toBeVisible();
  await expect(drawer.getByRole('status').filter({ hasText: /Streaming|Stream ended|Connecting/ })).toBeVisible();
});

test('network view shows the traffic entry points', async ({ page }) => {
  await page.goto(`${appPath()}?view=network`);
  const network = page.getByRole('tree', { name: `Network of ${appName ?? ''}` });
  const empty = page.getByRole('heading', { name: 'No networking resources' });
  await expect(network.or(empty)).toBeVisible();
  if (await network.isVisible()) {
    await expect(network.getByRole('treeitem', { name: /traffic$/ }).first()).toBeVisible();
  }
});
