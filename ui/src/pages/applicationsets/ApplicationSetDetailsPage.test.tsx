import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { ApplicationSetGenerateRequest } from '../../api/types';
import { applicationsFixture } from '../../mocks/fixtures';
import { generatedApplications } from '../../mocks/fixtures/applicationsets';
import { problem } from '../../mocks/handlers';
import { mockGenerate } from '../../mocks/handlers/applicationsets';
import { server } from '../../mocks/server';
import { renderApp } from '../../test/render';

function withGeneratedApps() {
  server.use(http.get('*/api/v1/applications', () => HttpResponse.json({ ...applicationsFixture, items: [...applicationsFixture.items, ...generatedApplications] })));
}

async function openDetails(path = '/applicationsets/guestbook') {
  withGeneratedApps();
  const result = renderApp(path);
  await screen.findByRole('tablist', { name: 'ApplicationSet views' });
  return result;
}

describe('ApplicationSet details (ASET-125, ASET-126)', () => {
  it('shows the status bar and a readable summary of generators, strategy and template', async () => {
    await openDetails();
    expect(await screen.findByRole('heading', { level: 1, name: 'guestbook' })).toBeInTheDocument();
    const status = screen.getByRole('region', { name: 'ApplicationSet status' });
    expect(status).toHaveTextContent('Progressing');
    expect(status).toHaveTextContent('3 reported');
    expect(status).toHaveTextContent('RollingSync, 2 steps');
    expect(screen.getByRole('heading', { name: 'Generator 1: List' })).toBeInTheDocument();
    expect(screen.getByRole('table', { name: 'List elements' })).toHaveTextContent('production');
    expect(screen.getByRole('list', { name: 'RollingSync steps' })).toHaveTextContent('env In (production), at most 50% at a time');
    expect(screen.getByText('Deployed resources are preserved')).toBeInTheDocument();
  });

  it('lists generated applications with health, sync and progressive sync step', async () => {
    const user = userEvent.setup();
    const { router } = await openDetails();
    await user.click(await screen.findByRole('tab', { name: 'Applications (2)' }));
    expect(router.state.location.search).toBe('?tab=applications');
    const table = screen.getByRole('table', { name: 'Generated applications' });
    const prod = within(table).getByRole('row', { name: /guestbook-production/ });
    expect(prod).toHaveTextContent('Progressing');
    expect(prod).toHaveTextContent('Out of sync');
    expect(prod).toHaveTextContent('Step 2 · Progressing');
    expect(within(prod).getByRole('link', { name: 'guestbook-production' })).toHaveAttribute('href', '/applications/guestbook-production');
  });

  it('opens the conditions tab from the status bar with guidance for errors', async () => {
    const user = userEvent.setup();
    withGeneratedApps();
    renderApp('/applicationsets/team-infra/clusters-addons');
    await user.click(await screen.findByRole('button', { name: '2 need attention' }));
    const list = screen.getByRole('list', { name: 'Conditions' });
    const error = within(list).getAllByRole('listitem')[0]!;
    expect(error).toHaveTextContent('ErrorOccurred');
    expect(error).toHaveTextContent('cluster selector matched no clusters');
    expect(error).toHaveTextContent('A generator failed. Check repository access');
    expect(within(error).getByRole('img', { name: 'Needs attention' })).toBeInTheDocument();
  });

  it('renders the resource tree of generated applications and the events', async () => {
    const user = userEvent.setup();
    await openDetails();
    await user.click(await screen.findByRole('tab', { name: 'Resource tree' }));
    const children = await screen.findByRole('list', { name: 'Applications of guestbook' });
    expect(within(children).getAllByRole('listitem')).toHaveLength(2);
    expect(within(children).getByRole('link', { name: 'guestbook-staging' })).toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: 'Events' }));
    const events = await screen.findByRole('table', { name: 'Events' });
    expect(within(events).getAllByRole('row')).toHaveLength(3);
    expect(events).toHaveTextContent('Waiting for step 1 applications to become Healthy');
  });

  it('previews an edited spec as a diff against the existing applications without saving', async () => {
    const user = userEvent.setup();
    const posted: ApplicationSetGenerateRequest[] = [];
    server.use(
      http.post('*/api/v1/applicationsets/generate', async ({ request }) => {
        const body = (await request.json()) as ApplicationSetGenerateRequest;
        posted.push(body);
        return HttpResponse.json({ applications: mockGenerate(body.applicationSet) });
      }),
    );
    let writes = 0;
    server.use(http.post('*/api/v1/applicationsets', () => {
      writes += 1;
      return HttpResponse.json({});
    }));
    await openDetails();
    await user.click(await screen.findByRole('tab', { name: 'Preview' }));
    const editor = screen.getByRole('textbox', { name: /ApplicationSet spec/ });
    const edited = (editor as HTMLTextAreaElement).value.replace('name: guestbook-{{.cluster}}', 'name: gb-{{.cluster}}');
    await user.clear(editor);
    await user.click(editor);
    await user.paste(edited);
    expect(editor).toHaveAccessibleDescription('Edited: differs from the saved spec.');
    await user.click(screen.getByRole('button', { name: 'Preview changes' }));

    const heading = await screen.findByRole('heading', { name: 'Preview result' });
    await waitFor(() => expect(heading).toHaveFocus());
    expect(screen.getByRole('list', { name: 'Summary of changes' })).toHaveTextContent('2 added0 modified2 removed0 unchanged');
    expect(screen.getByRole('region', { name: 'Changes to gb-staging' })).toHaveTextContent('added: name: gb-staging');
    expect(screen.getByRole('region', { name: 'Changes to guestbook-staging' })).toHaveTextContent('removed: name: guestbook-staging');
    expect(posted).toHaveLength(1);
    expect(posted[0]?.applicationSet.metadata).toEqual({ name: 'guestbook', namespace: 'patrol', labels: { team: 'web' } });
    expect(posted[0]?.applicationSet).not.toHaveProperty('status');
    expect(writes).toBe(0);

    await user.click(screen.getByRole('tab', { name: 'Desired (2)' }));
    expect(screen.getByLabelText('Desired applications')).toHaveTextContent('gb-production');
  });

  it('runs a preview of the saved spec from the header action', async () => {
    const user = userEvent.setup();
    const { router } = await openDetails();
    await user.click(await screen.findByRole('button', { name: 'Generate preview' }));
    expect(router.state.location.search).toBe('?tab=preview');
    expect(await screen.findByRole('list', { name: 'Summary of changes' })).toHaveTextContent('0 added');
  });

  it('reports YAML errors inline and permission errors with guidance', async () => {
    const user = userEvent.setup();
    server.use(http.post('*/api/v1/applicationsets/generate', () => problem(403, 'permission_denied', 'permission denied: applicationsets, create, default/guestbook')));
    await openDetails('/applicationsets/guestbook?tab=preview');
    const editor = await screen.findByRole('textbox', { name: /ApplicationSet spec/ });
    await user.clear(editor);
    await user.click(editor);
    await user.paste('generators: [\n');
    await user.click(screen.getByRole('button', { name: 'Preview changes' }));
    expect(editor).toHaveAttribute('aria-invalid', 'true');
    expect(editor).toHaveAccessibleDescription(/^Line \d+:/);

    await user.click(screen.getByRole('button', { name: 'Reset to saved spec' }));
    await user.click(screen.getByRole('button', { name: 'Preview changes' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Preview needs permission to create ApplicationSets');
    expect(alert).toHaveTextContent('applicationsets, create on project default');
  });

  it('explains deletion impact, requires the name and returns to the list', async () => {
    const user = userEvent.setup();
    let deleted = '';
    server.use(
      http.delete('*/api/v1/applicationsets/:name', ({ params }) => {
        deleted = String(params.name);
        return HttpResponse.json({});
      }),
    );
    const { router } = await openDetails();
    const trigger = await screen.findByRole('button', { name: 'Delete' });
    await user.click(trigger);
    const dialog = screen.getByRole('dialog', { name: 'Delete ApplicationSet guestbook?' });
    expect(dialog).toHaveTextContent('Deployed resources are kept');
    expect(dialog).toHaveTextContent('preserveResourcesOnDeletion is true');
    expect(dialog).toHaveTextContent('2 generated Applications will be deleted');
    const confirm = within(dialog).getByRole('button', { name: 'Delete ApplicationSet' });
    expect(confirm).toBeDisabled();
    expect(within(dialog).getByRole('textbox', { name: /Type guestbook to confirm/ })).toHaveFocus();
    await user.keyboard('guestbook');
    await user.click(confirm);
    await waitFor(() => expect(router.state.location.pathname).toBe('/applicationsets'));
    expect(deleted).toBe('guestbook');
    expect(await screen.findByText('ApplicationSet guestbook deleted')).toBeInTheDocument();
  });

  it('keeps the dialog open with the error when deletion fails, and Escape returns focus', async () => {
    const user = userEvent.setup();
    server.use(http.delete('*/api/v1/applicationsets/:name', () => problem(403, 'permission_denied', 'permission denied: applicationsets, delete')));
    await openDetails();
    const trigger = await screen.findByRole('button', { name: 'Delete' });
    await user.click(trigger);
    await user.keyboard('guestbook');
    await user.click(screen.getByRole('button', { name: 'Delete ApplicationSet' }));
    expect(await screen.findByText('The ApplicationSet was not deleted')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('shows preview environments for pull-request generators', async () => {
    const user = userEvent.setup();
    withGeneratedApps();
    renderApp('/applicationsets/shop-previews');
    await user.click(await screen.findByRole('tab', { name: 'Preview environments' }));
    const table = screen.getByRole('table', { name: 'Preview environments' });
    const pr42 = within(table).getByRole('row', { name: /Pull request 42|#42/ });
    expect(within(pr42).getByRole('link', { name: /#42/ })).toHaveAttribute('href', 'https://github.com/acme/shop/pull/42');
    expect(pr42).toHaveTextContent('feature/cart');
    expect(pr42).toHaveTextContent('9f8e7d6c');
    expect(pr42).toHaveTextContent('Healthy');
    expect(screen.getByText(/Deleted when the pull request closes/)).toBeInTheDocument();
  });

  it('explains a missing ApplicationSet (403, as the server answers for missing and forbidden) and links back to the list', async () => {
    renderApp('/applicationsets/does-not-exist');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('ApplicationSet does-not-exist could not be loaded');
    expect(alert).toHaveTextContent('does not exist or you are not allowed to see it. It may have been deleted or renamed');
    expect(screen.getByRole('link', { name: 'Back to ApplicationSets' })).toHaveAttribute('href', '/applicationsets');
  });
});
