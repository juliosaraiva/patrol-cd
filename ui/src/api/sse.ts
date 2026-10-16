import { buildURL, notifyUnauthorized, type QueryParams } from './client';
import { ApiError, networkError, problemToApiError, toApiError } from './errors';

/*
 * Server-Sent Events for /api/v1/stream/* and log routes.
 *
 * openStream() owns reconnection: exponential backoff with jitter, resume from the last event id
 * (Last-Event-ID header plus an optional resume query such as resourceVersion), a heartbeat
 * timeout (the server sends a comment every 15 s), and 401 → login redirect.
 *
 * Wire format (patrol-server): one JSON document per `data:` field (watch streams send
 * `{type, object}` with the object's resourceVersion as the event id), `: heartbeat` comments,
 * and a final `event: error` whose data is a problem document when the server ends a stream
 * because of a failure (slow consumer → 429/resource_exhausted, revoked session → 401, lost
 * permission → 403). The error event is handled like a failed connection with that status.
 *
 * It works with any EventSource-shaped constructor. The default, FetchEventSource, implements
 * the EventSource interface on top of fetch() because the native EventSource cannot (a) send a
 * Last-Event-ID header on a reconnect we initiate, (b) expose the HTTP status of a failed
 * connection (needed to tell 401 from a network blip), or (c) observe comment heartbeats.
 * The native window.EventSource is still accepted (`eventSource: window.EventSource`); the
 * heartbeat timeout is then disabled because comments are invisible to it.
 */

export const CONNECTING = 0;
export const OPEN = 1;
export const CLOSED = 2;

export interface PatrolEventSourceInit {
  withCredentials?: boolean;
  headers?: Readonly<Record<string, string>>;
}

/** The subset of the EventSource interface openStream relies on. */
export interface EventSourceLike {
  readonly readyState: number;
  readonly url: string;
  onopen: ((ev: Event) => void) | null;
  onmessage: ((ev: MessageEvent<string>) => void) | null;
  onerror: ((ev: Event) => void) | null;
  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void): void;
  close(): void;
  /**
   * Patrol extension: HTTP status of a failed connection when known (0 = network error). When
   * the constructor declares `reportsEnd`, `undefined` after an error event means the server
   * ended the response normally.
   */
  readonly status?: number | undefined;
  /** Patrol extension: problem details of a failed connection when known. */
  readonly error?: ApiError | undefined;
}

export interface EventSourceConstructor {
  new (url: string, init?: PatrolEventSourceInit): EventSourceLike;
  /** True when the implementation dispatches a "comment" event for SSE comments. */
  readonly supportsComments?: boolean;
  /**
   * True when a normal end of the response is distinguishable from a failure (status and error
   * both undefined). The native EventSource cannot tell them apart.
   */
  readonly reportsEnd?: boolean;
}

export interface SSEFields {
  event: string;
  data: string;
  id: string | undefined;
}

/**
 * Incremental parser for the text/event-stream format (WHATWG HTML §9.2.6). Feed decoded text
 * chunks; complete events and comments are reported through the callbacks.
 */
export function createSSEParser(handlers: {
  onEvent: (fields: SSEFields) => void;
  onComment?: (text: string) => void;
  onRetry?: (ms: number) => void;
}): { feed: (chunk: string) => void; flush: () => void } {
  let buffer = '';
  let data: string[] = [];
  let eventType = '';
  let id: string | undefined;
  let sawField = false;

  const dispatch = () => {
    if (sawField && data.length > 0) {
      handlers.onEvent({ event: eventType || 'message', data: data.join('\n'), id });
    }
    data = [];
    eventType = '';
    id = undefined;
    sawField = false;
  };

  const processLine = (line: string) => {
    if (line === '') {
      dispatch();
      return;
    }
    if (line.startsWith(':')) {
      handlers.onComment?.(line.slice(1).trimStart());
      return;
    }
    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    switch (field) {
      case 'event':
        eventType = value;
        sawField = true;
        break;
      case 'data':
        data.push(value);
        sawField = true;
        break;
      case 'id':
        if (!value.includes('\0')) id = value;
        sawField = true;
        break;
      case 'retry':
        if (/^\d+$/.test(value)) handlers.onRetry?.(Number(value));
        break;
      default:
        break;
    }
  };

  return {
    feed(chunk) {
      buffer += chunk;
      for (;;) {
        const match = /\r\n|\r|\n/.exec(buffer);
        if (!match) break;
        // A trailing "\r" may be the first half of "\r\n"; wait for more input.
        if (match[0] === '\r' && match.index === buffer.length - 1) break;
        processLine(buffer.slice(0, match.index));
        buffer = buffer.slice(match.index + match[0].length);
      }
    },
    flush() {
      if (buffer !== '') processLine(buffer);
      buffer = '';
      // An unterminated final event is discarded, as the specification requires.
      data = [];
      sawField = false;
    },
  };
}

