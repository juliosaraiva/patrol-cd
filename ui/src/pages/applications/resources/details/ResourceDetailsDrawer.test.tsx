import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resourcesAppFixture } from '../../../../mocks/fixtures/apps-resources';
import { problem } from '../../../../mocks/handlers';
import { appsResourcesRequests, resetAppsResourcesRequests } from '../../../../mocks/handlers/apps-resources';
import { server } from '../../../../mocks/server';
import { ResourceViews } from '../ResourceViews';
import { canIRequests, currentSearch, denyCanI, enableExec, renderAt, useResourceHandlers } from '../test/helpers';

vi.mock('./TerminalPanel', () => ({
  TerminalPanel: ({ pod }: { pod: string }) => <p>Terminal for {pod}</p>,
}));

beforeEach(() => useResourceHandlers());
afterEach(() => resetAppsResourcesRequests());

function openAt(node: string, extra = '') {
  const user = userEvent.setup();
  const view = renderAt(<ResourceViews app={resourcesAppFixture()} />, `/applications/shop?node=${encodeURIComponent(node)}${extra}`);
  return { user, view };
}

async function drawer(name: RegExp | string) {
  return screen.findByRole('dialog', { name });
}

function tabNames(dialog: HTMLElement) {
  return within(dialog)
    .getAllByRole('tab')
    .map((t) => t.textContent);
}

