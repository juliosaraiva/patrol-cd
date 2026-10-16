import { cloneElement, isValidElement, useEffect, useId, useRef, useState, type ReactElement, type ReactNode } from 'react';
import styles from './Tooltip.module.css';

export interface TooltipProps {
  content: ReactNode;
  /** A single focusable element (button, link, or an element with tabIndex=0). */
  children: ReactElement<{ 'aria-describedby'?: string }>;
  placement?: 'top' | 'bottom';
  /** Hover delay; focus shows immediately. */
  delayMs?: number;
  /**
   * Link the tooltip with aria-describedby (default). Turn off when the text duplicates the
   * trigger's accessible name (e.g. an IconButton's aria-label).
   */
  describe?: boolean;
}

/**
 * Supplementary text on hover and keyboard focus (WCAG 1.4.13: hoverable, dismissible with Esc,
 * persistent while hovered or focused). Never put essential information only in a tooltip.
 */
export function Tooltip({ content, children, placement = 'top', delayMs = 400, describe = true }: TooltipProps) {
  const id = useId();
  const [visible, setVisible] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const show = (delay: number) => {
    clearTimeout(timer.current);
    if (delay === 0) setVisible(true);
    else timer.current = setTimeout(() => setVisible(true), delay);
  };
  const hide = () => {
    clearTimeout(timer.current);
    setVisible(false);
  };

  useEffect(() => () => clearTimeout(timer.current), []);
  useEffect(() => {
    if (!visible) return undefined;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setVisible(false);
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [visible]);

  const child = isValidElement(children)
    ? cloneElement(children, {
        'aria-describedby':
          describe && visible ? [children.props['aria-describedby'], id].filter(Boolean).join(' ') : children.props['aria-describedby'],
      })
    : children;

  return (
    // Wrapper only observes hover/focus bubbling from the trigger; the trigger stays the control.
    // eslint-disable-next-line jsx-a11y/no-static-element-interactions
    <span
      className={styles.anchor}
      onMouseEnter={() => show(delayMs)}
      onMouseLeave={hide}
      onFocus={() => show(0)}
      onBlur={hide}
    >
      {child}
      {visible ? (
        <span role="tooltip" id={id} className={[styles.tooltip, placement === 'top' ? styles.top : styles.bottom].join(' ')}>
          {content}
        </span>
      ) : null}
    </span>
  );
}
