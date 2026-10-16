import { useEffect, useId, useRef, type CSSProperties, type ReactNode, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import { useFocusTrap } from '../hooks';
import { IconClose } from '../icons';
import { IconButton } from './Button';
import styles from './Overlay.module.css';

interface OverlayProps {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
  /** Element focused on open (defaults to the first focusable element). */
  initialFocusRef?: RefObject<HTMLElement | null>;
  /** Close when the backdrop is clicked (default true). Esc always closes. */
  closeOnBackdrop?: boolean;
}

/*
 * Body scroll lock shared by all open overlays. The page's own overflow is captured only when
 * the first overlay opens and restored only when the last one closes, so nested overlays can
 * close (or unmount together) in any order.
 */
let openOverlays = 0;
let overflowBeforeLock = '';

function useScrollLock(active: boolean) {
  useEffect(() => {
    if (!active) return undefined;
    if (openOverlays === 0) overflowBeforeLock = document.body.style.overflow;
    openOverlays += 1;
    document.body.style.overflow = 'hidden';
    return () => {
      openOverlays = Math.max(0, openOverlays - 1);
      if (openOverlays === 0) document.body.style.overflow = overflowBeforeLock;
    };
  }, [active]);
}

function OverlayFrame({
  kind,
  open,
  onClose,
  title,
  description,
  children,
  footer,
  initialFocusRef,
  closeOnBackdrop = true,
  className,
  style,
  side = 'end',
}: OverlayProps & {
  kind: 'dialog' | 'drawer';
  side?: 'start' | 'end';
  className?: string | undefined;
  style?: CSSProperties | undefined;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const descriptionId = useId();
  useFocusTrap(panelRef, open, initialFocusRef);
  useScrollLock(open);

  if (!open) return null;
  return createPortal(
    // Backdrop click is a pointer convenience; keyboard users close with Esc or the Close button.
    // eslint-disable-next-line jsx-a11y/no-static-element-interactions
    <div
      className={[
        styles.backdrop,
        kind === 'drawer' ? styles.drawerBackdrop : undefined,
        kind === 'drawer' && side === 'start' ? styles.drawerStart : undefined,
      ]
        .filter(Boolean)
        .join(' ')}
      onMouseDown={(e) => {
        if (closeOnBackdrop && e.target === e.currentTarget) onClose();
      }}
    >
      {/* The dialog container handles Escape for everything inside it (APG dialog pattern). */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descriptionId : undefined}
        tabIndex={-1}
        className={[kind === 'drawer' ? styles.drawer : styles.dialog, className].filter(Boolean).join(' ')}
        style={style}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            e.stopPropagation();
            onClose();
          }
        }}
      >
        <div className={styles.header}>
          <div className={styles.titles}>
            <h2 id={titleId} className={styles.title}>
              {title}
            </h2>
            {description ? (
              <p id={descriptionId} className={styles.description}>
                {description}
              </p>
            ) : null}
          </div>
          <IconButton aria-label="Close" icon={<IconClose />} onClick={onClose} size="sm" />
        </div>
        <div className={styles.body}>{children}</div>
        {footer ? <div className={styles.footer}>{footer}</div> : null}
      </div>
    </div>,
    document.body,
  );
}

export interface DialogProps extends OverlayProps {
  size?: 'md' | 'lg';
}

/** Modal dialog: focus trapped, Esc closes, focus returns to the opener. */
export function Dialog({ size = 'md', ...props }: DialogProps) {
  return <OverlayFrame kind="dialog" className={size === 'lg' ? styles.lg : undefined} {...props} />;
}

export interface DrawerProps extends OverlayProps {
  /** Panel width (CSS length); capped at the viewport width. */
  width?: string;
  side?: 'start' | 'end';
}

/** Slide-in panel (end edge by default) with the same modal semantics as Dialog. */
export function Drawer({ width, side = 'end', ...props }: DrawerProps) {
  return (
    <OverlayFrame
      kind="drawer"
      side={side}
      style={width ? ({ '--drawer-width': width } as CSSProperties) : undefined}
      {...props}
    />
  );
}