describe('resource details drawer', () => {
  it('opens from a deep link with the tabs a Deployment offers', async () => {
    openAt('apps/Deployment/shop/web');
    const dialog = await drawer(/Deployment\s*web/);
    expect(within(dialog).getByText('Namespace shop · apps/v1')).toBeInTheDocument();
    await waitFor(() => expect(tabNames(dialog)).toEqual(['Summary', 'Live manifest', 'Desired manifest', 'Diff', 'Events', 'Logs', 'Actions']));
    expect(within(dialog).getByRole('tab', { name: 'Summary' })).toHaveAttribute('aria-selected', 'true');
  });

  it('summarizes fields, health message, owner chain, safe links and labels', async () => {
    openAt('/Pod/shop/web-7d9f-b2');
    const dialog = await drawer(/Pod\s*web-7d9f-b2/);
    expect(within(dialog).getByText('back-off 5m0s restarting failed container')).toBeInTheDocument();
    const chain = within(dialog).getByRole('heading', { name: 'Owner chain' }).nextElementSibling as HTMLElement;
    expect(chain).toHaveTextContent(/Application\s*shop.*Deployment\s*web.*ReplicaSet\s*web-7d9f.*Pod\s*web-7d9f-b2/);
    expect(within(dialog).getByText('Restart Count')).toBeInTheDocument();
    expect(await within(dialog).findByRole('list', { name: 'Labels' })).toHaveTextContent(/app\s*=?\s*(equals)?\s*web/);
    // Clicking an owner selects it.
    await userEvent.setup().click(within(chain).getByRole('button', { name: /ReplicaSet/ }));
    expect(await drawer(/ReplicaSet\s*web-7d9f/)).toBeInTheDocument();
  });

  it('lists only http(s) resource links, opening in a new tab', async () => {
    openAt('apps/Deployment/shop/web');
    const dialog = await drawer(/Deployment\s*web/);
    const link = await within(dialog).findByRole('link', { name: /Dashboard/ });
    expect(link).toHaveAttribute('href', 'https://metrics.example.test/d/web');
    expect(link).toHaveAttribute('target', '_blank');
    expect(within(dialog).queryByText('Unsafe')).not.toBeInTheDocument();
  });

  it('edits the live manifest and applies a merge patch after reviewing the diff', async () => {
    const { user } = openAt('apps/Deployment/shop/web', '&tab=live');
    const dialog = await drawer(/Deployment\s*web/);
    const pre = await within(dialog).findByLabelText('Live manifest of Deployment web');
    expect(pre).toHaveTextContent('replicas: 3');
    expect(pre).not.toHaveTextContent('managedFields');
    await user.click(await within(dialog).findByRole('button', { name: 'Edit' }));
    const editor = within(dialog).getByRole('textbox', { name: 'Edit Deployment web (YAML)' });
    await user.clear(editor);
    await user.type(editor, 'kind: [[');
    await user.click(within(dialog).getByRole('button', { name: 'Review changes' }));
    expect(within(dialog).getByText(/Line \d+:/)).toBeInTheDocument();

    await user.clear(editor);
    const original = (pre.textContent ?? '').replace('replicas: 3', 'replicas: 5');
    await user.click(editor);
    await user.paste(original);
    await user.click(within(dialog).getByRole('button', { name: 'Review changes' }));
    const review = await screen.findByRole('dialog', { name: 'Review changes to Deployment web' });
    expect(within(review).getByRole('table', { name: 'Changes to Deployment web' })).toHaveTextContent('replicas: 5');
    await user.click(within(review).getByRole('button', { name: 'Apply changes' }));
    await waitFor(() => expect(appsResourcesRequests.some((r) => r.method === 'POST' && r.path.endsWith('/resource'))).toBe(true));
    const post = appsResourcesRequests.find((r) => r.method === 'POST' && r.path.endsWith('/resource'));
    expect(post?.body).toEqual({ patch: JSON.stringify({ spec: { replicas: 5 } }), patchType: 'merge' });
    expect(post?.query).toMatchObject({ kind: 'Deployment', resourceName: 'web', namespace: 'shop', group: 'apps', appNamespace: 'patrol' });
    expect(await screen.findByText('Changes applied to Deployment web')).toBeInTheDocument();
  });

  it('reports "no changes" instead of sending an empty patch', async () => {
    const { user } = openAt('apps/Deployment/shop/web', '&tab=live');
    const dialog = await drawer(/Deployment\s*web/);
    await user.click(await within(dialog).findByRole('button', { name: 'Edit' }));
    await user.click(within(dialog).getByRole('button', { name: 'Review changes' }));
    expect(within(dialog).getByText('There are no changes to apply.')).toBeInTheDocument();
  });

  it('marks Secrets as masked and does not offer editing', async () => {
    openAt('/Secret/shop/web-secret', '&tab=live');
    const dialog = await drawer(/Secret\s*web-secret/);
    expect(await within(dialog).findByText('Secret values are masked')).toBeInTheDocument();
    expect(within(dialog).getByLabelText('Live manifest of Secret web-secret')).toHaveTextContent('password: ++++++++');
    expect(within(dialog).queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument();
  });

  it('hides editing and logs without permission', async () => {
    denyCanI(['applications', 'update/apps/Deployment/shop/web'], ['applications', 'update'], ['logs', 'get']);
    openAt('apps/Deployment/shop/web', '&tab=live');
    const dialog = await drawer(/Deployment\s*web/);
    const guidance = await within(dialog).findByText(/^Editing needs the permission/);
    expect(guidance).toHaveTextContent('Editing needs the permission applications, update/apps/Deployment/shop/web on default/shop.');
    expect(within(dialog).queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument();
    expect(tabNames(dialog)).not.toContain('Logs');
  });

  it('offers editing with only the fine-grained update permission the server checks', async () => {
    // The server checks applications, update/<group>/<kind>/<namespace>/<name>; plain update is not needed.
    denyCanI(['applications', 'update']);
    openAt('apps/Deployment/shop/web', '&tab=live');
    const dialog = await drawer(/Deployment\s*web/);
    expect(await within(dialog).findByRole('button', { name: 'Edit' })).toBeInTheDocument();
    expect(within(dialog).queryByText(/Editing needs/)).not.toBeInTheDocument();
    expect(within(dialog).queryByText(/fine-grained RBAC inheritance/)).not.toBeInTheDocument();
    // The action travels as one encoded path segment, which the server decodes.
    expect(canIRequests).toContain('/api/v1/account/can-i/applications/update%2Fapps%2FDeployment%2Fshop%2Fweb/default/shop');
  });

  it('warns when only the application-wide update permission is granted', async () => {
    denyCanI(['applications', 'update/apps/Deployment/shop/web']);
    openAt('apps/Deployment/shop/web', '&tab=live');
    const dialog = await drawer(/Deployment\s*web/);
    expect(await within(dialog).findByRole('button', { name: 'Edit' })).toBeInTheDocument();
    expect(within(dialog).getByText(/fine-grained RBAC inheritance/)).toHaveTextContent(
      'Your role allows applications, update on this application but not applications, update/apps/Deployment/shop/web on default/shop.',
    );
  });

  it('names the fine-grained permission when the server refuses the patch', async () => {
    server.use(
      http.post('*/api/v1/applications/:name/resource', () =>
        problem(403, 'permission_denied', 'permission denied: applications, update/apps/Deployment/shop/web, default/shop'),
      ),
    );
    const { user } = openAt('apps/Deployment/shop/web', '&tab=live');
    const dialog = await drawer(/Deployment\s*web/);
    const pre = await within(dialog).findByLabelText('Live manifest of Deployment web');
    await user.click(await within(dialog).findByRole('button', { name: 'Edit' }));
    const editor = within(dialog).getByRole('textbox', { name: 'Edit Deployment web (YAML)' });
    await user.clear(editor);
    await user.click(editor);
    await user.paste((pre.textContent ?? '').replace('replicas: 3', 'replicas: 4'));
    await user.click(within(dialog).getByRole('button', { name: 'Review changes' }));
    const review = await screen.findByRole('dialog', { name: 'Review changes to Deployment web' });
    await user.click(within(review).getByRole('button', { name: 'Apply changes' }));
    const alert = await within(review).findByRole('alert');
    expect(alert).toHaveTextContent('You may not change this resource');
    expect(alert).toHaveTextContent('Applying needs the permission applications, update/apps/Deployment/shop/web on default/shop.');
  });

  it('shows the diff with markers and persists the compact and inline toggles', async () => {
    const { user } = openAt('apps/Deployment/shop/web', '&tab=diff');
    const dialog = await drawer(/Deployment\s*web/);
    const table = await within(dialog).findByRole('table', { name: 'Diff of Deployment web: live and desired state' });
    expect(within(table).getAllByText('Removed').length).toBeGreaterThan(0);
    expect(within(table).getAllByText('Added').length).toBeGreaterThan(0);
    expect(within(table).getByRole('columnheader', { name: 'Live' })).toBeInTheDocument();
    expect(within(dialog).getByText(/ignoreDifferences/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('switch', { name: 'Inline diff' }));
    expect(within(dialog).getByRole('columnheader', { name: 'Live → Desired' })).toBeInTheDocument();
    expect(JSON.parse(window.localStorage.getItem('patrol.appResources.prefs') ?? '{}')).toMatchObject({ inlineDiff: true });
    const skip = within(dialog).queryAllByRole('button', { name: /Show \d+ unchanged lines?/ });
    expect(skip.length).toBeGreaterThan(0);
    await user.click(within(dialog).getByRole('switch', { name: 'Compact diff' }));
    expect(within(dialog).queryAllByRole('button', { name: /Show \d+ unchanged/ })).toHaveLength(0);
  });

  it('explains that a resource to prune has no desired manifest', async () => {
    openAt('/ConfigMap/shop/legacy', '&tab=desired');
    const dialog = await drawer(/ConfigMap\s*legacy/);
    expect(await within(dialog).findByRole('heading', { name: 'Not in the source anymore' })).toBeInTheDocument();
  });

  it('lists events with a warning count in the tab', async () => {
    const { user } = openAt('/Pod/shop/web-7d9f-b2');
    const dialog = await drawer(/Pod\s*web-7d9f-b2/);
    const eventsTab = await within(dialog).findByRole('tab', { name: /Events\s*1 warning/ });
    await user.click(eventsTab);
    const table = await within(dialog).findByRole('table', { name: 'Events of Pod web-7d9f-b2' });
    const rows = within(table).getAllByRole('row');
    expect(rows[1]).toHaveTextContent('BackOff');
    expect(rows[1]).toHaveTextContent('Warning');
  });

  it('runs a resource action after confirmation and reports the result', async () => {
    const { user } = openAt('apps/Deployment/shop/web', '&tab=actions');
    const dialog = await drawer(/Deployment\s*web/);
    expect(await within(dialog).findByRole('button', { name: 'Run Pause' })).toBeDisabled();
    await user.click(within(dialog).getByRole('button', { name: 'Run Restart' }));
    const confirm = await screen.findByRole('dialog', { name: 'Run Restart?' });
    expect(confirm).toHaveTextContent('can interrupt the workload');
    await user.click(within(confirm).getByRole('button', { name: 'Run Restart' }));
    expect(await screen.findByText('Action Restart ran on Deployment web')).toBeInTheDocument();
    const post = appsResourcesRequests.find((r) => r.method === 'POST' && r.path.endsWith('/resource/actions'));
    expect(post?.body).toEqual({ action: 'restart', resourceActionParameters: [] });
  });

  it('sends parameters of a parameterized action', async () => {
    const { user } = openAt('apps/Deployment/shop/web', '&tab=actions');
    const dialog = await drawer(/Deployment\s*web/);
    await user.click(await within(dialog).findByRole('button', { name: 'Run Scale' }));
    const confirm = await screen.findByRole('dialog', { name: 'Run Scale?' });
    const input = within(confirm).getByRole('textbox', { name: 'replicas' });
    // Action discovery declares parameter names only (no defaults), so the field starts empty.
    expect(input).toHaveValue('');
    await user.type(input, '6');
    await user.click(within(confirm).getByRole('button', { name: 'Run Scale' }));
    await waitFor(() => expect(appsResourcesRequests.some((r) => r.path.endsWith('/resource/actions') && r.method === 'POST')).toBe(true));
    const post = appsResourcesRequests.find((r) => r.method === 'POST' && r.path.endsWith('/resource/actions'));
    expect(post?.body).toEqual({ action: 'scale', resourceActionParameters: [{ name: 'replicas', value: '6' }] });
  });

  it('keeps the action dialog open with guidance when the action fails', async () => {
    server.use(http.post('*/api/v1/applications/:name/resource/actions', () => problem(403, 'permission_denied', 'permission denied: applications, action/apps/Deployment/restart')));
    const { user } = openAt('apps/Deployment/shop/web', '&tab=actions');
    const dialog = await drawer(/Deployment\s*web/);
    await user.click(await within(dialog).findByRole('button', { name: 'Run Restart' }));
    const confirm = await screen.findByRole('dialog', { name: 'Run Restart?' });
    await user.click(within(confirm).getByRole('button', { name: 'Run Restart' }));
    expect(await within(confirm).findByRole('alert')).toHaveTextContent('Ask an administrator for the required RBAC permission');
  });

  it('deletes a resource with the chosen propagation after typing its name', async () => {
    const { user, view } = openAt('apps/ReplicaSet/shop/web-5c4b');
    const dialog = await drawer(/ReplicaSet\s*web-5c4b/);
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete ReplicaSet web-5c4b?' });
    const submit = within(confirm).getByRole('button', { name: 'Delete ReplicaSet' });
    expect(submit).toBeDisabled();
    await user.click(within(confirm).getByRole('radio', { name: /Non-cascading \(orphan\) delete/ }));
    await user.type(within(confirm).getByRole('textbox', { name: /to confirm/ }), 'web-5c4b');
    expect(submit).toBeEnabled();
    await user.click(submit);
    await waitFor(() => expect(appsResourcesRequests.some((r) => r.method === 'DELETE')).toBe(true));
    const del = appsResourcesRequests.find((r) => r.method === 'DELETE');
    expect(del?.query).toMatchObject({ orphan: 'true', kind: 'ReplicaSet', resourceName: 'web-5c4b' });
    expect(del?.query.force).toBeUndefined();
    await waitFor(() => expect(currentSearch(view).get('node')).toBeNull());
  });

  it('offers the terminal for pods only when exec is enabled and permitted', async () => {
    enableExec();
    const first = openAt('/Pod/shop/web-7d9f-a1', '&tab=terminal');
    const dialog = await drawer(/Pod\s*web-7d9f-a1/);
    expect(await within(dialog).findByText('Terminal for web-7d9f-a1')).toBeInTheDocument();
    first.view.unmount();

    denyCanI(['exec', 'create']);
    openAt('/Pod/shop/web-7d9f-a1', '&tab=terminal');
    const again = await drawer(/Pod\s*web-7d9f-a1/);
    await waitFor(() => expect(tabNames(again)).toContain('Logs'));
    expect(tabNames(again)).not.toContain('Terminal');
    expect(within(again).getByRole('tab', { name: 'Summary' })).toHaveAttribute('aria-selected', 'true');
  });

  it('links the logs to full screen only when the details page says the route renders them', async () => {
    window.localStorage.setItem('patrol.appResources.prefs', JSON.stringify({ logFollow: false }));
    const first = openAt('/Pod/shop/web-7d9f-a1', '&tab=logs');
    const dialog = await drawer(/Pod\s*web-7d9f-a1/);
    expect(await within(dialog).findByRole('group', { name: 'Log options' })).toBeInTheDocument();
    expect(within(dialog).queryByRole('link', { name: 'Open full screen' })).not.toBeInTheDocument();
    first.view.unmount();

    renderAt(<ResourceViews app={resourcesAppFixture()} fullscreenLogs />, `/applications/shop?node=${encodeURIComponent('/Pod/shop/web-7d9f-a1')}&tab=logs`);
    const again = await drawer(/Pod\s*web-7d9f-a1/);
    const link = await within(again).findByRole('link', { name: 'Open full screen' });
    await waitFor(() => expect(link).toHaveAttribute('href', '/applications/shop/shop/web/logs?podName=web-7d9f-a1'));
    window.localStorage.removeItem('patrol.appResources.prefs');
  });

  it('does not offer the terminal when exec is disabled', async () => {
    openAt('/Pod/shop/web-7d9f-a1');
    const dialog = await drawer(/Pod\s*web-7d9f-a1/);
    await waitFor(() => expect(tabNames(dialog)).toContain('Logs'));
    expect(tabNames(dialog)).not.toContain('Terminal');
  });

  it('explains missing resources and unknown deep links', async () => {
    const first = openAt('/PersistentVolumeClaim/shop/data');
    const dialog = await drawer(/PersistentVolumeClaim\s*data/);
    expect(within(dialog).getByText(/does not exist in the cluster\. Sync the application/)).toBeInTheDocument();
    expect(tabNames(dialog)).toEqual(['Summary', 'Desired manifest', 'Diff']);
    first.view.unmount();
    openAt('apps/Deployment/shop/gone');
    expect(await drawer('Resource not found')).toHaveTextContent('may have been deleted or renamed');
  });

  it('closes with Escape and clears the deep link', async () => {
    const { user, view } = openAt('apps/Deployment/shop/web', '&tab=events');
    await drawer(/Deployment\s*web/);
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(currentSearch(view).get('node')).toBeNull();
    expect(currentSearch(view).get('tab')).toBeNull();
  });

  it('switches tabs with the keyboard and stores the tab in the URL', async () => {
    const { user, view } = openAt('apps/Deployment/shop/web');
    const dialog = await drawer(/Deployment\s*web/);
    await waitFor(() => expect(tabNames(dialog)).toContain('Actions'));
    within(dialog).getByRole('tab', { name: 'Summary' }).focus();
    await user.keyboard('{ArrowRight}');
    expect(currentSearch(view).get('tab')).toBe('live');
    expect(within(dialog).getByRole('tab', { name: 'Live manifest' })).toHaveFocus();
  });
});
