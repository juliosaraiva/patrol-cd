import { expect, test } from '@playwright/test';

// Smoke test against a running Patrol UI. Requires PATROL_UI_URL (see playwright.config.ts).
// Optional PATROL_E2E_USERNAME / PATROL_E2E_PASSWORD enable the authenticated half; both come
// from the disposable test environment, never from a real account.

test('unauthenticated visit lands on the login page with a return URL', async ({ page }) => {
  await page.goto('applications');
  await expect(page).toHaveURL(/login\?return_url=/);
  await expect(page.getByRole('heading', { name: 'Sign in to Patrol CD' })).toBeVisible();
});

test('local login reaches the applications list', async ({ page }) => {
  const username = process.env.PATROL_E2E_USERNAME;
  const password = process.env.PATROL_E2E_PASSWORD;
  test.skip(!username || !password, 'PATROL_E2E_USERNAME/PATROL_E2E_PASSWORD not set');

  await page.goto('login');
  await page.getByLabel('Username').fill(username ?? '');
  await page.getByLabel('Password').fill(password ?? '');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Applications', level: 1 })).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible();
});
