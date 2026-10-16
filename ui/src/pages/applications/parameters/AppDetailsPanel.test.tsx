import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { useState } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Application } from '../../../api/types';
import { guestbookApp } from '../../../mocks/fixtures/apps-core';
import { problem } from '../../../mocks/handlers';
import { appsCoreMockStore, resetAppsCoreMockState } from '../../../mocks/handlers/apps-core';
import { server } from '../../../mocks/server';
import { renderApp, renderWithProviders } from '../../../test/render';
import { cloneJSON } from '../shared/app';
import { AppDetailsPanel } from './AppDetailsPanel';
import { manifestYAML } from './specEdit';

vi.mock('../resources/contract.ts', () => ({ ResourceViewsSlot: undefined, AppDiffPanel: undefined }));

// Whole-app integration tests (real routes, lazy pages, MSW): allow for a loaded CI machine.
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => resetAppsCoreMockState());

function capturePut(name: string) {
  const saved: Application[] = [];
  server.use(
    http.put(`*/api/v1/applications/${name}`, async ({ request }) => {
      saved.push((await request.clone().json()) as Application);
      return undefined;
    }),
  );
  return saved;
}

async function openDetails(name: string, tab = '') {
  const result = renderApp(`/applications/patrol/${name}?panel=details${tab ? `&appTab=${tab}` : ''}`);
  const drawer = await screen.findByRole('dialog', { name: `${name} details` });
  return { ...result, drawer };
}

async function replaceText(user: ReturnType<typeof userEvent.setup>, element: HTMLElement, text: string) {
  await user.clear(element);
  await user.click(element);
  await user.paste(text);
}

