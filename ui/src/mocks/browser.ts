import { getResponse } from 'msw';
import { setupWorker } from 'msw/browser';
import { handlers } from './handlers';

/** MSW worker for `pnpm dev:mock` (VITE_MOCK_API=1). Never bundled into production builds. */
export const worker = setupWorker(...handlers);

/**
 * Starts the service worker; where service workers are unavailable (some embedded browsers,
 * locked-down profiles) falls back to resolving fetch() calls against the same handlers
 * in-page. Only fetch is patched; the UI never uses XMLHttpRequest.
 */
export async function startMocking(): Promise<void> {
  try {
    await worker.start({
      serviceWorker: { url: new URL('mockServiceWorker.js', document.baseURI).pathname },
      onUnhandledRequest: 'bypass',
      quiet: true,
    });
    return;
  } catch (err) {
    console.warn('[mock] service worker unavailable, mocking fetch in-page instead', err);
  }
  const realFetch = window.fetch.bind(window);
  window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    const mocked = await getResponse(handlers, request.clone());
    return mocked ?? realFetch(request);
  };
}
