import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { applicationSetsFixture } from '../../mocks/fixtures/applicationsets';
import { problem } from '../../mocks/handlers';
import { server } from '../../mocks/server';
import { renderApp } from '../../test/render';
import { parseLabelSelector } from './ApplicationSetsListPage';

async function openList(path = '/applicationsets') {
  const result = renderApp(path);
  await screen.findByRole('heading', { level: 1, name: 'ApplicationSets' });
  const table = await screen.findByRole('table', { name: 'ApplicationSets' });
  return { ...result, table };
}

function names(table: HTMLElement): string[] {
  return within(table)
    .getAllByRole('link')
    .map((a) => a.textContent ?? '');
}

describe('ApplicationSets list (API-118, ASET-124)', () => {
  it('lists every ApplicationSet with generators, project, health, conditions and application count', async () => {
    const { table } = await openList();
    expect(names(table)).toEqual(['clusters-addons', 'guestbook', 'shop-previews']);
    const broken = within(table).getByRole('row', { name: /clusters-addons/ });
    expect(broken).toHaveTextContent('Matrix (Git, Clusters)');
    expect(broken).toHaveTextContent('team-infra');
    expect(broken).toHaveTextContent('infra');
    expect(broken).toHaveTextContent('Degraded');
    expect(broken).toHaveTextContent('2 need attention');
    const guestbook = within(table).getByRole('row', { name: /guestbook/ });
    expect(guestbook).toHaveTextContent('Progressing');
    expect(within(guestbook).getAllByRole('cell')[5]).toHaveTextContent('2');
    expect(screen.getByRole('list', { name: 'Health summary' })).toHaveTextContent('3 of 3 ApplicationSets');
  });

  it('filters by health through the URL and clears filters', async () => {
    const user = userEvent.setup();
    const { router } = await openList();
    await user.selectOptions(screen.getByRole('combobox', { name: 'Health' }), 'Degraded');
    expect(router.state.location.search).toBe('?health=Degraded');
    const table = screen.getByRole('table', { name: 'ApplicationSets' });
    expect(names(table)).toEqual(['clusters-addons']);
    await user.click(screen.getByRole('button', { name: 'Remove filter Health: Degraded' }));
    expect(router.state.location.search).toBe('');
    expect(names(screen.getByRole('table', { name: 'ApplicationSets' }))).toHaveLength(3);
  });

  it('restores filters from a deep link and explains when nothing matches', async () => {
    const user = userEvent.setup();
    renderApp('/applicationsets?namespace=team-infra&search=guest');
    expect(await screen.findByRole('heading', { name: 'No ApplicationSets match the filters' })).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Namespace' })).toHaveValue('team-infra');
    expect(screen.getByRole('searchbox', { name: 'Search ApplicationSets' })).toHaveValue('guest');
    await user.click(screen.getAllByRole('button', { name: 'Clear all filters' })[0]!);
    expect(await screen.findByRole('table', { name: 'ApplicationSets' })).toBeInTheDocument();
  });

  it('filters by project and by label selector', async () => {
    const user = userEvent.setup();
    const { router } = await openList();
    await user.selectOptions(screen.getByRole('combobox', { name: 'Project' }), 'previews');
    expect(names(screen.getByRole('table', { name: 'ApplicationSets' }))).toEqual(['shop-previews']);
    await user.selectOptions(screen.getByRole('combobox', { name: 'Project' }), '');
    const labels = screen.getByRole('textbox', { name: 'Labels' });
    await user.type(labels, 'team=web{Enter}');
    expect(router.state.location.search).toBe('?labels=team%3Dweb');
    expect(names(screen.getByRole('table', { name: 'ApplicationSets' }))).toEqual(['guestbook']);
  });

  it('focuses the page search with "/" and opens a row with Enter', async () => {
    const user = userEvent.setup();
    const { router } = await openList();
    await user.click(screen.getByRole('heading', { level: 1 }));
    await user.keyboard('/');
    const search = screen.getByRole('searchbox', { name: 'Search ApplicationSets' });
    expect(search).toHaveFocus();
    await user.keyboard('shop');
    await waitFor(() => expect(router.state.location.search).toBe('?search=shop'));
    expect(names(screen.getByRole('table', { name: 'ApplicationSets' }))).toEqual(['shop-previews']);
    const row = within(screen.getByRole('table', { name: 'ApplicationSets' })).getByRole('row', { name: /shop-previews/ });
    row.focus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/applicationsets/shop-previews'));
  });

  it('links namespaced ApplicationSets with the namespace segment', async () => {
    const { table } = await openList();
    expect(within(table).getByRole('link', { name: 'clusters-addons' })).toHaveAttribute('href', '/applicationsets/team-infra/clusters-addons');
    expect(within(table).getByRole('link', { name: 'guestbook' })).toHaveAttribute('href', '/applicationsets/guestbook');
  });

  it('guides first-time users when there are no ApplicationSets', async () => {
    server.use(http.get('*/api/v1/applicationsets', () => HttpResponse.json({ items: [], metadata: { resourceVersion: '1' } })));
    renderApp('/applicationsets');
    expect(await screen.findByRole('heading', { name: 'No ApplicationSets yet' })).toBeInTheDocument();
    expect(screen.getByLabelText('Create command')).toHaveTextContent('patrol appset create applicationset.yaml');
    expect(screen.getByRole('link', { name: 'Get the patrol CLI' })).toHaveAttribute('href', '/help');
  });

  it('explains a permission error with the next step', async () => {
    server.use(http.get('*/api/v1/applicationsets', () => problem(403, 'permission_denied', 'permission denied: applicationsets, get')));
    renderApp('/applicationsets');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('ApplicationSets could not be loaded');
    expect(alert).toHaveTextContent('Ask an administrator for the required RBAC permission');
  });

  it('shows a busy state while loading', async () => {
    let release: () => void = () => undefined;
    server.use(
      http.get('*/api/v1/applicationsets', async () => {
        await new Promise<void>((r) => {
          release = r;
        });
        return HttpResponse.json(applicationSetsFixture);
      }),
    );
    renderApp('/applicationsets');
    expect(await screen.findByText('Loading ApplicationSets')).toBeInTheDocument();
    release();
    expect(await screen.findByRole('table', { name: 'ApplicationSets' })).toBeInTheDocument();
  });
});

describe('parseLabelSelector', () => {
  it('parses equality, inequality and existence terms', () => {
    expect(parseLabelSelector('team=web, env!=prod,owner')).toEqual([
      { key: 'team', op: '=', value: 'web' },
      { key: 'env', op: '!=', value: 'prod' },
      { key: 'owner', op: 'exists', value: '' },
    ]);
    expect(parseLabelSelector('a b')).toBeNull();
    expect(parseLabelSelector('')).toEqual([]);
  });
});
