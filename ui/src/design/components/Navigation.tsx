import { useId, type ComponentPropsWithRef, type ReactNode } from 'react';
import { Link } from 'react-router';
import { IconChevronLeft, IconChevronRight, IconClose, IconSearch } from '../icons';
import { Button, IconButton } from './Button';
import { Input, Select } from './Form';
import styles from './Navigation.module.css';

export interface Crumb {
  label: string;
  /** Router path; omit for the current page. */
  to?: string;
}

export function Breadcrumbs({ items, className }: { items: readonly Crumb[]; className?: string }) {
  if (items.length === 0) return null;
  return (
    <nav aria-label="Breadcrumb" className={[styles.breadcrumbs, className].filter(Boolean).join(' ')}>
      <ol>
        {items.map((item, i) => {
          const last = i === items.length - 1;
          return (
            <li key={`${item.label}-${i}`} className={styles.crumb}>
              {last || !item.to ? (
                <span className={styles.current} aria-current={last ? 'page' : undefined}>
                  {item.label}
                </span>
              ) : (
                <Link to={item.to}>{item.label}</Link>
              )}
              {last ? null : (
                <span className={styles.separator} aria-hidden="true">
                  /
                </span>
              )}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}

export interface PaginationProps {
  /** 1-based page number. */
  page: number;
  pageCount: number;
  onPageChange: (page: number) => void;
  pageSize?: number;
  pageSizeOptions?: readonly number[];
  onPageSizeChange?: (size: number) => void;
  totalItems?: number;
  className?: string;
}

export function Pagination({
  page,
  pageCount,
  onPageChange,
  pageSize,
  pageSizeOptions = [10, 25, 50, 100],
  onPageSizeChange,
  totalItems,
  className,
}: PaginationProps) {
  const sizeId = useId();
  const count = Math.max(1, pageCount);
  const current = Math.min(Math.max(1, page), count);
  return (
    <nav aria-label="Pagination" className={[styles.pagination, className].filter(Boolean).join(' ')}>
      <div className={styles.pages}>
        <Button size="sm" icon={<IconChevronLeft />} disabled={current <= 1} onClick={() => onPageChange(current - 1)} aria-label="Previous page">
          Previous
        </Button>
        <span aria-live="polite">
          Page {current} of {count}
          {totalItems !== undefined ? ` (${totalItems} items)` : ''}
        </span>
        <Button size="sm" disabled={current >= count} onClick={() => onPageChange(current + 1)} aria-label="Next page">
          Next
          <IconChevronRight />
        </Button>
      </div>
      {pageSize !== undefined && onPageSizeChange ? (
        <div className={styles.pageSize}>
          <label htmlFor={sizeId}>Rows per page</label>
          <Select
            id={sizeId}
            size="sm"
            value={String(pageSize)}
            onChange={(e) => onPageSizeChange(Number(e.target.value))}
            options={pageSizeOptions.map((n) => ({ value: String(n), label: String(n) }))}
          />
        </div>
      ) : null}
    </nav>
  );
}

export interface SearchInputProps extends Omit<ComponentPropsWithRef<'input'>, 'onChange' | 'value' | 'size' | 'type' | 'onSubmit'> {
  value: string;
  onValueChange: (value: string) => void;
  /** Accessible name (also used as placeholder when none is given). */
  'aria-label': string;
  size?: 'sm' | 'md';
  /** Keyboard shortcut hint rendered as <kbd>, e.g. "/". */
  shortcut?: string;
  onSubmit?: (value: string) => void;
}

/** Search field: Esc clears (then blurs when already empty), Enter submits. */
export function SearchInput({ value, onValueChange, shortcut, onSubmit, placeholder, className, onKeyDown, size = 'md', ...rest }: SearchInputProps) {
  return (
    <div className={[styles.search, className].filter(Boolean).join(' ')}>
      <span className={styles.searchIcon} aria-hidden="true">
        <IconSearch />
      </span>
      <Input
        type="search"
        size={size}
        className={styles.searchInput}
        value={value}
        placeholder={placeholder ?? rest['aria-label']}
        autoComplete="off"
        spellCheck={false}
        onChange={(e) => onValueChange(e.target.value)}
        onKeyDown={(e) => {
          onKeyDown?.(e);
          if (e.defaultPrevented) return;
          if (e.key === 'Escape') {
            if (value !== '') {
              e.preventDefault();
              onValueChange('');
            } else {
              e.currentTarget.blur();
            }
          } else if (e.key === 'Enter' && onSubmit) {
            e.preventDefault();
            onSubmit(value);
          }
        }}
        {...rest}
      />
      <span className={styles.searchTrail}>
        {value !== '' ? (
          <IconButton aria-label="Clear search" icon={<IconClose size={14} />} size="sm" onClick={() => onValueChange('')} />
        ) : shortcut ? (
          <kbd className={styles.kbd} aria-hidden="true">
            {shortcut}
          </kbd>
        ) : null}
      </span>
    </div>
  );
}

export interface FilterChip {
  id: string;
  /** Full text, e.g. "Health: Degraded". */
  label: string;
}

export function FilterChips({
  chips,
  onRemove,
  onClearAll,
  label = 'Active filters',
}: {
  chips: readonly FilterChip[];
  onRemove: (id: string) => void;
  onClearAll?: () => void;
  label?: string;
}) {
  if (chips.length === 0) return null;
  return (
    <ul className={styles.chips} aria-label={label}>
      {chips.map((chip) => (
        <li key={chip.id} className={styles.chip}>
          <span>{chip.label}</span>
          <button type="button" className={styles.chipRemove} aria-label={`Remove filter ${chip.label}`} onClick={() => onRemove(chip.id)}>
            <IconClose size={12} />
          </button>
        </li>
      ))}
      {onClearAll && chips.length > 1 ? (
        <li>
          <button type="button" className={styles.clearAll} onClick={onClearAll}>
            Clear all filters
          </button>
        </li>
      ) : null}
    </ul>
  );
}

/** Layout for list toolbars: search and filter controls on one row, active chips below. */
export function FilterBar({ children, chips, actions, label = 'Filters' }: { children: ReactNode; chips?: ReactNode; actions?: ReactNode; label?: string }) {
  return (
    <div role="group" aria-label={label} className={styles.filterBar}>
      <div className={styles.filterRow}>
        {children}
        {actions}
      </div>
      {chips}
    </div>
  );
}
