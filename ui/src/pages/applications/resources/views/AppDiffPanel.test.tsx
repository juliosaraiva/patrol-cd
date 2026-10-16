import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';
import { resourcesAppFixture } from '../../../../mocks/fixtures/apps-resources';
import { problem } from '../../../../mocks/handlers';
import { server } from '../../../../mocks/server';
import { DiffViewer } from '../shared/DiffViewer';
import { currentSearch, renderAt, useResourceHandlers } from '../test/helpers';
import { AppDiffPanel } from './AppDiffPanel';

beforeEach(() => useResourceHandlers());

describe('AppDiffPanel', () => {
  it('lists out-of-sync resources with paths, skipping hooks and identical renderings', async () => {
    renderAt(<AppDiffPanel app={resourcesAppFixture()} />);
    expect(await screen.findByRole('status')).toHaveTextContent('3 resources differ from the desired state.');
    const headings = screen.getAllByRole('heading', { level: 3 }).map((h) => h.textContent);
    expect(headings).toEqual(['apps/Deployment/shop/web', '/ConfigMap/shop/legacy', '/PersistentVolumeClaim/shop/data']);
    expect(screen.queryByText(/Job\/shop\/db-migrate/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Service\/shop\/web/)).not.toBeInTheDocument();
    expect(screen.getByText(/ignoreDifferences/)).toBeInTheDocument();
  });

  it('opens a resource through the URL or the callback', async () => {
    const user = userEvent.setup();
    const view = renderAt(<AppDiffPanel app={resourcesAppFixture()} />);
    await user.click(await screen.findByRole('button', { name: 'Open details of Deployment web' }));
    expect(currentSearch(view).get('node')).toBe('apps/Deployment/shop/web');
    expect(currentSearch(view).get('tab')).toBe('diff');
    view.unmount();

    const opened: string[] = [];
    renderAt(<AppDiffPanel app={resourcesAppFixture()} onOpenResource={(key) => opened.push(key)} />);
    await user.click(await screen.findByRole('button', { name: 'Open details of ConfigMap legacy' }));
    expect(opened).toEqual(['/ConfigMap/shop/legacy']);
  });

  it('collapses and expands entries and toggles the diff layout', async () => {
    const user = userEvent.setup();
    renderAt(<AppDiffPanel app={resourcesAppFixture()} />);
    const toggle = await screen.findByRole('button', { name: 'apps/Deployment/shop/web' });
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await user.click(screen.getByRole('button', { name: 'Collapse all' }));
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Expand all' }));
    expect(screen.getAllByRole('table')).toHaveLength(3);
    await user.click(screen.getByRole('switch', { name: 'Inline diff' }));
    expect(screen.getAllByRole('columnheader', { name: 'Live → Desired' })).toHaveLength(3);
  });

  it('shows an empty state when everything is in sync', async () => {
    server.use(http.get('*/api/v1/applications/:name/managed-resources', () => HttpResponse.json({ items: [] })));
    renderAt(<AppDiffPanel app={resourcesAppFixture()} />);
    expect(await screen.findByRole('heading', { name: 'No differences' })).toBeInTheDocument();
  });

  it('explains load failures', async () => {
    server.use(http.get('*/api/v1/applications/:name/managed-resources', () => problem(503, 'unavailable', 'controller tree API unavailable')));
    renderAt(<AppDiffPanel app={resourcesAppFixture()} />);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Could not load the differences');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });
});

describe('DiffViewer', () => {
  const old = Array.from({ length: 40 }, (_, i) => `line ${i}`).join('\n');
  const changed = old.replace('line 20', 'line twenty');

  it('marks changes with text, not color only, and collapses unchanged lines in compact mode', async () => {
    const user = userEvent.setup();
    renderAt(<DiffViewer caption="Diff of sample" oldText={old} newText={changed} compact inline />);
    const table = screen.getByRole('table', { name: 'Diff of sample' });
    expect(within(table).getByText('Removed')).toBeInTheDocument();
    expect(within(table).getByText('Added')).toBeInTheDocument();
    expect(screen.getByText(/1 lines added, 1 lines removed/)).toBeInTheDocument();
    const skips = within(table).getAllByRole('button', { name: /Show \d+ unchanged lines/ });
    expect(skips).toHaveLength(2);
    await user.click(skips[0]!);
    expect(within(table).getAllByRole('button', { name: /Show \d+ unchanged lines/ })).toHaveLength(1);
    expect(within(table).getByText('line 0')).toBeInTheDocument();
  });

  it('pairs changes side by side', () => {
    renderAt(<DiffViewer caption="Side" oldText={'a\nb\nc'} newText={'a\nB\nc'} compact={false} inline={false} />);
    const table = screen.getByRole('table', { name: 'Side' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(4);
    expect(rows[2]).toHaveTextContent(/2.*Removed.*b.*2.*Added.*B/);
  });

  it('renders only a window of very long diffs', () => {
    const big = Array.from({ length: 5000 }, (_, i) => `key${i}: ${i}`).join('\n');
    const bigChanged = big.replace('key2500: 2500', 'key2500: changed');
    renderAt(<DiffViewer caption="Big" oldText={big} newText={bigChanged} compact={false} inline />);
    const table = screen.getByRole('table', { name: 'Big' });
    expect(within(table).getAllByRole('row').length).toBeLessThan(200);
    expect(screen.getByText(/1 lines added, 1 lines removed/)).toBeInTheDocument();
  });
});
