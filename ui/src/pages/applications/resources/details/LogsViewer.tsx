import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { streamApplicationLogs, streamPodLogs } from '../../../../api/endpoints/applications';
import { useLiveResource } from '../../../../api/hooks/apps-resources';
import { canonicalNamespace } from '../../../../api/queries/keys';
import { useSettings } from '../../../../api/queries';
import type { ApiError } from '../../../../api/errors';
import type { StreamState } from '../../../../api/sse';
import type { LogEntry } from '../../../../api/types';
import { Banner, Button, ButtonLink, FormField, Input, Select, Switch } from '../../../../design';
import { describeError } from '../../../../shell/errors';
import type { ResourceLogsViewerProps } from '../contract';
import { containerNames, parseJSONManifest } from '../model/manifest';
import { nonEmpty } from '../model/resources';
import { usePref } from '../model/prefs';
import styles from './Logs.module.css';

export interface LogLine {
  id: number;
  content: string;
  timeStamp: string | undefined;
  podName: string | undefined;
}

/** Most lines kept in memory; older lines are dropped (and counted). */
export const MAX_LINES = 50_000;
const LINE_HEIGHT = 18;
const OVERSCAN = 30;
const WRAP_PAGE = 2000;
const FLUSH_MS = 100;

const ANSI = new RegExp(`${String.fromCharCode(27)}\\[[0-9;?]*[A-Za-z]`, 'g');

export function stripAnsi(text: string): string {
  return text.replace(ANSI, '');
}

/** Client-side line filter: plain text matches a substring, a leading "!" inverts it. */
export function compileLogFilter(text: string, matchCase: boolean): (line: string) => boolean {
  const invert = text.startsWith('!');
  const needle = invert ? text.slice(1) : text;
  if (!needle) return () => true;
  const n = matchCase ? needle : needle.toLowerCase();
  return (line) => (matchCase ? line : line.toLowerCase()).includes(n) !== invert;
}

const SINCE_OPTIONS = [
  { value: '', label: 'All time' },
  { value: '60', label: 'Last minute' },
  { value: '300', label: 'Last 5 minutes' },
  { value: '900', label: 'Last 15 minutes' },
  { value: '3600', label: 'Last hour' },
  { value: '21600', label: 'Last 6 hours' },
  { value: '86400', label: 'Last 24 hours' },
];

const TAIL_OPTIONS = [
  { value: '100', label: 'Last 100 lines' },
  { value: '500', label: 'Last 500 lines' },
  { value: '1000', label: 'Last 1,000 lines' },
  { value: '5000', label: 'Last 5,000 lines' },
  { value: '', label: 'All lines' },
];

function statusText(state: StreamState, follow: boolean, error: ApiError | null, lines: number): string {
  if (error) return describeError(error).title;
  switch (state) {
    case 'connecting':
      return 'Connecting to the log stream…';
    case 'open':
      return follow ? 'Streaming new lines as they arrive.' : 'Loading lines…';
    case 'reconnecting':
      return 'Connection lost. Reconnecting; some lines may appear twice.';
    case 'closed':
      return `Stream ended. ${lines.toLocaleString()} ${lines === 1 ? 'line' : 'lines'} received.`;
  }
}

