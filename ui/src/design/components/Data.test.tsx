import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Code, CodeBlock, CopyButton, formatRelative, KeyValueList, Mono, Split, Timestamp } from './Data';

describe('Code and Mono', () => {
  it('render identifiers in dedicated elements', () => {
    render(
      <p>
        Run <Code>patrol app sync</Code> at <Mono>4f2c1a9</Mono>
      </p>,
    );
    expect(screen.getByText('patrol app sync').tagName).toBe('CODE');
    expect(screen.getByText('4f2c1a9').tagName).toBe('SPAN');
  });
});

describe('KeyValueList', () => {
  it('renders a description list and an em dash for empty values', () => {
    render(
      <KeyValueList
        items={[
          { key: 'project', label: 'Project', value: 'default' },
          { key: 'ns', label: 'Namespace', value: '' },
        ]}
      />,
    );
    expect(screen.getAllByRole('term').map((t) => t.textContent)).toEqual(['Project', 'Namespace']);
    expect(screen.getAllByRole('definition').map((d) => d.textContent)).toEqual(['default', '—']);
  });
});

describe('CopyButton', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('copies the value and announces the result', async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined);
    render(<CopyButton value="4f2c1a9d" label="Copy revision" />);
    await user.click(screen.getByRole('button', { name: 'Copy revision' }));
    expect(writeText).toHaveBeenCalledWith('4f2c1a9d');
    expect(await screen.findByText('Copied to clipboard')).toBeInTheDocument();
  });

  it('reports failure when the clipboard rejects', async () => {
    const user = userEvent.setup();
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new Error('denied'));
    render(<CopyButton value="x" label="Copy" />);
    await user.click(screen.getByRole('button', { name: 'Copy' }));
    expect(await screen.findByText('Copy failed: clipboard unavailable')).toBeInTheDocument();
  });
});

describe('CodeBlock', () => {
  it('is a focusable, named scroll region with a copy button', () => {
    render(<CodeBlock code={'kind: Deployment\n'} title="Live manifest" />);
    expect(screen.getByLabelText('Live manifest')).toHaveAttribute('tabindex', '0');
    expect(screen.getByRole('button', { name: 'Copy Live manifest' })).toBeInTheDocument();
  });
});

describe('Timestamp', () => {
  it('shows relative time with the absolute time available to all users', async () => {
    const now = Date.now();
    render(<Timestamp value={new Date(now - 3 * 60_000).toISOString()} />);
    const time = screen.getByText(/3 minutes ago/);
    expect(time.closest('time')).toHaveAttribute('datetime');
    await userEvent.tab();
    expect(screen.getByRole('tooltip')).toHaveTextContent(/Z\)$/);
  });

  it('formats past and future relative times', () => {
    const now = Date.UTC(2026, 9, 13, 12);
    expect(formatRelative(new Date(now - 5_000), now)).toBe('just now');
    expect(formatRelative(new Date(now - 2 * 3600_000), now)).toBe('2 hours ago');
    expect(formatRelative(new Date(now + 3 * 86400_000), now)).toBe('in 3 days');
  });

  it('renders a fallback for missing values', () => {
    render(<Timestamp value={undefined} />);
    expect(screen.getByText('—')).toBeInTheDocument();
  });
});

describe('Split', () => {
  it('exposes a keyboard-resizable separator', async () => {
    render(<Split label="Resize panels" start={<p>tree</p>} end={<p>details</p>} defaultSize={50} />);
    const separator = screen.getByRole('separator', { name: 'Resize panels' });
    expect(separator).toHaveAttribute('aria-valuenow', '50');
    separator.focus();
    await userEvent.keyboard('{ArrowRight}');
    expect(separator).toHaveAttribute('aria-valuenow', '55');
    await userEvent.keyboard('{Home}');
    expect(separator).toHaveAttribute('aria-valuenow', '20');
    await userEvent.keyboard('{End}');
    expect(separator).toHaveAttribute('aria-valuenow', '80');
  });
});
