import { act, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { server } from './mocks/server';
import type { StartedApp } from './main';

// The bootstrap renders with its own createRoot, outside Testing Library, so cleanup() cannot
// unmount it. A test that leaves it running keeps its watch streams and queries alive after the
// file's MSW server has closed: the stream then reconnects over the real network (jsdom's origin
// http://localhost:3000) and its 401 handling ran after jsdom teardown ("window is not defined",
// docs/verification/runs/2026-10-16-ui-flake.md).
let app: StartedApp | undefined;
let stopListening: (() => void) | undefined;

afterEach(() => {
  stopListening?.();
  stopListening = undefined;
  if (app) act(() => app?.stop());
  app = undefined;
  document.getElementById('root')?.remove();
});

/** Requests the mocked server receives for /api/v1/stream/* while the test runs. */
function recordStreams(): Request[] {
  const streams: Request[] = [];
  const onStart = ({ request }: { request: Request }) => {
    if (new URL(request.url).pathname.startsWith('/api/v1/stream/')) streams.push(request);
  };
  server.events.on('request:start', onStart);
  stopListening = () => server.events.removeListener('request:start', onStart);
  return streams;
}

// Runs the real bootstrap (src/main.tsx) once: browser router at jsdom's URL, MSW-backed API.
describe('app bootstrap', () => {
  it('renders the app and starts the UI extensions host at startup (API-120)', async () => {
    const root = document.createElement('div');
    root.id = 'root';
    document.body.appendChild(root);
    expect(window.extensionsAPI).toBeUndefined();
    const streams = recordStreams();

    const { started } = await import('./main');
    app = await started;
    expect(await screen.findByRole('heading', { level: 1, name: 'Applications' })).toBeInTheDocument();

    // Globals extension bundles rely on, defined without visiting a page that hosts a slot.
    expect(Object.keys(window.extensionsAPI ?? {}).sort()).toEqual([
      'registerAppViewExtension',
      'registerResourceExtension',
      'registerStatusPanelExtension',
      'registerSystemLevelExtension',
      'registerTopBarActionMenuExt',
    ]);
    expect(window.React?.version).toMatch(/^19\./);
    expect(typeof window.ReactJSXRuntime?.jsx).toBe('function');
    expect(typeof window.ReactDOM?.createPortal).toBe('function');
    expect(window.extensions).toEqual({ resources: {} });
    // extensions.js is requested once, with a classic script tag under the root path.
    const scripts = document.head.querySelectorAll<HTMLScriptElement>('script[data-patrol-extension]');
    expect([...scripts].map((s) => s.src)).toEqual([new URL('extensions.js', document.baseURI).href]);

    // Stopping the app unmounts it and closes the watch streams it opened (the application list
    // watch starts once the list has loaded), so nothing outlives the test.
    await waitFor(() => expect(streams.map((r) => new URL(r.url).pathname)).toContain('/api/v1/stream/applications'));
    act(() => app?.stop());
    app = undefined;
    expect(root).toBeEmptyDOMElement();
    expect(streams.filter((r) => !r.signal.aborted).map((r) => r.url)).toEqual([]);
  });
});
