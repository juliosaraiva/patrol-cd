import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { server } from '../mocks/server';
import { setUnauthorizedHandler } from './client';
import { streamApplicationLogs, streamPodLogs } from './endpoints/applications';
import type { ApiError } from './errors';
import {
  backoffDelay,
  CLOSED,
  CONNECTING,
  createSSEParser,
  FetchEventSource,
  OPEN,
  openStream,
  type EventSourceLike,
  type PatrolEventSourceInit,
  type SSEFields,
} from './sse';

/** Test double implementing the EventSource surface openStream uses. */
class FakeEventSource implements EventSourceLike {
  static readonly supportsComments: boolean = true;
  static readonly reportsEnd: boolean = true;
  static instances: FakeEventSource[] = [];
  readonly url: string;
  readonly init: PatrolEventSourceInit | undefined;
  readyState = CONNECTING;
  status: number | undefined;
  error: ApiError | undefined;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent<string>) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  closed = false;
  private listeners = new Map<string, ((ev: MessageEvent<string>) => void)[]>();

  constructor(url: string, init?: PatrolEventSourceInit) {
    this.url = url;
    this.init = init;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  close(): void {
    this.closed = true;
    this.readyState = CLOSED;
  }

  open() {
    this.readyState = OPEN;
    this.onopen?.(new Event('open'));
  }

  message(data: unknown, id?: string) {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(data), lastEventId: id ?? '' }));
  }

  comment() {
    for (const l of this.listeners.get('comment') ?? []) l(new MessageEvent('comment', { data: 'heartbeat' }));
  }

  /** A named server event (e.g. `event: error` with a problem document). */
  named(type: string, data: unknown) {
    for (const l of this.listeners.get(type) ?? []) l(new MessageEvent(type, { data: JSON.stringify(data) }));
  }

  fail(status?: number) {
    this.status = status;
    this.readyState = CLOSED;
    this.onerror?.(new Event('error'));
  }

  static latest(): FakeEventSource {
    const last = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    if (!last) throw new Error('no EventSource created');
    return last;
  }
}

describe('createSSEParser', () => {
  function parse(chunks: string[]) {
    const events: SSEFields[] = [];
    const comments: string[] = [];
    const parser = createSSEParser({ onEvent: (e) => events.push(e), onComment: (c) => comments.push(c) });
    for (const chunk of chunks) parser.feed(chunk);
    parser.flush();
    return { events, comments };
  }

  it('parses events split across chunks, multi-line data, ids and comments', () => {
    const { events, comments } = parse([': heart', 'beat\n\nid: 7\nda', 'ta: {"a":\ndata: 1}\n\n', 'event: custom\ndata: x\r\n\r\n']);
    expect(comments).toEqual(['heartbeat']);
    expect(events).toEqual([
      { event: 'message', data: '{"a":\n1}', id: '7' },
      { event: 'custom', data: 'x', id: undefined },
    ]);
  });

  it('handles CRLF split between chunks and drops an unterminated final event', () => {
    const { events } = parse(['data: one\r', '\n\r\n', 'data: partial']);
    expect(events).toEqual([{ event: 'message', data: 'one', id: undefined }]);
  });
});

describe('backoffDelay', () => {
  it('grows exponentially up to the cap, with bounded jitter', () => {
    const noJitter = { jitter: 0 };
    expect([0, 1, 2, 3, 10].map((n) => backoffDelay(n, noJitter))).toEqual([1000, 2000, 4000, 8000, 30000]);
    expect(backoffDelay(0, { jitter: 0.2 }, () => 0)).toBe(800);
    expect(backoffDelay(0, { jitter: 0.2 }, () => 1)).toBe(1200);
  });
});

