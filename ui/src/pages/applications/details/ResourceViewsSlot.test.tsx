import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { installExtensionGlobals, resetExtensionRegistry, type AppViewComponentProps } from '../../../extensions';
import { appsCoreMockStore, resetAppsCoreMockState } from '../../../mocks/handlers/apps-core';
import { renderApp } from '../../../test/render';
import { BUILT_IN_VIEWS, DEFAULT_VIEW_ANNOTATION, type ResourcesContractModule } from './ResourceViewsSlot';

// No vi.mock of ../resources/contract.ts here: this file exercises the real resource views module
// through the slot, so a contract change on either side fails a test.
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());

const DEPLOYMENT = 'apps/Deployment/guestbook/guestbook-ui';
/** The first render loads the lazy route and resource views chunks (slow on a cold, loaded machine). */
const LAZY = { timeout: 15_000 };

describe('resource views slot (real resources module)', () => {
  it('matches the props the shell passes', async () => {
    // Type-level check: the published module is assignable to the structural contract.
    const mod: ResourcesContractModule = await import('../resources/contract');
    expect(mod.ResourceViewsSlot).toBeDefined();
    expect(mod.AppDiffPanel).toBeDefined();
    expect(mod.ResourceLogsViewer).toBeDefined();
    // The shell's view switcher (app view extensions) offers the module's views in its order.
    const contract = await import('../resources/contract');
    expect(BUILT_IN_VIEWS.map((v) => v.id)).toEqual(contract.RESOURCE_VIEW_IDS);
    expect(BUILT_IN_VIEWS.map((v) => v.label)).toEqual(contract.RESOURCE_VIEW_IDS.map((id) => contract.RESOURCE_VIEW_LABELS[id]));
    expect(DEFAULT_VIEW_ANNOTATION).toBe(contract.DEFAULT_VIEW_ANNOTATION);
  });

  it("starts a selective sync from the resource drawer's Sync button", async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp(`/applications/patrol/guestbook?node=${encodeURIComponent(DEPLOYMENT)}`);
    const resource = await screen.findByRole('dialog', { name: /Deployment\s*guestbook-ui/ }, LAZY);
    await user.click(await within(resource).findByRole('button', { name: 'Sync' }));
    await waitFor(() => expect(new URLSearchParams(router.state.location.search).get('panel')).toBe('sync'));
    const params = new URLSearchParams(router.state.location.search);
    expect(params.get('resources')).toBe(DEPLOYMENT);
    expect(params.get('node')).toBeNull();
    const sync = await screen.findByRole('dialog', { name: 'Sync guestbook' });
    expect(within(sync).getByText(/Selective sync of 1 of 2 resources/)).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: /Deployment\s*guestbook-ui/ })).not.toBeInTheDocument();
  });

  it("links the resource drawer's Logs tab to the full-screen logs page", async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp(`/applications/patrol/guestbook?node=${encodeURIComponent(DEPLOYMENT)}&tab=logs`);
    const resource = await screen.findByRole('dialog', { name: /Deployment\s*guestbook-ui/ }, LAZY);
    await user.click(await within(resource).findByRole('link', { name: 'Open full screen' }));
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/applications\/guestbook\/guestbook\/[^/]+\/logs$/));
    expect(Object.fromEntries(new URLSearchParams(router.state.location.search))).toEqual({ kind: 'Deployment', group: 'apps', resourceName: 'guestbook-ui' });
    expect(await screen.findByRole('heading', { level: 1, name: 'Logs of Deployment guestbook-ui' })).toBeInTheDocument();
    expect(await screen.findByRole('log', { name: 'Logs of Deployment guestbook-ui' })).toBeInTheDocument();
  });

  it('opens the application details panel from the tree root node', async () => {
    const user = userEvent.setup({ delay: null });
    const { router } = renderApp('/applications/patrol/guestbook');
    const tree = await screen.findByRole('tree', { name: 'Resource tree of guestbook' }, LAZY);
    const [root] = within(tree).getAllByRole('treeitem');
    if (!root) throw new Error('tree root missing');
    await user.click(root);
    await waitFor(() => expect(new URLSearchParams(router.state.location.search).get('panel')).toBe('details'));
    expect(await screen.findByRole('dialog', { name: 'guestbook details' })).toBeInTheDocument();
  });
});

