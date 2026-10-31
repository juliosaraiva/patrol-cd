import { focusManager } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { ClusterInput } from '../../../api/types';
import { clusterFixture, clustersFixture } from '../../../mocks/fixtures/settings';
import { settingsStore } from '../../../mocks/handlers/settings';
import { server } from '../../../mocks/server';
import { emptyGet, expectToast, findDialog, renderSettings, tableRows } from '../testUtils';
import { changedClusterFields, clusterPath } from './clusterUtil';

const PROD = 'https://prod-eu.k8s.example.test';

describe('Settings → Clusters list', () => {
  it('lists clusters with version, connection, applications, cache, shard, project and namespaces', async () => {
    await renderSettings('/settings/clusters', 'Clusters');
    await screen.findByRole('table', { name: 'Clusters' });
    const rows = tableRows('Clusters');
    expect(rows).toHaveLength(2);
    const prod = rows.find((r) => r.textContent?.includes(PROD));
    expect(prod).toHaveTextContent('1.32');
    expect(prod).toHaveTextContent('Failed');
    expect(prod).toHaveTextContent('dial tcp 10.0.0.12:443: i/o timeout');
    expect(prod).toHaveTextContent('finance');
    expect(prod).toHaveTextContent('payments, search');
    expect(prod?.querySelector('[data-status-family="connection"][data-status="Failed"]')).not.toBeNull();
    const local = rows.find((r) => r.textContent?.includes('in-cluster'));
    expect(local).toHaveTextContent('412 resources');
    expect(local).toHaveTextContent('All namespaces');
  });

  it('opens details with the keyboard (Enter on a row) using the encoded server URL', async () => {
    const user = userEvent.setup();
    const { router } = await renderSettings('/settings/clusters', 'Clusters');
    await screen.findByRole('table', { name: 'Clusters' });
    const rows = tableRows('Clusters');
    const first = rows[0];
    if (!first) throw new Error('no rows');
    first.focus();
    await user.keyboard('{Enter}');
    await screen.findByRole('heading', { level: 1, name: 'in-cluster' });
    expect(router.state.location.pathname).toBe(clusterPath('https://kubernetes.default.svc'));
  });

  it('explains how to add a cluster', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/clusters', 'Clusters');
    await screen.findByRole('table', { name: 'Clusters' });
    await user.click(screen.getByRole('button', { name: 'Add a cluster' }));
    const dialog = await findDialog('Add a cluster');
    expect(dialog).toHaveTextContent('patrol cluster add <kubeconfig-context>');
    expect(dialog).toHaveTextContent('credentialsRef');
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Add a cluster' })).toHaveFocus();
  });

  it('shows an empty state', async () => {
    emptyGet('*/api/v1/clusters');
    await renderSettings('/settings/clusters', 'Clusters');
    expect(await screen.findByRole('heading', { name: 'No clusters' })).toBeInTheDocument();
  });

  it('reports load errors', async () => {
    server.use(http.get('*/api/v1/clusters', () => HttpResponse.error()));
    await renderSettings('/settings/clusters', 'Clusters');
    expect(await screen.findByRole('alert')).toHaveTextContent('Patrol server unavailable');
  });
});