/** EventSource implemented over fetch(); never reconnects on its own (openStream does). */
export class FetchEventSource implements EventSourceLike {
  static readonly supportsComments = true;
  static readonly reportsEnd = true;
  readonly url: string;
  readyState = CONNECTING;
  status: number | undefined;
  error: ApiError | undefined;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent<string>) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  private readonly listeners = new Map<string, Set<(ev: MessageEvent<string>) => void>>();
  private readonly abort = new AbortController();

  constructor(url: string, init: PatrolEventSourceInit = {}) {
    this.url = url;
    void this.run(init);
  }

  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void): void {
    let set = this.listeners.get(type);
    if (!set) {
      set = new Set();
      this.listeners.set(type, set);
    }
    set.add(listener);
  }

  close(): void {
    this.readyState = CLOSED;
    this.abort.abort();
  }

  private emit(type: string, ev: MessageEvent<string>) {
    if (this.readyState === CLOSED) return;
    if (type === 'message') this.onmessage?.(ev);
    for (const listener of this.listeners.get(type) ?? []) listener(ev);
  }

  private fail(status: number | undefined, error: ApiError | undefined) {
    if (this.readyState === CLOSED) return;
    this.status = status;
    this.error = error;
    this.readyState = CLOSED;
    this.abort.abort();
    this.onerror?.(new Event('error'));
  }

  private async run(init: PatrolEventSourceInit): Promise<void> {
    let response: Response;
    try {
      response = await fetch(this.url, {
        headers: { Accept: 'text/event-stream', ...init.headers },
        credentials: init.withCredentials ? 'include' : 'same-origin',
        cache: 'no-store',
        signal: this.abort.signal,
      });
    } catch (err) {
      if (this.abort.signal.aborted) return;
      this.fail(0, networkError(err));
      return;
    }
    if (!response.ok || !response.body) {
      this.fail(response.status, await toApiError(response));
      return;
    }
    if (this.readyState === CLOSED) return;
    this.readyState = OPEN;
    this.onopen?.(new Event('open'));

    const parser = createSSEParser({
      onEvent: ({ event, data, id }) =>
        this.emit(event, new MessageEvent(event, { data, lastEventId: id ?? '' })),
      onComment: (text) => this.emit('comment', new MessageEvent('comment', { data: text })),
    });
    const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        parser.feed(value);
      }
      parser.flush();
      this.fail(undefined, undefined);
    } catch (err) {
      if (this.abort.signal.aborted) return;
      this.fail(0, networkError(err));
    }
  }
}

export type StreamState = 'connecting' | 'open' | 'reconnecting' | 'closed';

export interface BackoffOptions {
  initialMs?: number;
  maxMs?: number;
  factor?: number;
  /** Fraction of the delay randomized in both directions (0 disables jitter). */
  jitter?: number;
}

export interface StreamOptions<T> {
  /** Server path, e.g. "/api/v1/stream/applications". */
  path: string;
  query?: QueryParams;
  /** Called with each decoded event. */
  onEvent: (event: T, meta: { id: string | undefined; type: string }) => void;
  onStateChange?: (state: StreamState) => void;
  /**
   * Reports connection failures. `willRetry` is false when the stream stops because of it: fatal
   * statuses (401/403/404) or `maxRetries` exhausted. A normal end of the response that is not
   * followed by a reconnect (e.g. a non-follow log request with `maxRetries: 0`) is not an error:
   * the state just becomes "closed".
   */
  onError?: (error: ApiError, willRetry: boolean) => void;
  /** Decodes `data`; defaults to JSON.parse. Events that fail to decode are skipped. */
  parse?: (data: string) => T;
  /** Named event types to listen to besides "message". */
  eventTypes?: readonly string[];
  /** Extra query merged in on reconnects, e.g. ({ resourceVersion }) for watches. */
  resumeQuery?: () => QueryParams | undefined;
  /**
   * Close and reconnect when nothing (event or comment) arrives for this long. Default 45 s,
   * for /api/v1/stream/* routes (server heartbeat every 15 s); 0 disables it for routes without
   * heartbeats (logs).
   */
  heartbeatTimeoutMs?: number;
  backoff?: BackoffOptions;
  /** Give up after this many consecutive failed attempts (default: never). */
  maxRetries?: number;
  lastEventId?: string;
  eventSource?: EventSourceConstructor;
  random?: () => number;
}

export interface StreamHandle {
  close: () => void;
  readonly state: StreamState;
  readonly lastEventId: string | undefined;
}

const FATAL_STATUSES = new Set([401, 403, 404]);

export function backoffDelay(attempt: number, options: BackoffOptions = {}, random = Math.random): number {
  const initial = options.initialMs ?? 1000;
  const max = options.maxMs ?? 30_000;
  const factor = options.factor ?? 2;
  const jitter = options.jitter ?? 0.2;
  const base = Math.min(max, initial * factor ** attempt);
  const spread = base * jitter;
  return Math.max(0, Math.round(base - spread + random() * 2 * spread));
}

