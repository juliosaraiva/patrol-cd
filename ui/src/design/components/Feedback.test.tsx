import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Button } from './Button';
import { Badge, Banner, EmptyState, Skeleton, ToastProvider, useToast } from './Feedback';
import { Tooltip } from './Tooltip';

describe('Banner', () => {
  it('states its tone in text, not only color', () => {
    render(
      <Banner tone="warning" title="Sync window closed">
        Manual sync is blocked until 18:00.
      </Banner>,
    );
    expect(screen.getByText('Warning:')).toHaveClass('sr-only');
    expect(screen.getByText('Sync window closed')).toBeInTheDocument();
  });

  it('uses role=alert for live errors and status for other live tones', () => {
    render(
      <>
        <Banner tone="error" live title="Sync failed" />
        <Banner tone="info" live title="Refreshing" />
      </>,
    );
    expect(screen.getByRole('alert')).toHaveTextContent('Sync failed');
    expect(screen.getByRole('status')).toHaveTextContent('Refreshing');
  });

  it('offers a labelled dismiss button', async () => {
    const onDismiss = vi.fn();
    render(<Banner title="Heads up" onDismiss={onDismiss} />);
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(onDismiss).toHaveBeenCalled();
  });
});

describe('Toast region', () => {
  function Trigger({ tone }: { tone?: 'info' | 'error' }) {
    const { toast } = useToast();
    return <Button onClick={() => toast({ title: tone === 'error' ? 'Sync failed' : 'Sync started', tone })}>Notify</Button>;
  }

  it('announces toasts politely and auto-dismisses non-error toasts', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: (ms) => vi.advanceTimersByTime(ms) });
    render(
      <ToastProvider>
        <Trigger />
      </ToastProvider>,
    );
    const region = screen.getByRole('region', { name: 'Notifications' });
    expect(region.querySelector('[aria-live="polite"]')).not.toBeNull();
    await user.click(screen.getByRole('button', { name: 'Notify' }));
    expect(region).toHaveTextContent('Sync started');
    act(() => {
      vi.advanceTimersByTime(6500);
    });
    expect(region).not.toHaveTextContent('Sync started');
  });

  it('keeps error toasts until dismissed', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: (ms) => vi.advanceTimersByTime(ms) });
    render(
      <ToastProvider>
        <Trigger tone="error" />
      </ToastProvider>,
    );
    await user.click(screen.getByRole('button', { name: 'Notify' }));
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    const region = screen.getByRole('region', { name: 'Notifications' });
    expect(region).toHaveTextContent('Sync failed');
    await user.click(screen.getByRole('button', { name: 'Dismiss notification' }));
    expect(region).not.toHaveTextContent('Sync failed');
  });
});

describe('EmptyState', () => {
  it('renders a heading, explanation and primary action', () => {
    render(
      <EmptyState title="No applications yet" action={<Button variant="primary">New application</Button>}>
        Create an application to start deploying.
      </EmptyState>,
    );
    expect(screen.getByRole('heading', { level: 2, name: 'No applications yet' })).toBeInTheDocument();
    expect(screen.getByText('Create an application to start deploying.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'New application' })).toBeInTheDocument();
  });
});

describe('Skeleton and Badge', () => {
  it('hides skeletons from assistive technology and renders badge text', () => {
    const { container } = render(
      <>
        <Skeleton width={120} />
        <Badge tone="warning">3 conditions</Badge>
      </>,
    );
    expect(container.querySelector('[aria-hidden="true"]')).not.toBeNull();
    expect(screen.getByText('3 conditions')).toBeInTheDocument();
  });
});

describe('Tooltip', () => {
  it('shows on focus, describes the trigger and hides on Escape', async () => {
    render(
      <Tooltip content="Last synced 3 minutes ago">
        <button type="button">Status</button>
      </Tooltip>,
    );
    const trigger = screen.getByRole('button', { name: 'Status' });
    await userEvent.tab();
    expect(trigger).toHaveFocus();
    const tip = screen.getByRole('tooltip');
    expect(tip).toHaveTextContent('Last synced 3 minutes ago');
    expect(trigger).toHaveAccessibleDescription('Last synced 3 minutes ago');
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('shows on hover after the delay', async () => {
    render(
      <Tooltip content="More" delayMs={0}>
        <button type="button">Hover me</button>
      </Tooltip>,
    );
    await userEvent.hover(screen.getByRole('button', { name: 'Hover me' }));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('More');
    await userEvent.unhover(screen.getByRole('button', { name: 'Hover me' }));
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();
  });
});
