import { expect, test, type Page } from '@playwright/test';

/*
 * Applications list and details journeys against a running Patrol UI (not run by `pnpm test`).
 *
 *   PATROL_UI_URL=https://localhost:8080/ PATROL_E2E_USERNAME=… PATROL_E2E_PASSWORD=… \
 *   PATROL_E2E_APP=guestbook [PATROL_E2E_APP_NAMESPACE=patrol] pnpm test:e2e e2e/apps-core.spec.ts
 *
 * PATROL_E2E_APP names a disposable application in the test environment (kind clusters
 * patrol-*), e.g. the guestbook example, with manual sync and at least one history entry for the
 * rollback journey. Credentials come from the disposable environment, never a real account.
 * Journeys that change the application (sync, rollback, edit) use dry runs or restore what they
 * change.
 */

const username = process.env.PATROL_E2E_USERNAME;
const password = process.env.PATROL_E2E_PASSWORD;
const appName = process.env.PATROL_E2E_APP ?? 'guestbook';
const appNamespace = process.env.PATROL_E2E_APP_NAMESPACE ?? '';

test.skip(!username || !password, 'PATROL_E2E_USERNAME/PATROL_E2E_PASSWORD not set');

async function login(page: Page) {
  await page.goto('login');
  await page.getByLabel('Username').fill(username ?? '');
  await page.getByLabel('Password').fill(password ?? '');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Applications', level: 1 })).toBeVisible();
}

function detailsPath(search = ''): string {
  const base = appNamespace ? `applications/${appNamespace}/${appName}` : `applications/${appName}`;
  return search ? `${base}?${search}` : base;
}

test.describe('applications list', () => {
  test.beforeEach(async ({ page }) => login(page));

  test('searches, filters through the URL and switches views', async ({ page }) => {
    const search = page.getByRole('searchbox', { name: 'Filter applications by name or namespace' });
    await page.keyboard.press('/');
    await expect(search).toBeFocused();
    await search.fill(appName);
    await expect(page).toHaveURL(new RegExp(`search=${appName}`));
    await expect(page.getByRole('article', { name: appName })).toBeVisible();

    await page.getByRole('button', { name: 'List' }).click();
    await expect(page).toHaveURL(/view=list/);
    await expect(page.getByRole('table', { name: 'Applications' })).toBeVisible();
    await page.getByRole('button', { name: 'Summary' }).click();
    await expect(page.getByRole('table', { name: 'Applications by sync status' })).toBeVisible();

    await page.reload();
    await expect(search).toHaveValue(appName);
  });

  test('opens an application with the keyboard from the tiles view', async ({ page }) => {
    await page.goto(`applications?view=tiles&search=${encodeURIComponent(appName)}`);
    await expect(page.getByRole('article', { name: appName })).toBeVisible();
    await page.locator('body').click({ position: { x: 1, y: 1 } });
    await page.keyboard.press('1');
    await page.keyboard.press('Enter');
    await expect(page.getByRole('heading', { level: 1, name: appName })).toBeVisible();
  });

  test('hard-refreshes a selected application and reports the result', async ({ page }) => {
    await page.goto(`applications?search=${encodeURIComponent(appName)}`);
    await page.getByRole('checkbox', { name: `Select ${appName}` }).check();
    await page.getByRole('button', { name: 'Refresh selected' }).click();
    const dialog = page.getByRole('dialog', { name: 'Refresh 1 application' });
    await dialog.getByRole('radio', { name: /Hard/ }).check();
    await dialog.getByRole('button', { name: 'Refresh 1 application' }).click();
    await expect(dialog.getByText('Finished: 1 succeeded, 0 failed.')).toBeVisible({ timeout: 30_000 });
  });
});

