import { act, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { useLocation } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Application, ApplicationTree } from '../api/types';
import { Tabs } from '../design';
import { applicationFixture } from '../mocks/fixtures';
import { renderWithProviders } from '../test/render';
import { installExtensionGlobals } from './loader';
import {
  registerAppViewExtension,
  registerResourceExtension,
  registerStatusPanelExtension,
  registerSystemLevelExtension,
  registerTopBarActionMenuExt,
  resetExtensionRegistry,
} from './registry';
import {
  AppViewExtensions,
  ResourceExtensionTabs,
  StatusPanelExtensions,
  SystemExtensionRoutes,
  TopBarExtensionActions,
  useAppViewExtensions,
  useResourceExtensionTabs,
  useSystemExtensionNavItems,
} from './slots';
import type { ExtensionComponentProps, FlyoutComponentProps, StatusPanelComponentProps } from './types';

const app: Application = applicationFixture('guestbook', { project: 'default' });
const tree: ApplicationTree = { nodes: [{ kind: 'Deployment', group: 'apps', name: 'guestbook-ui', namespace: 'guestbook' }] };
const deployment = { apiVersion: 'apps/v1', kind: 'Deployment', metadata: { name: 'guestbook-ui', namespace: 'guestbook' } };

afterEach(() => {
  resetExtensionRegistry();
  delete window.extensions;
  delete window.extensionsAPI;
});

function Location() {
  const location = useLocation();
  return <output aria-label="location">{`${location.pathname}${location.search}`}</output>;
}

describe('resource extension tabs (API-121)', () => {
  it('renders matching tabs sorted by title with application, resource and tree props', async () => {
    const user = userEvent.setup();
    const Summary = ({ resource, application, tree: t }: ExtensionComponentProps) => (
      <p>
        {resource.metadata?.name} in {application.metadata.name} ({t.nodes?.length} nodes)
      </p>
    );
    registerResourceExtension(Summary, 'apps', 'Deployment', 'Summary ext');
    registerResourceExtension(() => <p>Every resource</p>, '**', '*', 'Alpha');
    registerResourceExtension(() => <p>Pods only</p>, '**', 'Pod', 'Pods');

    renderWithProviders(<ResourceExtensionTabs group="apps" kind="Deployment" application={app} resource={deployment} tree={tree} />);
    const tabs = screen.getAllByRole('tab');
    expect(tabs.map((t) => t.textContent)).toEqual(['Alpha', 'Summary ext']);
    await user.click(screen.getByRole('tab', { name: 'Summary ext' }));
    expect(screen.getByText('guestbook-ui in guestbook (1 nodes)')).toBeInTheDocument();
  });

  it('adds Argo-style application tabs (argoproj.io/Application) to Patrol Applications', () => {
    registerResourceExtension(() => <p>App tab</p>, 'argoproj.io', 'Application', 'App ext');
    function Panel() {
      const ext = useResourceExtensionTabs({ group: 'cd.patrol.example', kind: 'Application', application: app, resource: app, tree });
      return <Tabs label="Application" items={[{ id: 'summary', label: 'Summary', content: <p>Built in</p> }, ...ext]} />;
    }
    renderWithProviders(<Panel />);
    expect(screen.getAllByRole('tab').map((t) => t.textContent)).toEqual(['Summary', 'App ext']);
  });

  it('contains a crashing extension and tells the user what failed', async () => {
    const user = userEvent.setup();
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const Broken = () => {
      throw new Error('boom');
    };
    registerResourceExtension(Broken, '*', 'Deployment', 'Broken');
    renderWithProviders(<ResourceExtensionTabs group="apps" kind="Deployment" application={app} resource={deployment} tree={tree} />);
    await user.click(screen.getByRole('tab', { name: 'Broken' }));
    expect(screen.getByText('Something went wrong with Extension for Deployment')).toBeInTheDocument();
  });

  it('shows tabs registered after the panel rendered, including hook-based components using window.ReactJSXRuntime', async () => {
    const user = userEvent.setup();
    installExtensionGlobals();
    renderWithProviders(<ResourceExtensionTabs group="apps" kind="Deployment" application={app} resource={deployment} tree={tree} />);
    expect(screen.queryByRole('tab')).not.toBeInTheDocument();

    // An extension compiled with the automatic runtime and externals {'react/jsx-runtime': 'ReactJSXRuntime'}.
    const { jsx, jsxs } = window.ReactJSXRuntime!;
    const Counter = () => {
      const [count, setCount] = window.React.useState(0);
      return jsxs('div', { children: [jsx('span', { children: `Clicked ${count}` }), jsx('button', { type: 'button', onClick: () => setCount((c) => c + 1), children: 'Increment' })] });
    };
    act(() => window.extensionsAPI!.registerResourceExtension(Counter, '**', '*', 'Counter'));
    await user.click(await screen.findByRole('tab', { name: 'Counter' }));
    await user.click(screen.getByRole('button', { name: 'Increment' }));
    expect(screen.getByText('Clicked 1')).toBeInTheDocument();
  });
});

