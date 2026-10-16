import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { resourcesAppFixture } from '../../../mocks/fixtures/apps-resources';
import { problem } from '../../../mocks/handlers';
import { resetAppsResourcesRequests } from '../../../mocks/handlers/apps-resources';
import { server } from '../../../mocks/server';
import { ResourceViews } from './ResourceViews';
import { currentSearch, renderAt, useResourceHandlers } from './test/helpers';

beforeEach(() => useResourceHandlers());
afterEach(() => resetAppsResourcesRequests());

async function treeItems() {
  const tree = await screen.findByRole('tree', { name: 'Resource tree of shop' });
  return { tree, items: within(tree).getAllByRole('treeitem') };
}

describe('ResourceViews', () => {
  it('shows a loading state, then the tree with the application root and managed resources', async () => {
    renderAt(<ResourceViews app={resourcesAppFixture()} />);
    expect(screen.getByText('Loading resources')).toBeInTheDocument();
    const { items } = await treeItems();
    const names = items.map((i) => i.getAttribute('aria-label'));
    expect(names[0]).toBe('Application shop, Degraded, Out of sync');
    expect(names).toContain('Deployment web, namespace shop, Degraded, Out of sync');
    expect(names).toContain('Job db-migrate, namespace shop, Healthy, Synced, sync hook');
    expect(names).toContain('ConfigMap legacy, namespace shop, Unknown health, Out of sync, requires pruning');
    expect(names).toContain('PersistentVolumeClaim data, namespace shop, Missing, Out of sync');
    // Children hang off their parents: Pods are below the ReplicaSet (level 4).
    const pod = items.find((i) => i.getAttribute('aria-label')?.startsWith('Pod web-7d9f-b2'));
    expect(pod).toHaveAttribute('aria-level', '4');
    expect(screen.getByText(/^16 resources\./)).toBeInTheDocument();
  });

  it('explains a failed tree load with recovery guidance and retries', async () => {
    server.use(http.get('*/api/v1/applications/:name/resource-tree', () => problem(403, 'permission_denied', 'permission denied: applications, get, default/shop')));
    renderAt(<ResourceViews app={resourcesAppFixture()} />);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Could not load the resource tree');
    expect(alert).toHaveTextContent('Ask an administrator for the required RBAC permission');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('shows guidance when the application has no resources', async () => {
    server.use(http.get('*/api/v1/applications/:name/resource-tree', () => HttpResponse.json({ nodes: [], orphanedNodes: [], hosts: [] })));
    const app = resourcesAppFixture();
    if (app.status) app.status.resources = [];
    renderAt(<ResourceViews app={app} />);
    expect(await screen.findByRole('heading', { name: 'No resources yet' })).toBeInTheDocument();
    expect(screen.getByText(/Sync it to apply the manifests/)).toBeInTheDocument();
  });

  it('switches views with the keyboard and keeps the view in the URL', async () => {
    const user = userEvent.setup();
    const view = renderAt(<ResourceViews app={resourcesAppFixture()} />);
    await treeItems();
    const tab = screen.getByRole('tab', { name: 'Tree' });
    expect(tab).toHaveAttribute('aria-selected', 'true');
    await user.click(tab);
    await user.keyboard('{ArrowRight}');
    expect(screen.getByRole('tab', { name: 'Pods' })).toHaveAttribute('aria-selected', 'true');
    expect(currentSearch(view).get('view')).toBe('pods');
    await user.keyboard('{End}');
    expect(currentSearch(view).get('view')).toBe('list');
    expect(await screen.findByRole('table', { name: /Resources \(\d+\)/ })).toBeInTheDocument();
  });

  it('honors the default-view annotation and the view URL parameter', async () => {
    const app = resourcesAppFixture({ annotations: { 'pref.cd.patrol.example/default-view': 'network' } });
    const first = renderAt(<ResourceViews app={app} />);
    expect(await screen.findByRole('tree', { name: 'Network of shop' })).toBeInTheDocument();
    first.unmount();
    renderAt(<ResourceViews app={app} />, '/applications/shop?view=list');
    expect(await screen.findByRole('table', { name: /Resources/ })).toBeInTheDocument();
  });

  it('filters from URL parameters and clears them', async () => {
    const user = userEvent.setup();
    const view = renderAt(<ResourceViews app={resourcesAppFixture()} />, '/applications/shop?view=list&kind=Pod');
    const table = await screen.findByRole('table', { name: 'Resources (5)' });
    expect(within(table).getAllByRole('row')).toHaveLength(6);
    expect(screen.getByRole('list', { name: 'Active resource filters' })).toHaveTextContent('Kind: Pod');
    await user.click(screen.getByRole('button', { name: 'Remove filter Kind: Pod' }));
    expect(currentSearch(view).get('kind')).toBeNull();
    expect(await screen.findByRole('table', { name: /Resources \(1[0-9]\)/ })).toBeInTheDocument();
  });

  it('filters by name with wildcards, sync status (hooks always shown) and health from the menus', async () => {
    const user = userEvent.setup();
    const view = renderAt(<ResourceViews app={resourcesAppFixture()} />, '/applications/shop?view=list');
    await screen.findByRole('table', { name: /Resources/ });
    await user.type(screen.getByRole('searchbox', { name: 'Filter resources by name' }), 'web-7d9f-*');
    expect(currentSearch(view).get('name')).toBe('web-7d9f-*');
    expect(await screen.findByRole('table', { name: 'Resources (3)' })).toBeInTheDocument();
    await user.clear(screen.getByRole('searchbox', { name: 'Filter resources by name' }));

    const filters = screen.getByRole('group', { name: 'Resource filters' });
    const syncButton = within(filters).getByRole('button', { name: 'Sync status' });
    await user.click(syncButton);
    await user.click(screen.getByRole('checkbox', { name: /Out of sync/ }));
    expect(currentSearch(view).get('sync')).toBe('OutOfSync');
    await user.keyboard('{Escape}');
    expect(syncButton).toHaveFocus();
    expect(syncButton).toHaveAttribute('aria-expanded', 'false');
    const table = await screen.findByRole('table', { name: 'Resources (4)' });
    // OutOfSync resources plus the hook, which ignores the sync filter.
    expect(within(table).getByText('db-migrate')).toBeInTheDocument();
    expect(within(table).getByText('legacy')).toBeInTheDocument();
  });

  it('shows orphaned resources only when the toggle is on', async () => {
    const user = userEvent.setup();
    const view = renderAt(<ResourceViews app={resourcesAppFixture()} />);
    const { tree } = await treeItems();
    expect(within(tree).queryByRole('treeitem', { name: /ConfigMap stray/ })).not.toBeInTheDocument();
    await user.click(screen.getByRole('switch', { name: 'Orphaned resources (1)' }));
    expect(currentSearch(view).get('orphaned')).toBe('true');
    expect(await within(tree).findByRole('treeitem', { name: /ConfigMap stray.*orphaned/ })).toBeInTheDocument();
  });

  it('hides the orphaned toggle when the project does not monitor orphaned resources', async () => {
    server.use(
      http.get('*/api/v1/projects/:name', ({ params }) =>
        HttpResponse.json({ metadata: { name: String(params.name) }, spec: { sourceRepos: ['*'], destinations: [] } }),
      ),
    );
    renderAt(<ResourceViews app={resourcesAppFixture()} />);
    await treeItems();
    await waitFor(() => expect(screen.queryByRole('switch', { name: /Orphaned resources/ })).not.toBeInTheDocument());
  });

  it('navigates the graph with the keyboard and opens details with Enter', async () => {
    const user = userEvent.setup();
    const view = renderAt(<ResourceViews app={resourcesAppFixture()} />);
    const { items } = await treeItems();
    const root = items[0]!;
    expect(root).toHaveAttribute('tabindex', '0');
    root.focus();
    await user.keyboard('{ArrowRight}');
    const first = document.activeElement;
    expect(first).toHaveAttribute('aria-level', '2');
    await user.keyboard('{ArrowDown}');
    expect(document.activeElement).not.toBe(first);
    await user.keyboard('{ArrowLeft}');
    // Leaf or expanded node: Left goes to the parent (the application) or collapses.
    await user.keyboard('{Home}');
    expect(document.activeElement).toBe(root);
    await user.keyboard('{End}{Enter}');
    expect(currentSearch(view).get('node')).toBeTruthy();
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
  });

  it('collapses and expands a node with the arrow keys', async () => {
    const user = userEvent.setup();
    renderAt(<ResourceViews app={resourcesAppFixture()} />);
    const { tree } = await treeItems();
    const deployment = within(tree).getByRole('treeitem', { name: /^Deployment web/ });
    expect(deployment).toHaveAttribute('aria-expanded', 'true');
    deployment.focus();
    await user.keyboard('{ArrowLeft}');
    const collapsed = within(tree).getByRole('treeitem', { name: /^Deployment web.*5 hidden descendants/ });
    expect(collapsed).toHaveAttribute('aria-expanded', 'false');
    expect(within(tree).queryByRole('treeitem', { name: /^ReplicaSet web-7d9f/ })).not.toBeInTheDocument();
    collapsed.focus();
    await user.keyboard('{ArrowRight}');
    expect(await within(tree).findByRole('treeitem', { name: /^ReplicaSet web-7d9f,/ })).toBeInTheDocument();
  });

  it('renders the accessible outline with the same items', async () => {
    const user = userEvent.setup();
    renderAt(<ResourceViews app={resourcesAppFixture()} />);
    await treeItems();
    await user.click(screen.getByRole('switch', { name: 'Show as outline' }));
    const outline = screen.getByRole('tree', { name: 'Resource tree of shop' });
    expect(outline.tagName).toBe('DIV');
    const rows = within(outline).getAllByRole('treeitem');
    expect(rows[0]).toHaveTextContent('shop');
    const deployment = rows.find((r) => r.textContent?.includes('deploy'));
    expect(deployment).toHaveAttribute('aria-level', '2');
    (rows[0]!).focus();
    await user.keyboard('{ArrowDown}');
    expect(document.activeElement).toBe(rows[1]);
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
  });

  it('calls onOpenApplication when the root node is activated', async () => {
    const user = userEvent.setup();
    let opened = 0;
    renderAt(<ResourceViews app={resourcesAppFixture()} onOpenApplication={() => (opened += 1)} />);
    const { items } = await treeItems();
    await user.click(items[0]!);
    expect(opened).toBe(1);
  });

  it('groups pods by node with node resources, and by parent resource', async () => {
    const user = userEvent.setup();
    const view = renderAt(<ResourceViews app={resourcesAppFixture()} />, '/applications/shop?view=pods');
    const nodeA = await screen.findByRole('region', { name: /node-a/ });
    expect(within(nodeA).getByRole('img', { name: /cpu: 750m requested by this application, 1.25 cores by other workloads, capacity 4 cores/ })).toBeInTheDocument();
    // Node labels the server propagates (application.allowedNodeLabels) are listed per node.
    expect(within(nodeA).getByRole('list', { name: 'Labels of node node-a' })).toHaveTextContent('topology.kubernetes.io/zone=zone-a');
    const nodeB = screen.getByRole('region', { name: /node-b/ });
    expect(within(nodeB).getByText('web-7d9f-b2')).toBeInTheDocument();
    expect(within(nodeB).getByRole('table', { name: 'Pods on node-b' })).toHaveTextContent('CrashLoopBackOff');
    await user.click(screen.getByRole('radio', { name: 'Parent resource' }));
    expect(currentSearch(view).get('podSortMode')).toBe('parentResource');
    expect(await screen.findByRole('region', { name: /ReplicaSet\/web-7d9f/ })).toBeInTheDocument();
    // Collapsing a group hides its table.
    const disclosure = screen.getByRole('button', { name: /ReplicaSet\/web-7d9f/ });
    await user.click(disclosure);
    expect(disclosure).toHaveAttribute('aria-expanded', 'false');
  });

  it('draws the network and lists external endpoints with safe links', async () => {
    renderAt(<ResourceViews app={resourcesAppFixture()} />, '/applications/shop?view=network');
    const tree = await screen.findByRole('tree', { name: 'Network of shop' });
    const labels = within(tree)
      .getAllByRole('treeitem')
      .map((i) => i.getAttribute('aria-label'));
    expect(labels).toContain('External traffic');
    expect(labels.some((l) => l?.startsWith('Ingress web'))).toBe(true);
    expect(labels.some((l) => l?.startsWith('Service web'))).toBe(true);
    expect(labels.some((l) => l?.startsWith('Pod web-7d9f-a1'))).toBe(true);
    const link = screen.getByRole('link', { name: /https:\/\/shop.example.test\// });
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    expect(screen.getByRole('table', { name: 'External endpoints' })).toHaveTextContent('203.0.113.10');
  });
});
