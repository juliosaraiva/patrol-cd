import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Table, windowSlots, type Column } from './Table';

interface Row {
  name: string;
  revision: number;
}

const columns: Column<Row>[] = [
  { id: 'name', header: 'Name', cell: (r) => r.name, sortValue: (r) => r.name },
  { id: 'revision', header: 'Revision', cell: (r) => r.revision, sortValue: (r) => r.revision, align: 'end' },
  { id: 'actions', header: 'Actions', cell: (r) => <button type="button">Open {r.name}</button>, hideHeader: true },
];

const rows: Row[] = [
  { name: 'app-10', revision: 3 },
  { name: 'app-2', revision: 1 },
  { name: 'app-1', revision: 2 },
];

const bodyNames = () =>
  screen
    .getAllByRole('row')
    .slice(1)
    .map((r) => within(r).getAllByRole('cell')[0]?.textContent);

describe('Table', () => {
  it('names the table and sorts with aria-sort on the header', async () => {
    render(<Table caption="Applications" columns={columns} rows={rows} getRowKey={(r) => r.name} />);
    expect(screen.getByRole('table', { name: 'Applications' })).toBeInTheDocument();
    const nameHeader = screen.getByRole('columnheader', { name: /Name/ });
    expect(nameHeader).toHaveAttribute('aria-sort', 'none');
    expect(screen.getByRole('columnheader', { name: 'Actions' })).not.toHaveAttribute('aria-sort');

    await userEvent.click(within(nameHeader).getByRole('button'));
    expect(nameHeader).toHaveAttribute('aria-sort', 'ascending');
    expect(bodyNames()).toEqual(['app-1', 'app-2', 'app-10']); // natural order

    await userEvent.click(within(nameHeader).getByRole('button'));
    expect(nameHeader).toHaveAttribute('aria-sort', 'descending');
    expect(bodyNames()).toEqual(['app-10', 'app-2', 'app-1']);

    const revisionHeader = screen.getByRole('columnheader', { name: /Revision/ });
    await userEvent.click(within(revisionHeader).getByRole('button'));
    expect(revisionHeader).toHaveAttribute('aria-sort', 'ascending');
    expect(nameHeader).toHaveAttribute('aria-sort', 'none');
    expect(bodyNames()).toEqual(['app-2', 'app-1', 'app-10']);
  });

  it('selects rows with checkboxes, select-all and Space', async () => {
    const onSelectionChange = vi.fn();
    render(
      <Table
        caption="Applications"
        columns={columns}
        rows={rows}
        getRowKey={(r) => r.name}
        selectable
        onSelectionChange={onSelectionChange}
      />,
    );
    const selectAll = screen.getByRole('checkbox', { name: 'Select all rows' });
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select app-2' }));
    expect(onSelectionChange).toHaveBeenLastCalledWith(new Set(['app-2']));
    expect((selectAll as HTMLInputElement).indeterminate).toBe(true);

    await userEvent.click(selectAll);
    expect(onSelectionChange).toHaveBeenLastCalledWith(new Set(['app-10', 'app-2', 'app-1']));
    expect(screen.getByRole('checkbox', { name: 'Deselect all rows' })).toBeChecked();

    const firstRow = screen.getAllByRole('row')[1];
    firstRow?.focus();
    await userEvent.keyboard(' ');
    expect(onSelectionChange).toHaveBeenLastCalledWith(new Set(['app-2', 'app-1']));
  });

  it('moves between rows with the keyboard and activates with Enter', async () => {
    const onRowActivate = vi.fn();
    render(<Table caption="Applications" columns={columns} rows={rows} getRowKey={(r) => r.name} onRowActivate={onRowActivate} />);
    const bodyRows = screen.getAllByRole('row').slice(1);
    expect(bodyRows.map((r) => r.getAttribute('tabindex'))).toEqual(['0', '-1', '-1']);

    // Tab order: sortable header buttons, then the single focusable row.
    await userEvent.tab();
    expect(screen.getByRole('button', { name: 'Name' })).toHaveFocus();
    await userEvent.tab();
    await userEvent.tab();
    expect(bodyRows[0]).toHaveFocus();
    await userEvent.keyboard('{ArrowDown}');
    expect(bodyRows[1]).toHaveFocus();
    expect(bodyRows[1]).toHaveAttribute('tabindex', '0');
    await userEvent.keyboard('{End}');
    expect(bodyRows[2]).toHaveFocus();
    await userEvent.keyboard('{Home}');
    expect(bodyRows[0]).toHaveFocus();
    await userEvent.keyboard('{ArrowUp}');
    expect(bodyRows[0]).toHaveFocus();

    await userEvent.keyboard('{Enter}');
    expect(onRowActivate).toHaveBeenCalledWith(rows[0]);

    // Clicking an interactive cell does not activate the row.
    await userEvent.click(screen.getByRole('button', { name: 'Open app-2' }));
    expect(onRowActivate).toHaveBeenCalledTimes(1);
  });

  it('shows the empty state when there are no rows', () => {
    render(<Table caption="Applications" columns={columns} rows={[]} getRowKey={(r) => r.name} empty="No applications yet" />);
    expect(screen.getByRole('cell', { name: 'No applications yet' })).toBeInTheDocument();
  });

  it('renders only a window of rows for large data sets and keeps keyboard navigation working', async () => {
    const many: Row[] = Array.from({ length: 5000 }, (_, i) => ({ name: `app-${i}`, revision: i }));
    render(<Table caption="Many" columns={columns} rows={many} getRowKey={(r) => r.name} height={400} rowHeight={40} />);
    const table = screen.getByRole('table', { name: 'Many' });
    expect(table).toHaveAttribute('aria-rowcount', '5001');
    const rendered = screen.getAllByRole('row').length - 1;
    expect(rendered).toBeLessThan(40);
    expect(screen.getAllByRole('row')[1]).toHaveAttribute('aria-rowindex', '2');

    screen.getAllByRole('row')[1]?.focus();
    await userEvent.keyboard('{End}');
    const last = screen.getByRole('row', { name: /app-4999/ });
    expect(last).toHaveFocus();
    expect(last).toHaveAttribute('aria-rowindex', '5001');
    expect(screen.getAllByRole('row').length - 1).toBeLessThan(40);

    // Scrolling the viewport changes the window.
    const viewport = table.parentElement;
    if (!viewport) throw new Error('scroll viewport missing');
    act(() => {
      viewport.scrollTop = 40 * 2500;
      fireEvent.scroll(viewport);
    });
    expect(screen.getByRole('row', { name: /app-2500 / })).toBeInTheDocument();
  });

  it('keeps the focused row mounted and the single tab stop when the window scrolls away from it', async () => {
    const many: Row[] = Array.from({ length: 5000 }, (_, i) => ({ name: `app-${i}`, revision: i }));
    render(<Table caption="Many" columns={columns} rows={many} getRowKey={(r) => r.name} height={400} rowHeight={40} />);
    const table = screen.getByRole('table', { name: 'Many' });
    const viewport = table.parentElement;
    if (!viewport) throw new Error('scroll viewport missing');
    const tbody = table.querySelector('tbody');
    if (!tbody) throw new Error('tbody missing');
    const tabStops = () => tbody.querySelectorAll('tr[tabindex="0"]');
    const scrollTo = (row: number) =>
      act(() => {
        viewport.scrollTop = 40 * row;
        fireEvent.scroll(viewport);
      });

    const first = screen.getByRole('row', { name: /app-0 / });
    first.focus();
    await userEvent.keyboard('{ArrowDown}');
    const focused = screen.getByRole('row', { name: /app-1 / });
    expect(focused).toHaveFocus();

    // Far below: the focused row stays in the DOM (same node), focused, at its row index.
    scrollTo(3000);
    expect(screen.getByRole('row', { name: /app-3000 / })).toBeInTheDocument();
    expect(document.activeElement).not.toBe(document.body);
    expect(focused).toBe(document.activeElement);
    expect(focused.isConnected).toBe(true);
    expect(focused).toHaveAttribute('aria-rowindex', '3');
    expect(tabStops()).toHaveLength(1);
    expect(tabStops()[0]).toBe(focused);
    // Spacers still account for every row, so the scroll height is unchanged.
    const spacerHeight = Array.from(tbody.querySelectorAll<HTMLElement>('tr[aria-hidden="true"]')).reduce(
      (sum, tr) => sum + Number.parseFloat(tr.style.height),
      0,
    );
    const renderedRows = tbody.querySelectorAll('tr[data-row-index]').length;
    expect(spacerHeight + renderedRows * 40).toBe(5000 * 40);

    // Keyboard navigation continues from the focused row and brings it back into the window.
    await userEvent.keyboard('{ArrowDown}');
    expect(screen.getByRole('row', { name: /app-2 / })).toHaveFocus();
    expect(tabStops()).toHaveLength(1);

    // The same holds when the window is above the focused row.
    await userEvent.keyboard('{End}');
    const last = screen.getByRole('row', { name: /app-4999 / });
    expect(last).toHaveFocus();
    scrollTo(0);
    expect(screen.getByRole('row', { name: /app-0 / })).toBeInTheDocument();
    expect(last).toHaveFocus();
    expect(tabStops()).toHaveLength(1);
    scrollTo(4999);
    expect(last).toHaveFocus();

    // Blur, scroll away: the tab stop is still the active row.
    last.blur();
    scrollTo(1000);
    expect(tabStops()).toHaveLength(1);
    expect(tabStops()[0]).toHaveAttribute('data-row-index', '4999');
  });
});

describe('windowSlots', () => {
  const heights = (slots: ReturnType<typeof windowSlots>) =>
    slots.reduce((sum, s) => sum + (s.kind === 'spacer' ? s.height : 10), 0);
  const order = (slots: ReturnType<typeof windowSlots>) => slots.map((s) => (s.kind === 'row' ? s.index : s.id));

  it('pins the active row before or after the window and keeps the total height', () => {
    expect(order(windowSlots(100, 40, 45, 42, 10))).toEqual(['top', 40, 41, 42, 43, 44, 'bottom']);
    expect(order(windowSlots(100, 40, 43, 5, 10))).toEqual(['top', 5, 'gap-after-active', 40, 41, 42, 'bottom']);
    expect(order(windowSlots(100, 40, 43, 90, 10))).toEqual(['top', 40, 41, 42, 'gap-before-active', 90, 'bottom']);
    expect(order(windowSlots(100, 0, 3, 99, 10))).toEqual([0, 1, 2, 'gap-before-active', 99]);
    for (const active of [0, 5, 42, 90, 99]) expect(heights(windowSlots(100, 40, 43, active, 10))).toBe(1000);
    expect(windowSlots(0, 0, 0, 0, 10)).toEqual([]);
  });
});
