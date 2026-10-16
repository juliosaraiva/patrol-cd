import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import { openTerminal } from '../../../../api/endpoints/applications';
import type { Application, TerminalMessage } from '../../../../api/types';
import { Banner, Button, FormField, Select, Switch } from '../../../../design';
import { usePrefersReducedMotion } from '../../../../design/hooks';
import styles from './Terminal.module.css';

export type TerminalState = 'connecting' | 'connected' | 'disconnected' | 'forbidden' | 'error';

export interface TerminalPanelProps {
  app: Application;
  namespace: string;
  pod: string;
  containers: readonly string[];
}

const SHELLS = [
  { value: '', label: 'Automatic (server default order)' },
  { value: 'bash', label: 'bash' },
  { value: 'sh', label: 'sh' },
  { value: 'powershell', label: 'powershell' },
  { value: 'cmd', label: 'cmd' },
];

/** WebSocket close codes the server uses when exec is not allowed (policy violation). */
const FORBIDDEN_CLOSE = new Set([1008, 4003]);

function cssVar(name: string, fallback: string): string {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return value || fallback;
}

const STATE_TEXT: Record<TerminalState, string> = {
  connecting: 'Connecting to the container…',
  connected: 'Connected.',
  disconnected: 'Disconnected. The shell exited or the connection dropped.',
  forbidden: 'Access denied: your account may not open a terminal in this application (exec, create).',
  error: 'The terminal connection failed.',
};

/**
 * Interactive shell in a pod container over the /terminal WebSocket (frames are
 * TerminalMessage JSON: stdin and resize out, stdout in). Tab and Escape go to the shell;
 * Shift+Escape moves focus out of the terminal.
 */
