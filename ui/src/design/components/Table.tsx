import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
} from 'react';
import { useControllableState } from '../hooks';
import { IconSortAscending, IconSortDescending, IconSortNone } from '../icons';
import styles from './Table.module.css';

export interface Column<T> {
  id: string;
  /** Header text; also used in accessible names. */
  header: string;
  cell: (row: T) => ReactNode;
  /** Makes the column sortable; return the comparable value for a row. */
  sortValue?: (row: T) => SortValue;
  width?: string;
  align?: 'start' | 'end';
  /** Visually hide the header text (it stays available to assistive technology). */
  hideHeader?: boolean;
}

export interface SortState {
  columnId: string;
  direction: 'ascending' | 'descending';
}

export interface TableProps<T> {
  /** Accessible name of the table (rendered as <caption>, visually hidden unless showCaption). */
  caption: string;
  showCaption?: boolean;
  columns: readonly Column<T>[];
  rows: readonly T[];
  getRowKey: (row: T) => string;
  /** Name used in "Select <name>" checkbox labels (defaults to the row key). */
  getRowLabel?: (row: T) => string;
  sort?: SortState | null;
  defaultSort?: SortState | null;
  onSortChange?: (sort: SortState | null) => void;
  /** Rows are already sorted (e.g. server-side); headers still reflect `sort`. */
  manualSort?: boolean;
  selectable?: boolean;
  selectedKeys?: ReadonlySet<string>;
  onSelectionChange?: (keys: Set<string>) => void;
  /** Enter on a focused row, or a click outside interactive cells. */
  onRowActivate?: (row: T) => void;
  rowHeight?: number;
  /** Scroll viewport height in px. Required for a sticky header; defaulted when virtualized. */
  height?: number;
  /** Row count above which only visible rows are rendered (default 200). */
  virtualizeThreshold?: number;
  empty?: ReactNode;
  busy?: boolean;
  className?: string;
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

export type SortValue = string | number | boolean | null | undefined;

export function compareValues(a: SortValue, b: SortValue): number {
  const aMissing = a === null || a === undefined || a === '';
  const bMissing = b === null || b === undefined || b === '';
  if (aMissing || bMissing) return aMissing === bMissing ? 0 : aMissing ? 1 : -1;
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  if (typeof a === 'boolean' && typeof b === 'boolean') return Number(a) - Number(b);
  return collator.compare(String(a), String(b));
}

const OVERSCAN = 8;

type BodySlot = { kind: 'row'; index: number } | { kind: 'spacer'; id: string; height: number };

/**
 * Rows to render for the window [start, end) plus spacers that keep the scroll height. The
 * active row (the body's single tab stop, possibly focused) is always rendered, even when the
 * window has scrolled away from it, so focus and the Tab entry point survive scrolling.
 */
export function windowSlots(count: number, start: number, end: number, activeIndex: number, rowHeight: number): BodySlot[] {
  const slots: BodySlot[] = [];
  const spacer = (id: string, rows: number) => {
    if (rows > 0) slots.push({ kind: 'spacer', id, height: rows * rowHeight });
  };
  const rowsFrom = (from: number, to: number) => {
    for (let i = from; i < to; i += 1) slots.push({ kind: 'row', index: i });
  };
  if (count === 0) return slots;
  // Spacer ids are chosen so that the active row never changes order relative to the keyed
  // siblings it already had: React then only inserts around it and never moves its DOM node
  // (moving a focused node blurs it). "top" always precedes and "bottom" always follows it.
  if (activeIndex < start) {
    spacer('top', activeIndex);
    rowsFrom(activeIndex, activeIndex + 1);
    spacer('gap-after-active', start - activeIndex - 1);
    rowsFrom(start, end);
    spacer('bottom', count - end);
  } else if (activeIndex >= end) {
    spacer('top', start);
    rowsFrom(start, end);
    spacer('gap-before-active', activeIndex - end);
    rowsFrom(activeIndex, activeIndex + 1);
    spacer('bottom', count - activeIndex - 1);
  } else {
    spacer('top', start);
    rowsFrom(start, end);
    spacer('bottom', count - end);
  }
  return slots;
}

const INTERACTIVE = 'a, button, input, select, textarea, label, [role="button"], [role="link"], [role="menuitem"]';

/**
 * Data table: sortable headers (aria-sort), optional row selection, keyboard row navigation
 * (one row in the Tab order; ArrowUp/Down, Home/End, PageUp/PageDown move, Enter activates,
 * Space toggles selection) and windowed rendering for large row counts (fixed row height).
 */
export function Table<T>({
  caption,
  showCaption = false,
  columns,
  rows,
  getRowKey,
  getRowLabel,
  sort: sortProp,
  defaultSort = null,
  onSortChange,
  manualSort = false,
  selectable = false,
  selectedKeys,
  onSelectionChange,
  onRowActivate,
  rowHeight = 36,
  height,
  virtualizeThreshold = 200,
  empty,
  busy = false,
  className,
}: TableProps<T>) {
  const [sort, setSort] = useControllableState<SortState | null>(sortProp, defaultSort, onSortChange);
  const [internalSelection, setInternalSelection] = useState<ReadonlySet<string>>(() => new Set());
  const selection = selectedKeys ?? internalSelection;
  const setSelection = (next: Set<string>) => {
    if (!selectedKeys) setInternalSelection(next);
    onSelectionChange?.(next);
  };

  const sortedRows = useMemo(() => {
    if (manualSort || !sort) return rows;
    const column = columns.find((c) => c.id === sort.columnId);
    const value = column?.sortValue;
    if (!value) return rows;
    const factor = sort.direction === 'ascending' ? 1 : -1;
    return [...rows].sort((a, b) => factor * compareValues(value(a), value(b)));
  }, [rows, columns, sort, manualSort]);

  const virtual = sortedRows.length > virtualizeThreshold;
  const viewportHeight = height ?? (virtual ? 480 : undefined);
  const containerRef = useRef<HTMLDivElement>(null);
  const tbodyRef = useRef<HTMLTableSectionElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [focusIndex, setFocusIndex] = useState(0);
  const pendingFocus = useRef<number | null>(null);

  const count = sortedRows.length;
  const activeIndex = Math.min(focusIndex, Math.max(0, count - 1));
  const visibleRows = viewportHeight ? Math.max(1, Math.floor(viewportHeight / rowHeight) - 1) : 10;
  const end = virtual ? Math.min(count, Math.ceil((scrollTop + (viewportHeight ?? 0)) / rowHeight) + OVERSCAN) : count;
  // Clamped to `end`: scrollTop can briefly exceed the content height after rows are removed.
  const start = virtual ? Math.min(end, Math.max(0, Math.floor(scrollTop / rowHeight) - OVERSCAN)) : 0;
  const bodySlots = useMemo(() => windowSlots(count, start, end, activeIndex, rowHeight), [count, start, end, activeIndex, rowHeight]);

  useEffect(() => {
    if (pendingFocus.current === null) return;
    const el = tbodyRef.current?.querySelector<HTMLElement>(`[data-row-index="${pendingFocus.current}"]`);
    if (el) {
      pendingFocus.current = null;
      el.focus({ preventScroll: virtual });
    }
  });

  const focusRow = (index: number) => {
    if (count === 0) return;
    const target = Math.min(count - 1, Math.max(0, index));
    setFocusIndex(target);
    pendingFocus.current = target;
    const container = containerRef.current;
    if (virtual && container && viewportHeight) {
      const top = target * rowHeight;
      const visibleBody = viewportHeight - rowHeight; // sticky header row
      let next = container.scrollTop;
      if (top < next) next = top;
      else if (top + rowHeight > next + visibleBody) next = top + rowHeight - visibleBody;
      if (next !== container.scrollTop) {
        container.scrollTop = next;
        setScrollTop(next);
      }
    }
  };

  const toggleRow = (key: string) => {
    const next = new Set(selection);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    setSelection(next);
  };

  const onRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, index: number, row: T) => {
    const fromRow = event.target === event.currentTarget;
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault();
        focusRow(index + 1);
        break;
      case 'ArrowUp':
        event.preventDefault();
        focusRow(index - 1);
        break;
      case 'Home':
        if (!fromRow) return;
        event.preventDefault();
        focusRow(0);
        break;
      case 'End':
        if (!fromRow) return;
        event.preventDefault();
        focusRow(count - 1);
        break;
      case 'PageDown':
        event.preventDefault();
        focusRow(index + visibleRows);
        break;
      case 'PageUp':
        event.preventDefault();
        focusRow(index - visibleRows);
        break;
      case 'Enter':
        if (fromRow && onRowActivate) {
          event.preventDefault();
          onRowActivate(row);
        }
        break;
      case ' ':
        if (fromRow && selectable) {
          event.preventDefault();
          toggleRow(getRowKey(row));
        }
        break;
      default:
        break;
    }
  };

  const onRowClick = (event: MouseEvent<HTMLTableRowElement>, index: number, row: T) => {
    setFocusIndex(index);
    if (!onRowActivate) return;
    const target = event.target as HTMLElement;
    if (target.closest(INTERACTIVE)) return;
    onRowActivate(row);
  };

  const toggleSort = (column: Column<T>) => {
    if (!column.sortValue) return;
    if (sort?.columnId === column.id) {
      setSort({ columnId: column.id, direction: sort.direction === 'ascending' ? 'descending' : 'ascending' });
    } else {
      setSort({ columnId: column.id, direction: 'ascending' });
    }
  };

  const allKeys = sortedRows.map(getRowKey);
  const selectedCount = allKeys.filter((k) => selection.has(k)).length;
  const allSelected = count > 0 && selectedCount === count;
  const someSelected = selectedCount > 0 && !allSelected;
  const headerCheckbox = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (headerCheckbox.current) headerCheckbox.current.indeterminate = someSelected;
  }, [someSelected]);

  const colCount = columns.length + (selectable ? 1 : 0);
  const style = { '--row-height': `${rowHeight}px` } as CSSProperties;
  const scrollStyle: CSSProperties | undefined = viewportHeight ? { maxHeight: viewportHeight } : undefined;

  const table = (
    <table
      className={styles.table}
      style={style}
      aria-rowcount={virtual ? count + 1 : undefined}
      aria-busy={busy || undefined}
    >
      <caption className={showCaption ? styles.caption : 'sr-only'}>{caption}</caption>
      <colgroup>
        {selectable ? <col style={{ width: 40 }} /> : null}
        {columns.map((c) => (
          <col key={c.id} style={c.width ? { width: c.width } : undefined} />
        ))}
      </colgroup>
      <thead>
        <tr aria-rowindex={virtual ? 1 : undefined}>
          {selectable ? (
            <th scope="col" className={[styles.th, styles.selectCell].join(' ')}>
              <input
                ref={headerCheckbox}
                type="checkbox"
                aria-label={allSelected ? 'Deselect all rows' : 'Select all rows'}
                checked={allSelected}
                disabled={count === 0}
                onChange={() => setSelection(allSelected ? new Set() : new Set(allKeys))}
              />
            </th>
          ) : null}
          {columns.map((column) => {
            const active = sort?.columnId === column.id;
            const ariaSort = column.sortValue ? (active ? sort.direction : 'none') : undefined;
            return (
              <th
                key={column.id}
                scope="col"
                aria-sort={ariaSort}
                className={[styles.th, column.align === 'end' ? styles.end : undefined].filter(Boolean).join(' ')}
              >
                {column.sortValue ? (
                  <button type="button" className={styles.sortButton} onClick={() => toggleSort(column)}>
                    <span className={column.hideHeader ? 'sr-only' : undefined}>{column.header}</span>
                    <span className={styles.sortIcon} aria-hidden="true">
                      {active ? sort.direction === 'ascending' ? <IconSortAscending size={12} /> : <IconSortDescending size={12} /> : <IconSortNone size={12} />}
                    </span>
                  </button>
                ) : (
                  <span className={column.hideHeader ? 'sr-only' : undefined}>{column.header}</span>
                )}
              </th>
            );
          })}
        </tr>
      </thead>
      <tbody ref={tbodyRef}>
        {count === 0 ? (
          <tr>
            <td className={[styles.td, styles.emptyCell].join(' ')} colSpan={colCount}>
              {empty ?? 'No items'}
            </td>
          </tr>
        ) : null}
        {bodySlots.map((slot) => {
          if (slot.kind === 'spacer') {
            return (
              <tr key={`s:${slot.id}`} className={styles.spacer} aria-hidden="true" style={{ height: slot.height }}>
                <td colSpan={colCount} />
              </tr>
            );
          }
          const { index } = slot;
          const row = sortedRows[index];
          if (row === undefined) return null;
          const key = getRowKey(row);
          const isSelected = selection.has(key);
          const label = getRowLabel?.(row) ?? key;
          return (
            // One keyed list for rows and spacers: the active row keeps its DOM node (and focus)
            // when it moves between the window and its pinned slot.
            <tr
              key={`r:${key}`}
              data-row-index={index}
              aria-rowindex={virtual ? index + 2 : undefined}
              tabIndex={index === activeIndex ? 0 : -1}
              className={[
                styles.row,
                isSelected ? styles.selected : undefined,
                onRowActivate ? styles.activatable : undefined,
              ]
                .filter(Boolean)
                .join(' ')}
              onKeyDown={(e) => onRowKeyDown(e, index, row)}
              onClick={(e) => onRowClick(e, index, row)}
              onFocus={(e) => {
                if (e.target === e.currentTarget) setFocusIndex(index);
              }}
            >
              {selectable ? (
                <td className={[styles.td, styles.selectCell].join(' ')}>
                  <input type="checkbox" tabIndex={-1} aria-label={`Select ${label}`} checked={isSelected} onChange={() => toggleRow(key)} />
                </td>
              ) : null}
              {columns.map((column) => (
                <td key={column.id} className={[styles.td, column.align === 'end' ? styles.end : undefined].filter(Boolean).join(' ')}>
                  {column.cell(row)}
                </td>
              ))}
            </tr>
          );
        })}
      </tbody>
    </table>
  );

  return (
    <div className={[styles.wrapper, className].filter(Boolean).join(' ')}>
      {viewportHeight ? (
        <div
          ref={containerRef}
          className={styles.scroll}
          style={scrollStyle}
          onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
        >
          {table}
        </div>
      ) : (
        table
      )}
    </div>
  );
}