describe('summary tab', () => {
  it('shows settings, links and the status badge snippet', async () => {
    const user = userEvent.setup({ delay: null });
    const { drawer } = await openDetails('guestbook');
    expect(within(drawer).getByRole('tab', { name: 'Summary' })).toHaveAttribute('aria-selected', 'true');
    expect(within(drawer).getByText('Automated, prune, self-heal')).toBeInTheDocument();
    expect(await within(drawer).findByRole('link', { name: 'Runbook' })).toHaveAttribute('href', 'https://runbooks.example.test/guestbook');
    expect(within(drawer).getByRole('link', { name: 'Dashboard' })).toHaveAttribute('rel', 'noopener noreferrer');
    expect(within(drawer).getByRole('img', { name: 'Status badge of guestbook' })).toHaveAttribute('src', expect.stringContaining('api/badge?name=guestbook&revision=true&showAppName=true'));
    await user.selectOptions(within(drawer).getByRole('combobox', { name: 'Snippet format' }), 'Markdown');
    const snippet = within(drawer).getByLabelText('Badge Markdown');
    expect(snippet.textContent).toMatch(/^\[!\[App Status\]\(http.*api\/badge\?name=guestbook&revision=true&showAppName=true\)\]\(http.*\/applications\/guestbook\)$/);
  });

  it('edits labels and the sync policy, then saves with the loaded resourceVersion', async () => {
    const user = userEvent.setup({ delay: null });
    const saved = capturePut('guestbook');
    const { drawer } = await openDetails('guestbook');
    await user.click(within(drawer).getByRole('button', { name: 'Edit summary' }));
    expect(within(drawer).getByRole('tab', { name: 'Summary (editing)' })).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Add label' }));
    await user.type(within(drawer).getByRole('textbox', { name: 'Key of label 3' }), 'tier');
    await user.type(within(drawer).getByRole('textbox', { name: 'Value of label 3' }), 'frontend');
    await user.click(within(drawer).getByRole('switch', { name: 'Self heal' }));
    await user.click(within(drawer).getByRole('button', { name: 'Save summary' }));
    expect(await screen.findByText('Summary saved')).toBeInTheDocument();
    expect(saved[0]?.metadata.labels).toEqual({ team: 'web', env: 'prod', tier: 'frontend' });
    expect(saved[0]?.metadata.resourceVersion).toBe('1');
    expect(saved[0]?.spec.syncPolicy?.automated).toEqual({ enabled: true, prune: true, selfHeal: false });
    await waitFor(() => expect(within(drawer).getByRole('tab', { name: 'Summary' })).toBeInTheDocument());
  });

  it('validates fields before saving', async () => {
    const user = userEvent.setup({ delay: null });
    const saved = capturePut('guestbook');
    const { drawer } = await openDetails('guestbook');
    await user.click(within(drawer).getByRole('button', { name: 'Edit summary' }));
    await user.click(within(drawer).getByRole('button', { name: 'Add label' }));
    await user.type(within(drawer).getByRole('textbox', { name: 'Key of label 3' }), 'bad key');
    await user.clear(within(drawer).getByRole('textbox', { name: 'Revision history limit' }));
    await user.type(within(drawer).getByRole('textbox', { name: 'Revision history limit' }), 'ten');
    await user.click(within(drawer).getByRole('button', { name: 'Save summary' }));
    expect(within(drawer).getByRole('alert')).toHaveTextContent('Some fields need attention');
    expect(within(drawer).getByRole('textbox', { name: 'Key of label 3' })).toHaveAttribute('aria-invalid', 'true');
    expect(within(drawer).getByText('Enter a whole number of at least 0.')).toBeInTheDocument();
    expect(saved).toHaveLength(0);
  });

  it('merges the draft onto the latest version after a conflict, keeping concurrent changes', async () => {
    const user = userEvent.setup({ delay: null });
    const saved: Application[] = [];
    let calls = 0;
    server.use(
      http.put('*/api/v1/applications/guestbook', async ({ request }) => {
        calls += 1;
        saved.push((await request.clone().json()) as Application);
        if (calls === 1) return problem(409, 'conflict', 'the object has been modified; please apply your changes to the latest version');
        return undefined;
      }),
    );
    const { drawer } = await openDetails('guestbook');
    await user.click(within(drawer).getByRole('button', { name: 'Edit summary' }));
    const store = appsCoreMockStore().apps.find((a) => a.metadata.name === 'guestbook');
    if (store) {
      store.metadata.resourceVersion = '7';
      store.metadata.annotations = { ...store.metadata.annotations, 'example.test/changed-by': 'someone-else' };
    }
    await user.clear(within(drawer).getByRole('textbox', { name: 'Namespace' }));
    await user.type(within(drawer).getByRole('textbox', { name: 'Namespace' }), 'guestbook-v2');
    await user.click(within(drawer).getByRole('button', { name: 'Save summary' }));
    const alert = await within(drawer).findByRole('alert');
    expect(alert).toHaveTextContent('The application changed since you loaded it');
    await user.click(within(alert).getByRole('button', { name: 'Reload latest' }));
    await waitFor(() => expect(within(drawer).queryByRole('alert')).not.toBeInTheDocument());
    // The edit is merged onto the latest version, and the merge is shown before saving.
    const merged = within(drawer).getByText('Your edits were merged onto the latest version').closest('[data-tone]');
    if (!(merged instanceof HTMLElement)) throw new Error('rebase notice missing');
    expect(within(merged).getByText('metadata.annotations["example.test/changed-by"]')).toBeInTheDocument();
    expect(within(drawer).getByRole('textbox', { name: 'Namespace' })).toHaveValue('guestbook-v2');
    expect(within(drawer).getByRole('textbox', { name: 'Key of annotation 2' })).toHaveValue('example.test/changed-by');
    await user.click(within(drawer).getByRole('button', { name: 'Save summary' }));
    expect(await screen.findByText('Summary saved')).toBeInTheDocument();
    expect(saved[1]?.metadata.resourceVersion).toBe('7');
    expect(saved[1]?.spec.destination.namespace).toBe('guestbook-v2');
    // The concurrent change survives the save.
    expect(saved[1]?.metadata.annotations).toEqual({
      'link.cd.patrol.example/dashboard': 'Dashboard|https://grafana.example.test/d/guestbook',
      'example.test/changed-by': 'someone-else',
    });
  });
});

