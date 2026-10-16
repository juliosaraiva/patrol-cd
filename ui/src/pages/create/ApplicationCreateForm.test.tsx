import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { useLocation } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import type { Application, RepositoryAppDetailsQuery, Settings } from '../../api/types';
import { settingsFixture } from '../../mocks/fixtures';
import {
  appDetailsFixture,
  createClustersFixture,
  createdApplication,
  createProjectsFixture,
  createRepositoriesFixture,
  GIT_REPO,
  HELM_REPO,
  pluginSettingsFixture,
} from '../../mocks/fixtures/create';
import { problem } from '../../mocks/handlers';
import { server } from '../../mocks/server';
import { renderWithProviders } from '../../test/render';
import { CreateApplicationPanel } from './CreateApplicationPanel';

function Opened() {
  const location = useLocation();
  return <p>{`Opened ${location.pathname}`}</p>;
}

interface Posted {
  url: URL;
  body: Application;
}

function setup(options: { settings?: Partial<Settings>; initial?: { repoURL?: string } } = {}) {
  const posted: Posted[] = [];
  const details: RepositoryAppDetailsQuery[] = [];
  server.use(
    http.get('*/api/v1/settings', () => HttpResponse.json({ ...settingsFixture, ...options.settings })),
    http.get('*/api/v1/repositories', () => HttpResponse.json(createRepositoriesFixture)),
    http.get('*/api/v1/projects', () => HttpResponse.json(createProjectsFixture)),
    http.get('*/api/v1/clusters', () => HttpResponse.json(createClustersFixture)),
    http.get('*/api/v1/settings/plugins', () => HttpResponse.json(pluginSettingsFixture)),
    http.post('*/api/v1/repositories/:repo/appdetails', async ({ request }) => {
      const body = (await request.json()) as RepositoryAppDetailsQuery;
      details.push(body);
      return HttpResponse.json(appDetailsFixture(body.source.path, body.source.chart));
    }),
    http.post('*/api/v1/applications', async ({ request }) => {
      const body = (await request.json()) as Application;
      posted.push({ url: new URL(request.url), body });
      return HttpResponse.json(createdApplication(body));
    }),
  );
  const onClose = vi.fn();
  renderWithProviders(<></>, {
    path: '/applications',
    routes: [
      { path: '/applications', element: <CreateApplicationPanel open onClose={onClose} initial={options.initial} /> },
      { path: '/applications/*', element: <Opened /> },
    ],
  });
  return { posted, details, onClose, user: userEvent.setup() };
}

/** Pastes into a field (much faster than typing long URLs key by key). */
async function fill(user: ReturnType<typeof userEvent.setup>, field: HTMLElement, text: string) {
  await user.click(field);
  await user.paste(text);
}

function datalistValues(input: HTMLElement): string[] {
  const list = document.getElementById(input.getAttribute('list') ?? '');
  return Array.from(list?.querySelectorAll('option') ?? []).map((o) => o.value);
}

async function fillGitApp(user: ReturnType<typeof userEvent.setup>, name = 'guestbook') {
  const dialog = await screen.findByRole('dialog', { name: 'New application' });
  await user.type(within(dialog).getByRole('textbox', { name: 'Application name' }), name);
  await fill(user, within(dialog).getByRole('combobox', { name: /Repository URL/ }), GIT_REPO);
  const path = within(dialog).getByRole('combobox', { name: /Path/ });
  await user.type(path, 'guestbook');
  await user.tab();
  await fill(user, within(dialog).getByRole('combobox', { name: /Cluster URL/ }), 'https://kubernetes.default.svc');
  await user.type(within(dialog).getByRole('textbox', { name: /^Namespace/ }), 'guestbook');
  return dialog;
}