/** Opens an auto-reconnecting SSE stream. Call close() to stop it. */
export function openStream<T>(options: StreamOptions<T>): StreamHandle {
  const Ctor = options.eventSource ?? FetchEventSource;
  const heartbeatMs = Ctor.supportsComments ? (options.heartbeatTimeoutMs ?? 45_000) : 0;
  const parse = options.parse ?? ((data: string) => JSON.parse(data) as T);
  let source: EventSourceLike | null = null;
  let state: StreamState = 'connecting';
  let attempt = 0;
  let lastEventId = options.lastEventId;
  let retryTimer: ReturnType<typeof setTimeout> | undefined;
  let heartbeatTimer: ReturnType<typeof setTimeout> | undefined;
  let stopped = false;

  const setState = (next: StreamState) => {
    if (state === next) return;
    state = next;
    options.onStateChange?.(next);
  };

  const clearHeartbeat = () => {
    if (heartbeatTimer !== undefined) clearTimeout(heartbeatTimer);
    heartbeatTimer = undefined;
  };

  /** Whether scheduleReconnect() will reconnect (rather than give up) after this failure. */
  const willRetry = () => !stopped && (options.maxRetries === undefined || attempt < options.maxRetries);

  const armHeartbeat = () => {
    if (heartbeatMs <= 0) return;
    clearHeartbeat();
    heartbeatTimer = setTimeout(() => {
      const current = source;
      source = null;
      current?.close();
      options.onError?.(new ApiError({ status: 0, code: 'unavailable', detail: 'Stream heartbeat timed out' }), willRetry());
      scheduleReconnect();
    }, heartbeatMs);
  };

  const stop = () => {
    stopped = true;
    clearHeartbeat();
    if (retryTimer !== undefined) clearTimeout(retryTimer);
    retryTimer = undefined;
    source?.close();
    source = null;
    setState('closed');
  };

  const scheduleReconnect = () => {
    if (stopped) return;
    clearHeartbeat();
    if (options.maxRetries !== undefined && attempt >= options.maxRetries) {
      stop();
      return;
    }
    setState('reconnecting');
    const delay = backoffDelay(attempt, options.backoff, options.random);
    attempt += 1;
    retryTimer = setTimeout(connect, delay);
  };

  /** Handles a lost connection or a server error event of the current source `es`. */
  const fail = (es: EventSourceLike, status: number | undefined, error: ApiError | undefined) => {
    source = null;
    es.close();
    if (status !== undefined && FATAL_STATUSES.has(status)) {
      options.onError?.(error ?? new ApiError({ status, code: 'unavailable', detail: 'Stream disconnected' }), false);
      stop();
      if (status === 401) notifyUnauthorized();
      return;
    }
    const retry = willRetry();
    const endedNormally = Ctor.reportsEnd === true && status === undefined && error === undefined;
    // A normal end with no reconnect to follow is completion, not an error.
    if (!(endedNormally && !retry)) {
      options.onError?.(error ?? new ApiError({ status: status ?? 0, code: 'unavailable', detail: 'Stream disconnected' }), retry);
    }
    scheduleReconnect();
  };

  const handleData = (ev: MessageEvent<string>, type: string) => {
    armHeartbeat();
    if (ev.lastEventId) lastEventId = ev.lastEventId;
    let decoded: T;
    try {
      decoded = parse(ev.data);
    } catch {
      return;
    }
    options.onEvent(decoded, { id: ev.lastEventId || undefined, type });
  };

  function connect() {
    retryTimer = undefined;
    if (stopped) return;
    const resume = attempt > 0 || lastEventId !== undefined ? options.resumeQuery?.() : undefined;
    const url = buildURL(options.path, { ...options.query, ...resume }).href;
    const headers: Record<string, string> = {};
    if (lastEventId !== undefined) headers['Last-Event-ID'] = lastEventId;
    const es = new Ctor(url, { headers });
    source = es;
    es.onopen = () => {
      if (source !== es) return;
      attempt = 0;
      setState('open');
      armHeartbeat();
    };
    es.onmessage = (ev) => {
      if (source === es) handleData(ev, 'message');
    };
    for (const type of options.eventTypes ?? []) {
      es.addEventListener(type, (ev) => {
        if (source === es) handleData(ev, type);
      });
    }
    es.addEventListener('comment', () => {
      if (source === es) armHeartbeat();
    });
    // Server-sent `event: error` (a problem document). The native EventSource also dispatches
    // connection failures to "error" listeners, without data; those go through onerror.
    es.addEventListener('error', (ev) => {
      if (source !== es || typeof ev.data !== 'string') return;
      const problem = problemToApiError(ev.data);
      if (problem) fail(es, problem.status, problem);
    });
    es.onerror = () => {
      if (source !== es) return;
      fail(es, es.status, es.error);
    };
  }

  options.onStateChange?.(state);
  connect();

  return {
    close: stop,
    get state() {
      return state;
    },
    get lastEventId() {
      return lastEventId;
    },
  };
}
