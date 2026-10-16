import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { AppProject, ProjectUpdateRequest } from '../../../api/types';
import { problem } from '../../../mocks/handlers';
import { settingsStore } from '../../../mocks/handlers/settings';
import { server } from '../../../mocks/server';
import { expectToast, findDialog, renderSettings, tableRows } from '../testUtils';

/** Renders a project page and waits until the project has loaded (tabs visible). */
async function renderProject(path: string, setup?: () => void) {
  const name = new URL(path, 'http://x').pathname.split('/').pop() ?? '';
  const result = await renderSettings(path, name, setup);
  await screen.findByRole('tablist', { name: 'Project sections' });
  return result;
}

const finance = () => settingsStore.projects.find((p) => p.metadata.name === 'finance');

/** Records the project of each PUT (the body is ProjectUpdateRequest: `{project}`). */
function capturePuts() {
  const puts: AppProject[] = [];
  server.use(
    http.put('*/api/v1/projects/:name', async ({ request }) => {
      const body = (await request.json()) as ProjectUpdateRequest;
      expect(Object.keys(body)).toEqual(['project']);
      const project = body.project as AppProject;
      puts.push(project);
      return HttpResponse.json({ ...project, metadata: { ...project.metadata, resourceVersion: '999' } });
    }),
  );
  return puts;
}

describe('Settings → Project details: summary', () => {
  it('shows sources, destinations and service accounts with deny patterns marked', async () => {
    await renderProject('/settings/projects/finance');
    const repos = screen.getByRole('list', { name: 'Source repositories' });
    expect(within(repos).getByText(/!https:\/\/git.example.test\/platform\/sandbox.git/)).toBeInTheDocument();
    expect(within(repos).getByText('(denied)')).toBeInTheDocument();
    expect(tableRows('Destinations')).toHaveLength(2);
    expect(tableRows('Destination service accounts')[0]).toHaveTextContent('payments-deployer');
    expect(await screen.findByRole('list', { name: 'Scoped repositories' })).toHaveTextContent('https://charts.example.test/stable');
  });

  it('adds a destination with validation and saves the whole project with its resourceVersion', async () => {
    const user = userEvent.setup();
    const puts = capturePuts();
    await renderProject('/settings/projects/finance');
    await user.click(screen.getByRole('button', { name: 'Edit destinations' }));
    const dialog = await findDialog('Edit destinations');
    await user.click(within(dialog).getByRole('button', { name: 'Add destination' }));
    expect(within(dialog).getByRole('textbox', { name: 'Server, destination 3' })).toHaveFocus();
    await user.keyboard('https://prod-eu.k8s.example.test');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Destination 3: Enter a namespace, or * for all namespaces.');
    await user.type(within(dialog).getByRole('textbox', { name: 'Namespace, destination 3' }), 'ledger');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Destinations updated');
    expect(puts[0]?.metadata.resourceVersion).toBe('10');
    expect(puts[0]?.spec.destinations?.[2]).toEqual({ server: 'https://prod-eu.k8s.example.test', namespace: 'ledger' });
    expect(puts[0]?.spec.roles).toHaveLength(1);
  });

  it('handles a concurrent change (409) with a reload action', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance');
    // Someone else updates the project after it was loaded.
    const current = finance();
    if (current) current.metadata.resourceVersion = '11';
    await user.click(screen.getByRole('button', { name: 'Edit source repositories' }));
    const dialog = await findDialog('Edit source repositories');
    await user.click(within(dialog).getByRole('button', { name: 'Remove repository 2' }));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(await within(dialog).findByText('Changed by someone else')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Reload latest' }));
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await user.click(screen.getByRole('button', { name: 'Edit source repositories' }));
    const again = await findDialog('Edit source repositories');
    await user.click(within(again).getByRole('button', { name: 'Remove repository 2' }));
    await user.click(within(again).getByRole('button', { name: 'Save' }));
    await expectToast('Source repositories updated');
    expect(finance()?.spec.sourceRepos).toEqual(['https://git.example.test/platform/*']);
  });

  it('edits the description and labels', async () => {
    const user = userEvent.setup();
    const puts = capturePuts();
    await renderProject('/settings/projects/finance');
    await user.click(screen.getByRole('button', { name: 'Edit general information' }));
    const dialog = await findDialog('Edit general information');
    await user.clear(within(dialog).getByLabelText('Description'));
    await user.type(within(dialog).getByLabelText('Description'), 'Money');
    await user.click(within(dialog).getByRole('button', { name: 'Add label' }));
    await user.keyboard('tier');
    await user.type(within(dialog).getByRole('textbox', { name: 'Value, label 2' }), 'gold');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Project updated');
    expect(puts[0]?.spec.description).toBe('Money');
    expect(puts[0]?.metadata.labels).toEqual({ team: 'finance', tier: 'gold' });
  });
});

