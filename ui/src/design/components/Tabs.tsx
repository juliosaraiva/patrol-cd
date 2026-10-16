import { useId, useRef, type KeyboardEvent, type ReactNode } from 'react';
import { useControllableState } from '../hooks';
import styles from './Tabs.module.css';

export interface TabItem {
  id: string;
  label: ReactNode;
  content: ReactNode;
  disabled?: boolean;
}

export interface TabsProps {
  /** Accessible name of the tab list. */
  label: string;
  items: readonly TabItem[];
  value?: string;
  defaultValue?: string;
  onValueChange?: (id: string) => void;
  className?: string;
}

/**
 * WAI-ARIA tabs with automatic activation: one tab in the Tab order (roving tabindex);
 * ArrowLeft/ArrowRight move and select, Home/End jump to the ends, disabled tabs are skipped.
 */
export function Tabs({ label, items, value, defaultValue, onValueChange, className }: TabsProps) {
  const baseId = useId();
  const firstEnabled = items.find((i) => !i.disabled)?.id ?? '';
  const [selected, setSelected] = useControllableState(value, defaultValue ?? firstEnabled, onValueChange);
  const tabRefs = useRef(new Map<string, HTMLButtonElement>());
  const enabled = items.filter((i) => !i.disabled);
  const active = items.find((i) => i.id === selected) ?? enabled[0];

  const move = (event: KeyboardEvent<HTMLButtonElement>, currentId: string) => {
    const index = enabled.findIndex((i) => i.id === currentId);
    let next: TabItem | undefined;
    switch (event.key) {
      case 'ArrowRight':
        next = enabled[(index + 1) % enabled.length];
        break;
      case 'ArrowLeft':
        next = enabled[(index - 1 + enabled.length) % enabled.length];
        break;
      case 'Home':
        next = enabled[0];
        break;
      case 'End':
        next = enabled[enabled.length - 1];
        break;
      default:
        return;
    }
    event.preventDefault();
    if (!next) return;
    setSelected(next.id);
    tabRefs.current.get(next.id)?.focus();
  };

  return (
    <div className={className}>
      <div role="tablist" aria-label={label} className={styles.tablist}>
        {items.map((item) => {
          const isActive = item.id === active?.id;
          return (
            <button
              key={item.id}
              ref={(el) => {
                if (el) tabRefs.current.set(item.id, el);
                else tabRefs.current.delete(item.id);
              }}
              type="button"
              role="tab"
              id={`${baseId}-tab-${item.id}`}
              aria-selected={isActive}
              aria-controls={`${baseId}-panel-${item.id}`}
              tabIndex={isActive ? 0 : -1}
              disabled={item.disabled}
              className={styles.tab}
              onClick={() => setSelected(item.id)}
              onKeyDown={(e) => move(e, item.id)}
            >
              {item.label}
            </button>
          );
        })}
      </div>
      {active ? (
        <div
          role="tabpanel"
          id={`${baseId}-panel-${active.id}`}
          aria-labelledby={`${baseId}-tab-${active.id}`}
          tabIndex={0}
          className={styles.panel}
        >
          {active.content}
        </div>
      ) : null}
    </div>
  );
}