describe('app view extensions (API-123)', () => {
  it('lists views whose shouldDisplay accepts the application and renders the selected one', () => {
    registerAppViewExtension(({ application }) => <p>Cost of {application.metadata.name}</p>, 'Cost', 'fa-dollar');
    registerAppViewExtension(() => <p>Hidden</p>, 'Prod only', 'fa-x', (a) => a.spec.project === 'prod');
    function Views() {
      const views = useAppViewExtensions(app);
      return (
        <>
          <ul aria-label="Views">
            {views.map((v) => (
              <li key={v.title}>{v.title}</li>
            ))}
          </ul>
          <AppViewExtensions application={app} tree={tree} view="Cost" />
        </>
      );
    }
    renderWithProviders(<Views />);
    expect(within(screen.getByRole('list', { name: 'Views' })).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['Cost']);
    expect(screen.getByRole('region', { name: 'Cost' })).toHaveTextContent('Cost of guestbook');
  });

  it('renders nothing for built-in views', () => {
    registerAppViewExtension(() => <p>Cost</p>, 'Cost', 'fa-dollar');
    renderWithProviders(<AppViewExtensions application={app} tree={tree} view="tree" />);
    expect(screen.queryByRole('region', { name: 'Cost' })).not.toBeInTheDocument();
    expect(screen.queryByText('Cost')).not.toBeInTheDocument();
  });
});

describe('status panel extensions (API-124)', () => {
  it('renders the item and opens and closes its flyout through ?extension=<id>', async () => {
    const user = userEvent.setup();
    const Item = ({ application, openFlyout }: StatusPanelComponentProps) => (
      <button type="button" onClick={openFlyout}>
        Rollout of {application.metadata.name}
      </button>
    );
    const Flyout = ({ application, tree: t }: FlyoutComponentProps) => (
      <p>
        Flyout for {application.metadata.name} with {t.nodes?.length} nodes
      </p>
    );
    registerStatusPanelExtension(Item, 'Rollout', 'rollout', Flyout);
    renderWithProviders(
      <>
        <StatusPanelExtensions application={app} tree={tree} />
        <Location />
      </>,
      { path: '/applications/guestbook' },
    );
    const trigger = within(screen.getByRole('group', { name: 'Rollout' })).getByRole('button', { name: 'Rollout of guestbook' });
    await user.click(trigger);
    const dialog = await screen.findByRole('dialog', { name: 'Rollout' });
    expect(dialog).toHaveTextContent('Flyout for guestbook with 1 nodes');
    expect(screen.getByRole('status', { name: 'location' })).toHaveTextContent('/applications/guestbook?extension=rollout');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'location' })).toHaveTextContent(/^\/applications\/guestbook$/);
    expect(trigger).toHaveFocus();
  });

  it('opens the flyout from a deep link', async () => {
    registerStatusPanelExtension(() => <span>Item</span>, 'Rollout', 'rollout', () => <p>Deep linked</p>);
    renderWithProviders(<StatusPanelExtensions application={app} tree={tree} />, { path: '/applications/guestbook?extension=rollout' });
    expect(await screen.findByRole('dialog', { name: 'Rollout' })).toHaveTextContent('Deep linked');
  });
});

