import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { settingsStore } from '../../../mocks/handlers/settings';
import { emptyGet, expectToast, failGet, findDialog, renderSettings, tableRows } from '../testUtils';

describe('Settings → Projects list', () => {
  it('lists projects with description and source and destination counts', async () => {
    await renderSettings('/settings/projects', 'Projects');
    await screen.findByRole('table', { name: 'Projects' });
    const rows = tableRows('Projects');
    expect(rows).toHaveLength(2);
    const finance = rows.find((r) => r.textContent?.includes('finance'));
    expect(finance).toHaveTextContent('Payments and ledger services.');
    const cells = within(finance ?? document.body).getAllByRole('cell');
    expect(cells.map((c) => c.textContent)).toEqual(['finance', 'Payments and ledger services.', '2', '2', '1', '2']);
  });

  it('sorts by name with the keyboard-operable column header', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/projects', 'Projects');
    await screen.findByRole('table', { name: 'Projects' });
    const header = screen.getByRole('columnheader', { name: /Name/ });
    expect(header).toHaveAttribute('aria-sort', 'ascending');
    await user.click(within(header).getByRole('button'));
    expect(header).toHaveAttribute('aria-sort', 'descending');
    expect(tableRows('Projects')[0]).toHaveTextContent('finance');
  });

  it('filters by search text', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/projects', 'Projects');
    await screen.findByRole('table', { name: 'Projects' });
    await user.type(screen.getByRole('searchbox', { name: 'Search projects' }), 'ledger');
    expect(tableRows('Projects')).toHaveLength(1);
    await user.keyboard('{Escape}');
    expect(tableRows('Projects')).toHaveLength(2);
  });

  it('shows the empty state and errors', async () => {
    emptyGet('*/api/v1/projects');
    await renderSettings('/settings/projects', 'Projects');
    expect(await screen.findByRole('heading', { name: 'No projects' })).toBeInTheDocument();
  });

  it('shows load errors', async () => {
    failGet('*/api/v1/projects');
    await renderSettings('/settings/projects', 'Projects');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Server error');
    expect(alert).toHaveTextContent('check the patrol-server logs');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('creates a project and opens it', async () => {
    const user = userEvent.setup();
    const { router } = await renderSettings('/settings/projects', 'Projects');
    await screen.findByRole('table', { name: 'Projects' });
    await user.click(screen.getByRole('button', { name: 'New project' }));
    const dialog = await findDialog('New project');
    await user.type(within(dialog).getByLabelText(/Name/), 'Team_A');
    await user.click(within(dialog).getByRole('button', { name: 'Create project' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Project name may contain only lowercase letters');
    await user.clear(within(dialog).getByLabelText(/Name/));
    await user.type(within(dialog).getByLabelText(/Name/), 'team-a');
    expect(within(dialog).queryByRole('alert')).not.toBeInTheDocument();
    await user.type(within(dialog).getByLabelText('Description'), 'Team A services');
    await user.click(within(dialog).getByRole('button', { name: 'Create project' }));
    await expectToast('Project created');
    await screen.findByRole('heading', { level: 1, name: 'team-a' });
    expect(router.state.location.pathname).toBe('/settings/projects/team-a');
    expect(settingsStore.projects.find((p) => p.metadata.name === 'team-a')?.spec.description).toBe('Team A services');
  });

  it('explains a duplicate project name', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/projects', 'Projects');
    await screen.findByRole('table', { name: 'Projects' });
    await user.click(screen.getByRole('button', { name: 'New project' }));
    const dialog = await findDialog('New project');
    await user.type(within(dialog).getByLabelText(/Name/), 'finance');
    await user.click(within(dialog).getByRole('button', { name: 'Create project' }));
    expect(await within(dialog).findByText('Already exists')).toBeInTheDocument();
  });
});
