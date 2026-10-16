// Playwright journeys for the create-appsets-ext workstream: application creation wizard,
// ApplicationSets, help, notifications catalog and UI extensions. Runs against PATROL_UI_URL
// (see playwright.config.ts); not part of `pnpm test`.
import { expect, test, type Page } from '@playwright/test';

/**
 * Shared steps.
 * Credentials come from the disposable test environment (PATROL_E2E_USERNAME/PASSWORD).
 */
function requireCredentials(): { username: string; password: string } {
  const username = process.env.PATROL_E2E_USERNAME ?? '';
  const password = process.env.PATROL_E2E_PASSWORD ?? '';
  test.skip(username === '' || password === '', 'PATROL_E2E_USERNAME/PATROL_E2E_PASSWORD not set');
  return { username, password };
}

async function login(page: Page): Promise<void> {
  const { username, password } = requireCredentials();
  await page.goto('login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password').fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Applications', level: 1 })).toBeVisible();
}

/** Unique, DNS-safe suffix so parallel runs never collide. */
function uniqueName(prefix: string): string {
  return `${prefix}-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e4).toString(36)}`;
}

/** Optional fixtures of the e2e environment; tests that need them skip when unset. */
const env = {
  gitRepo: process.env.PATROL_E2E_GIT_REPO ?? '',
  gitPath: process.env.PATROL_E2E_GIT_PATH ?? 'guestbook',
  helmRepo: process.env.PATROL_E2E_HELM_REPO ?? '',
  helmChart: process.env.PATROL_E2E_HELM_CHART ?? '',
  helmVersion: process.env.PATROL_E2E_HELM_VERSION ?? '',
  appSet: process.env.PATROL_E2E_APPSET ?? '',
  previewAppSet: process.env.PATROL_E2E_PR_APPSET ?? '',
  extensionPath: process.env.PATROL_E2E_EXTENSION_PATH ?? '',
};

// Application creation wizard (API-091) against a running server. Needs a registered Git
// repository (PATROL_E2E_GIT_REPO, path PATROL_E2E_GIT_PATH) and, for the Helm journey, a Helm
// repository with a chart (PATROL_E2E_HELM_REPO/CHART/VERSION). Created applications are deleted.

test.describe('create application', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('creates a Git directory application and opens it', async ({ page }) => {
    test.skip(env.gitRepo === '', 'PATROL_E2E_GIT_REPO not set');
    const name = uniqueName('e2e-git');
    await page.goto('applications?new=1');
    const panel = page.getByRole('dialog', { name: 'New application' });
    await panel.getByLabel('Application name').fill(name);
    await panel.getByLabel('Repository URL').fill(env.gitRepo);
    await panel.getByLabel('Path').fill(env.gitPath);
    await panel.getByLabel('Path').blur();
    await expect(panel.getByText(/^Detected: /)).toBeVisible();
    await panel.getByLabel('Cluster URL').fill('https://kubernetes.default.svc');
    await panel.getByRole('textbox', { name: /^Namespace/ }).fill(name);
    await panel.getByRole('checkbox', { name: /Auto-create namespace/ }).check();
    await panel.getByRole('button', { name: 'Create' }).click();
    await expect(page).toHaveURL(new RegExp(`applications/(.+/)?${name}`));
    await expect(page.getByRole('heading', { level: 1, name })).toBeVisible();
    await page.request.delete(`api/v1/applications/${name}?cascade=true`, { headers: { 'X-Requested-With': 'patrol' } });
  });

  test('creates a Helm chart application with a parameter override', async ({ page }) => {
    test.skip(env.helmRepo === '' || env.helmChart === '', 'PATROL_E2E_HELM_REPO/CHART not set');
    const name = uniqueName('e2e-helm');
    await page.goto('applications?new=1');
    const panel = page.getByRole('dialog', { name: 'New application' });
    await panel.getByLabel('Application name').fill(name);
    await panel.getByLabel('Repository URL').fill(env.helmRepo);
    await panel.getByLabel('Chart').fill(env.helmChart);
    await panel.getByLabel('Version').fill(env.helmVersion || '*');
    await panel.getByLabel('Version').blur();
    await expect(panel.getByText('Detected: Helm.')).toBeVisible();
    await panel.getByLabel('Cluster URL').fill('https://kubernetes.default.svc');
    await panel.getByRole('textbox', { name: /^Namespace/ }).fill(name);
    await panel.getByRole('button', { name: 'Create' }).click();
    await expect(page).toHaveURL(new RegExp(`applications/(.+/)?${name}`));
    await page.request.delete(`api/v1/applications/${name}?cascade=true`, { headers: { 'X-Requested-With': 'patrol' } });
  });

  test('round-trips YAML mode and shows server validation errors', async ({ page }) => {
    const name = uniqueName('e2e-yaml');
    await page.goto('applications?new=1');
    const panel = page.getByRole('dialog', { name: 'New application' });
    await panel.getByLabel('Application name').fill(name);
    await panel.getByRole('button', { name: 'Edit as YAML' }).click();
    const yaml = panel.getByLabel(/Application manifest/);
    await expect(yaml).toHaveValue(new RegExp(`name: ${name}`));
    await yaml.fill(
      [
        'apiVersion: cd.patrol.example/v1alpha1',
        'kind: Application',
        'metadata:',
        `  name: ${name}`,
        'spec:',
        '  project: default',
        '  source:',
        '    repoURL: https://git.invalid.example/unknown.git',
        '    path: app',
        '    targetRevision: HEAD',
        '  destination:',
        '    server: https://kubernetes.default.svc',
        `    namespace: ${name}`,
      ].join('\n'),
    );
    await panel.getByRole('button', { name: 'Form' }).click();
    await expect(panel.getByLabel('Repository URL')).toHaveValue('https://git.invalid.example/unknown.git');
    await expect(panel.getByLabel('Path')).toHaveValue('app');
    await panel.getByRole('button', { name: 'Create' }).click();
    await expect(panel.getByRole('alert')).toContainText(/rejected|cannot create|does not allow/);
  });

  test('works at 360px without horizontal scrolling', async ({ page }) => {
    await page.setViewportSize({ width: 360, height: 780 });
    await page.goto('applications?new=1');
    await expect(page.getByRole('dialog', { name: 'New application' })).toBeVisible();
    const overflow = await page.evaluate(() => {
      const root = (globalThis as unknown as { document: { documentElement: { scrollWidth: number; clientWidth: number } } }).document.documentElement;
      return root.scrollWidth - root.clientWidth;
    });
    expect(overflow).toBeLessThanOrEqual(0);
  });
});