describe('Settings → Project details: links', () => {
  it('shows http(s) project links only, opening in a new tab', async () => {
    server.use(
      http.get('*/api/v1/projects/:name/links', () =>
        HttpResponse.json({ items: [{ title: 'Runbook', url: 'https://runbooks.example.test/finance' }, { title: 'Bad', url: 'javascript:alert(1)' }] }),
      ),
    );
    await renderProject('/settings/projects/finance');
    const link = await screen.findByRole('link', { name: /Runbook/ });
    expect(link).toHaveAttribute('href', 'https://runbooks.example.test/finance');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    expect(screen.queryByRole('link', { name: /Bad/ })).not.toBeInTheDocument();
  });
});

describe('Settings → Project details: resources', () => {
  it('lists allow and deny lists and edits one with kind suggestions', async () => {
    const user = userEvent.setup();
    const puts = capturePuts();
    await renderProject('/settings/projects/finance?tab=resources');
    expect(screen.getByRole('tab', { name: 'Resources' })).toHaveAttribute('aria-selected', 'true');
    expect(tableRows('Cluster resource allow list')[0]).toHaveTextContent('core');
    expect(screen.getByText('Nothing denied.', { exact: false })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Edit namespace resource deny list' }));
    const dialog = await findDialog('Edit namespace resource deny list');
    expect(dialog.querySelector('datalist#project-resource-kinds option[value="Deployment"]')).not.toBeNull();
    await user.click(within(dialog).getByRole('button', { name: 'Add entry' }));
    await user.type(within(dialog).getByRole('combobox', { name: 'Group, entry 2' }), 'networking.k8s.io');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Entry 2: Enter a kind');
    await user.type(within(dialog).getByRole('combobox', { name: 'Kind, entry 2' }), 'NetworkPolicy');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Namespace resource deny list updated');
    expect(puts[0]?.spec.namespaceResourceBlacklist).toEqual([
      { group: '', kind: 'ResourceQuota' },
      { group: 'networking.k8s.io', kind: 'NetworkPolicy' },
    ]);
  });
});

describe('Settings → Project details: roles and tokens', () => {
  it('creates a role, validating policies against the project and role', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=roles');
    await user.click(screen.getByRole('button', { name: 'Add role' }));
    const dialog = await findDialog('Add role');
    await user.type(within(dialog).getByLabelText(/Role name/), 'viewer');
    await user.click(within(dialog).getByRole('button', { name: 'Insert read-only policy' }));
    expect(within(dialog).getByRole('textbox', { name: 'Policy 1' })).toHaveValue('p, proj:finance:viewer, applications, get, finance/*, allow');
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    await user.keyboard('p, proj:other:viewer, applications, get, other/*, allow');
    await user.click(within(dialog).getByRole('button', { name: 'Add group' }));
    await user.keyboard('a,b');
    await user.click(within(dialog).getByRole('button', { name: 'Create role' }));
    const summary = within(dialog).getByRole('alert');
    expect(summary).toHaveTextContent('Policy 2: The subject must be "proj:finance:viewer".');
    expect(summary).toHaveTextContent('Group 1: Group names containing "," must be wrapped in double quotes.');
    await user.click(within(dialog).getByRole('button', { name: 'Remove policy 2' }));
    await user.clear(within(dialog).getByRole('textbox', { name: 'Group 1' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Group 1' }), 'finance-readers');
    await user.click(within(dialog).getByRole('button', { name: 'Create role' }));
    await expectToast('Role created');
    expect(finance()?.spec.roles?.map((r) => r.name)).toEqual(['deployer', 'viewer']);
  });

  it('creates a token shown once, then deletes it', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=roles');
    await user.click(screen.getByRole('button', { name: 'Edit role deployer' }));
    const drawer = await findDialog('Role deployer');
    expect(tableRows('Tokens of role deployer')).toHaveLength(1);
    await user.type(within(drawer).getByLabelText('Expires in'), 'soon');
    await user.click(within(drawer).getByRole('button', { name: 'Create token' }));
    expect(within(drawer).getByRole('alert')).toHaveTextContent('Use a duration such as 30m');
    await user.clear(within(drawer).getByLabelText('Expires in'));
    await user.type(within(drawer).getByLabelText('Expires in'), '7d');
    expect(within(drawer).getByLabelText('Expires in')).toHaveAccessibleDescription('Expires 7d after creation.');
    await user.type(within(drawer).getByLabelText('Token ID'), 'release');
    await user.click(within(drawer).getByRole('button', { name: 'Create token' }));
    const tokenDialog = await findDialog('Token for role deployer');
    expect(within(tokenDialog).getByRole('textbox', { name: 'Token' })).toHaveValue('mock.jwt.1');
    expect(tokenDialog).toHaveTextContent('shown only once');
    await user.click(within(tokenDialog).getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Token for role deployer' })).not.toBeInTheDocument());
    await waitFor(() => expect(tableRows('Tokens of role deployer')).toHaveLength(2));
    await user.click(screen.getByRole('button', { name: 'Delete token release' }));
    await user.click(within(await findDialog('Delete token?')).getByRole('button', { name: 'Delete token' }));
    await expectToast('Token deleted');
    await waitFor(() => expect(tableRows('Tokens of role deployer')).toHaveLength(1));
  });

  it('does not write a token deleted in the drawer back when the role is saved', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=roles');
    await user.click(screen.getByRole('button', { name: 'Edit role deployer' }));
    const drawer = await findDialog('Role deployer');
    await user.click(within(drawer).getByRole('button', { name: 'Delete token ci-pipeline' }));
    await user.click(within(await findDialog('Delete token?')).getByRole('button', { name: 'Delete token' }));
    await expectToast('Token deleted');
    expect(await within(drawer).findByText('No tokens issued for this role.')).toBeInTheDocument();
    const stored = finance();
    expect(stored?.spec.roles?.[0]?.jwtTokens).toEqual([]);
    const puts = capturePuts();
    await user.type(within(drawer).getByLabelText('Description'), ' (CI)');
    await user.click(within(drawer).getByRole('button', { name: 'Save role' }));
    await expectToast('Role updated');
    const saved = puts[0];
    expect(saved?.metadata.resourceVersion).toBe(stored?.metadata.resourceVersion);
    expect(saved?.spec.roles?.[0]?.description).toMatch(/\(CI\)$/);
    expect(JSON.stringify(saved?.spec.roles)).not.toContain('ci-pipeline');
  });

  it('lists and revokes tokens that share an issue time by their ID', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=roles', () => {
      // Same iat second as the fixture's ci-pipeline token, different jti.
      finance()?.status?.jwtTokensByRole?.deployer?.items?.push({ iat: 1760000000, exp: 1823072000, id: 'nightly' });
    });
    const roleRow = tableRows('Project roles')[0];
    if (!roleRow) throw new Error('expected the deployer role row');
    expect(within(roleRow).getAllByRole('cell').map((c) => c.textContent)).toContain('2');
    await user.click(screen.getByRole('button', { name: 'Edit role deployer' }));
    await findDialog('Role deployer');
    expect(tableRows('Tokens of role deployer').map((r) => r.textContent)).toEqual([
      expect.stringContaining('ci-pipeline'),
      expect.stringContaining('nightly'),
    ]);
    await user.click(screen.getByRole('button', { name: 'Delete token nightly' }));
    await user.click(within(await findDialog('Delete token?')).getByRole('button', { name: 'Delete token' }));
    await expectToast('Token deleted');
    await waitFor(() => expect(tableRows('Tokens of role deployer')).toHaveLength(1));
    expect(tableRows('Tokens of role deployer')[0]).toHaveTextContent('ci-pipeline');
    expect(finance()?.status?.jwtTokensByRole?.deployer?.items?.map((t) => t.id)).toEqual(['ci-pipeline']);
  });

  it('deletes all expired tokens of a role', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=roles', () => {
      const project = settingsStore.projects.find((p) => p.metadata.name === 'finance');
      project?.status?.jwtTokensByRole?.deployer?.items?.push({ iat: 1700000000, exp: 1700003600, id: 'old' });
    });
    await user.click(screen.getByRole('button', { name: 'Edit role deployer' }));
    await findDialog('Role deployer');
    expect(tableRows('Tokens of role deployer')).toHaveLength(2);
    await user.click(screen.getByRole('button', { name: 'Delete 1 expired token' }));
    await expectToast('Deleted 1 expired token');
    await waitFor(() => expect(tableRows('Tokens of role deployer')).toHaveLength(1));
  });

  it('deletes a role after confirmation', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=roles');
    await user.click(screen.getByRole('button', { name: 'Delete role deployer' }));
    const dialog = await findDialog('Delete role?');
    expect(dialog).toHaveTextContent('1 token(s) stop working');
    await user.click(within(dialog).getByRole('button', { name: 'Delete role' }));
    await expectToast('Role deleted');
    expect(await screen.findByRole('heading', { name: 'No roles' })).toBeInTheDocument();
  });
});

