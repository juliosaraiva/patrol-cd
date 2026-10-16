import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Application, ApplicationPatchRequest } from '../../api/types';
import { applicationFixture } from '../../mocks/fixtures';
import { notificationServicesFixture, notificationTriggersFixture } from '../../mocks/fixtures/notifications';
import { problem } from '../../mocks/handlers';
import { server } from '../../mocks/server';
import { renderWithProviders } from '../../test/render';
import { applicationRBACObject, SubscriptionEditor } from './SubscriptionEditor';

const P = 'notifications.cd.patrol.example';

function app(annotations: Record<string, string> = {}): Application {
  const a = applicationFixture('guestbook', { project: 'default' });
  a.metadata.annotations = annotations;
  return a;
}

function useCatalog() {
  server.use(
    http.get('*/api/v1/notifications/triggers', () => HttpResponse.json(notificationTriggersFixture)),
    http.get('*/api/v1/notifications/services', () => HttpResponse.json(notificationServicesFixture)),
  );
}

function capturePatch() {
  const calls: { url: URL; body: ApplicationPatchRequest }[] = [];
  server.use(
    http.patch('*/api/v1/applications/:name', async ({ request }) => {
      const body = (await request.json()) as ApplicationPatchRequest;
      calls.push({ url: new URL(request.url), body });
      const base = app();
      const annotations = (JSON.parse(body.patch) as { metadata: { annotations: Record<string, string | null> } }).metadata.annotations;
      base.metadata.annotations = Object.fromEntries(Object.entries(annotations).filter((e): e is [string, string] => e[1] !== null));
      return HttpResponse.json(base);
    }),
  );
  return calls;
}