describe('Settings → Cluster details', () => {
  it('shows connection, general, cache information and conditions', async () => {
    await renderSettings(clusterPath(PROD), 'prod-eu');
    expect(screen.getByRole('heading', { name: 'Connection' })).toBeInTheDocument();
    expect(screen.getByText('Patrol cannot reach this cluster')).toBeInTheDocument();
    expect(screen.getByText('Last sync failed')).toBeInTheDocument();
    // From RegisteredCluster.credentials flags; values are never returned.
    expect(screen.getByText('Bearer token (custom CA)')).toBeInTheDocument();
    expect(screen.getByRole('list', { name: 'Labels' })).toHaveTextContent('env=prod');
    expect(screen.getByText('payments, search')).toBeInTheDocument();
  });

  it.each([
    { name: 'the implicit cluster, stamped on every read', inCluster: true, status: 'Failed', since: false, sync: 'Not reported' },
    { name: 'an object at the same server', inCluster: undefined, status: 'Failed', since: true, sync: 'Last sync failed' },
    { name: 'an object that has not failed', inCluster: undefined, status: 'Unknown', since: true, sync: 'Not synced yet' },
  ] as const)('claims only what it knows about $name', async ({ inCluster, status, since, sync }) => {
    const local = 'https://kubernetes.default.svc';
    await renderSettings(clusterPath(local), 'in-cluster', () => {
      const connectionState = { status, attemptedAt: '2026-10-15T08:00:00Z' };
      settingsStore.clusters = [{ ...clusterFixture('in-cluster', { server: local }, { connectionState }), inCluster }];
    });
    const value = (label: string) => screen.getByText(label).nextElementSibling;
    expect(value('State since')?.textContent !== '—').toBe(since);
    expect(value('Last cache sync')).toHaveTextContent(sync);
  });

  it('shows guidance for an unknown cluster (the server answers 403 for missing clusters)', async () => {
    await renderSettings(clusterPath('https://nope.example.test'), 'Cluster');
    expect(await screen.findByRole('heading', { name: 'Cluster not found' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to clusters' })).toHaveAttribute('href', '/settings/clusters');
  });

  it('edits name, namespaces and labels sending only the changed fields', async () => {
    const user = userEvent.setup();
    const calls: { fields: string[]; body: ClusterInput }[] = [];
    const prod = clustersFixture()[1];
    server.use(
      http.put('*/api/v1/clusters/:server', async ({ request }) => {
        const body = (await request.json()) as ClusterInput;
        calls.push({ fields: new URL(request.url).searchParams.getAll('updatedFields'), body });
        return HttpResponse.json({ ...prod, spec: { ...prod?.spec, name: body.name, namespaces: body.namespaces } });
      }),
    );
    await renderSettings(clusterPath(PROD), 'prod-eu');
    await user.click(screen.getByRole('button', { name: 'Edit' }));
    const dialog = await findDialog('Edit cluster');
    const name = within(dialog).getByRole('textbox', { name: 'Name' });
    await user.clear(name);
    await user.type(name, 'prod-europe');
    await user.click(within(dialog).getByRole('button', { name: 'Add namespace' }));
    expect(within(dialog).getByRole('textbox', { name: 'Namespace 3' })).toHaveFocus();
    await user.keyboard('Bad_NS');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Namespace 3: Use lowercase letters');
    await user.clear(within(dialog).getByRole('textbox', { name: 'Namespace 3' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Namespace 3' }), 'ledger');
    await user.click(within(dialog).getByRole('button', { name: 'Remove label 2' }));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Cluster updated');
    expect(calls).toHaveLength(1);
    expect(calls[0]?.fields).toEqual(['name', 'namespaces', 'labels']);
    // The flat ClusterInput; no `config`, so the stored credentials are untouched.
    expect(calls[0]?.body).toMatchObject({ server: PROD, name: 'prod-europe', namespaces: ['payments', 'search', 'ledger'] });
    expect(calls[0]?.body.labels).toEqual({ env: 'prod' });
    expect(calls[0]?.body.config).toBeUndefined();
    // A status write since the last poll must not refuse this save.
    expect(calls[0]?.body.resourceVersion).toBeUndefined();
  });

  it('leaves out of the save what another writer changed while the form was open', async () => {
    const user = userEvent.setup();
    const calls: string[][] = [];
    server.use(
      http.put('*/api/v1/clusters/:server', ({ request }) => {
        calls.push(new URL(request.url).searchParams.getAll('updatedFields'));
      }),
    );
    await renderSettings(clusterPath(PROD), 'prod-eu');
    await user.click(screen.getByRole('button', { name: 'Edit' }));
    const dialog = await findDialog('Edit cluster');
    const name = within(dialog).getByRole('textbox', { name: 'Name' });
    await user.clear(name);
    await user.type(name, 'prod-europe');
    const stored = settingsStore.clusters.find((c) => c.spec.server === PROD);
    if (!stored) throw new Error('no cluster');
    stored.metadata.labels = { ...stored.metadata.labels, oncall: 'sre' };
    // Stands in for the poll: the page behind the form shows the new label.
    focusManager.setFocused(false);
    focusManager.setFocused(true);
    try {
      await screen.findByText(/oncall/);
    } finally {
      focusManager.setFocused(undefined);
    }
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Cluster updated');
    expect(calls).toEqual([['name']]);
    expect(stored.metadata.labels).toMatchObject({ oncall: 'sre' });
  });

  it('invalidates the cache and rotates credentials with a confirmation', async () => {
    const user = userEvent.setup();
    let rotated = 0;
    server.use(
      http.post('*/api/v1/clusters/:server/rotate-auth', () => {
        rotated += 1;
        return HttpResponse.json({});
      }),
    );
    await renderSettings(clusterPath(PROD), 'prod-eu');
    await user.click(screen.getByRole('button', { name: 'Invalidate cache' }));
    await expectToast('Cache invalidated');
    await user.click(screen.getByRole('button', { name: 'Rotate credentials' }));
    const dialog = await findDialog('Rotate cluster credentials?');
    await user.click(within(dialog).getByRole('button', { name: 'Rotate' }));
    await expectToast('Credentials rotated');
    expect(rotated).toBe(1);
  });

  it('explains when credentials cannot be rotated', async () => {
    const user = userEvent.setup();
    await renderSettings(clusterPath('https://kubernetes.default.svc'), 'in-cluster');
    await user.click(screen.getByRole('button', { name: 'Rotate credentials' }));
    const dialog = await findDialog('Rotate cluster credentials?');
    await user.click(within(dialog).getByRole('button', { name: 'Rotate' }));
    expect(await within(dialog).findByText('Unable to rotate the cluster credentials')).toBeInTheDocument();
    expect(dialog).toHaveTextContent('no bearer token to rotate');
  });

  it('removes a cluster after typing its name, warning about its applications', async () => {
    const user = userEvent.setup();
    const { router } = await renderSettings(clusterPath('https://kubernetes.default.svc'), 'in-cluster');
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    const dialog = await findDialog('Remove cluster?');
    expect(dialog).toHaveTextContent('3 applications deploy to this cluster');
    const confirm = within(dialog).getByRole('button', { name: 'Remove cluster' });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByRole('textbox'), 'in-cluster');
    expect(confirm).toBeEnabled();
    await user.click(confirm);
    await expectToast('Cluster removed');
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings/clusters'));
    expect(settingsStore.clusters).toHaveLength(1);
  });
});

describe('changedClusterFields', () => {
  it('reports only changed fields', () => {
    const cluster = clustersFixture()[1];
    if (!cluster) throw new Error('fixture');
    const base = {
      name: cluster.spec.name,
      namespaces: [...(cluster.spec.namespaces ?? [])],
      clusterResources: false,
      project: cluster.spec.project ?? '',
      labels: Object.entries(cluster.metadata.labels ?? {}).map(([key, value]) => ({ key, value })),
      annotations: Object.entries(cluster.metadata.annotations ?? {}).map(([key, value]) => ({ key, value })),
    };
    expect(changedClusterFields(cluster, base)).toEqual([]);
    expect(changedClusterFields(cluster, { ...base, project: '', clusterResources: true })).toEqual(['clusterResources', 'project']);
    expect(changedClusterFields(cluster, { ...base, namespaces: [...base.namespaces, ' '] })).toEqual([]);
  });
});
