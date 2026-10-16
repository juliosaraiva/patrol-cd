import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '../../mocks/server';
import { failGet, renderSettings } from './testUtils';
import { SETTINGS_SECTIONS } from './sections';

describe('Settings index', () => {
  it('links every section with a short description', async () => {
    const user = userEvent.setup();
    const { router } = await renderSettings('/settings', 'Settings');
    const main = screen.getByRole('main');
    for (const section of SETTINGS_SECTIONS) {
      const link = within(main).getByRole('link', { name: new RegExp(section.title) });
      expect(link).toHaveAttribute('href', `/settings/${section.path}`);
      expect(link).toHaveTextContent(section.description);
    }
    await user.click(within(main).getByRole('link', { name: /^Clusters/ }));
    await screen.findByRole('heading', { level: 1, name: 'Clusters' });
    expect(router.state.location.pathname).toBe('/settings/clusters');
  });
});

describe('Settings → Appearance', () => {
  it('switches the theme and keeps it for this browser', async () => {
    const user = userEvent.setup();
    await renderSettings('/settings/appearance', 'Appearance');
    const group = screen.getByRole('group', { name: 'Theme' });
    expect(within(group).getByRole('radio', { name: /System/ })).toBeChecked();
    await user.click(within(group).getByRole('radio', { name: /Dark/ }));
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark');
    expect(window.localStorage.getItem('patrol.theme')).toBe('dark');
    await user.keyboard('{ArrowUp}');
    expect(within(group).getByRole('radio', { name: /Light/ })).toBeChecked();
    expect(document.documentElement).toHaveAttribute('data-theme', 'light');
  });
});

describe('Settings → Notifications', () => {
  it('lists configured services, triggers with their descriptions and templates', async () => {
    await renderSettings('/settings/notifications', 'Notifications');
    expect(await screen.findByRole('list', { name: 'Services' })).toHaveTextContent('slack');
    const triggers = await screen.findByRole('list', { name: 'Triggers' });
    const items = within(triggers).getAllByRole('listitem');
    // Sorted by name (the first child is the name); each shows a description (default catalog here).
    expect(items.map((li) => li.firstElementChild?.textContent)).toEqual([
      'on-deployed',
      'on-health-degraded',
      'on-sync-failed',
    ]);
    expect(within(triggers).getByText('on-sync-failed').parentElement).toHaveTextContent(
      'Application syncing has failed (default catalog description)',
    );
    expect(within(triggers).getByText('on-deployed').parentElement).toHaveTextContent('Application is synced and healthy. Triggered once per commit.');
    expect(screen.getByRole('list', { name: 'Templates' })).toHaveTextContent('app-deployed');
    expect(screen.getAllByText('3 configured')).toHaveLength(2);
  });

  it('shows a server-provided trigger description instead of the catalog one', async () => {
    await renderSettings('/settings/notifications', 'Notifications', () =>
      server.use(
        http.get('*/api/v1/notifications/triggers', () =>
          HttpResponse.json({ items: [{ name: 'on-deployed', description: 'Deployed to production' }, { name: 'on-pager' }] }),
        ),
      ),
    );
    const triggers = await screen.findByRole('list', { name: 'Triggers' });
    expect(within(triggers).getByText('on-deployed').parentElement).toHaveTextContent('Deployed to production');
    expect(within(triggers).getByText('on-pager').parentElement).toHaveTextContent('Custom trigger');
  });

  it('explains a failure per catalog', async () => {
    failGet('*/api/v1/notifications/triggers', 503, 'unavailable', 'notifications controller unavailable');
    await renderSettings('/settings/notifications', 'Notifications');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Triggers could not be loaded');
    expect(alert).toHaveTextContent('notifications controller unavailable');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    expect(await screen.findByRole('list', { name: 'Services' })).toBeInTheDocument();
  });
});
