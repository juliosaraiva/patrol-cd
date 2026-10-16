import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { IconPlus } from '../icons';
import { Button, IconButton } from './Button';
import { Spinner } from './Spinner';

describe('Button', () => {
  it('renders each variant and size as a native button', () => {
    render(
      <>
        <Button variant="primary">Primary</Button>
        <Button variant="secondary" size="sm">
          Secondary
        </Button>
        <Button variant="danger">Danger</Button>
        <Button variant="ghost">Ghost</Button>
      </>,
    );
    for (const name of ['Primary', 'Secondary', 'Danger', 'Ghost']) {
      const button = screen.getByRole('button', { name });
      expect(button).toHaveAttribute('type', 'button');
    }
    expect(screen.getByRole('button', { name: 'Primary' }).className).toMatch(/primary/);
    expect(screen.getByRole('button', { name: 'Secondary' }).className).toMatch(/sm/);
  });

  it('activates with Enter and Space', async () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Sync</Button>);
    await userEvent.tab();
    expect(screen.getByRole('button', { name: 'Sync' })).toHaveFocus();
    await userEvent.keyboard('{Enter}');
    await userEvent.keyboard(' ');
    expect(onClick).toHaveBeenCalledTimes(2);
  });

  it('marks loading buttons busy and ignores clicks', async () => {
    const onClick = vi.fn();
    render(
      <Button loading onClick={onClick}>
        Saving
      </Button>,
    );
    const button = screen.getByRole('button', { name: 'Saving' });
    expect(button).toHaveAttribute('aria-busy', 'true');
    await userEvent.click(button);
    expect(onClick).not.toHaveBeenCalled();
  });
});

describe('IconButton', () => {
  it('exposes its aria-label as the accessible name and hides the icon', () => {
    render(<IconButton aria-label="Add application" icon={<IconPlus />} />);
    const button = screen.getByRole('button', { name: 'Add application' });
    expect(button.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
  });
});

describe('Spinner', () => {
  it('announces a status label', () => {
    render(<Spinner label="Loading applications" />);
    expect(screen.getByRole('status')).toHaveTextContent('Loading applications');
  });
});
