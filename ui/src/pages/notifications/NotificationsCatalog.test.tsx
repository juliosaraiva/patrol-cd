import { screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { notificationServicesFixture, notificationTemplatesFixture, notificationTriggersFixture } from '../../mocks/fixtures/notifications';
import { problem } from '../../mocks/handlers';
import { server } from '../../mocks/server';
import { renderWithProviders } from '../../test/render';
import { NotificationsCatalog } from './NotificationsCatalog';

function useCatalog() {
  server.use(
    http.get('*/api/v1/notifications/services', () => HttpResponse.json(notificationServicesFixture)),
    http.get('*/api/v1/notifications/triggers', () => HttpResponse.json(notificationTriggersFixture)),
    http.get('*/api/v1/notifications/templates', () => HttpResponse.json(notificationTemplatesFixture)),
  );
}

// The catalog is a component, not a route: /settings/notifications (settings workstream) is the
// routed page and is asked to render <NotificationsCatalog /> instead of its names-only lists.
describe('notifications catalog', () => {
  it('lists services, triggers with descriptions and templates', async () => {
    useCatalog();
    renderWithProviders(<NotificationsCatalog />);
    expect(screen.getByRole('heading', { level: 2, name: 'Triggers' })).toBeInTheDocument();
    const services = await screen.findByRole('list', { name: 'Services' });
    expect(within(services).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['email', 'slack', 'webhook.ops']);
    const triggers = screen.getByRole('list', { name: 'Triggers' });
    expect(within(triggers).getAllByRole('listitem')).toHaveLength(8);
    expect(within(triggers).getByText('on-deployed').parentElement).toHaveTextContent(
      'Application is synced and healthy. Triggered once per commit. (default catalog description)',
    );
    expect(within(screen.getByRole('list', { name: 'Templates' })).getAllByRole('listitem')).toHaveLength(8);
    expect(screen.getAllByText('8 configured')).toHaveLength(2);
  });

  it('prefers server-provided descriptions and marks custom triggers', async () => {
    server.use(
      http.get('*/api/v1/notifications/services', () => HttpResponse.json({ items: [] })),
      http.get('*/api/v1/notifications/triggers', () =>
        HttpResponse.json({ items: [{ name: 'on-deployed', description: 'Deployed to production' }, { name: 'on-pager' }] }),
      ),
      http.get('*/api/v1/notifications/templates', () => HttpResponse.json({ items: [] })),
    );
    renderWithProviders(<NotificationsCatalog />);
    const triggers = await screen.findByRole('list', { name: 'Triggers' });
    expect(within(triggers).getByText('on-deployed').parentElement).toHaveTextContent('Deployed to production');
    expect(within(triggers).getByText('on-pager').parentElement).toHaveTextContent('Custom trigger');
    expect(screen.getByText(/No services are configured/)).toHaveTextContent('service.slack');
    expect(screen.getByText(/No templates are configured/)).toBeInTheDocument();
  });

  it('explains failures per section with a retry', async () => {
    server.use(
      http.get('*/api/v1/notifications/services', () => problem(503, 'unavailable', 'notifications configuration unavailable')),
      http.get('*/api/v1/notifications/triggers', () => HttpResponse.json(notificationTriggersFixture)),
      http.get('*/api/v1/notifications/templates', () => HttpResponse.json(notificationTemplatesFixture)),
    );
    renderWithProviders(<NotificationsCatalog />);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Services could not be loaded');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    expect(await screen.findByRole('list', { name: 'Triggers' })).toBeInTheDocument();
  });
});
