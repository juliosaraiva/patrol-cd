import { resolveServerURL } from '../basePath';
import { request, seg, segs, type CallOptions } from '../client';
import type {
  Account,
  AccountList,
  CanIResponse,
  CreateAccountTokenRequest,
  LogoutResponse,
  PluginSettings,
  SessionCreateRequest,
  SessionResponse,
  Settings,
  TokenResponse,
  UpdatePasswordRequest,
  UserInfo,
  Version,
} from '../types';

/** POST /api/v1/session — local login. `cookie` makes the server set the session cookie. */
export function createSession(
  credentials: SessionCreateRequest,
  options: { cookie?: boolean } & CallOptions = {},
): Promise<SessionResponse> {
  return request('/api/v1/session', {
    method: 'POST',
    query: { cookie: options.cookie ?? true },
    body: credentials,
    authRedirect: false,
    signal: options.signal,
  });
}

/** DELETE /api/v1/session — logout; may return the OIDC end-session URL. */
export async function deleteSession(options: CallOptions = {}): Promise<LogoutResponse> {
  const res = await request<LogoutResponse | undefined>('/api/v1/session', {
    method: 'DELETE',
    authRedirect: false,
    signal: options.signal,
  });
  return res ?? {};
}

/** GET /api/v1/session/userinfo — never redirects; a 401 means "logged out". */
export function getUserInfo(options: CallOptions = {}): Promise<UserInfo> {
  return request('/api/v1/session/userinfo', { authRedirect: false, signal: options.signal });
}

/**
 * GET /api/v1/auth/login — browser navigation target that starts the OIDC flow. Returns the URL
 * to assign to window.location; it is not fetched.
 */
export function ssoLoginURL(returnURL: string): string {
  const url = resolveServerURL('/api/v1/auth/login');
  url.searchParams.set('return_url', returnURL);
  return url.href;
}

/** GET /api/v1/account */
export function listAccounts(options: CallOptions = {}): Promise<AccountList> {
  return request('/api/v1/account', { signal: options.signal });
}

/** GET /api/v1/account/{name} */
export function getAccount(name: string, options: CallOptions = {}): Promise<Account> {
  return request(`/api/v1/account/${seg(name)}`, { signal: options.signal });
}

/**
 * GET /api/v1/account/can-i/{resource}/{action}/{subresource...} — e.g.
 * canI('applications', 'sync', 'default/guestbook').
 */
export function canI(resource: string, action: string, subresource: string, options: CallOptions = {}): Promise<CanIResponse> {
  return request(`/api/v1/account/can-i/${seg(resource)}/${seg(action)}/${segs(subresource || '*')}`, {
    signal: options.signal,
  });
}

/** PUT /api/v1/account/password */
export async function updatePassword(body: UpdatePasswordRequest, options: CallOptions = {}): Promise<void> {
  await request('/api/v1/account/password', { method: 'PUT', body, signal: options.signal });
}

/** POST /api/v1/account/{name}/token */
export function createAccountToken(name: string, body: CreateAccountTokenRequest, options: CallOptions = {}): Promise<TokenResponse> {
  return request(`/api/v1/account/${seg(name)}/token`, { method: 'POST', body, signal: options.signal });
}

/** DELETE /api/v1/account/{name}/token/{id} */
export async function deleteAccountToken(name: string, id: string, options: CallOptions = {}): Promise<void> {
  await request(`/api/v1/account/${seg(name)}/token/${seg(id)}`, { method: 'DELETE', signal: options.signal });
}

/** GET /api/v1/settings — public subset when unauthenticated, so it never redirects. */
export function getSettings(options: CallOptions = {}): Promise<Settings> {
  return request('/api/v1/settings', { authRedirect: false, signal: options.signal });
}

/** GET /api/v1/settings/plugins */
export function getPluginSettings(options: CallOptions = {}): Promise<PluginSettings> {
  return request('/api/v1/settings/plugins', { signal: options.signal });
}

/** GET /api/v1/version */
export function getVersion(options: CallOptions = {}): Promise<Version> {
  return request('/api/v1/version', { authRedirect: false, signal: options.signal });
}
