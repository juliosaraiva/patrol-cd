import { useQueryClient } from '@tanstack/react-query';
import { useEffect, type ReactNode } from 'react';
import { Navigate, useLocation } from 'react-router';
import { appBaseURL, toRouterPath } from '../api/basePath';
import { setUnauthorizedHandler } from '../api/client';
import { ssoLoginURL } from '../api/endpoints/session';
import { queryKeys } from '../api/queries/keys';
import { useSettings, useUserInfo } from '../api/queries/resources';
import type { Settings, UserInfo } from '../api/types';
import { browser } from '../lib/browser';
import { readStorage, writeStorage } from '../lib/storage';

export const DEFAULT_ROUTE = '/applications';
const SSO_REDIRECT_KEY = 'patrol.ssoRedirectAt';
const SSO_REDIRECT_WINDOW_MS = 5000;

/** True when an OIDC provider or at least one Dex connector is configured. */
export function isSSOConfigured(settings: Settings | undefined): boolean {
  return Boolean(settings?.oidcConfig?.name) || (settings?.dexConfig?.connectors?.length ?? 0) > 0;
}

/** Label for the SSO button, following upstream wording. */
export function ssoButtonLabel(settings: Settings | undefined): string {
  if (settings?.oidcConfig?.name) return `Log in via ${settings.oidcConfig.name}`;
  const connectors = settings?.dexConfig?.connectors ?? [];
  if (connectors.length === 1 && connectors[0]) return `Log in via ${connectors[0].name}`;
  return 'Log in with SSO';
}

/**
 * Anonymous access: the server reports users.anonymous.enabled through the provisional
 * `anonymousEnabled` setting. (Upstream instead infers it from tool versions being visible to a
 * logged-out caller; Patrol relies on the explicit flag.)
 */
export function isAnonymousSession(settings: Settings | undefined, user: UserInfo | undefined): boolean {
  if (user?.loggedIn) return false;
  return settings?.anonymousEnabled === true;
}

/**
 * Router path to resume after login; only same-origin paths under the base are accepted. A path
 * starting with two slashes or backslashes is rejected: react-router renders it as an external
 * (protocol-relative) link, which would make `return_url` an open redirect.
 */
export function safeReturnPath(returnURL: string | null | undefined): string {
  if (!returnURL) return DEFAULT_ROUTE;
  const path = toRouterPath(returnURL);
  if (!path || !path.startsWith('/') || /^[\\/]{2}/.test(path) || path === '/') return DEFAULT_ROUTE;
  if (/^\/(login|logout)(?=[/?#]|$)/.test(path)) return DEFAULT_ROUTE;
  return path;
}

/** Absolute URL of a router path, used as return_url for the server (SSO) and login page. */
export function absoluteAppURL(routerPath: string): string {
  return new URL(routerPath.replace(/^\/+/, ''), appBaseURL()).href;
}

/** Starts SSO unless we already redirected in the last few seconds (avoids redirect loops). */
export function startSSORedirect(returnURL: string): boolean {
  const last = Number(readStorage(SSO_REDIRECT_KEY, 'session') ?? '0');
  if (Date.now() - last < SSO_REDIRECT_WINDOW_MS) return false;
  writeStorage(SSO_REDIRECT_KEY, String(Date.now()), 'session');
  browser.assign(ssoLoginURL(returnURL));
  return true;
}

/**
 * Bridges API 401s into session state: the cached user becomes "logged out", and
 * <RequireSession> redirects to login with the current location as return URL.
 */
export function SessionSync() {
  const queryClient = useQueryClient();
  useEffect(
    () =>
      setUnauthorizedHandler(() => {
        queryClient.setQueryData<UserInfo>(queryKeys.session.userInfo, { loggedIn: false });
      }),
    [queryClient],
  );
  return null;
}

export interface SessionInfo {
  user: UserInfo | undefined;
  settings: Settings | undefined;
  loggedIn: boolean;
  anonymous: boolean;
}

/** Session state for components inside <RequireSession>. */
export function useSession(): SessionInfo {
  const user = useUserInfo().data;
  const settings = useSettings().data;
  return {
    user,
    settings,
    loggedIn: user?.loggedIn === true,
    anonymous: isAnonymousSession(settings, user),
  };
}

function RedirectToLogin({ settings }: { settings: Settings | undefined }) {
  const location = useLocation();
  const returnPath = `${location.pathname}${location.search}${location.hash}`;
  const returnURL = absoluteAppURL(returnPath);
  const sso = isSSOConfigured(settings);
  useEffect(() => {
    if (sso) startSSORedirect(returnURL);
  }, [sso, returnURL]);
  if (sso) return null;
  return <Navigate to={`/login?return_url=${encodeURIComponent(returnURL)}`} replace />;
}

/**
 * Gate for authenticated routes: waits for settings + user info, lets logged-in and anonymous
 * sessions through, and sends everyone else to login (SSO directly when configured).
 */
export function RequireSession({ children, fallback }: { children: ReactNode; fallback: ReactNode }) {
  const settings = useSettings();
  const user = useUserInfo();
  if (user.isPending || settings.isPending) return <>{fallback}</>;
  if (user.isError) throw user.error;
  if (user.data.loggedIn || isAnonymousSession(settings.data, user.data)) return <>{children}</>;
  return <RedirectToLogin settings={settings.data} />;
}
