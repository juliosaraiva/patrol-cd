import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { Checkbox, Input } from '../../../../design';
import { useClickOutside } from '../../../../design/hooks';
import { IconChevronDown } from '../../../../design/icons';
import styles from './MultiSelectFilter.module.css';

export interface MultiSelectOption {
  value: string;
  label: string;
  /** Extra content after the label (e.g. a status glyph). */
  adornment?: ReactNode;
}

export interface MultiSelectFilterProps {
  /** Button and group label, e.g. "Kind". */
  label: string;
  options: readonly MultiSelectOption[];
  selected: readonly string[];
  onChange: (values: string[]) => void;
  /** Show a text box to narrow long option lists (default: more than 8 options). */
  searchable?: boolean;
}

/**
 * Disclosure button opening a checkbox list (multi-select filter). Esc or a click outside
 * closes it and focus returns to the button; Tab moves through the checkboxes normally.
 */
export function MultiSelectFilter({ label, options, selected, onChange, searchable }: MultiSelectFilterProps) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const panelId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const showSearch = searchable ?? options.length > 8;
  const refs = useMemo(() => [rootRef], []);
  useClickOutside(refs, open, () => setOpen(false));

  useEffect(() => {
    if (!open) return;
    const first = panelRef.current?.querySelector<HTMLElement>('input');
    first?.focus();
  }, [open]);

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? options.filter((o) => o.label.toLowerCase().includes(q) || o.value.toLowerCase().includes(q)) : options;
  }, [options, query]);

  const close = () => {
    setOpen(false);
    setQuery('');
    buttonRef.current?.focus();
  };

  const onPanelKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      close();
    }
  };

  const toggle = (value: string) => {
    const set = new Set(selected);
    if (set.has(value)) set.delete(value);
    else set.add(value);
    onChange(options.map((o) => o.value).filter((v) => set.has(v)).concat([...set].filter((v) => !options.some((o) => o.value === v))));
  };

  return (
    <div ref={rootRef} className={styles.root}>
      <button
        ref={buttonRef}
        type="button"
        className={[styles.trigger, selected.length > 0 ? styles.active : undefined].filter(Boolean).join(' ')}
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        onClick={() => (open ? close() : setOpen(true))}
      >
        <span>{label}</span>
        {selected.length > 0 ? (
          <span className={styles.count}>
            <span className="sr-only">, </span>
            {selected.length}
            <span className="sr-only"> selected</span>
          </span>
        ) : null}
        <IconChevronDown size={14} />
      </button>
      {open ? (
        // Escape is handled for the whole panel (disclosure pattern).
        // eslint-disable-next-line jsx-a11y/no-static-element-interactions
        <div ref={panelRef} id={panelId} className={styles.panel} onKeyDown={onPanelKeyDown}>
          <fieldset className={styles.fieldset}>
            <legend className="sr-only">{label}</legend>
            {showSearch ? (
              <Input size="sm" aria-label={`Search ${label.toLowerCase()} options`} value={query} onChange={(e) => setQuery(e.target.value)} />
            ) : null}
            {visible.length === 0 ? <p className={styles.none}>No options</p> : null}
            <div className={styles.options}>
              {visible.map((option) => (
                <Checkbox
                  key={option.value}
                  label={
                    <span className={styles.optionLabel}>
                      {option.label}
                      {option.adornment}
                    </span>
                  }
                  checked={selected.includes(option.value)}
                  onChange={() => toggle(option.value)}
                />
              ))}
            </div>
            {selected.length > 0 ? (
              <button type="button" className={styles.clear} onClick={() => onChange([])}>
                Clear {label.toLowerCase()} filter
              </button>
            ) : null}
          </fieldset>
        </div>
      ) : null}
    </div>
  );
}
