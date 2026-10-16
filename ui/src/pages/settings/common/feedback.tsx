import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { isApiError } from '../../../api/errors';
import { Banner, Button, ButtonLink, Dialog, FormField, Input, Skeleton } from '../../../design';
import { describeError } from '../../../shell/errors';
import styles from './common.module.css';

export interface ActionErrorGuidance {
  title: string;
  message: string;
  /** The object changed on the server (409): offer to reload the latest version. */
  conflict: boolean;
  /** The session ended: offer to sign in again. */
  login: boolean;
}

/**
 * Explains why a write (create, update, delete, validate) failed and what to do next. `action`
 * is a lowercase verb phrase such as "save the project" or "delete the repository".
 */
export function describeActionError(error: unknown, action: string): ActionErrorGuidance {
  const base = { conflict: false, login: false };
  if (!isApiError(error)) {
    const g = describeError(error);
    return { ...base, title: `Unable to ${action}`, message: g.message };
  }
  const detail = error.detail;
  switch (error.code) {
    case 'conflict':
      return {
        ...base,
        conflict: true,
        title: 'Changed by someone else',
        message: `${detail} Another change was saved after you opened this view. Reload to get the latest version, then reapply your change.`,
      };
    case 'already_exists':
      return { ...base, title: 'Already exists', message: `${detail} Choose a different name, or edit the existing entry instead.` };
    case 'invalid_argument':
      return { ...base, title: `Unable to ${action}`, message: `${detail} Correct the values and try again.` };
    case 'failed_precondition':
      return { ...base, title: `Unable to ${action}`, message: `${detail} Resolve the condition described, then try again.` };
    case 'permission_denied':
      return {
        ...base,
        title: 'Permission denied',
        message: `You are not allowed to ${action}. ${detail} Ask an administrator to grant the RBAC permission.`,
      };
    case 'not_found':
      return { ...base, title: 'No longer exists', message: `${detail} It may have been deleted by someone else. Refresh the list and try again.` };
    case 'unauthenticated':
      return { ...base, login: true, title: 'Your session has ended', message: 'Sign in again, then repeat the change.' };
    default: {
      const g = describeError(error);
      return { ...base, title: `Unable to ${action}`, message: `${g.title}. ${g.message}` };
    }
  }
}

/** Inline, announced error for a failed write, with reload or sign-in actions when useful. */
export function ActionError({ error, action, onReload }: { error: unknown; action: string; onReload?: (() => void) | undefined }) {
  const guidance = describeActionError(error, action);
  let actions: ReactNode;
  if (guidance.conflict && onReload) {
    actions = (
      <Button size="sm" onClick={onReload}>
        Reload latest
      </Button>
    );
  } else if (guidance.login) {
    actions = (
      <ButtonLink size="sm" to="/login">
        Sign in
      </ButtonLink>
    );
  }
  return (
    <Banner tone="error" live title={guidance.title} actions={actions}>
      {guidance.message}
    </Banner>
  );
}

export interface FieldError {
  /** id of the control; the summary links to it. */
  id: string;
  message: string;
}

/**
 * Accessible error summary shown above a form after a failed submit. It receives focus when it
 * appears (or when `focusKey` changes) and links to each invalid control.
 */
export function ErrorSummary({ errors, focusKey }: { errors: readonly FieldError[]; focusKey: number }) {
  const ref = useRef<HTMLDivElement>(null);
  const titleId = useId();
  useEffect(() => {
    if (errors.length > 0) ref.current?.focus();
    // Only re-focus on a new submit attempt, not on every edit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusKey]);
  if (errors.length === 0) return null;
  return (
    <div ref={ref} tabIndex={-1} className={styles.summary} role="alert" aria-labelledby={titleId}>
      <Banner tone="error" title={<span id={titleId}>{errors.length === 1 ? 'Fix 1 problem before saving' : `Fix ${errors.length} problems before saving`}</span>}>
        <ul>
          {errors.map((e) => (
            <li key={`${e.id}-${e.message}`}>
              <a
                href={`#${e.id}`}
                onClick={(event) => {
                  event.preventDefault();
                  const target = document.getElementById(e.id);
                  // A control inside a collapsed <details> cannot take focus: expand its ancestors first.
                  for (let d = target?.closest('details'); d; d = d.parentElement?.closest('details')) d.open = true;
                  target?.focus();
                }}
              >
                {e.message}
              </a>
            </li>
          ))}
        </ul>
      </Banner>
    </div>
  );
}

/**
 * Tracks validation errors and submit attempts for a form. With `live` (the form's current
 * errors, computed on every render) messages update as the user fixes them after the first
 * submit; without it they refresh on the next submit.
 */
export function useFormErrors(live?: readonly FieldError[]) {
  const [checked, setChecked] = useState<readonly FieldError[]>([]);
  const [attempt, setAttempt] = useState(0);
  const [submitted, setSubmitted] = useState(false);
  const errors = submitted ? (live ?? checked) : [];
  return {
    errors,
    attempt,
    submitted,
    /** Records a submit attempt; returns true when there are no errors. */
    check: (next?: readonly FieldError[]): boolean => {
      const result = next ?? live ?? [];
      setChecked(result);
      setSubmitted(true);
      setAttempt((a) => a + 1);
      return result.length === 0;
    },
    /** Error message for a control id, shown inline once the user tried to submit. */
    errorFor: (id: string): string | undefined => errors.find((e) => e.id === id)?.message,
    reset: () => {
      setChecked([]);
      setSubmitted(false);
    },
  };
}

export interface ConfirmDialogProps {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  title: string;
  /** What will happen and what it affects. */
  children?: ReactNode;
  confirmLabel: string;
  busy?: boolean;
  error?: unknown;
  /** The action for error messages, e.g. "delete the cluster". */
  action: string;
  /** When set, the user must type this text before confirming (high-impact deletions). */
  confirmText?: string | undefined;
  danger?: boolean;
}

/** Confirmation for destructive actions; Esc or Cancel closes without changes. */
export function ConfirmDialog({
  open,
  onClose,
  onConfirm,
  title,
  children,
  confirmLabel,
  busy = false,
  error,
  action,
  confirmText,
  danger = true,
}: ConfirmDialogProps) {
  const [typed, setTyped] = useState('');
  useEffect(() => {
    if (!open) setTyped('');
  }, [open]);
  const blocked = confirmText !== undefined && typed !== confirmText;
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant={danger ? 'danger' : 'primary'} loading={busy} disabled={blocked} onClick={onConfirm}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      <div className={styles.stackTight}>
        {error ? <ActionError error={error} action={action} /> : null}
        {children}
        {confirmText !== undefined ? (
          <FormField label={<>Type <strong>{confirmText}</strong> to confirm</>} required>
            <Input
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !blocked) onConfirm();
              }}
            />
          </FormField>
        ) : null}
      </div>
    </Dialog>
  );
}

/** Placeholder rows while a list loads; the container is marked busy for assistive technology. */
export function LoadingBlock({ label, rows = 4 }: { label: string; rows?: number }) {
  return (
    <div className={styles.loading} aria-busy="true" aria-label={label} role="status">
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} height={28} />
      ))}
    </div>
  );
}