describe('application creation wizard (API-091)', () => {
  it('creates a Git directory application with repository, revision and path suggestions, then opens it', async () => {
    const { posted, user } = setup();
    const dialog = await screen.findByRole('dialog', { name: 'New application' });
    expect(within(dialog).getByRole('textbox', { name: 'Application name' })).toHaveFocus();
    const repo = within(dialog).getByRole('combobox', { name: /Repository URL/ });
    await waitFor(() => expect(datalistValues(repo)).toEqual(expect.arrayContaining([GIT_REPO, HELM_REPO])));

    await fillGitApp(user);
    const revision = within(dialog).getByRole('combobox', { name: /Revision/ });
    await waitFor(() => expect(datalistValues(revision)).toEqual(['main', 'release-1.4', 'v1.4.0', 'v1.3.2']));
    expect(datalistValues(within(dialog).getByRole('combobox', { name: /Path/ }))).toEqual(['guestbook', 'helm-guestbook', 'kustomize-guestbook']);
    expect(await within(dialog).findByText('Detected: Directory.')).toBeInTheDocument();

    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect(await screen.findByText('Opened /applications/guestbook')).toBeInTheDocument();
    expect(posted).toHaveLength(1);
    expect(posted[0]?.url.searchParams.get('validate')).toBe('true');
    expect(posted[0]?.url.searchParams.get('upsert')).toBe('false');
    expect(posted[0]?.body).toEqual({
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'Application',
      metadata: { name: 'guestbook' },
      spec: {
        project: 'default',
        source: { repoURL: GIT_REPO, path: 'guestbook', targetRevision: 'HEAD' },
        destination: { server: 'https://kubernetes.default.svc', namespace: 'guestbook' },
      },
    });
    expect(await screen.findByText('Application guestbook created')).toBeInTheDocument();
  });

  it('creates a Helm chart application with chart and version pickers and parameters from app details', async () => {
    const { posted, details, user } = setup();
    const dialog = await screen.findByRole('dialog', { name: 'New application' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Application name' }), 'web');
    await fill(user, within(dialog).getByRole('combobox', { name: /Repository URL/ }), HELM_REPO);
    // A registered Helm repository fixes the type.
    expect(within(dialog).getByRole('combobox', { name: 'Type' })).toHaveValue('helm');
    expect(within(dialog).getByRole('combobox', { name: 'Type' })).toBeDisabled();
    const chart = within(dialog).getByRole('combobox', { name: /Chart/ });
    await waitFor(() => expect(datalistValues(chart)).toEqual(['nginx', 'redis']));
    await user.type(chart, 'nginx');
    const version = within(dialog).getByRole('combobox', { name: /Version/ });
    await user.click(version);
    expect(datalistValues(version)).toEqual(['15.4.0', '15.3.1']);
    await user.type(version, '15.4.0');
    await user.tab();

    const replicas = await within(dialog).findByRole('textbox', { name: 'replicaCount' });
    expect(replicas).toHaveAttribute('placeholder', '1');
    expect(within(dialog).getByRole('combobox', { name: 'Source type' })).toHaveDisplayValue('Auto (Helm)');
    await user.type(replicas, '3');
    await user.click(within(dialog).getByRole('checkbox', { name: 'values-production.yaml' }));
    await fill(user, within(dialog).getByRole('combobox', { name: /Cluster URL/ }), 'https://staging.example.test');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));

    await screen.findByText('Opened /applications/web');
    expect(details.some((d) => d.source.chart === 'nginx' && d.source.targetRevision === '15.4.0')).toBe(true);
    expect(posted[0]?.body.spec.source).toEqual({
      repoURL: HELM_REPO,
      chart: 'nginx',
      targetRevision: '15.4.0',
      helm: { parameters: [{ name: 'replicaCount', value: '3' }], valueFiles: ['values-production.yaml'] },
    });
  });

  it('creates a multi-source application with a values ref source', async () => {
    const { posted, user } = setup();
    const dialog = await fillGitApp(user, 'multi');
    await user.click(within(dialog).getByRole('switch', { name: 'Multiple sources' }));
    await user.click(within(dialog).getByRole('button', { name: 'Add source' }));
    const second = within(dialog).getByRole('group', { name: 'Source 2' });
    await fill(user, within(second).getByRole('combobox', { name: /Repository URL/ }), GIT_REPO);
    await user.type(within(second).getByRole('textbox', { name: /Ref/ }), 'values');
    expect(within(second).queryByRole('group', { name: 'Parameters' })).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    await screen.findByText('Opened /applications/multi');
    expect(posted[0]?.body.spec.source).toBeUndefined();
    expect(posted[0]?.body.spec.sources).toEqual([
      { repoURL: GIT_REPO, path: 'guestbook', targetRevision: 'HEAD' },
      { repoURL: GIT_REPO, targetRevision: 'HEAD', ref: 'values' },
    ]);
  });

  it('keeps form and YAML in sync both ways and reports YAML errors inline', async () => {
    const { user } = setup();
    const dialog = await fillGitApp(user, 'roundtrip');
    await user.click(within(dialog).getByRole('button', { name: 'Edit as YAML' }));
    const yaml = within(dialog).getByRole('textbox', { name: /Application manifest/ });
    expect((yaml as HTMLTextAreaElement).value).toContain('name: roundtrip');
    expect((yaml as HTMLTextAreaElement).value).toContain('path: guestbook');

    const edited = (yaml as HTMLTextAreaElement).value.replace('namespace: guestbook', 'namespace: edited-ns');
    await user.clear(yaml);
    await user.click(yaml);
    await user.paste(edited);
    await user.click(within(dialog).getByRole('button', { name: 'Form' }));
    expect(within(dialog).getByRole('textbox', { name: /^Namespace/ })).toHaveValue('edited-ns');

    await user.click(within(dialog).getByRole('button', { name: 'Edit as YAML' }));
    const again = within(dialog).getByRole('textbox', { name: /Application manifest/ });
    await user.click(again);
    await user.paste('\n  bad: [');
    expect(again).toHaveAttribute('aria-invalid', 'true');
    expect(again).toHaveAccessibleDescription(/^Line \d+:/);
    await user.click(within(dialog).getByRole('button', { name: 'Form' }));
    // Still in YAML mode until the document parses.
    expect(within(dialog).getByRole('button', { name: 'Edit as YAML' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('explains half-finished YAML edits inline instead of failing (deleted repoURL, mistyped fields)', async () => {
    const { posted, user } = setup();
    const dialog = await fillGitApp(user, 'partial');
    await user.click(within(dialog).getByRole('button', { name: 'Edit as YAML' }));
    const yaml = within(dialog).getByRole<HTMLTextAreaElement>('textbox', { name: /Application manifest/ });

    // Delete the repoURL line the way an editor would: select it, press Backspace.
    const start = yaml.value.indexOf('    repoURL:');
    const end = yaml.value.indexOf('\n', start) + 1;
    await user.click(yaml);
    yaml.setSelectionRange(start, end);
    await user.keyboard('{Backspace}');
    expect(yaml.value).not.toContain('repoURL');
    expect(yaml).toHaveAttribute('aria-invalid', 'true');
    expect(yaml).toHaveAccessibleDescription(/^spec\.source\.repoURL: Enter a repository URL\./);

    // Type a mistyped field key by key; every intermediate document must keep the editor usable.
    await user.type(yaml, '  syncPolicy:{Enter}    syncOptions: CreateNamespace=true');
    expect(yaml).toHaveAccessibleDescription(/^Line \d+: spec\.syncPolicy\.syncOptions must be a list\./);
    await user.click(within(dialog).getByRole('button', { name: 'Form' }));
    expect(within(dialog).getByRole('button', { name: 'Edit as YAML' })).toHaveAttribute('aria-pressed', 'true');

    // Fix the type; the form then opens with the empty repository field and validation names it.
    const fixedText = yaml.value.replace('syncOptions: CreateNamespace=true', 'syncOptions: [CreateNamespace=true]');
    await user.clear(yaml);
    await user.click(yaml);
    await user.paste(fixedText);
    await user.click(within(dialog).getByRole('button', { name: 'Form' }));
    expect(within(dialog).getByRole('combobox', { name: /Repository URL/ })).toHaveValue('');
    expect(within(dialog).getByRole('checkbox', { name: /Auto-create namespace/ })).toBeChecked();
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Enter a repository URL.');
    expect(posted).toHaveLength(0);
  });

  it('validates before calling the server and focuses the first invalid field', async () => {
    const { posted, user } = setup();
    const dialog = await screen.findByRole('dialog', { name: 'New application' });
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    const name = within(dialog).getByRole('textbox', { name: 'Application name' });
    expect(name).toHaveAttribute('aria-invalid', 'true');
    expect(name).toHaveAccessibleDescription('Enter a name.');
    expect(name).toHaveFocus();
    const alert = within(dialog).getByRole('alert');
    expect(alert).toHaveTextContent('The application cannot be created yet');
    expect(alert).toHaveTextContent('Choose a destination cluster.');
    expect(posted).toHaveLength(0);
  });

  it('shows server validation errors with guidance and suggests upsert for existing names', async () => {
    const { posted, user } = setup();
    server.use(
      http.post('*/api/v1/applications', async ({ request }) => {
        const url = new URL(request.url);
        const body = (await request.json()) as Application;
        posted.push({ url, body });
        if (url.searchParams.get('upsert') !== 'true') return problem(409, 'already_exists', 'existing application spec is different, use upsert flag to force update');
        return HttpResponse.json(createdApplication(body));
      }),
    );
    const dialog = await fillGitApp(user, 'exists');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    const alert = await within(dialog).findByRole('alert');
    expect(alert).toHaveTextContent('An application named exists already exists');
    expect(alert).toHaveTextContent('turn on "Upsert"');
    await user.click(within(dialog).getByRole('checkbox', { name: /Upsert/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    await screen.findByText('Opened /applications/exists');
    expect(posted.map((p) => p.url.searchParams.get('upsert'))).toEqual(['false', 'true']);
  });

  it('reports a spec the server rejects during validation', async () => {
    const { user } = setup();
    server.use(
      http.post('*/api/v1/applications', () =>
        problem(400, 'invalid_argument', 'application spec for broken is invalid: InvalidSpecError: repository not accessible: repository not found'),
      ),
    );
    const dialog = await fillGitApp(user, 'broken');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    const alert = await within(dialog).findByRole('alert');
    expect(alert).toHaveTextContent('The server rejected the application');
    expect(alert).toHaveTextContent('repository not accessible');
    expect(alert).toHaveTextContent('Correct the spec and create it again.');
  });

  it('writes sync policy, sync options and the deletion finalizer; Replace only when allowed', async () => {
    const { posted, user } = setup({ settings: { syncWithReplaceAllowed: false } });
    const dialog = await fillGitApp(user, 'policy');
    expect(within(dialog).queryByRole('checkbox', { name: /^Replace/ })).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: /Automatic/ }));
    await user.click(within(dialog).getByRole('checkbox', { name: /Prune resources/ }));
    await user.click(within(dialog).getByRole('checkbox', { name: /Self heal/ }));
    await user.click(within(dialog).getByRole('checkbox', { name: /Auto-create namespace/ }));
    await user.selectOptions(within(dialog).getByRole('combobox', { name: 'Prune propagation policy' }), 'background');
    await user.click(within(dialog).getByRole('checkbox', { name: /Delete resources when the application is deleted/ }));
    await user.click(within(dialog).getByRole('checkbox', { name: /^Validate/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    await screen.findByText('Opened /applications/policy');
    expect(posted[0]?.url.searchParams.get('validate')).toBe('false');
    expect(posted[0]?.body.metadata.finalizers).toEqual(['resources-finalizer.cd.patrol.example']);
    expect(posted[0]?.body.spec.syncPolicy).toEqual({
      automated: { prune: true, selfHeal: true },
      syncOptions: ['CreateNamespace=true', 'PrunePropagationPolicy=background'],
    });
  });

  it('offers Replace when the server allows it', async () => {
    setup({ settings: { syncWithReplaceAllowed: true } });
    const dialog = await screen.findByRole('dialog', { name: 'New application' });
    expect(await within(dialog).findByRole('checkbox', { name: /^Replace/ })).toBeInTheDocument();
  });

  it('creates in another namespace when apps in any namespace are enabled', async () => {
    const { posted, user } = setup({ settings: { appsInAnyNamespaceEnabled: true } });
    const dialog = await fillGitApp(user, 'scoped');
    await user.type(within(dialog).getByRole('textbox', { name: /Application namespace/ }), 'team-a');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect(await screen.findByText('Opened /applications/team-a/scoped')).toBeInTheDocument();
    expect(posted[0]?.body.metadata).toEqual({ name: 'scoped', namespace: 'team-a' });
  });

  it('hides the application namespace field when apps in any namespace are disabled', async () => {
    setup({ settings: { appsInAnyNamespaceEnabled: false } });
    const dialog = await screen.findByRole('dialog', { name: 'New application' });
    expect(within(dialog).queryByRole('textbox', { name: /Application namespace/ })).not.toBeInTheDocument();
  });

  it('configures the source hydrator when it is enabled', async () => {
    const { posted, user } = setup({ settings: { hydratorEnabled: true } });
    const dialog = await screen.findByRole('dialog', { name: 'New application' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Application name' }), 'hydrated');
    await fill(user, within(dialog).getByRole('combobox', { name: /Repository URL/ }), GIT_REPO);
    await user.type(within(dialog).getByRole('combobox', { name: /Path/ }), 'guestbook');
    await user.click(within(dialog).getByRole('switch', { name: 'Use the source hydrator' }));
    expect(within(dialog).getByRole('textbox', { name: /Dry source repository URL/ })).toHaveValue(GIT_REPO);
    const syncPath = within(dialog).getByRole('textbox', { name: /Sync path/ });
    await user.clear(syncPath);
    await user.type(syncPath, 'envs/dev/guestbook');
    await fill(user, within(dialog).getByRole('combobox', { name: /Cluster URL/ }), 'https://kubernetes.default.svc');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    await screen.findByText('Opened /applications/hydrated');
    expect(posted[0]?.body.spec.source).toBeUndefined();
    expect(posted[0]?.body.spec.sourceHydrator).toEqual({
      drySource: { repoURL: GIT_REPO, targetRevision: 'HEAD', path: 'guestbook' },
      syncSource: { targetBranch: 'environments/dev', path: 'envs/dev/guestbook' },
    });
  });

  it('chooses the destination by cluster name', async () => {
    const { posted, user } = setup();
    const dialog = await fillGitApp(user, 'byname');
    await user.click(within(dialog).getByRole('radio', { name: 'Name' }));
    const name = within(dialog).getByRole('combobox', { name: /Cluster name/ });
    expect(datalistValues(name)).toEqual(['in-cluster', 'staging']);
    await user.type(name, 'staging');
    await user.click(within(dialog).getByRole('button', { name: 'Create' }));
    await screen.findByText('Opened /applications/byname');
    expect(posted[0]?.body.spec.destination).toEqual({ name: 'staging', namespace: 'guestbook' });
  });

  it('prefills the repository and closes on Cancel or Escape', async () => {
    const { onClose, user } = setup({ initial: { repoURL: GIT_REPO } });
    const dialog = await screen.findByRole('dialog', { name: 'New application' });
    expect(within(dialog).getByRole('combobox', { name: /Repository URL/ })).toHaveValue(GIT_REPO);
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalledTimes(1);
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});
