import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { createBrowserRouter } from 'react-router';
import { routerBasename } from './api/basePath';
import { createQueryClient } from './api/queryClient';
import { App } from './app/App';
import { appRoutes } from './app/routes';
import { initTheme } from './design/theme';
import { initExtensions } from './extensions';
import './design/global.css';

async function enableMocking(): Promise<void> {
  // Statically false in production builds, so the mock layer is not bundled.
  if (import.meta.env.VITE_MOCK_API !== '1') return;
  const { startMocking } = await import('./mocks/browser');
  await startMocking();
}

/** The running app. In a browser it lives as long as the page; the bootstrap test stops it. */
export interface StartedApp {
  /** Unmounts the app (closing its streams and timers) and disposes the router and query cache. */
  stop: () => void;
}

async function main(): Promise<StartedApp> {
  initTheme();
  await enableMocking();
  const container = document.getElementById('root');
  if (!container) throw new Error('missing #root element');
  const router = createBrowserRouter(appRoutes, { basename: routerBasename() || '/' });
  const queryClient = createQueryClient();
  const root = createRoot(container);
  root.render(
    <StrictMode>
      <App router={router} queryClient={queryClient} />
    </StrictMode>,
  );
  // UI extensions (API-120): window.React/ReactDOM/ReactJSXRuntime and window.extensionsAPI are
  // defined now, then extensions.js loads; slots and extension routes re-render as bundles register.
  void initExtensions();
  return {
    stop() {
      root.unmount();
      router.dispose();
      queryClient.clear();
    },
  };
}

/** Resolves once the first render is scheduled. */
export const started: Promise<StartedApp> = main();