describe('SubscriptionEditor (NOTIF-109)', () => {
  it('lists existing subscriptions in a table and explains the empty state', async () => {
    useCatalog();
    const { unmount } = renderWithProviders(<SubscriptionEditor application={app({ [`${P}/subscribe.on-sync-failed.slack`]: 'ops;oncall' })} />);
    const table = await screen.findByRole('table', { name: 'Notification subscriptions' });
    expect(within(table).getByRole('row', { name: /on-sync-failed/ })).toHaveTextContent('ops, oncall');
    unmount();
    renderWithProviders(<SubscriptionEditor application={app()} />);
    expect(await screen.findByText(/Not subscribed to any trigger/)).toBeInTheDocument();
  });

  it('adds a subscription with keyboard only and saves a merge patch of that key', async () => {
    const user = userEvent.setup();
    useCatalog();
    const calls = capturePatch();
    renderWithProviders(<SubscriptionEditor application={app()} />);
    await user.click(await screen.findByRole('button', { name: 'Edit subscriptions' }));
    const trigger = screen.getByRole('combobox', { name: /Trigger/ });
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAttribute('list');
    await user.keyboard('on-sync-succeeded');
    await user.tab();
    await user.keyboard('slack');
    await user.tab();
    await user.keyboard('my-channel1; my-channel2');
    await user.click(screen.getByRole('button', { name: 'Save subscriptions' }));

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0]?.body.patchType).toBe('merge');
    expect(JSON.parse(calls[0]?.body.patch ?? '{}')).toEqual({
      metadata: { annotations: { [`${P}/subscribe.on-sync-succeeded.slack`]: 'my-channel1;my-channel2' } },
    });
    expect(await screen.findByText('Notification subscriptions saved')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Edit subscriptions' })).toHaveFocus();
  });

  it('removes a subscription by sending null for its key', async () => {
    const user = userEvent.setup();
    useCatalog();
    const calls = capturePatch();
    renderWithProviders(
      <SubscriptionEditor application={app({ [`${P}/subscribe.on-sync-failed.slack`]: 'ops', [`${P}/subscribe.on-deployed.email`]: 'a@example.test' })} />,
    );
    await user.click(await screen.findByRole('button', { name: 'Edit subscriptions' }));
    await user.click(screen.getByRole('button', { name: 'Remove Subscription 1' }));
    // Focus moves to the next row instead of being lost.
    expect(screen.getByRole('combobox', { name: /Trigger/ })).toHaveFocus();
    await user.click(screen.getByRole('button', { name: 'Save subscriptions' }));
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(JSON.parse(calls[0]?.body.patch ?? '{}')).toEqual({ metadata: { annotations: { [`${P}/subscribe.on-deployed.email`]: null } } });
  });

  it('validates recipients and required fields before saving', async () => {
    const user = userEvent.setup();
    useCatalog();
    const calls = capturePatch();
    renderWithProviders(<SubscriptionEditor application={app()} />);
    await user.click(await screen.findByRole('button', { name: 'Edit subscriptions' }));
    await user.type(screen.getByRole('combobox', { name: /Service/ }), 'email');
    await user.type(screen.getByRole('textbox', { name: /Recipients/ }), 'not-an-email');
    await user.click(screen.getByRole('button', { name: 'Save subscriptions' }));
    expect(screen.getByRole('combobox', { name: /Trigger/ })).toHaveAccessibleDescription('Choose a trigger.');
    expect(screen.getByRole('textbox', { name: /Recipients/ })).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByRole('textbox', { name: /Recipients/ })).toHaveAccessibleDescription(/"not-an-email" is not an email address./);
    expect(screen.getByRole('alert')).toHaveTextContent('Fix the highlighted fields');
    expect(calls).toHaveLength(0);
  });

  it('warns about unknown names but still saves free text', async () => {
    const user = userEvent.setup();
    useCatalog();
    const calls = capturePatch();
    renderWithProviders(<SubscriptionEditor application={app()} />);
    await user.click(await screen.findByRole('button', { name: 'Edit subscriptions' }));
    await user.type(screen.getByRole('combobox', { name: /Trigger/ }), 'on-custom');
    await user.type(screen.getByRole('combobox', { name: /Service/ }), 'webhook');
    expect(screen.getByRole('combobox', { name: /Trigger/ })).toHaveAccessibleDescription(/"on-custom" is not a configured trigger/);
    await user.click(screen.getByRole('button', { name: 'Save subscriptions' }));
    await waitFor(() => expect(calls).toHaveLength(1));
  });

  it('keeps working when the catalog cannot be loaded', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('*/api/v1/notifications/triggers', () => problem(503, 'unavailable', 'notifications configuration unavailable')),
      http.get('*/api/v1/notifications/services', () => HttpResponse.json({ items: [] })),
    );
    capturePatch();
    renderWithProviders(<SubscriptionEditor application={app()} />);
    await user.click(await screen.findByRole('button', { name: 'Edit subscriptions' }));
    expect(await screen.findByText('Suggestions unavailable')).toBeInTheDocument();
    await user.type(screen.getByRole('combobox', { name: /Trigger/ }), 'on-anything');
    expect(screen.getByRole('combobox', { name: /Trigger/ })).not.toHaveAttribute('aria-invalid');
  });

  it('shows the server error with guidance when the update is forbidden', async () => {
    const user = userEvent.setup();
    useCatalog();
    server.use(http.patch('*/api/v1/applications/:name', () => problem(403, 'permission_denied', 'permission denied: applications, update, default/guestbook')));
    renderWithProviders(<SubscriptionEditor application={app()} />);
    await user.click(await screen.findByRole('button', { name: 'Edit subscriptions' }));
    await user.type(screen.getByRole('combobox', { name: /Trigger/ }), 'on-deployed');
    await user.type(screen.getByRole('combobox', { name: /Service/ }), 'webhook');
    await user.click(screen.getByRole('button', { name: 'Save subscriptions' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Subscriptions were not saved');
    expect(alert).toHaveTextContent('Ask an administrator for the required RBAC permission');
  });

  it('is read-only without applications, update', async () => {
    useCatalog();
    server.use(http.get('*/api/v1/account/can-i/applications/update/*', () => HttpResponse.json({ value: 'no' })));
    renderWithProviders(<SubscriptionEditor application={app({ [`${P}/subscribe.on-sync-failed.slack`]: 'ops' })} />);
    expect(await screen.findByText(/lacks applications, update/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Edit subscriptions' })).not.toBeInTheDocument();
  });

  it('shows default-trigger and YAML subscriptions read-only', async () => {
    useCatalog();
    renderWithProviders(<SubscriptionEditor application={app({ [`${P}/subscribe.slack`]: 'ops', [`${P}/subscriptions`]: '- trigger: [on-deployed]' })} />);
    expect(await screen.findByRole('heading', { name: 'Other subscriptions' })).toBeInTheDocument();
    expect(screen.getByText(/Default triggers of/)).toHaveTextContent('Default triggers of slack to ops');
    expect(screen.getByLabelText('Subscriptions annotation')).toHaveTextContent('- trigger: [on-deployed]');
  });

  it('cancels without saving and restores focus to the edit button', async () => {
    const user = userEvent.setup();
    useCatalog();
    renderWithProviders(<SubscriptionEditor application={app()} />);
    await user.click(await screen.findByRole('button', { name: 'Edit subscriptions' }));
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.getByRole('button', { name: 'Edit subscriptions' })).toHaveFocus();
  });
});

describe('applicationRBACObject', () => {
  it('uses the 3-part form outside the control-plane namespace', () => {
    expect(applicationRBACObject(applicationFixture('a', { namespace: 'patrol' }), 'patrol')).toBe('default/a');
    expect(applicationRBACObject(applicationFixture('a', { namespace: 'team' }), 'patrol')).toBe('default/team/a');
  });
});
