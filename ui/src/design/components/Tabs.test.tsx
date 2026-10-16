import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { Tabs } from './Tabs';

const items = [
  { id: 'tree', label: 'Tree', content: <p>tree view</p> },
  { id: 'pods', label: 'Pods', content: <p>pods view</p> },
  { id: 'net', label: 'Network', content: <p>network view</p>, disabled: true },
  { id: 'list', label: 'List', content: <p>list view</p> },
];

describe('Tabs', () => {
  it('exposes tablist/tab/tabpanel relationships', () => {
    render(<Tabs label="Views" items={items} />);
    expect(screen.getByRole('tablist', { name: 'Views' })).toBeInTheDocument();
    const tree = screen.getByRole('tab', { name: 'Tree' });
    expect(tree).toHaveAttribute('aria-selected', 'true');
    const panel = screen.getByRole('tabpanel', { name: 'Tree' });
    expect(panel).toHaveTextContent('tree view');
    expect(tree).toHaveAttribute('aria-controls', panel.id);
  });

  it('uses a roving tabindex and arrow keys (skipping disabled tabs)', async () => {
    render(<Tabs label="Views" items={items} />);
    await userEvent.tab();
    expect(screen.getByRole('tab', { name: 'Tree' })).toHaveFocus();
    expect(screen.getByRole('tab', { name: 'Pods' })).toHaveAttribute('tabindex', '-1');

    await userEvent.keyboard('{ArrowRight}');
    expect(screen.getByRole('tab', { name: 'Pods' })).toHaveFocus();
    expect(screen.getByRole('tab', { name: 'Pods' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('tabpanel')).toHaveTextContent('pods view');

    await userEvent.keyboard('{ArrowRight}');
    expect(screen.getByRole('tab', { name: 'List' })).toHaveFocus();

    await userEvent.keyboard('{ArrowRight}');
    expect(screen.getByRole('tab', { name: 'Tree' })).toHaveFocus();

    await userEvent.keyboard('{ArrowLeft}');
    expect(screen.getByRole('tab', { name: 'List' })).toHaveFocus();

    await userEvent.keyboard('{Home}');
    expect(screen.getByRole('tab', { name: 'Tree' })).toHaveFocus();
    await userEvent.keyboard('{End}');
    expect(screen.getByRole('tab', { name: 'List' })).toHaveAttribute('aria-selected', 'true');

    // Tab moves from the tab list into the panel.
    await userEvent.tab();
    expect(screen.getByRole('tabpanel')).toHaveFocus();
  });
});
