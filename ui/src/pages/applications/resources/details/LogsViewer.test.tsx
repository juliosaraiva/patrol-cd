import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resourcesAppFixture } from '../../../../mocks/fixtures/apps-resources';
import { problem } from '../../../../mocks/handlers';
import { appsResourcesRequests, resetAppsResourcesRequests } from '../../../../mocks/handlers/apps-resources';
import { server } from '../../../../mocks/server';
import { renderAt, useResourceHandlers } from '../test/helpers';
import { compileLogFilter, ResourceLogsViewer, stripAnsi } from './LogsViewer';

beforeEach(() => {
  useResourceHandlers();
  window.localStorage.setItem('patrol.appResources.prefs', JSON.stringify({ logFollow: false }));
});
afterEach(() => resetAppsResourcesRequests());

const app = resourcesAppFixture();

function logRequests() {
  return appsResourcesRequests.filter((r) => r.path.endsWith('/logs'));
}

describe('log filter helpers', () => {
  it('matches substrings, inverts with !, and honors match case', () => {
    expect(compileLogFilter('error', false)('An ERROR here')).toBe(true);
    expect(compileLogFilter('error', true)('An ERROR here')).toBe(false);
    expect(compileLogFilter('!GET', false)('GET /healthz')).toBe(false);
    expect(compileLogFilter('!GET', false)('POST /cart')).toBe(true);
    expect(compileLogFilter('', false)('anything')).toBe(true);
    expect(compileLogFilter('!', false)('anything')).toBe(true);
  });

  it('strips ANSI escape sequences', () => {
    expect(stripAnsi(`${String.fromCharCode(27)}[31mred${String.fromCharCode(27)}[0m text`)).toBe('red text');
  });
});

