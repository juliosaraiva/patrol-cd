import '@testing-library/jest-dom/vitest';
import { cleanup, configure } from '@testing-library/react';
import { afterAll, afterEach, beforeAll, beforeEach, vi } from 'vitest';
import { resetMockState } from '../mocks/handlers';
import { server } from '../mocks/server';
import { beginTest, blockRealNetwork, checkDocumentAfterCleanup, finishTest, onUnhandledRequest, startGuards, stopGuards } from './guards';

// Lazy route modules are transformed on first use; give async queries room on cold runs.
configure({ asyncUtilTimeout: 5000 });

// Before server.listen(): MSW wraps the current fetch, which then never reaches the network.
blockRealNetwork();
beforeAll(() => {
  startGuards();
  // Unmocked requests fail (like the "error" strategy) and fail the test that made them.
  server.listen({ onUnhandledRequest });
});
const hasDOM = typeof window !== 'undefined';

beforeEach(() => {
  beginTest();
});

afterEach(() => {
  try {
    server.resetHandlers();
    resetMockState();
    vi.useRealTimers();
    if (!hasDOM) return;
    cleanup();
    checkDocumentAfterCleanup();
    window.localStorage.clear();
    window.sessionStorage.clear();
    document.documentElement.removeAttribute('data-theme');
    document.head.querySelectorAll('base').forEach((b) => b.remove());
  } finally {
    finishTest();
  }
});
afterAll(() => {
  server.close();
  stopGuards();
});

// jsdom lacks matchMedia; components treat it as optional, but provide a stub for consistency.
if (hasDOM && typeof window.matchMedia !== 'function') {
  window.matchMedia = (query: string) =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      addListener: () => undefined,
      removeListener: () => undefined,
      dispatchEvent: () => false,
    }) as MediaQueryList;
}
