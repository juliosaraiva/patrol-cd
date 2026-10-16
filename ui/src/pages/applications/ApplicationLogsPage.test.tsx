import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, http } from 'msw';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { problem } from '../../mocks/handlers';
import { resetAppsCoreMockState } from '../../mocks/handlers/apps-core';
import { server } from '../../mocks/server';
import { renderApp } from '../../test/render';
import { logsTargetFrom } from './ApplicationLogsPage';

// Whole-app tests with the real log viewer from the resource views module (no mock).
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());

/** Records the log stream requests, then lets the regular mock answer. */
function recordLogRequests() {
  const urls: URL[] = [];
  const record = ({ request }: { request: Request }) => {
    urls.push(new URL(request.url));
    return undefined;
  };
  server.use(http.get('*/api/v1/applications/:name/pods/:pod/logs', record), http.get('*/api/v1/applications/:name/logs', record));
  return urls;
}

describe('logsTargetFrom', () => {
  it('reads a pod, a workload, or nothing from the query string', () => {
    expect(logsTargetFrom(new URLSearchParams('podName=web-1'), 'shop')).toEqual({ kind: 'Pod', namespace: 'shop', name: 'web-1' });
    expect(logsTargetFrom(new URLSearchParams('kind=Deployment&group=apps&resourceName=web'), 'shop')).toEqual({
      group: 'apps',
      kind: 'Deployment',
      namespace: 'shop',
      name: 'web',
    });
    expect(logsTargetFrom(new URLSearchParams('kind=Pod&group='), 'shop')).toBeNull();
  });
});

describe('full-screen logs page', () => {
  it("streams a pod's logs in the chosen container with a link back to the resource", async () => {
    const urls = recordLogRequests();
    renderApp('/applications/guestbook/guestbook/sidecar/logs?podName=guestbook-ui-5d8f-x1');
    expect(await screen.findByRole('heading', { level: 1, name: 'Logs of Pod guestbook-ui-5d8f-x1' })).toBeInTheDocument();
    const output = await screen.findByRole('log', { name: 'Logs of Pod guestbook-ui-5d8f-x1' });
    expect(await within(output).findByText('[sidecar] starting web server on :8080')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Container' })).toHaveValue('sidecar');
    expect(screen.queryByRole('link', { name: 'Open full screen' })).not.toBeInTheDocument();
    const request = urls[0];
    expect(request?.pathname).toMatch(/\/applications\/guestbook\/pods\/guestbook-ui-5d8f-x1\/logs$/);
    expect(request?.searchParams.get('namespace')).toBe('guestbook');
    expect(request?.searchParams.get('container')).toBe('sidecar');
    const back = screen.getByRole('link', { name: 'Back to resource' });
    expect(back).toHaveAttribute('href', `/applications/guestbook?${new URLSearchParams({ node: '/Pod/guestbook/guestbook-ui-5d8f-x1', tab: 'logs' }).toString()}`);
    expect(document.title).toBe('Logs of guestbook-ui-5d8f-x1 · guestbook · Applications · Patrol CD');
  });

  it("aggregates a workload's pods on the namespaced route", async () => {
    const user = userEvent.setup({ delay: null });
    const urls = recordLogRequests();
    const { router } = renderApp('/applications/patrol/guestbook/guestbook/-/logs?kind=Deployment&group=apps&resourceName=guestbook-ui');
    const output = await screen.findByRole('log', { name: 'Logs of Deployment guestbook-ui' });
    expect(await within(output).findByText('ERROR connection refused: cache:6379')).toBeInTheDocument();
    await waitFor(() => expect(urls.length).toBeGreaterThan(0));
    expect(urls[0]?.pathname).toMatch(/\/applications\/guestbook\/logs$/);
    expect(Object.fromEntries(urls[0]?.searchParams ?? [])).toMatchObject({ kind: 'Deployment', group: 'apps', resourceName: 'guestbook-ui', appNamespace: 'patrol' });
    await user.click(screen.getByRole('link', { name: 'Back to resource' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/applications/patrol/guestbook'));
    expect(new URLSearchParams(router.state.location.search).get('node')).toBe('apps/Deployment/guestbook/guestbook-ui');
  });

  it('explains how to pick a pod when the query string names none', async () => {
    renderApp('/applications/guestbook/guestbook/-/logs');
    expect(await screen.findByRole('heading', { name: 'No pod or workload selected' })).toBeInTheDocument();
    expect(screen.getByText(/go to its Logs tab and choose Open full screen/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open the application' })).toHaveAttribute('href', '/applications/guestbook');
  });

  it('shows the loading state, then a missing application with a way back', async () => {
    server.use(
      http.get('*/api/v1/applications/:name', async () => {
        await delay(50);
        return problem(404, 'not_found', 'application "gone" not found');
      }),
    );
    renderApp('/applications/gone/default/web/logs?podName=web-1');
    expect(await screen.findByText('Loading application')).toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: 'Application not found' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to applications' })).toHaveAttribute('href', '/applications');
    expect(screen.queryByRole('log')).not.toBeInTheDocument();
  });

  it('offers a retry when the server fails', async () => {
    const user = userEvent.setup({ delay: null });
    let calls = 0;
    server.use(
      http.get('*/api/v1/applications/guestbook', () => {
        calls += 1;
        return calls === 1 ? problem(503, 'unavailable', 'patrol-server is restarting') : undefined;
      }),
    );
    renderApp('/applications/guestbook/guestbook/-/logs?podName=guestbook-ui-5d8f-x1');
    const alert = await screen.findByRole('alert');
    await user.click(within(alert).getByRole('button', { name: 'Retry' }));
    expect(await screen.findByRole('log', { name: 'Logs of Pod guestbook-ui-5d8f-x1' })).toBeInTheDocument();
  });
});
