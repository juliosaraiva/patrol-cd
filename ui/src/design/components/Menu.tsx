import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { useClickOutside } from '../hooks';
import styles from './Menu.module.css';

export type MenuEntry =
  | {
      type?: 'item';
      id: string;
      label: string;
      onSelect: () => void;
      icon?: ReactNode;
      disabled?: boolean;
      danger?: boolean;
    }
  | { type: 'separator'; id: string };

export interface MenuProps {
  /**
   * Renders the trigger. Spread `props` onto a <button> (Button/IconButton): they carry the
   * ref, aria-haspopup/expanded/controls and the open/keyboard handlers.
   */
  trigger: (props: MenuTriggerProps) => ReactNode;
  items: readonly MenuEntry[];
  /** Accessible name of the menu (defaults to the trigger's name via aria-labelledby). */
  label?: string;
  /** Non-interactive text shown above the items (e.g. "Signed in as admin"). */
  header?: ReactNode;
  align?: 'start' | 'end';
}

export interface MenuTriggerProps {
  ref: (el: HTMLButtonElement | null) => void;
  id: string;
  'aria-haspopup': 'menu';
  'aria-expanded': boolean;
  'aria-controls': string | undefined;
  onClick: () => void;
  onKeyDown: (event: KeyboardEvent<HTMLButtonElement>) => void;
}

/**
 * Menu button (WAI-ARIA APG): Enter/Space/ArrowDown open on the first item, ArrowUp on the last;
 * arrows wrap, Home/End jump, typing a letter moves to the next matching item, Esc closes and
 * returns focus to the trigger, Tab closes.
 */
export function Menu({ trigger, items, label, header, align = 'start' }: MenuProps) {
  const [open, setOpen] = useState(false);
  const baseId = useId();
  const triggerId = `${baseId}-trigger`;
  const menuId = `${baseId}-menu`;
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const itemRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const actionable = items.filter((i): i is Extract<MenuEntry, { onSelect: () => void }> => i.type !== 'separator');

  const focusItem = (index: number) => {
    const enabled = actionable.map((item, i) => (item.disabled ? -1 : i)).filter((i) => i >= 0);
    if (enabled.length === 0) return;
    const target = enabled.includes(index) ? index : enabled[0];
    if (target !== undefined) itemRefs.current[target]?.focus();
  };

  const pendingFocus = useRef<'first' | 'last' | null>(null);

  const openMenu = (focus: 'first' | 'last') => {
    pendingFocus.current = focus;
    setOpen(true);
  };

  useEffect(() => {
    if (!open || pendingFocus.current === null) return;
    const enabled = itemRefs.current.filter((el): el is HTMLButtonElement => el !== null && !el.disabled);
    const target = pendingFocus.current === 'first' ? enabled[0] : enabled[enabled.length - 1];
    pendingFocus.current = null;
    target?.focus();
  }, [open]);

  const close = (restoreFocus: boolean) => {
    setOpen(false);
    if (restoreFocus) triggerRef.current?.focus();
  };

  useClickOutside([rootRef], open, () => close(false));

  const onTriggerKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key === 'ArrowDown' || event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      openMenu('first');
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      openMenu('last');
    }
  };

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const current = itemRefs.current.findIndex((el) => el === document.activeElement);
    const enabled = actionable.map((item, i) => (item.disabled ? -1 : i)).filter((i) => i >= 0);
    const pos = enabled.indexOf(current);
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault();
        focusItem(enabled[(pos + 1) % enabled.length] ?? 0);
        return;
      case 'ArrowUp':
        event.preventDefault();
        focusItem(enabled[(pos - 1 + enabled.length) % enabled.length] ?? 0);
        return;
      case 'Home':
        event.preventDefault();
        focusItem(enabled[0] ?? 0);
        return;
      case 'End':
        event.preventDefault();
        focusItem(enabled[enabled.length - 1] ?? 0);
        return;
      case 'Escape':
        event.preventDefault();
        event.stopPropagation();
        close(true);
        return;
      case 'Tab':
        close(false);
        return;
      default:
        break;
    }
    if (event.key.length === 1 && /\S/.test(event.key)) {
      const letter = event.key.toLowerCase();
      const order = [...enabled.slice(pos + 1), ...enabled.slice(0, pos + 1)];
      const match = order.find((i) => actionable[i]?.label.toLowerCase().startsWith(letter));
      if (match !== undefined) focusItem(match);
    }
  };

  let actionIndex = -1;
  return (
    <div ref={rootRef} className={styles.root}>
      {trigger({
        ref: (el) => {
          triggerRef.current = el;
        },
        id: triggerId,
        'aria-haspopup': 'menu',
        'aria-expanded': open,
        'aria-controls': open ? menuId : undefined,
        onClick: () => (open ? close(false) : openMenu('first')),
        onKeyDown: onTriggerKeyDown,
      })}
      {open ? (
        <div className={[styles.menu, align === 'end' ? styles.end : styles.start].join(' ')}>
          {header ? <div className={styles.header}>{header}</div> : null}
          <div
            id={menuId}
            role="menu"
            aria-label={label}
            aria-labelledby={label ? undefined : triggerId}
            tabIndex={-1}
            onKeyDown={onMenuKeyDown}
          >
            {items.map((entry) => {
              if (entry.type === 'separator') return <div key={entry.id} role="separator" className={styles.separator} />;
              actionIndex += 1;
              const index = actionIndex;
              return (
                <button
                  key={entry.id}
                  ref={(el) => {
                    itemRefs.current[index] = el;
                  }}
                  type="button"
                  role="menuitem"
                  tabIndex={-1}
                  disabled={entry.disabled}
                  className={[styles.item, entry.danger ? styles.danger : undefined].filter(Boolean).join(' ')}
                  onClick={() => {
                    close(true);
                    entry.onSelect();
                  }}
                >
                  {entry.icon ? <span className={styles.icon}>{entry.icon}</span> : null}
                  {entry.label}
                </button>
              );
            })}
          </div>
        </div>
      ) : null}
    </div>
  );
}