describe('openStream', () => {
  beforeEach(() => {
    FakeEventSource.instances = [];
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const backoff = { initialMs: 1000, maxMs: 8000, factor: 2, jitter: 0 };

  it('delivers decoded events and reports state changes', () => {
    const events: unknown[] = [];
    const states: string[] = [];
    const handle = openStream<{ type: string }>({
      path: '/api/v1/stream/applications',
      query: { projects: ['a'] },
      eventSource: FakeEventSource,
      onEvent: (e) => events.push(e),
      onStateChange: (s) => states.push(s),
    });
    const es = FakeEventSource.latest();
    expect(new URL(es.url).pathname).toBe('/api/v1/stream/applications');
    expect(new URL(es.url).searchParams.get('projects')).toBe('a');
    es.open();
    es.message({ type: 'ADDED' }, '1');
    expect(events).toEqual([{ type: 'ADDED' }]);
    expect(handle.lastEventId).toBe('1');
    handle.close();
    expect(es.closed).toBe(true);
    expect(states).toEqual(['connecting', 'open', 'closed']);
  });

  it('reconnects with exponential backoff and resumes with Last-Event-ID', () => {
    const errors: boolean[] = [];
    openStream({
      path: '/api/v1/stream/applications',
      eventSource: FakeEventSource,
      backoff,
      onEvent: () => undefined,
      onError: (_e, willRetry) => errors.push(willRetry),
      resumeQuery: () => ({ resourceVersion: '42' }),
    });
    const first = FakeEventSource.latest();
    first.open();
    first.message({}, 'evt-9');
    first.fail();
    expect(first.closed).toBe(true);
    expect(errors).toEqual([true]);

    vi.advanceTimersByTime(999);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    const second = FakeEventSource.latest();
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(second.init?.headers?.['Last-Event-ID']).toBe('evt-9');
    expect(new URL(second.url).searchParams.get('resourceVersion')).toBe('42');

    // Failing again before opening doubles the delay.
    second.fail();
    vi.advanceTimersByTime(1999);
    expect(FakeEventSource.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(3);

    // A successful open resets the backoff.
    const third = FakeEventSource.latest();
    third.open();
    third.fail();
    vi.advanceTimersByTime(1000);
    expect(FakeEventSource.instances).toHaveLength(4);
  });

  it('reconnects when the heartbeat stops and stays connected while comments arrive', () => {
    const errors: string[] = [];
    openStream({
      path: '/api/v1/stream/applications',
      eventSource: FakeEventSource,
      backoff,
      heartbeatTimeoutMs: 45_000,
      onEvent: () => undefined,
      onError: (e) => errors.push(e.detail),
    });
    const es = FakeEventSource.latest();
    es.open();
    vi.advanceTimersByTime(40_000);
    es.comment();
    vi.advanceTimersByTime(40_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(5_000);
    expect(es.closed).toBe(true);
    expect(errors).toEqual(['Stream heartbeat timed out']);
    vi.advanceTimersByTime(1000);
    expect(FakeEventSource.instances).toHaveLength(2);
  });

  it('stops and triggers the login redirect on 401', () => {
    const unauthorized = vi.fn();
    const restore = setUnauthorizedHandler(unauthorized);
    const errors: boolean[] = [];
    const handle = openStream({
      path: '/api/v1/stream/applications',
      eventSource: FakeEventSource,
      backoff,
      onEvent: () => undefined,
      onError: (_e, willRetry) => errors.push(willRetry),
    });
    FakeEventSource.latest().fail(401);
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(errors).toEqual([false]);
    expect(unauthorized).toHaveBeenCalledTimes(1);
    expect(handle.state).toBe('closed');
    restore();
  });

  it('gives up after maxRetries consecutive failures', () => {
    const handle = openStream({ path: '/x', eventSource: FakeEventSource, backoff, maxRetries: 1, onEvent: () => undefined });
    FakeEventSource.latest().fail();
    vi.advanceTimersByTime(1000);
    FakeEventSource.latest().fail();
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(handle.state).toBe('closed');
  });

  it('reports willRetry=false on the failure that exhausts maxRetries', () => {
    const errors: [string, boolean][] = [];
    const handle = openStream({
      path: '/x',
      eventSource: FakeEventSource,
      backoff,
      maxRetries: 1,
      onEvent: () => undefined,
      onError: (e, willRetry) => errors.push([e.detail, willRetry]),
    });
    FakeEventSource.latest().fail(0);
    vi.advanceTimersByTime(1000);
    FakeEventSource.latest().fail(503);
    expect(errors).toEqual([
      ['Stream disconnected', true],
      ['Stream disconnected', false],
    ]);
    expect(handle.state).toBe('closed');
  });

  it('treats a normal end with no reconnect to follow as completion, not an error', () => {
    const errors: [string, boolean][] = [];
    const states: string[] = [];
    openStream({
      path: '/x',
      eventSource: FakeEventSource,
      maxRetries: 0,
      onEvent: () => undefined,
      onError: (e, willRetry) => errors.push([e.detail, willRetry]),
      onStateChange: (s) => states.push(s),
    });
    const es = FakeEventSource.latest();
    es.open();
    es.message({ line: 'done' });
    es.fail(); // response ended: no status, no error
    expect(errors).toEqual([]);
    expect(states).toEqual(['connecting', 'open', 'closed']);
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it('still reports a network failure of a single-shot stream, with willRetry=false', () => {
    const errors: [number, boolean][] = [];
    openStream({ path: '/x', eventSource: FakeEventSource, maxRetries: 0, onEvent: () => undefined, onError: (e, r) => errors.push([e.status, r]) });
    FakeEventSource.latest().fail(0);
    expect(errors).toEqual([[0, false]]);
  });

  it('reports an end as an error when the EventSource cannot tell an end from a failure', () => {
    class NativeLike extends FakeEventSource {
      static override readonly reportsEnd = false;
    }
    const errors: boolean[] = [];
    openStream({ path: '/x', eventSource: NativeLike, maxRetries: 0, onEvent: () => undefined, onError: (_e, r) => errors.push(r) });
    FakeEventSource.latest().fail();
    expect(errors).toEqual([false]);
  });

  it('reports willRetry=false when a heartbeat timeout ends a stream without retries', () => {
    const errors: boolean[] = [];
    openStream({ path: '/x', eventSource: FakeEventSource, maxRetries: 0, heartbeatTimeoutMs: 1000, onEvent: () => undefined, onError: (_e, r) => errors.push(r) });
    FakeEventSource.latest().open();
    vi.advanceTimersByTime(1000);
    expect(errors).toEqual([false]);
  });

  it('does not apply the heartbeat timeout to log streams (no heartbeats on those routes)', () => {
    const errors: string[] = [];
    const callbacks = { eventSource: FakeEventSource, onEvent: () => undefined, onError: (e: ApiError) => errors.push(e.detail) };
    const follow = streamApplicationLogs('guestbook', { follow: true, container: 'app' }, callbacks);
    const followSource = FakeEventSource.latest();
    followSource.open();
    const pod = streamPodLogs('guestbook', 'web-0', { follow: true }, callbacks);
    const podSource = FakeEventSource.latest();
    podSource.open();
    vi.advanceTimersByTime(10 * 60_000); // a quiet container for ten minutes
    expect(errors).toEqual([]);
    expect(followSource.closed).toBe(false);
    expect(podSource.closed).toBe(false);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(new URL(followSource.url).pathname).toBe('/api/v1/applications/guestbook/logs');
    expect(new URL(podSource.url).pathname).toBe('/api/v1/applications/guestbook/pods/web-0/logs');
    follow.close();
    pod.close();
  });

  it('handles a server error event (problem document): 401 stops and redirects, 429 reconnects', () => {
    const unauthorized = vi.fn();
    const restore = setUnauthorizedHandler(unauthorized);
    const errors: [string, number, boolean][] = [];
    const events: unknown[] = [];
    const handle = openStream({
      path: '/api/v1/stream/applications',
      eventSource: FakeEventSource,
      backoff,
      onEvent: (e) => events.push(e),
      onError: (e, willRetry) => errors.push([e.code, e.status, willRetry]),
    });
    const first = FakeEventSource.latest();
    first.open();
    // Slow consumer: the server ends the stream with a 429 problem; the client reconnects.
    first.named('error', { type: 'urn:patrol-cd:problem:resource_exhausted', title: 'Too Many Requests', status: 429, code: 'resource_exhausted', detail: 'reconnect to resume' });
    expect(first.closed).toBe(true);
    expect(errors).toEqual([['resource_exhausted', 429, true]]);
    expect(events).toEqual([]);
    vi.advanceTimersByTime(1000);
    const second = FakeEventSource.latest();
    expect(FakeEventSource.instances).toHaveLength(2);
    second.open();
    // Revoked session while streaming: stop and go to the login page.
    second.named('error', { title: 'Unauthorized', status: 401, code: 'unauthenticated', detail: 'session expired' });
    expect(errors[1]).toEqual(['unauthenticated', 401, false]);
    expect(unauthorized).toHaveBeenCalledTimes(1);
    expect(handle.state).toBe('closed');
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(2);
    restore();
  });

  it('ignores error events without a problem document (the native EventSource reports connection errors there)', () => {
    const errors: unknown[] = [];
    openStream({ path: '/x', eventSource: FakeEventSource, backoff, onEvent: () => undefined, onError: (e) => errors.push(e) });
    const es = FakeEventSource.latest();
    es.open();
    es.named('error', 'not a problem');
    expect(errors).toEqual([]);
    expect(es.closed).toBe(false);
  });

  it('ignores events from a source it already replaced and skips undecodable data', () => {
    const events: unknown[] = [];
    openStream({ path: '/x', eventSource: FakeEventSource, backoff, onEvent: (e) => events.push(e) });
    const first = FakeEventSource.latest();
    first.open();
    first.onmessage?.(new MessageEvent('message', { data: 'not json' }));
    first.fail();
    first.message({ late: true });
    expect(events).toEqual([]);
  });
});

describe('FetchEventSource', () => {
  it('dispatches a server `event: error` with its problem document to error listeners', async () => {
    server.use(
      http.get('*/api/v1/stream/ended', () =>
        new HttpResponse('data: {"type":"ADDED","object":{}}\n\nevent: error\ndata: {"title":"Forbidden","status":403,"code":"permission_denied","detail":"no longer allowed"}\n\n', {
          headers: { 'Content-Type': 'text/event-stream' },
        }),
      ),
    );
    const es = new FetchEventSource(new URL('/api/v1/stream/ended', document.baseURI).href);
    const named: string[] = [];
    es.addEventListener('error', (ev) => named.push(ev.data));
    await new Promise<void>((resolve) => {
      es.onerror = () => resolve();
    });
    expect(named).toHaveLength(1);
    expect(JSON.parse(named[0] ?? '{}')).toMatchObject({ status: 403, code: 'permission_denied' });
  });

  it('streams events over fetch and reports the HTTP status of a failed connection', async () => {
    server.use(
      http.get('*/api/v1/stream/ok', ({ request }) => {
        expect(request.headers.get('Last-Event-ID')).toBe('5');
        return new HttpResponse(': hi\n\nid: 6\ndata: {"n":1}\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
      }),
      http.get('*/api/v1/stream/denied', () =>
        HttpResponse.json({ title: 'no', status: 401, code: 'unauthenticated', detail: 'expired' }, { status: 401, headers: { 'Content-Type': 'application/problem+json' } }),
      ),
    );

    const ok = new FetchEventSource(new URL('/api/v1/stream/ok', document.baseURI).href, { headers: { 'Last-Event-ID': '5' } });
    const received: { data: string; id: string }[] = [];
    let comments = 0;
    ok.addEventListener('comment', () => (comments += 1));
    const ended = new Promise<void>((resolve) => {
      ok.onmessage = (ev) => received.push({ data: ev.data, id: ev.lastEventId });
      ok.onerror = () => resolve();
    });
    await ended;
    expect(received).toEqual([{ data: '{"n":1}', id: '6' }]);
    expect(comments).toBe(1);
    expect(ok.status).toBeUndefined();

    const denied = new FetchEventSource(new URL('/api/v1/stream/denied', document.baseURI).href);
    await new Promise<void>((resolve) => {
      denied.onerror = () => resolve();
    });
    expect(denied.status).toBe(401);
    expect(denied.error).toMatchObject({ code: 'unauthenticated', detail: 'expired' });
  });
});