describe('ResourceLogsViewer', () => {
  it('streams a pod log, filters it and reports the end of the stream', async () => {
    const user = userEvent.setup();
    renderAt(<ResourceLogsViewer app={app} target={{ kind: 'Pod', namespace: 'shop', name: 'web-7d9f-a1' }} containers={['web', 'sidecar']} />);
    const output = await screen.findByRole('log', { name: 'Logs of Pod web-7d9f-a1' });
    expect(await within(output).findByText('starting web server on :8080')).toBeInTheDocument();
    expect(await screen.findByText(/Stream ended\. 2 lines received\./)).toBeInTheDocument();
    const request = logRequests()[0];
    expect(request?.path).toMatch(/\/applications\/shop\/pods\/web-7d9f-a1\/logs$/);
    expect(request?.query).toMatchObject({ container: 'web', tailLines: '1000', follow: 'false', previous: 'false', namespace: 'shop', appNamespace: 'patrol' });

    await user.type(screen.getByRole('textbox', { name: 'Filter' }), 'healthz');
    expect(within(output).queryByText('starting web server on :8080')).not.toBeInTheDocument();
    expect(within(output).getByText('GET /healthz 200')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('Showing 1 of 2 lines.');
    await user.clear(screen.getByRole('textbox', { name: 'Filter' }));
    await user.type(screen.getByRole('textbox', { name: 'Filter' }), '!healthz');
    expect(within(output).queryByText('GET /healthz 200')).not.toBeInTheDocument();
  });

  it('restarts the stream with the chosen container, previous and tail options', async () => {
    const user = userEvent.setup();
    renderAt(<ResourceLogsViewer app={app} target={{ kind: 'Pod', namespace: 'shop', name: 'web-7d9f-a1' }} containers={['web', 'sidecar']} initContainers={['init']} />);
    const output = await screen.findByRole('log');
    await within(output).findByText('starting web server on :8080');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Container' }), 'sidecar');
    expect(await within(output).findByText('[sidecar] starting web server on :8080')).toBeInTheDocument();
    await user.click(screen.getByRole('switch', { name: 'Previous container' }));
    expect(await within(output).findByText('[previous] starting web server on :8080')).toBeInTheDocument();
    await user.selectOptions(screen.getByRole('combobox', { name: 'Lines' }), '');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Since' }), '3600');
    await waitFor(() => expect(logRequests().at(-1)?.query).toMatchObject({ container: 'sidecar', previous: 'true', sinceSeconds: '3600' }));
    expect(logRequests().at(-1)?.query.tailLines).toBeUndefined();
    expect(screen.getByRole('option', { name: 'init (init container)' })).toBeInTheDocument();
  });

  it('aggregates workload logs with pod names and loads containers from the live manifest', async () => {
    renderAt(<ResourceLogsViewer app={app} target={{ group: 'apps', kind: 'Deployment', namespace: 'shop', name: 'web' }} />);
    const output = await screen.findByRole('log', { name: 'Logs of Deployment web' });
    expect(await within(output).findByText('ERROR connection refused: cache:6379')).toBeInTheDocument();
    expect(within(output).getAllByText(/web-7d9f-b2/).length).toBeGreaterThan(0);
    const request = logRequests()[0];
    expect(request?.path).toMatch(/\/applications\/shop\/logs$/);
    expect(request?.query).toMatchObject({ kind: 'Deployment', group: 'apps', resourceName: 'web', container: 'web' });
    expect(screen.getByRole('switch', { name: 'Pod names' })).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByRole('option', { name: 'migrate-check (init container)' })).toBeInTheDocument();
  });

  it('shows timestamps, wraps lines and offers a full-screen link only when asked to', async () => {
    const user = userEvent.setup();
    const view = renderAt(<ResourceLogsViewer app={app} target={{ kind: 'Pod', namespace: 'shop', name: 'web-7d9f-a1' }} containers={['web']} />);
    // Without fullscreenLink the full-screen route may still be a placeholder: no link.
    expect(screen.queryByRole('link', { name: 'Open full screen' })).not.toBeInTheDocument();
    view.unmount();
    renderAt(<ResourceLogsViewer app={app} target={{ kind: 'Pod', namespace: 'shop', name: 'web-7d9f-a1' }} containers={['web']} fullscreenLink />);
    const output = await screen.findByRole('log');
    await within(output).findByText('starting web server on :8080');
    await user.click(screen.getByRole('switch', { name: 'Timestamps' }));
    expect(within(output).getByText(/2026-10-16T09:00:00Z/)).toBeInTheDocument();
    await user.click(screen.getByRole('switch', { name: 'Wrap lines' }));
    expect(output.className).toMatch(/wrap/);
    expect(screen.getByRole('link', { name: 'Open full screen' })).toHaveAttribute('href', '/applications/shop/shop/web/logs?podName=web-7d9f-a1');
  });

  it('downloads the shown lines as a file', async () => {
    const user = userEvent.setup();
    const created: Blob[] = [];
    const createObjectURL = vi.fn((blob: Blob) => {
      created.push(blob);
      return 'blob:logs';
    });
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL, revokeObjectURL: vi.fn() }));
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    renderAt(<ResourceLogsViewer app={app} target={{ kind: 'Pod', namespace: 'shop', name: 'web-7d9f-a1' }} containers={['web']} />);
    await within(await screen.findByRole('log')).findByText('starting web server on :8080');
    await user.click(screen.getByRole('button', { name: 'Download' }));
    expect(click).toHaveBeenCalled();
    const anchor = click.mock.contexts[0] as HTMLAnchorElement;
    expect(anchor.download).toBe('web-7d9f-a1-web.log');
    const blob = created[0];
    expect(blob).toBeDefined();
    const text = await new Promise<string>((resolve) => {
      const reader = new FileReader();
      reader.onload = () => resolve(typeof reader.result === 'string' ? reader.result : '');
      reader.readAsText(blob!);
    });
    expect(text).toBe('starting web server on :8080\nGET /healthz 200');
    vi.unstubAllGlobals();
  });

  it('explains a permission error with a retry', async () => {
    server.use(http.get('*/api/v1/applications/:name/pods/:pod/logs', () => problem(403, 'permission_denied', 'permission denied: logs, get, default/shop')));
    renderAt(<ResourceLogsViewer app={app} target={{ kind: 'Pod', namespace: 'shop', name: 'web-7d9f-a1' }} containers={['web']} />);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('You do not have access');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('follows a live stream until it is closed', async () => {
    window.localStorage.setItem('patrol.appResources.prefs', JSON.stringify({ logFollow: true }));
    const view = renderAt(<ResourceLogsViewer app={app} target={{ kind: 'Pod', namespace: 'shop', name: 'web-7d9f-a1' }} containers={['web']} />);
    const output = await screen.findByRole('log');
    expect(await within(output).findByText('GET /healthz 200')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('Streaming new lines as they arrive.');
    expect(logRequests()[0]?.query.follow).toBe('true');
    view.unmount();
  });
});
