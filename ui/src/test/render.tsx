import { QueryClient } from '@tanstack/react-query';
import { render, type RenderResult } from '@testing-library/react';
import { QueryClientProvider } from '@tanstack/react-query';
import type { ReactElement, ReactNode } from 'react';
import { createMemoryRouter, RouterProvider, type RouteObject } from 'react-router';
import { App } from '../app/App';
import { appRoutes } from '../app/routes';
import { RootLayout } from '../app/RootLayout';

export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: Infinity }, mutations: { retry: false } },
  });
}

/** Renders the whole app (real routes, providers, MSW-backed API) at `path`. */
export function renderApp(path = '/applications'): RenderResult & { router: ReturnType<typeof createMemoryRouter>; queryClient: QueryClient } {
  const router = createMemoryRouter(appRoutes, { initialEntries: [path] });
  const queryClient = createTestQueryClient();
  const result = render(<App router={router} queryClient={queryClient} />);
  return { ...result, router, queryClient };
}

/** Renders a component with query client, a memory router and the root layout (toasts). */
export function renderWithProviders(ui: ReactElement, options: { path?: string; routes?: RouteObject[] } = {}): RenderResult {
  const queryClient = createTestQueryClient();
  const Wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  // Same root layout as the app, so toasts render inside the router.
  const routes: RouteObject[] = [{ element: <RootLayout />, children: options.routes ?? [{ path: '*', element: ui }] }];
  const router = createMemoryRouter(routes, { initialEntries: [options.path ?? '/'] });
  return render(<RouterProvider router={router} />, { wrapper: Wrapper });
}