// ApplicationSets (API-118, ASET-124..126). PATROL_E2E_APPSET names an existing ApplicationSet
// (namespace/name or name); PATROL_E2E_PR_APPSET one with a pull-request generator.

test.describe('ApplicationSets', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('lists ApplicationSets with URL-synchronized filters and "/" search', async ({ page }) => {
    await page.goto('applicationsets');
    await expect(page.getByRole('heading', { level: 1, name: 'ApplicationSets' })).toBeVisible();
    const table = page.getByRole('table', { name: 'ApplicationSets' });
    const empty = page.getByRole('heading', { name: 'No ApplicationSets yet' });
    await expect(table.or(empty)).toBeVisible();
    test.skip(await empty.isVisible(), 'no ApplicationSets in this environment');
    await page.getByRole('combobox', { name: 'Health' }).selectOption('Healthy');
    await expect(page).toHaveURL(/health=Healthy/);
    await page.getByRole('heading', { level: 1 }).click();
    await page.keyboard.press('/');
    await expect(page.getByRole('searchbox', { name: 'Search ApplicationSets' })).toBeFocused();
  });

  test('shows details, generated applications and conditions, and previews the spec', async ({ page }) => {
    test.skip(env.appSet === '', 'PATROL_E2E_APPSET not set');
    await page.goto(`applicationsets/${env.appSet}`);
    await expect(page.getByRole('region', { name: 'ApplicationSet status' })).toBeVisible();
    await page.getByRole('tab', { name: /Applications/ }).click();
    await expect(page.getByRole('table', { name: 'Generated applications' })).toBeVisible();
    await page.getByRole('tab', { name: /Conditions/ }).click();
    await expect(page.getByRole('list', { name: 'Conditions' }).or(page.getByText(/No conditions reported/))).toBeVisible();
    await page.getByRole('button', { name: 'Generate preview' }).click();
    await expect(page).toHaveURL(/tab=preview/);
    await expect(page.getByRole('heading', { name: 'Preview result' }).or(page.getByRole('alert'))).toBeVisible();
  });

  test('explains the deletion impact without deleting', async ({ page }) => {
    test.skip(env.appSet === '', 'PATROL_E2E_APPSET not set');
    await page.goto(`applicationsets/${env.appSet}`);
    await page.getByRole('button', { name: 'Delete' }).click();
    const dialog = page.getByRole('dialog', { name: /Delete ApplicationSet/ });
    await expect(dialog).toContainText('preserveResourcesOnDeletion');
    await expect(dialog.getByRole('button', { name: 'Delete ApplicationSet' })).toBeDisabled();
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
  });

  test('groups pull-request previews with links to the pull requests', async ({ page }) => {
    test.skip(env.previewAppSet === '', 'PATROL_E2E_PR_APPSET not set');
    await page.goto(`applicationsets/${env.previewAppSet}?tab=previews`);
    await expect(page.getByRole('table', { name: 'Preview environments' })).toBeVisible();
    await expect(page.getByText(/Deleted when the pull request closes|Not cleaned up automatically/)).toBeVisible();
  });
});

