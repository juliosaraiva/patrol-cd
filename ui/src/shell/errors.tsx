import type { ReactNode } from 'react';
import { isRouteErrorResponse, useLocation, useNavigate, useRouteError } from 'react-router';
import { isApiError } from '../api/errors';
import { absoluteAppURL } from '../app/session';
import { Banner, Button, ButtonLink, CodeBlock, EmptyState, Spinner } from '../design';
import { IconError, IconWarning } from '../design/icons';
import { browser } from '../lib/browser';
import { useDocumentTitle } from '../lib/useDocumentTitle';
import styles from './Shell.module.css';

export interface ErrorGuidance {
  title: string;
  message: string;
  /** What the user can do next. */
  recovery: 'retry' | 'login' | 'navigate' | 'contact';
}

/** User-facing explanation and next step for any error a page can encounter. */
export function describeError(error: unknown): ErrorGuidance {
  if (isApiError(error)) {
    switch (error.code) {
      case 'unauthenticated':
        return { title: 'Your session has ended', message: 'Sign in again to continue where you left off.', recovery: 'login' };
      case 'permission_denied':
        return {
          title: 'You do not have access',
          message: `${error.detail} Ask an administrator for the required RBAC permission, or go back.`,
          recovery: 'navigate',
        };
      case 'not_found':
        return { title: 'Not found', message: `${error.detail} It may have been deleted or renamed.`, recovery: 'navigate' };
      case 'unavailable':
        return {
          title: 'Patrol server unavailable',
          message: `${error.detail}. Check your network connection and that patrol-server is running, then retry.`,
          recovery: 'retry',
        };
      case 'resource_exhausted':
        return {
          title: 'Too many requests',
          message: error.retryAfterSeconds ? `Retry in ${error.retryAfterSeconds} seconds.` : 'Wait a moment and retry.',
          recovery: 'retry',
        };
      case 'conflict':
        return { title: 'The object changed', message: `${error.detail} Reload to get the latest version, then reapply your change.`, recovery: 'retry' };
      // patrol-server's problem titles are HTTP reason phrases ("Bad Request"); name the situation instead.
      case 'invalid_argument':
        return { title: 'The request was rejected', message: error.detail, recovery: 'navigate' };
      case 'already_exists':
        return { title: 'It already exists', message: error.detail, recovery: 'navigate' };
      case 'failed_precondition':
        return { title: 'Not possible in the current state', message: error.detail, recovery: 'navigate' };
      default:
        if (error.status >= 500) {
          return { title: 'Server error', message: `${error.detail}. Retry; if it persists, check the patrol-server logs.`, recovery: 'retry' };
        }
        return { title: error.title, message: error.detail, recovery: 'navigate' };
    }
  }
  if (isRouteErrorResponse(error)) {
    if (error.status === 404) return { title: 'Page not found', message: 'This address does not match any page.', recovery: 'navigate' };
    return { title: `${error.status} ${error.statusText}`, message: String(error.data ?? ''), recovery: 'retry' };
  }
  return {
    title: 'Something went wrong',
    message: 'The page hit an unexpected error. Reload the page; if it happens again, report it with the details below.',
    recovery: 'contact',
  };
}

/**
 * Inline error for a failed query or mutation, with a retry action when it makes sense.
 * `message` replaces the generic explanation when the page knows better (e.g. a 403 that the
 * server also returns for missing objects).
 */
export function ErrorBanner({ error, onRetry, title, message }: { error: unknown; onRetry?: () => void; title?: string; message?: string | undefined }) {
  const guidance = describeError(error);
  return (
    <Banner
      tone="error"
      live
      title={title ?? guidance.title}
      actions={
        onRetry && guidance.recovery !== 'login' ? (
          <Button size="sm" onClick={onRetry}>
            Retry
          </Button>
        ) : undefined
      }
    >
      {message ?? guidance.message}
    </Banner>
  );
}

/**
 * Explanation for a 403 on a single object: patrol-server answers 403 for missing and forbidden
 * ApplicationSets, clusters and repositories alike, so the UI cannot tell which one it is.
 */
export function missingOrForbiddenMessage(error: unknown, noun: string): string | undefined {
  return isApiError(error) && error.status === 403
    ? `The ${noun} does not exist or you are not allowed to see it. It may have been deleted or renamed; otherwise ask an administrator for access.`
    : undefined;
}

function errorDetails(error: unknown): string | null {
  if (error instanceof Error) return [error.name, error.message, error.stack].filter(Boolean).join('\n');
  if (isRouteErrorResponse(error)) return null;
  try {
    return JSON.stringify(error, null, 2);
  } catch {
    return String(error);
  }
}

function ErrorView({ error, home }: { error: unknown; home: ReactNode }) {
  const navigate = useNavigate();
  const location = useLocation();
  const guidance = describeError(error);
  useDocumentTitle(guidance.title);
  const details = guidance.recovery === 'contact' ? errorDetails(error) : null;
  let action: ReactNode;
  switch (guidance.recovery) {
    case 'login':
      action = (
        <ButtonLink
          variant="primary"
          to={`/login?return_url=${encodeURIComponent(absoluteAppURL(`${location.pathname}${location.search}`))}`}
        >
          Sign in again
        </ButtonLink>
      );
      break;
    case 'retry':
      action = (
        <Button variant="primary" onClick={() => void navigate(0)}>
          Retry
        </Button>
      );
      break;
    case 'contact':
      action = (
        <Button variant="primary" onClick={() => browser.reload()}>
          Reload page
        </Button>
      );
      break;
    default:
      action = null;
  }
  return (
    <div role="alert">
      <EmptyState
        headingLevel={1}
        icon={guidance.recovery === 'navigate' ? <IconWarning size={24} /> : <IconError size={24} />}
        title={guidance.title}
        action={action}
        secondaryAction={home}
      >
        <p>{guidance.message}</p>
        {details ? (
          <details className={styles.errorDetails}>
            <summary>Technical details</summary>
            <CodeBlock code={details} title="Error" wrap maxHeight={240} />
          </details>
        ) : null}
      </EmptyState>
    </div>
  );
}

/** errorElement for routes inside the shell: keeps navigation visible. */
export function RouteError() {
  const error = useRouteError();
  return <ErrorView error={error} home={<ButtonLink to="/applications">Go to applications</ButtonLink>} />;
}

/** errorElement for the root (shell itself failed). */
export function RootError() {
  const error = useRouteError();
  return (
    <main className={styles.main}>
      <ErrorView error={error} home={<ButtonLink to="/login">Go to sign in</ButtonLink>} />
    </main>
  );
}

/** Shown while the first route module loads (lazy routes) and during session bootstrap. */
export function AppLoading() {
  return (
    <div className={styles.fullPage}>
      <Spinner size="lg" label="Loading Patrol CD" showLabel />
    </div>
  );
}

export function NotFoundPage() {
  const location = useLocation();
  useDocumentTitle('Page not found');
  return (
    <EmptyState
      headingLevel={1}
      icon={<IconWarning size={24} />}
      title="Page not found"
      action={
        <ButtonLink variant="primary" to="/applications">
          Go to applications
        </ButtonLink>
      }
      secondaryAction={<ButtonLink to="/settings">Open settings</ButtonLink>}
    >
      <p>
        Nothing exists at <code>{location.pathname}</code>. Check the address, or use the navigation to find what you were
        looking for.
      </p>
    </EmptyState>
  );
}