describe('parameters and sources tab', () => {
  it('edits Helm parameters with override markers on a multi-source application', async () => {
    const user = userEvent.setup({ delay: null });
    server.use(
      http.post('*/api/v1/repositories/:repo/appdetails', () =>
        HttpResponse.json({ type: 'Helm', helm: { valueFiles: ['values.yaml'], parameters: [{ name: 'image.tag', value: '1.4.2' }, { name: 'replicaCount', value: '1' }] } }),
      ),
    );
    const saved = capturePut('payments');
    const { drawer } = await openDetails('payments', 'parameters');
    expect(within(drawer).getByRole('tab', { name: 'Sources' })).toHaveAttribute('aria-selected', 'true');
    await user.click(within(drawer).getByRole('button', { name: 'Edit sources' }));
    const source1 = within(drawer).getByRole('group', { name: 'Source 1' });
    const table = await within(source1).findByRole('table', { name: 'Helm parameters' });
    await within(table).findByRole('row', { name: /image\.tag/ });
    expect(within(table).getByRole('row', { name: /replicaCount.*Overridden/ })).toBeInTheDocument();
    expect(within(table).getByRole('row', { name: /image\.tag.*Chart default/ })).toBeInTheDocument();
    const tag = within(table).getByRole('textbox', { name: 'Value of image.tag' });
    await user.clear(tag);
    await user.type(tag, '1.5.0');
    expect(within(table).getByRole('row', { name: /image\.tag.*Overridden/ })).toBeInTheDocument();
    await user.click(within(table).getByRole('button', { name: 'Reset replicaCount' }));
    expect(within(table).getByRole('textbox', { name: 'Value of replicaCount' })).toHaveValue('1');
    await user.click(within(drawer).getByRole('button', { name: 'Save sources' }));
    expect(await screen.findByText('Sources saved')).toBeInTheDocument();
    expect(saved[0]?.spec.sources?.[0]?.helm?.parameters).toEqual([{ name: 'image.tag', value: '1.5.0' }]);
    expect(saved[0]?.spec.sources?.[1]).toEqual({ repoURL: 'https://git.example.test/platform/apps.git', targetRevision: 'main', ref: 'values' });
  });

  it('validates refs and adds or removes sources', async () => {
    const user = userEvent.setup({ delay: null });
    const saved = capturePut('payments');
    const { drawer } = await openDetails('payments', 'parameters');
    await user.click(within(drawer).getByRole('button', { name: 'Edit sources' }));
    const source2 = within(drawer).getByRole('group', { name: 'Source 2' });
    const ref = within(source2).getByRole('textbox', { name: 'Ref' });
    await user.clear(ref);
    await user.type(ref, 'bad ref');
    await user.click(within(drawer).getByRole('button', { name: 'Add source' }));
    expect(within(drawer).getByRole('group', { name: 'Source 3' })).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Save sources' }));
    expect(within(source2).getByText('Use letters, digits, "-" or "_".')).toBeInTheDocument();
    expect(within(within(drawer).getByRole('group', { name: 'Source 3' })).getByText('Enter the repository URL.')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Remove source 3' }));
    await user.clear(ref);
    await user.type(ref, 'values');
    await user.click(within(drawer).getByRole('button', { name: 'Save sources' }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]?.spec.sources).toHaveLength(2);
  });

  it('edits Kustomize images and validates patches YAML', async () => {
    const user = userEvent.setup({ delay: null });
    const saved = capturePut('search');
    const { drawer } = await openDetails('search', 'parameters');
    await user.click(within(drawer).getByRole('button', { name: 'Edit parameters' }));
    const images = within(drawer).getByRole('textbox', { name: 'Images' });
    await replaceText(user, images, 'search=registry.example.test/search:2.0.0\nsidecar:1.1');
    const patches = within(drawer).getByRole('textbox', { name: 'Patches' });
    await replaceText(user, patches, 'target: [');
    await user.click(within(drawer).getByRole('button', { name: 'Save parameters' }));
    expect(within(drawer).getByText(/^Line 1, column/)).toBeInTheDocument();
    await replaceText(user, patches, '- path: patches/replicas.yaml\n');
    await user.click(within(drawer).getByRole('button', { name: 'Save parameters' }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]?.spec.source?.kustomize).toEqual({
      images: ['search=registry.example.test/search:2.0.0', 'sidecar:1.1'],
      namePrefix: 'prod-',
      patches: [{ path: 'patches/replicas.yaml' }],
    });
  });

  it('adds Kustomize common labels and annotations, keeping a row while its key is retyped', async () => {
    const user = userEvent.setup({ delay: null });
    const saved = capturePut('search');
    const { drawer } = await openDetails('search', 'parameters');
    await user.click(within(drawer).getByRole('button', { name: 'Edit parameters' }));
    await user.click(within(drawer).getByRole('button', { name: 'Add common label' }));
    const key = within(drawer).getByRole('textbox', { name: 'Key of common label 1' });
    expect(key).toHaveValue('');
    await user.type(key, 'team');
    await user.clear(key);
    // Clearing the key keeps the row (and the focus) instead of dropping it.
    expect(within(drawer).getByRole('textbox', { name: 'Key of common label 1' })).toBe(key);
    expect(key).toHaveFocus();
    await user.type(key, 'tier');
    await user.type(within(drawer).getByRole('textbox', { name: 'Value of common label 1' }), 'backend');
    await user.click(within(drawer).getByRole('button', { name: 'Add common label' }));
    await user.type(within(drawer).getByRole('textbox', { name: 'Key of common label 2' }), 'bad key');
    await user.click(within(drawer).getByRole('button', { name: 'Add common annotation' }));
    await user.type(within(drawer).getByRole('textbox', { name: 'Key of common annotation 1' }), 'example.test/owner');
    await user.type(within(drawer).getByRole('textbox', { name: 'Value of common annotation 1' }), 'search-team');
    await user.click(within(drawer).getByRole('button', { name: 'Save parameters' }));
    expect(within(drawer).getByRole('textbox', { name: 'Key of common label 2' })).toHaveAttribute('aria-invalid', 'true');
    expect(saved).toHaveLength(0);
    await user.click(within(drawer).getByRole('button', { name: 'Remove common label 2' }));
    await user.click(within(drawer).getByRole('button', { name: 'Save parameters' }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]?.spec.source?.kustomize).toEqual({
      images: ['search=registry.example.test/search:1.9.0'],
      namePrefix: 'prod-',
      commonLabels: { tier: 'backend' },
      commonAnnotations: { 'example.test/owner': 'search-team' },
    });
  });

  it('adds entries to a map parameter announced by the plugin', async () => {
    const user = userEvent.setup({ delay: null });
    const app = appsCoreMockStore().apps.find((a) => a.metadata.name === 'search');
    if (app) app.spec.source = { repoURL: 'https://git.example.test/platform/apps.git', path: 'search', targetRevision: 'main', plugin: { name: 'cue' } };
    server.use(
      http.post('*/api/v1/repositories/:repo/appdetails', () =>
        HttpResponse.json({ type: 'Plugin', plugin: { parametersAnnouncement: [{ name: 'values', title: 'Values', collectionType: 'map', map: { replicas: '1' } }] } }),
      ),
    );
    const saved = capturePut('search');
    const { drawer } = await openDetails('search', 'parameters');
    await user.click(within(drawer).getByRole('button', { name: 'Edit parameters' }));
    const values = await within(drawer).findByRole('group', { name: 'Values' });
    expect(within(values).getByRole('textbox', { name: 'Key of Values entry 1' })).toHaveValue('replicas');
    await user.click(within(values).getByRole('button', { name: 'Add Values entry' }));
    const key = within(values).getByRole('textbox', { name: 'Key of Values entry 2' });
    await user.type(key, 'image');
    await user.type(within(values).getByRole('textbox', { name: 'Value of Values entry 2' }), 'v2');
    await user.click(within(drawer).getByRole('button', { name: 'Save parameters' }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]?.spec.source?.plugin).toEqual({ name: 'cue', parameters: [{ name: 'values', map: { replicas: '1', image: 'v2' } }] });
  });

  it('renders plugin parameters from the announcement', async () => {
    const user = userEvent.setup({ delay: null });
    const app = appsCoreMockStore().apps.find((a) => a.metadata.name === 'search');
    if (app) app.spec.source = { repoURL: 'https://git.example.test/platform/apps.git', path: 'search', targetRevision: 'main', plugin: { name: 'cue' } };
    server.use(
      http.post('*/api/v1/repositories/:repo/appdetails', () =>
        HttpResponse.json({
          type: 'Plugin',
          plugin: { parametersAnnouncement: [{ name: 'environment', title: 'Environment', tooltip: 'Target environment', required: true, string: 'staging' }] },
        }),
      ),
    );
    const saved = capturePut('search');
    const { drawer } = await openDetails('search', 'parameters');
    await user.click(within(drawer).getByRole('button', { name: 'Edit parameters' }));
    const env = await within(drawer).findByRole('textbox', { name: /^Environment/ });
    expect(env).toHaveValue('staging');
    expect(env).toHaveAccessibleDescription('Target environment');
    expect(within(drawer).getByText(/Available plugins: cue, tanka/)).toBeInTheDocument();
    await user.clear(env);
    await user.type(env, 'prod');
    await user.click(within(drawer).getByRole('button', { name: 'Save parameters' }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]?.spec.source?.plugin).toEqual({ name: 'cue', parameters: [{ name: 'environment', string: 'prod' }] });
  });
});

describe('manifest tab', () => {
  it('validates the YAML and saves the edited spec', async () => {
    const user = userEvent.setup({ delay: null });
    const saved = capturePut('guestbook');
    const { drawer, router } = await openDetails('guestbook');
    await user.click(within(drawer).getByRole('tab', { name: 'Manifest' }));
    expect(new URLSearchParams(router.state.location.search).get('appTab')).toBe('manifest');
    expect(within(drawer).getByRole<HTMLTextAreaElement>('textbox', { name: 'Application manifest (read-only)' }).value).toContain('kind: Application');
    await user.click(within(drawer).getByRole('button', { name: 'Edit manifest' }));
    const editor = within(drawer).getByRole('textbox', { name: /Application manifest \(YAML\)/ });
    const original = appsCoreMockStore().apps.find((a) => a.metadata.name === 'guestbook');
    if (!original) throw new Error('fixture missing');
    await replaceText(user, editor, manifestYAML(original).replace('name: guestbook\n', 'name: renamed\n'));
    expect(within(drawer).getByText('metadata.name cannot change; create a new application to rename it.')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Save manifest' }));
    expect(saved).toHaveLength(0);
    await replaceText(user, editor, manifestYAML(original).replace('namespace: guestbook\n', 'namespace: guestbook-prod\n'));
    expect(within(drawer).getByText('The manifest is valid.')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Save manifest' }));
    expect(await screen.findByText('Manifest saved')).toBeInTheDocument();
    expect(saved[0]?.spec.destination.namespace).toBe('guestbook-prod');
    expect(saved[0]?.status).toEqual(original.status);
  });
});

describe('concurrent changes seen through the watch stream', () => {
  /** Lets a test replace the live application, as a watch event does on the details page. */
  let setLive: (app: Application) => void = () => undefined;

  function Harness({ initial }: { initial: Application }) {
    const [app, setApp] = useState(initial);
    const [tab, setTab] = useState('summary');
    setLive = setApp;
    return <AppDetailsPanel app={app} open onClose={() => undefined} tab={tab} onTabChange={setTab} reloadApp={() => Promise.resolve(app)} />;
  }

  /** PUTs answered with the request body (no resourceVersion check), recorded in order. */
  function acceptPuts() {
    const saved: Application[] = [];
    server.use(
      http.put('*/api/v1/applications/guestbook', async ({ request }) => {
        const body = (await request.clone().json()) as Application;
        saved.push(body);
        return HttpResponse.json(body);
      }),
    );
    return saved;
  }

  function withChanges(app: Application, change: (a: Application) => void): Application {
    const next = cloneJSON(app);
    change(next);
    return next;
  }

  async function editSummary(initial: Application) {
    const user = userEvent.setup({ delay: null });
    renderWithProviders(<Harness initial={initial} />);
    const drawer = await screen.findByRole('dialog', { name: 'guestbook details' });
    await user.click(within(drawer).getByRole('button', { name: 'Edit summary' }));
    return { user, drawer };
  }

  it('saves onto a status-only update without a conflict or a stale warning', async () => {
    const saved = acceptPuts();
    const initial = withChanges(guestbookApp(), (a) => {
      a.metadata.resourceVersion = '5';
    });
    const { user, drawer } = await editSummary(initial);
    await user.click(within(drawer).getByRole('button', { name: 'Add label' }));
    await user.type(within(drawer).getByRole('textbox', { name: 'Key of label 3' }), 'tier');
    await user.type(within(drawer).getByRole('textbox', { name: 'Value of label 3' }), 'frontend');
    act(() =>
      setLive(
        withChanges(initial, (a) => {
          a.metadata.resourceVersion = '6';
          if (a.status) a.status.health = { status: 'Progressing' };
        }),
      ),
    );
    expect(within(drawer).queryByText(/changed on the server/)).not.toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Save summary' }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]?.metadata.resourceVersion).toBe('6');
    expect(saved[0]?.metadata.labels).toEqual({ team: 'web', env: 'prod', tier: 'frontend' });
    expect(saved[0]?.status?.health?.status).toBe('Progressing');
  });

  it('holds the save until server-side edits are merged and reviewed', async () => {
    const saved = acceptPuts();
    const initial = withChanges(guestbookApp(), (a) => {
      a.metadata.resourceVersion = '5';
      a.metadata.annotations = { ...a.metadata.annotations, 'cd.patrol.example/refresh': 'normal' };
    });
    const { user, drawer } = await editSummary(initial);
    const namespace = within(drawer).getByRole('textbox', { name: 'Namespace' });
    await user.clear(namespace);
    await user.type(namespace, 'guestbook-v2');
    act(() =>
      setLive(
        withChanges(initial, (a) => {
          a.metadata.resourceVersion = '6';
          // The controller finished the refresh; someone else labelled the app and moved it.
          delete a.metadata.annotations?.['cd.patrol.example/refresh'];
          a.metadata.labels = { ...a.metadata.labels, owner: 'platform' };
          a.spec.destination.namespace = 'guestbook-prod';
        }),
      ),
    );
    const stale = within(drawer).getByText('This application changed on the server since you started editing').closest('[data-tone]');
    if (!(stale instanceof HTMLElement)) throw new Error('stale notice missing');
    expect(within(stale).getByText('metadata.labels.owner')).toBeInTheDocument();
    expect(within(stale).getByText('metadata.annotations["cd.patrol.example/refresh"]')).toBeInTheDocument();

    await user.click(within(drawer).getByRole('button', { name: 'Save summary' }));
    expect(within(drawer).getByRole('alert')).toHaveTextContent('Review the changes made on the server before saving');
    expect(saved).toHaveLength(0);

    await user.click(within(drawer).getByRole('button', { name: 'Use latest version' }));
    const merged = (await within(drawer).findByText('Your edits were merged onto the latest version')).closest('[data-tone]');
    if (!(merged instanceof HTMLElement)) throw new Error('rebase notice missing');
    expect(within(merged).getByText('metadata.labels.owner')).toBeInTheDocument();
    expect(within(merged).getByText(/your values will be saved/)).toBeInTheDocument();
    expect(within(merged).getByText('spec.destination.namespace')).toBeInTheDocument();
    expect(within(drawer).queryByText('This application changed on the server since you started editing')).not.toBeInTheDocument();
    expect(within(drawer).getByRole('textbox', { name: 'Namespace' })).toHaveValue('guestbook-v2');

    await user.click(within(drawer).getByRole('button', { name: 'Save summary' }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]?.metadata.resourceVersion).toBe('6');
    expect(saved[0]?.metadata.labels).toEqual({ team: 'web', env: 'prod', owner: 'platform' });
    expect(saved[0]?.metadata.annotations).toEqual({ 'link.cd.patrol.example/dashboard': 'Dashboard|https://grafana.example.test/d/guestbook' });
    expect(saved[0]?.spec.destination.namespace).toBe('guestbook-v2');
  });
});