describe('app view extensions in the view switcher (API-123)', () => {
  afterEach(() => {
    resetExtensionRegistry();
    delete window.extensionsAPI;
    delete window.extensions;
  });

  function CostView({ application, tree }: AppViewComponentProps) {
    return (
      <p>
        Cost of {application.metadata.name}: {tree.nodes?.length ?? 0} resources
      </p>
    );
  }

  function register() {
    installExtensionGlobals();
    window.extensionsAPI?.registerAppViewExtension(CostView, 'Cost', 'fa-dollar');
    window.extensionsAPI?.registerAppViewExtension(() => <p>Production only</p>, 'Prod only', 'fa-x', (app) => app.spec.project === 'prod');
  }

  const search = (router: ReturnType<typeof renderApp>['router']) => new URLSearchParams(router.state.location.search);

  it('offers registered views after the built-in ones and renders the selected one with the live tree', async () => {
    const user = userEvent.setup({ delay: null });
    register();
    const { router } = renderApp('/applications/patrol/guestbook');
    const switcher = await screen.findByRole('tablist', { name: 'Application views' }, LAZY);
    expect(within(switcher).getAllByRole('tab').map((t) => t.textContent)).toEqual(['Tree', 'Pods', 'Network', 'List', 'Cost']);
    expect(within(switcher).getByRole('tab', { name: 'Tree' })).toHaveAttribute('aria-selected', 'true');
    // The resource views show the selected built-in view without a second switcher.
    expect(await screen.findByRole('tree', { name: 'Resource tree of guestbook' }, LAZY)).toBeInTheDocument();
    expect(screen.queryByRole('tablist', { name: 'Resource views' })).not.toBeInTheDocument();

    await user.click(within(switcher).getByRole('tab', { name: 'Cost' }));
    expect(search(router).get('view')).toBe('Cost');
    const view = await screen.findByRole('region', { name: 'Cost' });
    await waitFor(() => expect(view).toHaveTextContent(/^Cost of guestbook: [1-9]\d* resources$/));
    expect(screen.queryByRole('tree', { name: 'Resource tree of guestbook' })).not.toBeInTheDocument();

    await user.click(within(switcher).getByRole('tab', { name: 'List' }));
    expect(search(router).get('view')).toBe('list');
    expect(await screen.findByRole('table', { name: /Resources \(\d+\)/ }, LAZY)).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Cost' })).not.toBeInTheDocument();
  });

  it('opens an extension view from ?view=<title> and falls back to the tree for a hidden one', async () => {
    register();
    const first = renderApp('/applications/patrol/guestbook?view=Cost');
    expect(await screen.findByRole('region', { name: 'Cost' }, LAZY)).toHaveTextContent(/^Cost of guestbook/);
    expect(screen.getByRole('tab', { name: 'Cost' })).toHaveAttribute('aria-selected', 'true');
    first.unmount();

    renderApp('/applications/patrol/guestbook?view=Prod%20only');
    const switcher = await screen.findByRole('tablist', { name: 'Application views' }, LAZY);
    expect(within(switcher).queryByRole('tab', { name: 'Prod only' })).not.toBeInTheDocument();
    expect(within(switcher).getByRole('tab', { name: 'Tree' })).toHaveAttribute('aria-selected', 'true');
    expect(await screen.findByRole('tree', { name: 'Resource tree of guestbook' }, LAZY)).toBeInTheDocument();
    expect(screen.queryByText('Production only')).not.toBeInTheDocument();
  });

  it('honors a default-view annotation naming an extension, and ?view= over it', async () => {
    register();
    const app = appsCoreMockStore().apps.find((a) => a.metadata.name === 'guestbook');
    if (!app) throw new Error('guestbook fixture missing');
    app.metadata.annotations = { ...app.metadata.annotations, [DEFAULT_VIEW_ANNOTATION]: 'Cost' };
    const first = renderApp('/applications/patrol/guestbook');
    expect(await screen.findByRole('region', { name: 'Cost' }, LAZY)).toBeInTheDocument();
    first.unmount();

    renderApp('/applications/patrol/guestbook?view=pods');
    expect(await screen.findByRole('tab', { name: 'Pods' }, LAZY)).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByRole('region', { name: 'Cost' })).not.toBeInTheDocument();
  });

  it('keeps the resource views switcher when no view extension applies', async () => {
    installExtensionGlobals();
    window.extensionsAPI?.registerAppViewExtension(() => <p>Production only</p>, 'Prod only', 'fa-x', () => false);
    renderApp('/applications/patrol/guestbook');
    expect(await screen.findByRole('tablist', { name: 'Resource views' }, LAZY)).toBeInTheDocument();
    expect(screen.queryByRole('tablist', { name: 'Application views' })).not.toBeInTheDocument();
  });
});
