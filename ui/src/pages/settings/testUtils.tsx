import { screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { expect, vi } from 'vitest';
import { settingsFixture } from '../../mocks/fixtures';
import { problem } from '../../mocks/handlers';
import { resetSettingsMockStore } from '../../mocks/handlers/settings';
import { server } from '../../mocks/server';
import { renderApp } from '../../test/render';

// Settings flows type into many fields; under a fully parallel suite they need more than the
// default 10 s. Applies to every test file that imports these helpers.
vi.setConfig({ testTimeout: 30_000 });

/** Renders the app at a Settings path with a fresh mock store and waits for the page heading. */
export async function renderSettings(path: string, heading: string | RegExp, setup?: () => void) {
  resetSettingsMockStore();
  setup?.();
  const result = renderApp(path);
  await screen.findByRole('heading', { level: 1, name: heading });
  return result;
}

/** Makes GET `route` fail with a problem response. */
export function failGet(route: string, status = 500, code: Parameters<typeof problem>[1] = 'internal', detail = 'backend exploded') {
  server.use(http.get(route, () => problem(status, code, detail)));
}

/** Makes GET `route` return an empty list. */
export function emptyGet(route: string) {
  server.use(http.get(route, () => HttpResponse.json({ items: [], metadata: { resourceVersion: '1' } })));
}

/** Enables the source hydrator in the settings response. */
export function withHydrator() {
  server.use(http.get('*/api/v1/settings', () => HttpResponse.json({ ...settingsFixture, hydratorEnabled: true })));
}

/** The open modal dialog with the given name. */
export async function findDialog(name: string | RegExp) {
  return screen.findByRole('dialog', { name });
}

/** Rows of a table found by its caption (header row excluded). */
export function tableRows(caption: string | RegExp) {
  const table = screen.getByRole('table', { name: caption });
  const rows = within(table).getAllByRole('row');
  return rows.slice(1);
}

export async function expectToast(title: string | RegExp) {
  const region = screen.getByRole('region', { name: 'Notifications' });
  expect(await within(region).findByText(title)).toBeInTheDocument();
}
