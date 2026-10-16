import { http, HttpResponse } from 'msw';
import type { ApplicationResourcePatchRequest, LogEntry, ResourceActionRunRequest } from '../../api/types';
import {
  liveManifestFixture,
  logLinesFixture,
  managedResourcesFixture,
  resourceActionsFixture,
  resourceEventsFixture,
  resourceLinksFixture,
  resourcesProjectFixture,
  resourcesTreeFixture,
} from '../fixtures/apps-resources';
import { emptyObject, problem } from './util';

/*
 * Resource routes of an application (workstream apps-resources). Every application gets the
 * "shop" fixture tree, so the views render in dev mock mode for any listed application.
 * Requests are recorded in `appsResourcesRequests` for tests.
 */

export const appsResourcesRequests: { method: string; path: string; query: Record<string, string>; body?: unknown }[] = [];

function record(request: Request, body?: unknown) {
  const url = new URL(request.url);
  appsResourcesRequests.push({ method: request.method, path: url.pathname, query: Object.fromEntries(url.searchParams), body });
}

export function resetAppsResourcesRequests(): void {
  appsResourcesRequests.length = 0;
}

/**
 * SSE body with one `data:` line per entry (no event IDs, as the log routes send); follow
 * streams stay open until aborted.
 */
export function sseResponse(request: Request, entries: readonly unknown[], keepOpen: boolean): HttpResponse<ReadableStream<Uint8Array>> {
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const entry of entries) controller.enqueue(encoder.encode(`data: ${JSON.stringify(entry)}\n\n`));
      if (!keepOpen) {
        controller.close();
        return;
      }
      request.signal.addEventListener('abort', () => {
        try {
          controller.close();
        } catch {
          // Already closed.
        }
      });
    },
  });
  return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' } });
}

function logEntries(url: URL, podName?: string): LogEntry[] {
  const filtered = podName ? logLinesFixture.filter((l) => l.podName === podName) : logLinesFixture;
  const lines = filtered.length > 0 ? filtered : logLinesFixture;
  const container = url.searchParams.get('container') ?? '';
  const previous = url.searchParams.get('previous') === 'true';
  return lines.map((l) => ({ ...l, content: previous ? `[previous] ${l.content}` : container === 'sidecar' ? `[sidecar] ${l.content}` : l.content }));
}

/** Final entry of a finished (non-follow) log stream: `last: true` and no content. */
const LAST_ENTRY: LogEntry = { content: '', podName: '', last: true, timeStamp: '2026-10-16T09:00:04Z', timeStampStr: '2026-10-16T09:00:04Z' };

/** The log routes: SSE LogEntry events, or with download=true the lines as a text attachment. */
function logsResponse(request: Request, podName?: string) {
  const url = new URL(request.url);
  const entries = logEntries(url, podName);
  if (url.searchParams.get('download') === 'true') {
    return new HttpResponse(entries.map((e) => `${e.content}\n`).join(''), {
      headers: { 'Content-Type': 'application/octet-stream', 'Content-Disposition': 'attachment;filename="logs.log"' },
    });
  }
  const follow = url.searchParams.get('follow') === 'true';
  return sseResponse(request, follow ? entries : [...entries, LAST_ENTRY], follow);
}

export const appsResourcesHandlers = [
  http.get('*/api/v1/applications/:name/resource-tree', ({ request }) => {
    record(request);
    return HttpResponse.json(resourcesTreeFixture());
  }),
  http.get('*/api/v1/applications/:name/managed-resources', ({ request }) => {
    record(request);
    return HttpResponse.json(managedResourcesFixture());
  }),
  http.get('*/api/v1/applications/:name/resource', ({ request }) => {
    record(request);
    const url = new URL(request.url);
    const text = liveManifestFixture(url.searchParams.get('kind') ?? '', url.searchParams.get('resourceName') ?? '');
    return text ? HttpResponse.json({ manifest: text }) : problem(404, 'not_found', 'resource not found');
  }),
  http.post('*/api/v1/applications/:name/resource', async ({ request }) => {
    const body = (await request.json()) as ApplicationResourcePatchRequest;
    record(request, body);
    const url = new URL(request.url);
    return HttpResponse.json({ manifest: liveManifestFixture(url.searchParams.get('kind') ?? '', url.searchParams.get('resourceName') ?? '') ?? '{}' });
  }),
  http.delete('*/api/v1/applications/:name/resource', ({ request }) => {
    record(request);
    return HttpResponse.json(emptyObject);
  }),
  http.get('*/api/v1/applications/:name/resource/actions', ({ request }) => {
    record(request);
    return HttpResponse.json(resourceActionsFixture(new URL(request.url).searchParams.get('kind') ?? ''));
  }),
  http.post('*/api/v1/applications/:name/resource/actions', async ({ request }) => {
    const body = (await request.json()) as ResourceActionRunRequest;
    record(request, body);
    return HttpResponse.json(emptyObject);
  }),
  http.get('*/api/v1/applications/:name/resource/links', ({ request }) => {
    record(request);
    return HttpResponse.json(resourceLinksFixture(new URL(request.url).searchParams.get('kind') ?? ''));
  }),
  http.get('*/api/v1/applications/:name/events', ({ request }) => {
    record(request);
    return HttpResponse.json(resourceEventsFixture(new URL(request.url).searchParams.get('resourceName')));
  }),
  http.get('*/api/v1/applications/:name/pods/:pod/logs', ({ request, params }) => {
    record(request);
    return logsResponse(request, String(params.pod));
  }),
  http.get('*/api/v1/applications/:name/logs', ({ request }) => {
    record(request);
    return logsResponse(request);
  }),
  http.get('*/api/v1/projects/:name', ({ params }) => HttpResponse.json(resourcesProjectFixture(String(params.name)))),
];
