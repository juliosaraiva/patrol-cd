import { http, HttpResponse } from 'msw';
import { applicationsFixture } from '../fixtures';
import { emptyList, problem } from './util';

/**
 * Read-only fallbacks so every page renders in dev mock mode; per-area handlers come first. Lists
 * follow the published shapes: CRD-backed lists carry `metadata`, others only `items`.
 */
export const resourceHandlers = [
  http.get('*/api/v1/applications', () => HttpResponse.json(applicationsFixture)),
  http.get('*/api/v1/applications/:name', ({ params }) => {
    const app = applicationsFixture.items.find((a) => a.metadata.name === params.name);
    return app ? HttpResponse.json(app) : problem(404, 'not_found', `application "${String(params.name)}" not found`);
  }),
  http.get('*/api/v1/applicationsets', () => HttpResponse.json(emptyList)),
  http.get('*/api/v1/projects', () => HttpResponse.json(emptyList)),
  http.get('*/api/v1/repositories', () => HttpResponse.json(emptyList)),
  http.get('*/api/v1/repocreds', () => HttpResponse.json(emptyList)),
  http.get('*/api/v1/clusters', () => HttpResponse.json(emptyList)),
  http.get('*/api/v1/certificates', () => HttpResponse.json({ items: [] })),
  http.get('*/api/v1/gpgkeys', () => HttpResponse.json({ items: [] })),
  http.get('*/api/v1/scmproviders', () => HttpResponse.json(emptyList)),
  http.get('*/api/v1/account', () => HttpResponse.json({ items: [] })),
  http.get('*/api/v1/notifications/:kind', () => HttpResponse.json({ items: [] })),
  // Watch streams stay open and send the 15 s heartbeat comment until the client disconnects;
  // data events are `{type, object}` (WatchEvent_*), sent by per-test handlers.
  http.get('*/api/v1/stream/*', ({ request }) => {
    const encoder = new TextEncoder();
    let timer: ReturnType<typeof setInterval> | undefined;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode(': connected\n\n'));
        timer = setInterval(() => controller.enqueue(encoder.encode(': heartbeat\n\n')), 15_000);
        request.signal.addEventListener('abort', () => {
          clearInterval(timer);
          try {
            controller.close();
          } catch {
            // Already closed by cancel().
          }
        });
      },
      cancel() {
        clearInterval(timer);
      },
    });
    return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' } });
  }),
];