// Help page (API-109, API-111) and notifications catalog (NOTIF-101..103).

test.describe('help and notifications', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('help lists CLI downloads and version information', async ({ page }) => {
    await page.goto('help');
    await expect(page.getByRole('heading', { level: 1, name: 'Docs & help' })).toBeVisible();
    const downloads = page.getByRole('list', { name: 'CLI downloads' });
    await expect(downloads.getByRole('link', { name: /from this server/ })).toHaveAttribute('href', 'download/patrol-linux');
    await expect(page.getByRole('heading', { name: 'Version' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Copy version information' })).toBeVisible();
  });

  test('settings show the notification catalog', async ({ page }) => {
    await page.goto('settings/notifications');
    await expect(page.getByRole('heading', { level: 1, name: 'Notifications' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Triggers' })).toBeVisible();
  });

  test('the notification catalog describes the triggers', async ({ page }) => {
    await page.goto('settings/notifications');
    const triggers = page.getByRole('list', { name: 'Triggers' });
    const none = page.getByText(/^No triggers are configured/);
    await expect(triggers.or(none)).toBeVisible();
    test.skip(await none.isVisible(), 'no notification triggers configured in this environment');
    // Each trigger shows its name followed by a description (or "Custom trigger").
    await expect(triggers.getByRole('listitem').first()).toContainText(/\S+\s+\S+/);
    await expect(page.getByText(/configured$/).first()).toBeVisible();
  });
});

// UI extensions host (API-120..126, API-131, API-132). The globals are always checked; the
// system-level page needs an installed test extension registering PATROL_E2E_EXTENSION_PATH.

test.describe('UI extensions', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('exposes the React 19 host globals and the registration API', async ({ page }) => {
    // main.tsx starts the host with the app (initExtensions), so the applications list has them.
    const globals = await page.evaluate(() => {
      const w = globalThis as unknown as Record<string, Record<string, unknown> | undefined>;
      return {
        react: typeof w.React?.version === 'string' ? w.React.version : '',
        jsx: typeof w.ReactJSXRuntime?.jsx,
        portal: typeof w.ReactDOM?.createPortal,
        api: Object.keys(w.extensionsAPI ?? {}).sort(),
        legacy: typeof w.extensions?.resources,
      };
    });
    expect(globals.react.startsWith('19.')).toBe(true);
    expect(globals.jsx).toBe('function');
    expect(globals.portal).toBe('function');
    expect(globals.api).toEqual([
      'registerAppViewExtension',
      'registerResourceExtension',
      'registerStatusPanelExtension',
      'registerSystemLevelExtension',
      'registerTopBarActionMenuExt',
    ]);
    expect(globals.legacy).toBe('object');
  });

  test('a failing bundle does not break the UI', async ({ page }) => {
    const errors: string[] = [];
    page.on('console', (msg) => {
      if (msg.type() === 'error') errors.push(msg.text());
    });
    await page.goto('applications');
    await expect(page.getByRole('heading', { name: 'Applications', level: 1 })).toBeVisible();
    // Any extension load failure is logged with the upstream prefix and isolated.
    for (const e of errors.filter((t) => t.includes('failed to load'))) expect(e).toMatch(/^Extension .+ failed to load:/);
  });

  test('renders a registered system-level extension page from the sidebar', async ({ page }) => {
    test.skip(env.extensionPath === '', 'PATROL_E2E_EXTENSION_PATH not set');
    const path = `/${env.extensionPath.replace(/^\/+|\/+$/g, '')}`;
    await page.goto('applications');
    // The sidebar (in the DOM at every viewport width) links to the page once the bundle registers it.
    await expect(page.locator(`nav[aria-label="Main"] a[href$="${path}"]`).first()).toBeAttached();
    await page.goto(path.slice(1));
    // The catch-all route shows a spinner while bundles load, then the extension page (h1 = its title), not the 404.
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    await expect(page.getByRole('heading', { level: 1, name: 'Page not found' })).toHaveCount(0);
  });
});
