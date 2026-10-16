import type { ReactNode } from 'react';
import { isApiError } from '../../../api/errors';
import { Banner, Button } from '../../../design';

export interface ActionErrorInfo {
  title: string;
  message: string;
  /** What to do next. */
  nextStep: string;
  /** Which recovery button makes sense. */
  recovery: 'retry' | 'reload' | 'fix' | 'login' | 'none';
}

/**
 * Explains a failed application action (sync, save, rollback, delete…) with a next step, mapping
 * the API error codes (RFC 9457 `code`) to guidance specific to applications.
 */
export function describeActionError(error: unknown, action: string): ActionErrorInfo {
  if (!isApiError(error)) {
    const message = error instanceof Error ? error.message : String(error);
    return { title: `Could not ${action}`, message, nextStep: 'Retry; if it fails again, reload the page.', recovery: 'retry' };
  }
  const detail = error.detail;
  switch (error.code) {
    case 'conflict':
      return {
        title: 'The application changed since you loaded it',
        message: detail,
        nextStep: 'Reload the latest version: your edits are merged onto it (fields you did not edit take the server values); review the result and save again.',
        recovery: 'reload',
      };
    case 'failed_precondition':
      return {
        title: `Cannot ${action} right now`,
        message: detail,
        nextStep:
          'Check what the message names: an active sync window that denies manual syncs, an operation that is still running, auto-sync being enabled, or the application being deleted.',
        recovery: 'none',
      };
    case 'permission_denied':
      return {
        title: 'Permission denied',
        message: detail,
        nextStep: `Your account lacks the RBAC permission to ${action} this application. Ask an administrator to grant it in the project roles or the RBAC policy.`,
        recovery: 'none',
      };
    case 'invalid_argument':
      return {
        title: 'The request was rejected as invalid',
        message: detail,
        nextStep: 'Correct the values named in the message and try again.',
        recovery: 'fix',
      };
    case 'already_exists':
      return { title: 'Already exists', message: detail, nextStep: 'Choose a different name or edit the existing object.', recovery: 'fix' };
    case 'not_found':
      return {
        title: 'Application not found',
        message: detail,
        nextStep: 'It may have been deleted or renamed. Return to the applications list.',
        recovery: 'none',
      };
    case 'unauthenticated':
      return { title: 'Your session has ended', message: detail, nextStep: 'Sign in again, then repeat the action.', recovery: 'login' };
    case 'resource_exhausted':
      return {
        title: 'Too many requests',
        message: detail,
        nextStep: error.retryAfterSeconds ? `Retry in ${error.retryAfterSeconds} seconds.` : 'Wait a moment and retry.',
        recovery: 'retry',
      };
    case 'unavailable':
      return {
        title: 'Patrol server unavailable',
        message: detail,
        nextStep: 'Check your connection and that patrol-server is running, then retry.',
        recovery: 'retry',
      };
    case 'internal':
      return {
        title: `Could not ${action}`,
        message: detail,
        nextStep: 'Retry. If it keeps failing, check the patrol-server and controller logs.',
        recovery: 'retry',
      };
  }
}

/** Inline, announced error for a failed action with the matching recovery button. */
export function ActionError({
  error,
  action,
  onRetry,
  onReload,
  children,
}: {
  error: unknown;
  action: string;
  onRetry?: () => void;
  onReload?: () => void;
  children?: ReactNode;
}) {
  const info = describeActionError(error, action);
  let button: ReactNode = null;
  if (info.recovery === 'reload' && onReload) {
    button = (
      <Button size="sm" onClick={onReload}>
        Reload latest
      </Button>
    );
  } else if (info.recovery === 'retry' && onRetry) {
    button = (
      <Button size="sm" onClick={onRetry}>
        Retry
      </Button>
    );
  }
  return (
    <Banner tone="error" live title={info.title} actions={button}>
      <p>{info.message}</p>
      <p>{info.nextStep}</p>
      {children}
    </Banner>
  );
}
