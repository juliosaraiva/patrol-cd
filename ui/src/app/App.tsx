import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { RouterProvider, type createBrowserRouter } from 'react-router';
import { CustomStyles } from '../shell/CustomStyles';
import { SessionSync } from './session';

type Router = ReturnType<typeof createBrowserRouter>;

/** Providers shared by the browser entry point and tests. */
export function App({ router, queryClient }: { router: Router; queryClient: QueryClient }) {
  return (
    <QueryClientProvider client={queryClient}>
      <SessionSync />
      <CustomStyles />
      {/* ToastProvider is in the root route (RootLayout) so toast actions can be router links. */}
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
}