test.describe('application details', () => {
  test.beforeEach(async ({ page }) => login(page));

  test('shows status families and runs a dry-run sync, then follows its operation', async ({ page }) => {
    await page.goto(detailsPath());
    await expect(page.getByRole('heading', { level: 1, name: appName })).toBeVisible();
    await expect(page.getByRole('region', { name: 'App health' })).toBeVisible();
    await expect(page.getByRole('region', { name: 'Sync status' })).toBeVisible();

    await page.getByRole('button', { name: 'Sync', exact: true }).click();
    const drawer = page.getByRole('dialog', { name: `Sync ${appName}` });
    await drawer.getByRole('checkbox', { name: /^Dry run/ }).check();
    await drawer.getByRole('button', { name: 'Start dry run' }).click();
    await page.getByRole('link', { name: 'View operation' }).click();
    const operations = page.getByRole('dialog', { name: `Operations of ${appName}` });
    await expect(operations.getByRole('region', { name: 'Operation summary' })).toBeVisible();
    await expect(operations.locator('[data-status-family="operation"]').first()).toContainText(/Succeeded|Running|Pending/, { timeout: 60_000 });
  });

  test('opens history and the rollback confirmation without rolling back', async ({ page }) => {
    await page.goto(detailsPath('panel=history'));
    const drawer = page.getByRole('dialog', { name: `History of ${appName}` });
    await expect(drawer).toBeVisible();
    const rollback = drawer.getByRole('button', { name: /^(Roll back|Redeploy) to deployment/ }).first();
    test.skip((await rollback.count()) === 0, 'the application has no deployment history');
    await rollback.click();
    const dialog = page.getByRole('dialog', { name: /to deployment \d+\?$/ });
    await expect(dialog.getByRole('checkbox', { name: /^Dry run/ })).toBeVisible();
    await dialog.getByRole('button', { name: 'Cancel' }).click();
  });

  test('edits a label in the summary and restores it', async ({ page }) => {
    await page.goto(detailsPath('panel=details'));
    const drawer = page.getByRole('dialog', { name: `${appName} details` });
    await drawer.getByRole('button', { name: 'Edit summary' }).click();
    await drawer.getByRole('button', { name: 'Add label' }).click();
    const keys = drawer.getByRole('textbox', { name: /^Key of label \d+$/ });
    const index = await keys.count();
    await drawer.getByRole('textbox', { name: `Key of label ${index}` }).fill('e2e.patrol.example/probe');
    await drawer.getByRole('textbox', { name: `Value of label ${index}` }).fill('yes');
    await drawer.getByRole('button', { name: 'Save summary' }).click();
    await expect(page.getByText('Summary saved')).toBeVisible();

    await drawer.getByRole('button', { name: 'Edit summary' }).click();
    const count = await drawer.getByRole('textbox', { name: /^Key of label \d+$/ }).count();
    for (let i = 1; i <= count; i += 1) {
      const key = drawer.getByRole('textbox', { name: `Key of label ${i}` });
      if ((await key.inputValue()) === 'e2e.patrol.example/probe') {
        await drawer.getByRole('button', { name: `Remove label ${i}` }).click();
        break;
      }
    }
    await drawer.getByRole('button', { name: 'Save summary' }).click();
    await expect(page.getByText('Summary saved').last()).toBeVisible();
  });

  test('starts a selective sync from a resource drawer without syncing', async ({ page }) => {
    await page.goto(detailsPath('view=list'));
    const table = page.getByRole('table', { name: /^Resources \(\d+\)$/ });
    await expect(table).toBeVisible();
    await table.getByRole('row').nth(1).press('Enter');
    const resource = page.getByRole('dialog');
    await resource.getByRole('button', { name: 'Sync', exact: true }).click();
    await expect(page).toHaveURL(/panel=sync/);
    await expect(page).toHaveURL(/resources=/);
    const drawer = page.getByRole('dialog', { name: `Sync ${appName}` });
    await expect(drawer.getByText(/Selective sync of 1 of \d+ resources/)).toBeVisible();
    await drawer.getByRole('button', { name: 'Cancel' }).click();
  });

  test('opens pod logs full screen and returns to the resource', async ({ page }) => {
    await page.goto(detailsPath('view=pods'));
    const pods = page.getByRole('table', { name: /^Pods on / }).first();
    test.skip((await pods.count()) === 0, 'the application has no pods');
    await pods.getByRole('row').nth(1).press('Enter');
    const resource = page.getByRole('dialog');
    await resource.getByRole('tab', { name: 'Logs' }).click();
    await resource.getByRole('link', { name: 'Open full screen' }).click();
    await expect(page).toHaveURL(/\/logs\?podName=/);
    await expect(page.getByRole('heading', { level: 1, name: /^Logs of Pod / })).toBeVisible();
    await expect(page.getByRole('log', { name: /^Logs of Pod / })).toBeVisible();
    await page.getByRole('link', { name: 'Back to resource' }).click();
    await expect(page).toHaveURL(/node=/);
    await expect(page.getByRole('dialog').getByRole('tab', { name: 'Logs' })).toHaveAttribute('aria-selected', 'true');
  });

  test('renders at 360 px without horizontal scrolling', async ({ page }) => {
    await page.setViewportSize({ width: 360, height: 780 });
    await page.goto(detailsPath());
    await expect(page.getByRole('heading', { level: 1, name: appName })).toBeVisible();
    const overflow = await page.evaluate<number>('document.documentElement.scrollWidth - document.documentElement.clientWidth');
    expect(overflow).toBeLessThanOrEqual(0);
  });
});
