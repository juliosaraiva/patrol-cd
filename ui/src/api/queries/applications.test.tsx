import { QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import type { Application, ApplicationList } from '../types';
import { describe, expect, it } from 'vitest';
import { applicationFixture, applicationsFixture } from '../../mocks/fixtures';
import { server } from '../../mocks/server';
import { createTestQueryClient } from '../../test/render';
import { useApplication, useApplications, useUpdateApplication } from './applications';
import { queryKeys } from './keys';

function sse(events: unknown[], onRequest?: (url: URL) => void) {
  return http.get('*/api/v1/stream/applications', ({ request }) => {
    onRequest?.(new URL(request.url));
    const encoder = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        events.forEach((e, i) => controller.enqueue(encoder.encode(`id: ${i + 1}\ndata: ${JSON.stringify(e)}\n\n`)));
        // Leave the stream open, like a real watch.
        request.signal.addEventListener('abort', () => controller.close());
      },
    });
    return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream' } });
  });
}

describe('useApplications', () => {
  it('lists applications and merges watch events into the cached list and detail entries', async () => {
    const watched: URL[] = [];
    const degraded = applicationFixture('guestbook', { health: 'Degraded' });
    const added = applicationFixture('billing');
    server.use(
      sse(
        [
          { type: 'MODIFIED', object: degraded },
          { type: 'ADDED', object: added },
          { type: 'DELETED', object: applicationFixture('search') },
        ],
        (url) => watched.push(url),
      ),
    );
    const queryClient = createTestQueryClient();
    const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    const { result, unmount } = renderHook(() => useApplications({ projects: ['default'] }), { wrapper });

    await waitFor(() => expect(result.current.data?.items).toHaveLength(applicationsFixture.items.length));
    await waitFor(() => expect(result.current.data?.items.map((a) => a.metadata.name)).toEqual(['guestbook', 'payments', 'billing']), {
      timeout: 3000,
    });
    expect(result.current.data?.items[0]?.status?.health?.status).toBe('Degraded');
    // 'patrol' is the control-plane namespace (settings.controllerNamespace): keyed without it.
    expect(queryClient.getQueryData(queryKeys.applications.detail('billing'))).toEqual(added);
    expect(watched[0]?.searchParams.get('resourceVersion')).toBe('100');
    expect(watched[0]?.searchParams.getAll('projects')).toEqual(['default']);
    unmount();
  });

  it('does not open a watch when disabled', async () => {
    let watchCalls = 0;
    server.use(sse([], () => (watchCalls += 1)));
    const queryClient = createTestQueryClient();
    const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    const { result } = renderHook(() => useApplications({}, { watch: false }), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    await new Promise((r) => setTimeout(r, 50));
    expect(watchCalls).toBe(0);
  });
});

function wrapperFor() {
  const queryClient = createTestQueryClient();
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  return { queryClient, wrapper };
}

function withVersion(app: Application, resourceVersion: string, health?: string): Application {
  return {
    ...app,
    metadata: { ...app.metadata, resourceVersion },
    status: { ...app.status, ...(health ? { health: { status: health } } : {}) },
  };
}

describe('application cache keys', () => {
  it('delivers mutation results to the default-namespace details page (no scope)', async () => {
    const updated = withVersion(applicationFixture('guestbook'), '2');
    server.use(http.put('*/api/v1/applications/guestbook', () => HttpResponse.json(updated)));
    const { queryClient, wrapper } = wrapperFor();
    const { result } = renderHook(() => ({ app: useApplication('guestbook'), update: useUpdateApplication() }), { wrapper });
    await waitFor(() => expect(result.current.app.data?.metadata.resourceVersion).toBe('1'));

    const current = result.current.app.data;
    if (!current) throw new Error('application not loaded');
    await act(() => result.current.update.mutateAsync({ app: current }));
    await waitFor(() => expect(result.current.app.data?.metadata.resourceVersion).toBe('2'));
    const detailKeys = queryClient
      .getQueryCache()
      .findAll({ queryKey: ['applications', 'detail'] })
      .map((q) => q.queryKey);
    expect(detailKeys).toEqual([queryKeys.applications.detail('guestbook')]);
  });

  it('treats the control-plane namespace and no namespace as the same application', async () => {
    const { queryClient, wrapper } = wrapperFor();
    const { result } = renderHook(
      () => ({ bare: useApplication('guestbook'), explicit: useApplication('guestbook', { appNamespace: 'patrol' }), other: useApplication('guestbook', { appNamespace: 'team-a' }) }),
      { wrapper },
    );
    await waitFor(() => expect(result.current.explicit.data).toBeDefined());
    expect(result.current.bare.data).toBe(result.current.explicit.data);
    await waitFor(() => expect(result.current.other.isFetched).toBe(true));
    // Only canonical keys were ever fetched (the explicit scope waited for settings).
    const fetched = queryClient
      .getQueryCache()
      .findAll({ queryKey: ['applications', 'detail'], predicate: (q) => q.state.dataUpdatedAt > 0 || q.state.fetchStatus !== 'idle' })
      .map((q) => q.queryKey[2]);
    expect(fetched.sort()).toEqual(['', 'team-a']);
  });

  it('delivers list watch events to the default-namespace details page', async () => {
    const degraded = withVersion(applicationFixture('guestbook', { health: 'Degraded' }), '101');
    server.use(sse([{ type: 'MODIFIED', object: degraded }]));
    const { wrapper } = wrapperFor();
    const { result } = renderHook(() => ({ list: useApplications(), app: useApplication('guestbook') }), { wrapper });
    await waitFor(() => expect(result.current.app.data?.status?.health?.status).toBe('Degraded'), { timeout: 3000 });
  });
});

describe('watched list refetch', () => {
  /** Server with an ordered event log: a watch from resourceVersion N gets every later event. */
  function watchServer() {
    const log: { rv: number; event: unknown }[] = [];
    const watches: string[] = [];
    let snapshot: ApplicationList = applicationsFixture;
    const handlers = [
      http.get('*/api/v1/applications', () => HttpResponse.json(snapshot)),
      http.get('*/api/v1/stream/applications', ({ request }) => {
        const from = Number(new URL(request.url).searchParams.get('resourceVersion') ?? '0');
        watches.push(String(from));
        const encoder = new TextEncoder();
        const body = new ReadableStream<Uint8Array>({
          start(controller) {
            for (const { rv, event } of log) {
              if (rv > from) controller.enqueue(encoder.encode(`data: ${JSON.stringify(event)}\n\n`));
            }
            request.signal.addEventListener('abort', () => controller.close());
          },
        });
        return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream' } });
      }),
    ];
    return {
      handlers,
      watches,
      record: (rv: number, event: unknown) => log.push({ rv, event }),
      setSnapshot: (list: ApplicationList) => {
        snapshot = list;
      },
    };
  }

  it('restarts the watch from a refetched snapshot so events it predates are not lost', async () => {
    const srv = watchServer();
    server.use(...srv.handlers);
    // Event 105 already happened when the watch (from 100) starts.
    srv.record(105, { type: 'MODIFIED', object: withVersion(applicationFixture('guestbook', { health: 'Degraded' }), '105') });
    const { queryClient, wrapper } = wrapperFor();
    const { result } = renderHook(() => useApplications(), { wrapper });
    const guestbookHealth = () => result.current.data?.items.find((a) => a.metadata.name === 'guestbook')?.status?.health?.status;
    await waitFor(() => expect(guestbookHealth()).toBe('Degraded'), { timeout: 3000 });
    expect(srv.watches).toEqual(['100']);

    // A refetch returns a snapshot taken at 103, before event 105 (e.g. a lagging cache).
    srv.setSnapshot({ ...applicationsFixture, metadata: { resourceVersion: '103' } });
    await act(() => queryClient.invalidateQueries({ queryKey: queryKeys.applications.lists() }));
    await waitFor(() => expect(srv.watches).toEqual(['100', '103']));
    // The restarted watch replays 105 on top of the older snapshot.
    await waitFor(() => expect(guestbookHealth()).toBe('Degraded'), { timeout: 3000 });
    expect(result.current.data?.metadata?.resourceVersion).toBe('103');
  });

  it('does not refetch a watched list on window focus', () => {
    const { queryClient, wrapper } = wrapperFor();
    renderHook(() => useApplications(), { wrapper });
    const query = queryClient.getQueryCache().find({ queryKey: queryKeys.applications.list({}) });
    expect(query?.observers[0]?.options.refetchOnWindowFocus).toBe(false);
  });
});
