import { defineConfig, devices } from '@playwright/test';

/**
 * Browser tests run against a deployed Patrol UI (kind environment or a local
 * `patrolcd server`). They are not part of `pnpm test`.
 *
 *   PATROL_UI_URL=https://localhost:8080/ pnpm test:e2e
 *
 * PATROL_UI_URL must include the --rootpath if the server uses one (trailing slash included).
 */
const baseURL = process.env.PATROL_UI_URL ?? 'http://localhost:8080/';

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL,
    ignoreHTTPSErrors: process.env.PATROL_UI_INSECURE === '1',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 7'] } },
  ],
});
