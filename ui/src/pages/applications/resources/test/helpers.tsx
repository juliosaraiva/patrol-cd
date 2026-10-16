import { useLocation } from 'react-router';
import { http, HttpResponse } from 'msw';
import type { ReactElement } from 'react';
import { renderWithProviders } from '../../../../test/render';
import { appsResourcesHandlers } from '../../../../mocks/handlers/apps-resources';
import { settingsFixture } from '../../../../mocks/fixtures';
import { server } from '../../../../mocks/server';

/** Prints the current location so tests can assert URL state. */
export function LocationProbe() {
  const location = useLocation();
  return (
    <span hidden data-testid="location">
      {`${location.pathname}${location.search}`}
    </span>
  );
}

/** Gives this workstream's handlers priority over other areas' mocks of the same routes. */
export function useResourceHandlers(): void {
  server.use(...appsResourcesHandlers);
}

export function renderAt(ui: ReactElement, path = '/applications/shop') {
  return renderWithProviders(
    <>
      {ui}
      <LocationProbe />
    </>,
    { path },
  );
}

export function currentSearch(container: { getByTestId: (id: string) => HTMLElement }): URLSearchParams {
  const text = container.getByTestId('location').textContent ?? '';
  return new URLSearchParams(text.includes('?') ? text.slice(text.indexOf('?')) : '');
}

/** Paths of the can-i requests made since the last `denyCanI` (raw, still percent-encoded). */
export const canIRequests: string[] = [];

/**
 * Answers can-i "no" for the given (resource, action) pairs and "yes" otherwise. Fine-grained
 * actions ("update/apps/Deployment/shop/web") arrive as one percent-encoded path segment.
 */
export function denyCanI(...rules: [resource: string, action: string][]) {
  canIRequests.length = 0;
  server.use(
    http.get('*/api/v1/account/can-i/:resource/:action/*', ({ params, request }) => {
      canIRequests.push(new URL(request.url).pathname);
      const action = decodeURIComponent(String(params.action));
      const denied = rules.some(([r, a]) => r === params.resource && a === action);
      return HttpResponse.json({ value: denied ? 'no' : 'yes' });
    }),
  );
}

export function enableExec() {
  server.use(http.get('*/api/v1/settings', () => HttpResponse.json({ ...settingsFixture, execEnabled: true })));
}