describe('top bar extensions (API-125)', () => {
  it('shows actions allowed by shouldDisplay and opens their flyout', async () => {
    const user = userEvent.setup();
    registerTopBarActionMenuExt(() => <span>Promote</span>, 'Promote release', 'promote', () => <p>Promotion form</p>);
    registerTopBarActionMenuExt(() => <span>Prod</span>, 'Prod only', 'prod', () => <p>x</p>, (a) => a?.spec.project === 'prod');
    registerTopBarActionMenuExt(() => <span>Middle</span>, 'Middle action', 'middle', undefined, undefined, undefined, true);

    renderWithProviders(<TopBarExtensionActions application={app} tree={tree} placement="end" />);
    expect(screen.queryByRole('button', { name: 'Prod only' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Middle action' })).not.toBeInTheDocument();
    const action = screen.getByRole('button', { name: 'Promote release' });
    expect(action).toHaveTextContent('Promote');
    await user.click(action);
    expect(await screen.findByRole('dialog', { name: 'Promote release' })).toHaveTextContent('Promotion form');
  });

  it('places isMiddle actions in the middle group', () => {
    registerTopBarActionMenuExt(() => <span>Middle</span>, 'Middle action', 'middle', undefined, undefined, undefined, true);
    renderWithProviders(<TopBarExtensionActions application={app} tree={tree} placement="middle" />);
    expect(screen.getByRole('button', { name: 'Middle action' })).toBeInTheDocument();
  });
});

describe('system level extensions (API-122)', () => {
  function Nav() {
    const items = useSystemExtensionNavItems();
    return (
      <nav aria-label="Extensions">
        {items.map((i) => (
          <a key={i.to} href={i.to}>
            {i.label}
          </a>
        ))}
      </nav>
    );
  }

  it('adds a navigation entry and renders the page at its path with a page heading and title', async () => {
    registerSystemLevelExtension(() => <p>Hello World</p>, 'Test Ext', '/hello', 'fa-flask');
    renderWithProviders(
      <>
        <Nav />
        <SystemExtensionRoutes fallback={<p>Not found</p>} />
      </>,
      { path: '/hello' },
    );
    expect(within(screen.getByRole('navigation', { name: 'Extensions' })).getByRole('link', { name: 'Test Ext' })).toHaveAttribute('href', '/hello');
    expect(screen.getByRole('heading', { level: 1, name: 'Test Ext' })).toBeInTheDocument();
    expect(screen.getByText('Hello World')).toBeInTheDocument();
    await vi.waitFor(() => expect(document.title).toBe('Test Ext · Patrol CD'));
  });

  it('falls back to the given element for unknown paths and switches when the extension registers', () => {
    renderWithProviders(<SystemExtensionRoutes fallback={<p>Not found</p>} />, { path: '/late' });
    expect(screen.getByText('Not found')).toBeInTheDocument();
    act(() => registerSystemLevelExtension(() => <p>Late page</p>, 'Late', 'late'));
    expect(screen.getByText('Late page')).toBeInTheDocument();
  });

  it('keeps a stateful component when the page re-renders', async () => {
    const user = userEvent.setup();
    const Stateful = () => {
      const [n, setN] = useState(0);
      return (
        <button type="button" onClick={() => setN(n + 1)}>
          Count {n}
        </button>
      );
    };
    registerSystemLevelExtension(Stateful, 'Stateful', '/stateful');
    renderWithProviders(<SystemExtensionRoutes fallback={null} />, { path: '/stateful' });
    await user.click(screen.getByRole('button', { name: 'Count 0' }));
    act(() => registerSystemLevelExtension(() => null, 'Other', '/other'));
    expect(screen.getByRole('button', { name: 'Count 1' })).toBeInTheDocument();
  });
});
