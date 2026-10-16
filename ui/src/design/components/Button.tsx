import type { ComponentPropsWithRef, ReactNode } from 'react';
import { Link, type LinkProps } from 'react-router';
import styles from './Button.module.css';
import { Spinner } from './Spinner';

export type ButtonVariant = 'primary' | 'secondary' | 'danger' | 'ghost';
export type ButtonSize = 'sm' | 'md';

export function buttonClassName(variant: ButtonVariant = 'secondary', size: ButtonSize = 'md', extra?: string): string {
  return [styles.button, styles[variant], size === 'sm' ? styles.sm : undefined, extra].filter(Boolean).join(' ');
}

export interface ButtonProps extends ComponentPropsWithRef<'button'> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  /** Leading icon (decorative). */
  icon?: ReactNode;
  /** Shows a spinner, sets aria-busy and blocks clicks while an action runs. */
  loading?: boolean;
}

export function Button({
  variant = 'secondary',
  size = 'md',
  icon,
  loading = false,
  className,
  children,
  type = 'button',
  disabled,
  onClick,
  ...rest
}: ButtonProps) {
  return (
    <button
      type={type}
      className={buttonClassName(variant, size, className)}
      disabled={disabled}
      aria-busy={loading || undefined}
      onClick={loading ? (e) => e.preventDefault() : onClick}
      {...rest}
    >
      {loading ? <Spinner size="sm" label="" /> : icon ? <span className={styles.icon}>{icon}</span> : null}
      {children}
    </button>
  );
}

export interface ButtonLinkProps extends LinkProps {
  variant?: ButtonVariant;
  size?: ButtonSize;
  icon?: ReactNode;
}

/** Router link styled as a button (navigation, not an action). */
export function ButtonLink({ variant = 'secondary', size = 'md', icon, className, children, ...rest }: ButtonLinkProps) {
  return (
    <Link className={buttonClassName(variant, size, className)} {...rest}>
      {icon ? <span className={styles.icon}>{icon}</span> : null}
      {children}
    </Link>
  );
}

export interface IconButtonProps extends Omit<ComponentPropsWithRef<'button'>, 'children'> {
  /** Required accessible name: the icon alone conveys nothing to assistive technology. */
  'aria-label': string;
  icon: ReactNode;
  variant?: ButtonVariant;
  size?: ButtonSize;
}

export function IconButton({ icon, variant = 'ghost', size = 'md', className, type = 'button', ...rest }: IconButtonProps) {
  return (
    <button type={type} className={buttonClassName(variant, size, [styles.iconButton, className].filter(Boolean).join(' '))} {...rest}>
      <span className={styles.icon}>{icon}</span>
    </button>
  );
}
