import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { Button } from './Button';
import { Dialog, Drawer } from './Overlay';

function DialogHarness({ kind = 'dialog' }: { kind?: 'dialog' | 'drawer' }) {
  const [open, setOpen] = useState(false);
  const Overlay = kind === 'dialog' ? Dialog : Drawer;
  return (
    <>
      <Button onClick={() => setOpen(true)}>Delete application</Button>
      <Overlay
        open={open}
        onClose={() => setOpen(false)}
        title="Delete guestbook?"
        description="This removes the application and its resources."
        footer={
          <>
            <Button onClick={() => setOpen(false)}>Cancel</Button>
            <Button variant="danger">Delete</Button>
          </>
        }
      >
        <input aria-label="Type the name to confirm" />
      </Overlay>
    </>
  );
}

describe.each(['dialog', 'drawer'] as const)('%s', (kind) => {
  it('is a labelled modal that traps focus, closes on Esc and returns focus', async () => {
    render(<DialogHarness kind={kind} />);
    const opener = screen.getByRole('button', { name: 'Delete application' });
    await userEvent.click(opener);

    const dialog = screen.getByRole('dialog', { name: 'Delete guestbook?' });
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(dialog).toHaveAccessibleDescription('This removes the application and its resources.');

    const close = screen.getByRole('button', { name: 'Close' });
    expect(close).toHaveFocus();
    await userEvent.tab();
    expect(screen.getByRole('textbox', { name: 'Type the name to confirm' })).toHaveFocus();
    await userEvent.tab();
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await userEvent.tab();
    expect(screen.getByRole('button', { name: 'Delete' })).toHaveFocus();
    await userEvent.tab();
    expect(close).toHaveFocus();
    await userEvent.tab({ shift: true });
    expect(screen.getByRole('button', { name: 'Delete' })).toHaveFocus();

    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(opener).toHaveFocus();
  });
});

describe('scroll lock', () => {
  function Nested({ drawerOpen, dialogOpen }: { drawerOpen: boolean; dialogOpen: boolean }) {
    return (
      <Drawer open={drawerOpen} onClose={() => undefined} title="New application">
        <Dialog open={dialogOpen} onClose={() => undefined} title="Discard changes?">
          <Button>Discard</Button>
        </Dialog>
      </Drawer>
    );
  }

  it('restores the page overflow when nested overlays unmount together', () => {
    // The drawer opens first, then a confirm dialog inside it; "Discard" removes both at once.
    const { rerender, unmount } = render(<Nested drawerOpen dialogOpen={false} />);
    rerender(<Nested drawerOpen dialogOpen />);
    expect(screen.getByRole('dialog', { name: 'Discard changes?' })).toBeInTheDocument();
    expect(document.body.style.overflow).toBe('hidden');
    unmount();
    expect(document.body.style.overflow).toBe('');
  });

  it('restores the page overflow when the outer overlay closes first', () => {
    document.body.style.overflow = 'auto';
    function Siblings({ first, second }: { first: boolean; second: boolean }) {
      return (
        <>
          <Drawer open={first} onClose={() => undefined} title="Panel" />
          <Dialog open={second} onClose={() => undefined} title="Confirm" />
        </>
      );
    }
    const { rerender } = render(<Siblings first second={false} />);
    rerender(<Siblings first second />);
    expect(document.body.style.overflow).toBe('hidden');
    act(() => rerender(<Siblings first={false} second />));
    expect(document.body.style.overflow).toBe('hidden');
    act(() => rerender(<Siblings first={false} second={false} />));
    expect(document.body.style.overflow).toBe('auto');
    document.body.style.overflow = '';
  });
});
