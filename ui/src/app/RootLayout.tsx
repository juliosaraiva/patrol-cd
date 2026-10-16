import { Outlet } from 'react-router';
import { ToastProvider } from '../design';

/**
 * Pathless root route shared by the shell and the auth pages. Providers whose rendered output
 * can contain router links (toast actions such as a "View application" ButtonLink) live here,
 * inside the router, rather than around <RouterProvider>.
 */
export function RootLayout() {
  return (
    <ToastProvider>
      <Outlet />
    </ToastProvider>
  );
}