export function TerminalPanel({ app, namespace, pod, containers }: TerminalPanelProps) {
  const reducedMotion = usePrefersReducedMotion();
  const hostRef = useRef<HTMLDivElement>(null);
  const exitRef = useRef<HTMLButtonElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const [container, setContainer] = useState(containers[0] ?? '');
  const [shell, setShell] = useState('');
  const [state, setState] = useState<TerminalState>('connecting');
  const [attempt, setAttempt] = useState(0);
  const [screenReader, setScreenReader] = useState(false);
  const screenReaderRef = useRef(screenReader);
  screenReaderRef.current = screenReader;

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return undefined;
    setState('connecting');
    const term = new Terminal({
      convertEol: true,
      cursorBlink: !reducedMotion,
      fontFamily: cssVar('--font-mono', 'monospace'),
      fontSize: 13,
      screenReaderMode: screenReaderRef.current,
      theme: {
        background: cssVar('--color-surface-subtle', '#f4f5f7'),
        foreground: cssVar('--color-text', '#16191d'),
        cursor: cssVar('--color-accent', '#0b6b66'),
        selectionBackground: cssVar('--color-accent-subtle', '#e5f2f1'),
      },
    });
    termRef.current = term;
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    const safeFit = () => {
      try {
        fit.fit();
      } catch {
        // Not measurable yet (hidden or zero-sized host).
      }
    };
    safeFit();

    let socket: WebSocket | null = null;
    const send = (message: TerminalMessage) => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(message));
    };
    // Closes a socket we are done with. Its handlers are detached first: the close event fires
    // asynchronously and must not report a replaced (or unmounted) connection as disconnected.
    const retire = (ws: WebSocket | null) => {
      if (!ws) return;
      ws.onopen = null;
      ws.onmessage = null;
      ws.onerror = null;
      ws.onclose = null;
      ws.close();
    };
    const connect = () => {
      const ws = openTerminal({
        appName: app.metadata.name ?? '',
        appNamespace: app.metadata.namespace,
        projectName: app.spec.project,
        namespace,
        pod,
        container,
        shell: shell || undefined,
      });
      socket = ws;
      // Events of a socket that is no longer current are ignored (belt and braces with retire()).
      const current = () => socket === ws;
      ws.onopen = () => {
        if (!current()) return;
        setState('connected');
        // TerminalMessage requires `data`; the server ignores it on resize (UI-5).
        send({ operation: 'resize', data: '', cols: term.cols, rows: term.rows });
      };
      ws.onmessage = (event: MessageEvent) => {
        if (!current()) return;
        const raw = typeof event.data === 'string' ? event.data : '';
        let message: (Partial<TerminalMessage> & { Code?: unknown }) | null;
        try {
          message = JSON.parse(raw) as Partial<TerminalMessage> & { Code?: unknown } | null;
        } catch {
          term.write(raw);
          return;
        }
        if (message?.Code !== undefined && message.Code !== null) {
          // The server asks for a reconnect (e.g. after a session token refresh). The status stays
          // as it is (no "connecting" announcement) unless the new socket fails.
          retire(ws);
          connect();
          return;
        }
        if (message?.operation === 'stdout' && typeof message.data === 'string') term.write(message.data);
      };
      ws.onerror = () => {
        if (current()) setState('error');
      };
      ws.onclose = (event: CloseEvent) => {
        if (!current()) return;
        if (FORBIDDEN_CLOSE.has(event.code) || /forbidden|denied|permission/i.test(event.reason)) setState('forbidden');
        else setState((s) => (s === 'error' ? s : 'disconnected'));
      };
    };
    connect();

    const dataSub = term.onData((data) => send({ operation: 'stdin', data, rows: term.rows, cols: term.cols }));
    const resizeSub = term.onResize(({ cols, rows }) => send({ operation: 'resize', data: '', cols, rows }));
    const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(safeFit);
    observer?.observe(host);
    return () => {
      observer?.disconnect();
      dataSub.dispose();
      resizeSub.dispose();
      retire(socket);
      socket = null;
      term.dispose();
      termRef.current = null;
    };
    // screenReader is applied live below without reconnecting.
  }, [app.metadata.name, app.metadata.namespace, app.spec.project, namespace, pod, container, shell, attempt, reducedMotion]);

  useEffect(() => {
    if (termRef.current) termRef.current.options.screenReaderMode = screenReader;
  }, [screenReader]);

  // Shift+Escape leaves the terminal (captured before xterm sees it); plain Escape stays in the
  // shell and must not close the surrounding drawer.
  const onKeyDownCapture = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape' && event.shiftKey) {
      event.preventDefault();
      event.stopPropagation();
      exitRef.current?.focus();
    }
  };
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape') event.stopPropagation();
  };

  const live = state === 'connected' || state === 'connecting';
  return (
    <div className={styles.root}>
      <div className={styles.controls}>
        {containers.length > 0 ? (
          <FormField label="Container" className={styles.field}>
            <Select size="sm" value={container} onChange={(e) => setContainer(e.target.value)} options={containers.map((c) => ({ value: c, label: c }))} />
          </FormField>
        ) : null}
        <FormField label="Shell" className={styles.field}>
          <Select size="sm" value={shell} onChange={(e) => setShell(e.target.value)} options={SHELLS} />
        </FormField>
        <Switch checked={screenReader} onChange={setScreenReader} label="Screen reader mode" />
        <Button ref={exitRef} size="sm" onClick={() => setAttempt((a) => a + 1)}>
          {live ? 'Reconnect' : 'Connect again'}
        </Button>
      </div>
      <p className={styles.hint}>Tab and Escape are sent to the shell. Press Shift+Escape to move focus out of the terminal.</p>
      <p className={styles.status} role="status">
        {STATE_TEXT[state]}
      </p>
      {state === 'forbidden' ? (
        <Banner tone="error" title="Terminal not permitted">
          Opening a shell needs the <code>exec, create</code> permission on this application. Ask an administrator to grant it.
        </Banner>
      ) : null}
      {state === 'error' || state === 'disconnected' ? (
        <Banner tone={state === 'error' ? 'error' : 'warning'} title={state === 'error' ? 'Connection failed' : 'Disconnected'}>
          {state === 'error'
            ? 'Check that the pod is running and that the selected shell exists in the container, then connect again.'
            : 'Connect again to start a new shell session.'}
        </Banner>
      ) : null}
      {/* Host for xterm (its own textarea takes keyboard and screen reader input). The key handlers keep */}
      {/* Escape in the shell (the drawer must not close) and let Shift+Escape leave the terminal. */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
      <div
        ref={hostRef}
        className={styles.terminal}
        aria-label={`Terminal in ${pod}${container ? `, container ${container}` : ''}`}
        role="group"
        onKeyDownCapture={onKeyDownCapture}
        onKeyDown={onKeyDown}
      />
    </div>
  );
}
