import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Button } from './Button';
import { Menu } from './Menu';

function setup() {
  const onSync = vi.fn();
  const onRefresh = vi.fn();
  const onDelete = vi.fn();
  render(
    <Menu
      header="guestbook"
      trigger={(props) => <Button {...props}>Actions</Button>}
      items={[
        { id: 'sync', label: 'Sync', onSelect: onSync },
        { id: 'refresh', label: 'Refresh', onSelect: onRefresh },
        { id: 'rollback', label: 'Rollback', onSelect: vi.fn(), disabled: true },
        { type: 'separator', id: 'sep' },
        { id: 'delete', label: 'Delete', onSelect: onDelete, danger: true },
      ]}
    />,
  );
  return { onSync, onRefresh, onDelete, trigger: screen.getByRole('button', { name: 'Actions' }) };
}

describe('Menu', () => {
  it('opens from the keyboard on the first item and navigates with arrows, Home/End and typeahead', async () => {
    const { trigger, onRefresh } = setup();
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    trigger.focus();
    await userEvent.keyboard('{ArrowDown}');
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    const menu = screen.getByRole('menu', { name: 'Actions' });
    expect(trigger).toHaveAttribute('aria-controls', menu.id);
    expect(screen.getByRole('menuitem', { name: 'Sync' })).toHaveFocus();

    await userEvent.keyboard('{ArrowDown}');
    expect(screen.getByRole('menuitem', { name: 'Refresh' })).toHaveFocus();
    await userEvent.keyboard('{ArrowDown}'); // skips disabled Rollback
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveFocus();
    await userEvent.keyboard('{ArrowDown}'); // wraps
    expect(screen.getByRole('menuitem', { name: 'Sync' })).toHaveFocus();
    await userEvent.keyboard('{End}');
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveFocus();
    await userEvent.keyboard('{Home}');
    expect(screen.getByRole('menuitem', { name: 'Sync' })).toHaveFocus();
    await userEvent.keyboard('r');
    expect(screen.getByRole('menuitem', { name: 'Refresh' })).toHaveFocus();
    expect(screen.getByRole('separator')).toBeInTheDocument();

    await userEvent.keyboard('{Enter}');
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('opens on the last item with ArrowUp and closes with Escape, restoring focus', async () => {
    const { trigger } = setup();
    trigger.focus();
    await userEvent.keyboard('{ArrowUp}');
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveFocus();
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('closes on outside click', async () => {
    const { trigger } = setup();
    await userEvent.click(trigger);
    expect(screen.getByRole('menu')).toBeInTheDocument();
    await userEvent.click(document.body);
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });
});