describe('Settings → Project details: sync windows', () => {
  it('lists windows with their open state from the server', async () => {
    server.use(
      http.get('*/api/v1/projects/:name/syncwindows', () =>
        HttpResponse.json({ windows: [{ kind: 'deny', schedule: '0 22 * * *', duration: '8h', timeZone: 'Europe/Berlin', applications: ['payments'], manualSync: true, description: 'Nightly freeze' }] }),
      ),
    );
    await renderProject('/settings/projects/finance?tab=windows');
    const rows = tableRows('Sync windows');
    expect(rows).toHaveLength(2);
    await waitFor(() => expect(rows[0]).toHaveTextContent('Open'));
    expect(rows[0]).toHaveTextContent('Deny');
    expect(rows[0]).toHaveTextContent('applications payments');
    expect(rows[0]).toHaveTextContent('Allowed');
    expect(rows[1]).toHaveTextContent('Closed');
  });

  it('adds a window with cron validation and a preview of the next windows', async () => {
    const user = userEvent.setup();
    const puts = capturePuts();
    await renderProject('/settings/projects/finance?tab=windows');
    await user.click(screen.getByRole('button', { name: 'Add sync window' }));
    const dialog = await findDialog('Add sync window');
    await user.type(within(dialog).getByLabelText(/Schedule/), '61 * * *');
    await user.type(within(dialog).getByLabelText(/Duration/), '90');
    await user.click(within(dialog).getByRole('button', { name: 'Add window' }));
    const summary = within(dialog).getByRole('alert');
    expect(summary).toHaveTextContent('Expected 5 fields');
    expect(summary).toHaveTextContent('Use a positive duration');
    expect(summary).toHaveTextContent('Add at least one application, namespace or cluster');
    await user.click(within(summary).getByRole('link', { name: /Add at least one application/ }));
    expect(within(dialog).getByRole('button', { name: 'Add application' })).toHaveFocus();

    await user.clear(within(dialog).getByLabelText(/Schedule/));
    await user.type(within(dialog).getByLabelText(/Schedule/), '0 9 * * 1-5');
    expect(within(dialog).getByLabelText(/Schedule/)).toHaveAccessibleDescription('Monday to Friday at 09:00');
    await user.clear(within(dialog).getByLabelText(/Duration/));
    await user.type(within(dialog).getByLabelText(/Duration/), '1h30m');
    expect(within(dialog).getByRole('list', { name: 'Next windows' }).children).toHaveLength(3);
    await user.selectOptions(within(dialog).getByLabelText(/Time zone/), 'UTC');
    await user.click(within(dialog).getByRole('button', { name: 'Add application' }));
    await user.keyboard('payments');
    await user.click(within(dialog).getByRole('radio', { name: /Deny/ }));
    await user.click(within(dialog).getByRole('checkbox', { name: /Allow manual syncs/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Add window' }));
    await expectToast('Sync window added');
    expect(puts[0]?.spec.syncWindows?.[2]).toEqual({
      kind: 'deny',
      schedule: '0 9 * * 1-5',
      duration: '1h30m',
      timeZone: 'UTC',
      applications: ['payments'],
      manualSync: true,
    });
  });

  it('deletes a window from its action menu', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=windows');
    await user.click(screen.getByRole('button', { name: 'Actions for sync window 2 (0 9 * * 1-5)' }));
    await user.click(screen.getByRole('menuitem', { name: 'Delete' }));
    await user.click(within(await findDialog('Delete sync window?')).getByRole('button', { name: 'Delete window' }));
    await expectToast('Sync window deleted');
    expect(finance()?.spec.syncWindows).toHaveLength(1);
  });
});

describe('Settings → Project details: signature keys, orphaned resources, events', () => {
  it('flags keys missing from the keyring and validates key IDs', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=integrity');
    expect(await screen.findByText('Release Bot <release@example.test>')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Edit signature keys' }));
    const dialog = await findDialog('Edit signature keys');
    await user.click(within(dialog).getByRole('button', { name: 'Add key' }));
    await user.keyboard('xyz');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Key 2: Use the 16-character hexadecimal key ID.');
    await user.clear(within(dialog).getByRole('combobox', { name: 'Key ID 2' }));
    await user.type(within(dialog).getByRole('combobox', { name: 'Key ID 2' }), 'abcdefabcdefabcd');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Signature keys updated');
    expect(await screen.findByText('Some keys are not trusted yet')).toBeInTheDocument();
    expect(finance()?.spec.signatureKeys).toEqual([{ keyID: '4AEE18F83AFDEB23' }, { keyID: 'ABCDEFABCDEFABCD' }]);
  });

  it('adds a source integrity policy', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=integrity');
    await user.click(screen.getByRole('button', { name: 'Edit source integrity policies' }));
    const dialog = await findDialog('Edit source integrity policies');
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Repository URL pattern 1' }), 'https://git.example.test/platform/*');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Add at least one key ID');
    await user.selectOptions(within(dialog).getByLabelText('Verification'), 'strict');
    await user.click(within(dialog).getByRole('button', { name: 'Add key' }));
    await user.keyboard('4AEE18F83AFDEB23');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Source integrity policies updated');
    expect(finance()?.spec.sourceIntegrity).toEqual({
      git: { policies: [{ repos: [{ url: 'https://git.example.test/platform/*' }], gpg: { mode: 'strict', keys: ['4AEE18F83AFDEB23'] } }] },
    });
  });

  it('turns orphaned resource monitoring off', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=orphaned');
    expect(tableRows('Ignored orphaned resources')[0]).toHaveTextContent('kube-root-ca.crt');
    await user.click(screen.getByRole('button', { name: 'Edit orphaned resources monitoring' }));
    const dialog = await findDialog('Edit orphaned resources monitoring');
    const toggle = within(dialog).getByRole('switch', { name: 'Monitor orphaned resources' });
    expect(toggle).toHaveAttribute('aria-checked', 'true');
    await user.click(toggle);
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await expectToast('Orphaned resource monitoring updated');
    expect(finance()?.spec.orphanedResources).toBeUndefined();
  });

  it('lists events and shows an empty state', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance?tab=events');
    expect((await screen.findAllByRole('row')).length).toBeGreaterThan(1);
    expect(tableRows('Project events')[0]).toHaveTextContent('admin updated project');
    await user.click(screen.getByRole('link', { name: 'Projects' }));
    await screen.findByRole('heading', { level: 1, name: 'Projects' });
    await user.click(await screen.findByRole('link', { name: 'default' }));
    await screen.findByRole('heading', { level: 1, name: 'default' });
    await user.click(screen.getByRole('tab', { name: 'Events' }));
    expect(await screen.findByRole('heading', { name: 'No recent events' })).toBeInTheDocument();
  });
});

