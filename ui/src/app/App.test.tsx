import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter, type RouteObject } from 'react-router';
import { describe, expect, it } from 'vitest';
import { Button, ButtonLink, useToast } from '../design';
import { createTestQueryClient } from '../test/render';
import { App } from './App';
import { appRoutes } from './routes';

function RaiseToast() {
  const { toast } = useToast();
  return (
    <Button
      onClick={() =>
        toast({
          title: 'Application created',
          tone: 'success',
          durationMs: null,
          action: (
            <ButtonLink to="/applications/guestbook" size="sm">
              View application
            </ButtonLink>
          ),
        })
      }
    >
      Create
    </Button>
  );
}

describe('App providers', () => {
  it('renders toasts inside the router so toast actions can be router links', async () => {
    const user = userEvent.setup();
    const [root] = appRoutes;
    if (!root?.children) throw new Error('expected the pathless root route');
    // The real root route (RootLayout) with one extra page that raises a toast.
    const routes: RouteObject[] = [{ ...root, children: [{ path: 'toast-test', element: <RaiseToast /> }, ...root.children] }];
    const router = createMemoryRouter(routes, { initialEntries: ['/toast-test'] });
    render(<App router={router} queryClient={createTestQueryClient()} />);

    await user.click(screen.getByRole('button', { name: 'Create' }));
    const region = screen.getByRole('region', { name: 'Notifications' });
    const action = within(region).getByRole('link', { name: 'View application' });
    expect(action).toHaveAttribute('href', '/applications/guestbook');

    await user.click(action);
    await waitFor(() => expect(router.state.location.pathname).toBe('/applications/guestbook'));
    // The toast region outlives the navigation (it belongs to the root layout).
    expect(within(screen.getByRole('region', { name: 'Notifications' })).getByText('Application created')).toBeInTheDocument();
  });
});
