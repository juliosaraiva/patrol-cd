import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resourcesAppFixture } from '../../../../mocks/fixtures/apps-resources';
import { renderAt } from '../test/helpers';
import { TerminalPanel } from './TerminalPanel';

interface FakeTerm {
  written: string[];
  dataHandler: ((data: string) => void) | null;
  options: Record<string, unknown>;
  disposed: boolean;
}

const terms: FakeTerm[] = [];

vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    written: string[] = [];
    dataHandler: ((data: string) => void) | null = null;
    options: Record<string, unknown>;
    disposed = false;
    constructor(options: Record<string, unknown>) {
      this.options = { ...options };
      terms.push(this);
    }
    loadAddon() {
      return undefined;
    }
    open(el: HTMLElement) {
      const textarea = document.createElement('textarea');
      textarea.setAttribute('aria-label', 'Terminal input');
      el.appendChild(textarea);
    }
    write(data: string) {
      this.written.push(data);
    }
    onData(handler: (data: string) => void) {
      this.dataHandler = handler;
      return { dispose: () => undefined };
    }
    onResize() {
      return { dispose: () => undefined };
    }
    dispose() {
      this.disposed = true;
    }
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit() {
      return undefined;
    }
  },
}));
vi.mock('@xterm/xterm/css/xterm.css', () => ({}));

/**
 * Minimal browser-like WebSocket: as in browsers, close() fires the close event asynchronously,
 * through whatever `onclose` handler is attached when the event is dispatched.
 */