/** Streams the logs of a Pod, or of every pod of a workload, with filtering and download. */
export function ResourceLogsViewer({
  app,
  target,
  container: initialContainer,
  containers: givenContainers,
  initContainers: givenInit,
  fullscreen = false,
  fullscreenLink = false,
}: ResourceLogsViewerProps) {
  const appName = app.metadata.name ?? '';
  const scope = useMemo(() => ({ appNamespace: app.metadata.namespace, project: app.spec.project }), [app.metadata.namespace, app.spec.project]);
  const settings = useSettings();
  const isPod = target.kind === 'Pod';
  const live = useLiveResource(appName, { group: target.group, kind: target.kind, namespace: target.namespace, name: target.name }, scope, {
    enabled: givenContainers === undefined,
  });
  const names = useMemo(() => {
    if (givenContainers) return { containers: [...givenContainers], initContainers: [...(givenInit ?? [])] };
    return containerNames(parseJSONManifest(live.data?.manifest));
  }, [givenContainers, givenInit, live.data]);

  const [container, setContainer] = useState(initialContainer ?? '');
  const effectiveContainer = nonEmpty(container) ?? names.containers[0] ?? '';
  const [previous, setPrevious] = useState(false);
  const [since, setSince] = useState('');
  const [tail, setTail] = useState('1000');
  const [follow, setFollow] = usePref('logFollow');
  const [wrap, setWrap] = usePref('logWrap');
  const [timestamps, setTimestamps] = usePref('logTimestamps');
  const [podNames, setPodNames] = useState(!isPod);
  const [filter, setFilter] = useState('');
  const [matchCase, setMatchCase] = useState(false);
  const [buffer, setBuffer] = useState<{ lines: LogLine[]; dropped: number }>({ lines: [], dropped: 0 });
  const { lines, dropped } = buffer;
  const [state, setState] = useState<StreamState>('connecting');
  const [error, setError] = useState<ApiError | null>(null);
  const [restart, setRestart] = useState(0);
  const [copied, setCopied] = useState<string | null>(null);
  const ready = givenContainers !== undefined || !live.isPending;
  const outputId = useId();

  useEffect(() => {
    if (!ready || !appName) return undefined;
    setBuffer({ lines: [], dropped: 0 });
    setError(null);
    setState('connecting');
    let pending: LogLine[] = [];
    let nextId = 0;
    const flush = () => {
      if (pending.length === 0) return;
      const batch = pending;
      pending = [];
      setBuffer((prev) => {
        const merged = prev.lines.length + batch.length > MAX_LINES ? [...prev.lines, ...batch].slice(-MAX_LINES) : prev.lines.concat(batch);
        return { lines: merged, dropped: prev.dropped + prev.lines.length + batch.length - merged.length };
      });
    };
    const timer = setInterval(flush, FLUSH_MS);
    const callbacks = {
      onEvent: (entry: LogEntry) => {
        if (entry.last && !entry.content) return;
        pending.push({ id: nextId++, content: stripAnsi(entry.content ?? ''), timeStamp: entry.timeStamp ?? entry.timeStampStr, podName: entry.podName });
      },
      onStateChange: (s: StreamState) => {
        if (s === 'closed') flush();
        setState(s);
      },
      onError: (err: ApiError, willRetry: boolean) => {
        if (!willRetry) setError(err);
      },
    };
    const common = {
      container: effectiveContainer || undefined,
      sinceSeconds: since ? Number(since) : undefined,
      tailLines: tail ? Number(tail) : undefined,
      follow,
      previous,
      namespace: target.namespace || undefined,
      ...scope,
    };
    const handle = isPod
      ? streamPodLogs(appName, target.name, common, callbacks)
      : streamApplicationLogs(appName, { ...common, kind: target.kind, group: nonEmpty(target.group), resourceName: target.name }, callbacks);
    return () => {
      clearInterval(timer);
      handle.close();
    };
  }, [ready, appName, scope, isPod, target.name, target.kind, target.group, target.namespace, effectiveContainer, since, tail, follow, previous, restart]);

  const matches = useMemo(() => compileLogFilter(filter, matchCase), [filter, matchCase]);
  const visible = useMemo(() => (filter ? lines.filter((l) => matches(l.content)) : lines), [lines, filter, matches]);
  const formatLine = useCallback(
    (l: LogLine) => [timestamps && l.timeStamp ? l.timeStamp : null, podNames && l.podName ? l.podName : null, l.content].filter((p) => p !== null).join(' '),
    [timestamps, podNames],
  );

  // Output windowing and follow-scrolling.
  const scrollRef = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewport, setViewport] = useState(fullscreen ? 640 : 420);
  const [atBottom, setAtBottom] = useState(true);
  const [wrapLimit, setWrapLimit] = useState(WRAP_PAGE);
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    if (el.clientHeight > 0 && el.clientHeight !== viewport) setViewport(el.clientHeight);
    if (follow && atBottom) {
      el.scrollTop = el.scrollHeight;
      setScrollTop(el.scrollTop);
    }
  }, [visible, follow, atBottom, viewport]);

  const total = visible.length;
  let rendered: LogLine[];
  let topPad = 0;
  let bottomPad = 0;
  let hiddenEarlier = 0;
  if (wrap) {
    hiddenEarlier = Math.max(0, total - wrapLimit);
    rendered = visible.slice(hiddenEarlier);
  } else {
    const start = Math.max(0, Math.floor(scrollTop / LINE_HEIGHT) - OVERSCAN);
    const end = Math.min(total, Math.ceil((scrollTop + viewport) / LINE_HEIGHT) + OVERSCAN);
    rendered = visible.slice(start, end);
    topPad = start * LINE_HEIGHT;
    bottomPad = (total - end) * LINE_HEIGHT;
  }

  const download = () => {
    const text = visible.map(formatLine).join('\n');
    const fileName = `${target.name}${effectiveContainer ? `-${effectiveContainer}` : ''}.log`;
    if (typeof URL.createObjectURL !== 'function') return;
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = fileName;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(visible.map(formatLine).join('\n'));
      setCopied(`Copied ${visible.length.toLocaleString()} lines to the clipboard`);
    } catch {
      setCopied('Copying failed: the browser blocked clipboard access. Use Download instead.');
    }
  };

  const ns = canonicalNamespace(app.metadata.namespace, settings.data?.controllerNamespace);
  const fullscreenPath = `/applications/${ns ? `${encodeURIComponent(ns)}/` : ''}${encodeURIComponent(appName)}/${encodeURIComponent(target.namespace || '-')}/${encodeURIComponent(effectiveContainer || '-')}/logs?${new URLSearchParams(
    isPod ? { podName: target.name } : { kind: target.kind, group: target.group ?? '', resourceName: target.name },
  ).toString()}`;

  const containerOptions = [
    ...names.containers.map((c) => ({ value: c, label: c })),
    ...names.initContainers.map((c) => ({ value: c, label: `${c} (init container)` })),
  ];
  const label = `${target.kind} ${target.name}`;

  return (
    <div className={[styles.root, fullscreen ? styles.fullscreen : undefined].filter(Boolean).join(' ')}>
      <div className={styles.controls} role="group" aria-label="Log options">
        {containerOptions.length > 0 ? (
          <FormField label="Container" className={styles.field}>
            <Select size="sm" options={containerOptions} value={effectiveContainer} onChange={(e) => setContainer(e.target.value)} />
          </FormField>
        ) : null}
        <FormField label="Since" className={styles.field}>
          <Select size="sm" options={SINCE_OPTIONS} value={since} onChange={(e) => setSince(e.target.value)} />
        </FormField>
        <FormField label="Lines" className={styles.field}>
          <Select size="sm" options={TAIL_OPTIONS} value={tail} onChange={(e) => setTail(e.target.value)} />
        </FormField>
        <FormField label="Filter" help="Prefix with ! to hide matching lines" className={styles.filterField}>
          <Input size="sm" value={filter} onChange={(e) => setFilter(e.target.value)} spellCheck={false} />
        </FormField>
      </div>
      <div className={styles.toggles}>
        <Switch checked={follow} onChange={setFollow} label="Follow" />
        <Switch checked={previous} onChange={setPrevious} label="Previous container" />
        <Switch checked={wrap} onChange={setWrap} label="Wrap lines" />
        <Switch checked={timestamps} onChange={setTimestamps} label="Timestamps" />
        {!isPod ? <Switch checked={podNames} onChange={setPodNames} label="Pod names" /> : null}
        <Switch checked={matchCase} onChange={setMatchCase} label="Match case" />
      </div>
      <div className={styles.actions}>
        <Button size="sm" onClick={download} disabled={visible.length === 0}>
          Download
        </Button>
        <Button size="sm" onClick={() => void copy()} disabled={visible.length === 0}>
          Copy
        </Button>
        <Button size="sm" onClick={() => setRestart((r) => r + 1)}>
          {state === 'closed' ? 'Reload' : 'Restart stream'}
        </Button>
        {fullscreenLink && !fullscreen ? (
          <ButtonLink size="sm" variant="ghost" to={fullscreenPath}>
            Open full screen
          </ButtonLink>
        ) : null}
      </div>
      <p className={styles.status} role="status">
        {statusText(state, follow, error, lines.length)}
        {filter ? ` Showing ${visible.length.toLocaleString()} of ${lines.length.toLocaleString()} lines.` : ''}
        {copied ? ` ${copied}.` : ''}
      </p>
      {error ? (
        <Banner tone="error" live title={describeError(error).title} actions={<Button size="sm" onClick={() => setRestart((r) => r + 1)}>Retry</Button>}>
          {describeError(error).message}
        </Banner>
      ) : null}
      {dropped > 0 ? (
        <p className={styles.note}>
          {dropped.toLocaleString()} older lines were dropped to keep the page responsive. Download keeps only the lines shown.
        </p>
      ) : null}
      {/* The output scrolls; it is focusable so keyboard users can scroll it. It is not a live region (too chatty). */}
      <div
        ref={scrollRef}
        id={outputId}
        role="log"
        aria-live="off"
        aria-label={`Logs of ${label}`}
        // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- scrollable output must be keyboard scrollable
        tabIndex={0}
        className={[styles.output, wrap ? styles.wrap : undefined].filter(Boolean).join(' ')}
        onScroll={(e) => {
          const el = e.currentTarget;
          setScrollTop(el.scrollTop);
          setAtBottom(el.scrollTop + el.clientHeight >= el.scrollHeight - LINE_HEIGHT);
        }}
      >
        {wrap && hiddenEarlier > 0 ? (
          <button type="button" className={styles.earlier} onClick={() => setWrapLimit((l) => l + WRAP_PAGE)}>
            Show {Math.min(WRAP_PAGE, hiddenEarlier).toLocaleString()} earlier lines
          </button>
        ) : null}
        {topPad > 0 ? <div aria-hidden="true" style={{ height: topPad }} /> : null}
        {rendered.map((line) => (
          <div key={line.id} className={styles.line}>
            {timestamps && line.timeStamp ? <span className={styles.timestamp}>{line.timeStamp} </span> : null}
            {podNames && line.podName ? <span className={styles.pod}>{line.podName} </span> : null}
            {line.content}
          </div>
        ))}
        {bottomPad > 0 ? <div aria-hidden="true" style={{ height: bottomPad }} /> : null}
        {total === 0 && state !== 'connecting' && !error ? (
          <p className={styles.empty}>{filter ? 'No lines match the filter.' : 'No log lines yet.'}</p>
        ) : null}
      </div>
      {follow && !atBottom ? (
        <Button
          size="sm"
          onClick={() => {
            const el = scrollRef.current;
            if (el) el.scrollTop = el.scrollHeight;
            setAtBottom(true);
          }}
        >
          Jump to latest
        </Button>
      ) : null}
    </div>
  );
}
