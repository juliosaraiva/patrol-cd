import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, Navigate, useNavigate, useSearchParams } from 'react-router';
import { ssoLoginURL, createSession } from '../../api/endpoints/session';
import { isApiError } from '../../api/errors';
import { useSettings, useUserInfo } from '../../api/queries/resources';
import { absoluteAppURL, isAnonymousSession, isSSOConfigured, safeReturnPath, ssoButtonLabel } from '../../app/session';
import { Banner, Button, buttonClassName, FormField, Input, Spinner } from '../../design';
import { PatrolMark } from '../../design/icons';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import styles from './Auth.module.css';

interface FieldErrors {
  username?: string;
  password?: string;
}

/** Turns a failed login into a message that says what happened and what to do. */
export function loginErrorMessage(error: unknown): { title: string; detail: string; retryAfter?: number } {
  if (isApiError(error)) {
    if (error.status === 429 || error.code === 'resource_exhausted') {
      const wait = error.retryAfterSeconds;
      return {
        title: 'Too many sign-in attempts',
        detail: wait
          ? `Sign-in is temporarily locked for this account. Try again in ${wait} second${wait === 1 ? '' : 's'}.`
          : 'Sign-in is temporarily locked for this account. Wait a few minutes and try again.',
        ...(wait ? { retryAfter: wait } : {}),
      };
    }
    if (error.status === 401 || error.code === 'unauthenticated') {
      return { title: 'Sign-in failed', detail: 'The username or password is incorrect.' };
    }
    if (error.status === 403 || error.code === 'permission_denied') {
      return { title: 'Sign-in not allowed', detail: error.detail };
    }
    if (error.status === 0 || error.code === 'unavailable') {
      return { title: 'Patrol server unavailable', detail: 'The server could not be reached. Check your connection and try again.' };
    }
    return { title: 'Sign-in failed', detail: error.detail };
  }
  return { title: 'Sign-in failed', detail: 'An unexpected error occurred. Try again.' };
}

/** /login — local username/password and SSO entry point (API-087). */
export function Component() {
  useDocumentTitle('Sign in');
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const settings = useSettings();
  const user = useUserInfo();
  const returnPath = safeReturnPath(params.get('return_url'));
  const ssoError = params.get('has_sso_error') === 'true' || params.has('sso_error');

  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({});
  const [submitError, setSubmitError] = useState<ReturnType<typeof loginErrorMessage> | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [lockedUntil, setLockedUntil] = useState<number | null>(null);
  const usernameRef = useRef<HTMLInputElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (lockedUntil === null) return undefined;
    const t = setTimeout(() => setLockedUntil(null), Math.max(0, lockedUntil - Date.now()));
    return () => clearTimeout(t);
  }, [lockedUntil]);

  if (settings.isPending || user.isPending) {
    return (
      <div className={styles.page}>
        <Spinner size="lg" label="Loading sign-in options" showLabel />
      </div>
    );
  }

  if (user.data?.loggedIn) return <Navigate to={returnPath} replace />;

  const config = settings.data;
  const sso = isSSOConfigured(config);
  const localEnabled = config?.userLoginsDisabled !== true;
  const anonymous = isAnonymousSession(config, user.data);

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const errors: FieldErrors = {};
    if (!username.trim()) errors.username = 'Enter your username.';
    if (!password) errors.password = 'Enter your password.';
    setFieldErrors(errors);
    if (errors.username || errors.password) {
      (errors.username ? usernameRef : passwordRef).current?.focus();
      return;
    }
    setSubmitting(true);
    setSubmitError(null);
    try {
      await createSession({ username: username.trim(), password }, { cookie: true });
      // Drop everything cached for the previous (anonymous or expired) session.
      queryClient.removeQueries({ predicate: (q) => q.queryKey[0] !== 'settings' && q.queryKey[0] !== 'version' });
      await navigate(returnPath, { replace: true });
    } catch (err) {
      const message = loginErrorMessage(err);
      setSubmitError(message);
      setPassword('');
      if (message.retryAfter) setLockedUntil(Date.now() + message.retryAfter * 1000);
      passwordRef.current?.focus();
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className={styles.page}>
      <main className={styles.card}>
        <div className={styles.brand}>
          <PatrolMark size={28} />
          <span>Patrol CD</span>
        </div>
        <h1 className={styles.title}>Sign in to Patrol CD</h1>

        {ssoError ? (
          <Banner tone="error" title="Single sign-on failed" live>
            {params.get('sso_error') ?? 'The identity provider did not complete the sign-in. Try again or contact your administrator.'}
          </Banner>
        ) : null}

        {sso ? (
          <a className={buttonClassName('primary', 'md', styles.fullWidth)} href={ssoLoginURL(absoluteAppURL(returnPath))}>
            {ssoButtonLabel(config)}
          </a>
        ) : null}

        {sso && localEnabled ? (
          <div className={styles.divider} role="separator">
            <span>or sign in with a local account</span>
          </div>
        ) : null}

        {localEnabled ? (
          <form className={styles.form} onSubmit={(e) => void onSubmit(e)} noValidate aria-label="Local account sign-in">
            {submitError ? (
              <Banner tone="error" title={submitError.title} live>
                {submitError.detail}
              </Banner>
            ) : null}
            <FormField label="Username" error={fieldErrors.username} required>
              <Input
                ref={usernameRef}
                name="username"
                autoComplete="username"
                autoCapitalize="none"
                spellCheck={false}
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </FormField>
            <FormField label="Password" error={fieldErrors.password} required>
              <Input
                ref={passwordRef}
                name="password"
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </FormField>
            <Button type="submit" variant={sso ? 'secondary' : 'primary'} loading={submitting} disabled={lockedUntil !== null} className={styles.fullWidth}>
              Sign in
            </Button>
          </form>
        ) : null}

        {!localEnabled && !sso ? (
          <Banner tone="warning" title="Sign-in is not available">
            Local accounts are disabled and no single sign-on provider is configured. Contact your Patrol CD administrator.
          </Banner>
        ) : null}

        {anonymous ? (
          <p className={styles.footnote}>
            <Link to={returnPath}>Continue without signing in</Link>
          </p>
        ) : null}
      </main>
    </div>
  );
}