describe('Settings → Project details: deletion and errors', () => {
  it('warns about applications that use the project and explains the refusal', async () => {
    const user = userEvent.setup();
    await renderProject('/settings/projects/finance');
    await user.click(screen.getByRole('button', { name: 'Delete project' }));
    const dialog = await findDialog('Delete project?');
    expect(await within(dialog).findByText('1 application uses this project')).toBeInTheDocument();
    expect(within(dialog).getByRole('link', { name: 'payments' })).toHaveAttribute('href', '/applications/payments');
    await user.type(within(dialog).getByRole('textbox'), 'finance');
    await user.click(within(dialog).getByRole('button', { name: 'Delete project' }));
    const error = await within(dialog).findByText('Unable to delete the project');
    expect(error.closest('[data-tone]')).toHaveTextContent('referenced by 1 application');
  });

  it('deletes an unused project and returns to the list', async () => {
    const user = userEvent.setup();
    const { router } = await renderProject('/settings/projects/scratch', () => {
      settingsStore.projects.push({ metadata: { name: 'scratch', resourceVersion: '1' }, spec: {} });
    });
    await user.click(screen.getByRole('button', { name: 'Delete project' }));
    const dialog = await findDialog('Delete project?');
    await user.type(within(dialog).getByRole('textbox'), 'scratch');
    await user.click(within(dialog).getByRole('button', { name: 'Delete project' }));
    await expectToast('Project deleted');
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings/projects'));
  });

  it('shows guidance for a missing project and for permission errors on save', async () => {
    await renderSettings('/settings/projects/missing', 'missing');
    expect(await screen.findByRole('heading', { name: 'Project not found' })).toBeInTheDocument();
  });

  it('explains a permission error when saving', async () => {
    const user = userEvent.setup();
    server.use(http.put('*/api/v1/projects/:name', () => problem(403, 'permission_denied', 'permission denied: projects, update, finance')));
    await renderProject('/settings/projects/finance');
    await user.click(screen.getByRole('button', { name: 'Edit source namespaces' }));
    const dialog = await findDialog('Edit source namespaces');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    const error = await within(dialog).findByText('Permission denied');
    expect(error.closest('[data-tone]')).toHaveTextContent('You are not allowed to update the project.');
  });
});