class FakeSocket {
  static OPEN = 1;
  static instances: FakeSocket[] = [];
  readyState = 0;
  sent: unknown[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onclose: ((e: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(public url: string) {
    FakeSocket.instances.push(this);
  }
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close(code = 1000, reason = '') {
    if (this.readyState === 3) return;
    this.readyState = 2;
    setTimeout(() => {
      this.readyState = 3;
      this.onclose?.({ code, reason });
    }, 0);
  }
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
}

beforeEach(() => {
  FakeSocket.instances = [];
  terms.length = 0;
  vi.stubGlobal('WebSocket', FakeSocket);
});
afterEach(() => vi.unstubAllGlobals());

function lastSocket(): FakeSocket {
  const socket = FakeSocket.instances.at(-1);
  if (!socket) throw new Error('no socket');
  return socket;
}

describe('TerminalPanel', () => {
  it('connects to the terminal WebSocket, relays stdout and stdin and resizes', async () => {
    renderAt(<TerminalPanel app={resourcesAppFixture()} namespace="shop" pod="web-7d9f-a1" containers={['web', 'sidecar']} />);
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    const socket = lastSocket();
    const url = new URL(socket.url);
    expect(url.protocol).toBe('ws:');
    expect(url.pathname).toBe('/terminal');
    expect(Object.fromEntries(url.searchParams)).toEqual({
      appName: 'shop',
      appNamespace: 'patrol',
      projectName: 'default',
      namespace: 'shop',
      pod: 'web-7d9f-a1',
      container: 'web',
    });
    expect(screen.getByRole('status')).toHaveTextContent('Connecting to the container');
    act(() => socket.open());
    expect(screen.getByRole('status')).toHaveTextContent('Connected.');
    expect(socket.sent[0]).toEqual({ operation: 'resize', data: '', cols: 80, rows: 24 });
    act(() => socket.onmessage?.({ data: JSON.stringify({ operation: 'stdout', data: '$ ' }) }));
    expect(terms[0]?.written).toEqual(['$ ']);
    act(() => terms[0]?.dataHandler?.('ls\r'));
    expect(socket.sent[1]).toEqual({ operation: 'stdin', data: 'ls\r', rows: 24, cols: 80 });
  });

  it('reconnects with the chosen container and shell', async () => {
    const user = userEvent.setup();
    renderAt(<TerminalPanel app={resourcesAppFixture()} namespace="shop" pod="web-7d9f-a1" containers={['web', 'sidecar']} />);
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Container' }), 'sidecar');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Shell' }), 'sh');
    const url = new URL(lastSocket().url);
    expect(url.searchParams.get('container')).toBe('sidecar');
    expect(url.searchParams.get('shell')).toBe('sh');
    expect(terms[0]?.disposed).toBe(true);
  });

  it('reconnects when the server asks for it', async () => {
    renderAt(<TerminalPanel app={resourcesAppFixture()} namespace="shop" pod="web-7d9f-a1" containers={['web']} />);
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    const first = lastSocket();
    act(() => first.open());
    act(() => first.onmessage?.({ data: JSON.stringify({ Code: 1 }) }));
    expect(FakeSocket.instances).toHaveLength(2);
    expect(first.readyState).toBeGreaterThanOrEqual(2);
    const second = lastSocket();
    act(() => second.open());
    expect(screen.getByRole('status')).toHaveTextContent('Connected.');
    // The old socket's close event arrives after the new one opened; it must not end the session.
    await act(() => new Promise((resolve) => setTimeout(resolve, 10)));
    expect(first.readyState).toBe(3);
    expect(screen.getByRole('status')).toHaveTextContent('Connected.');
    expect(screen.queryByText('Disconnected')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reconnect' })).toBeInTheDocument();
    // Input goes to the new socket only.
    act(() => terms[0]?.dataHandler?.('id\r'));
    expect(second.sent.at(-1)).toEqual({ operation: 'stdin', data: 'id\r', rows: 24, cols: 80 });
    expect(first.sent).not.toContainEqual(expect.objectContaining({ data: 'id\r' }));
  });

  it('ignores a forbidden close of a replaced socket that arrives after the new one opened', async () => {
    renderAt(<TerminalPanel app={resourcesAppFixture()} namespace="shop" pod="web-7d9f-a1" containers={['web']} />);
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    const first = lastSocket();
    act(() => first.open());
    // A server that closes the old socket with a policy code while asking for a reconnect.
    const close = first.close.bind(first);
    first.close = () => close(1008, 'token expired, permission check');
    act(() => first.onmessage?.({ data: JSON.stringify({ Code: 1 }) }));
    act(() => lastSocket().open());
    await act(() => new Promise((resolve) => setTimeout(resolve, 10)));
    expect(screen.getByRole('status')).toHaveTextContent('Connected.');
    expect(screen.queryByText('Terminal not permitted')).not.toBeInTheDocument();
  });

  it('does not report the closed socket after unmounting', async () => {
    const errors = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const view = renderAt(<TerminalPanel app={resourcesAppFixture()} namespace="shop" pod="web-7d9f-a1" containers={['web']} />);
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    const socket = lastSocket();
    act(() => socket.open());
    view.unmount();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(socket.readyState).toBe(3);
    expect(socket.onclose).toBeNull();
    expect(errors).not.toHaveBeenCalled();
    errors.mockRestore();
  });

  it('shows the forbidden and disconnected states with recovery guidance', async () => {
    const user = userEvent.setup();
    renderAt(<TerminalPanel app={resourcesAppFixture()} namespace="shop" pod="web-7d9f-a1" containers={['web']} />);
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    act(() => lastSocket().onclose?.({ code: 1008, reason: 'permission denied' }));
    expect(screen.getByRole('status')).toHaveTextContent('Access denied');
    expect(screen.getByText('Terminal not permitted')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Connect again' }));
    expect(FakeSocket.instances).toHaveLength(2);
    act(() => lastSocket().open());
    act(() => lastSocket().onclose?.({ code: 1000, reason: '' }));
    expect(screen.getByText('Disconnected')).toBeInTheDocument();
  });

  it('keeps Escape in the shell and leaves with Shift+Escape', async () => {
    const user = userEvent.setup();
    renderAt(<TerminalPanel app={resourcesAppFixture()} namespace="shop" pod="web-7d9f-a1" containers={['web']} />);
    const input = await screen.findByRole('textbox', { name: 'Terminal input' });
    input.focus();
    await user.keyboard('{Shift>}{Escape}{/Shift}');
    expect(screen.getByRole('button', { name: 'Reconnect' })).toHaveFocus();
  });

  it('toggles screen reader mode without reconnecting', async () => {
    const user = userEvent.setup();
    renderAt(<TerminalPanel app={resourcesAppFixture()} namespace="shop" pod="web-7d9f-a1" containers={['web']} />);
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    await user.click(screen.getByRole('switch', { name: 'Screen reader mode' }));
    expect(terms[0]?.options.screenReaderMode).toBe(true);
    expect(FakeSocket.instances).toHaveLength(1);
  });
});
