import { resolveServerURL } from '../basePath';
import { buildURL, request, seg, segs, type CallOptions, type RequestOptions } from '../client';
import type { NotificationServiceList, NotificationTemplateList, NotificationTriggerList } from '../types';

/** GET /api/v1/notifications/services */
export function listNotificationServices(options: CallOptions = {}): Promise<NotificationServiceList> {
  return request('/api/v1/notifications/services', { signal: options.signal });
}

/** GET /api/v1/notifications/triggers */
export function listNotificationTriggers(options: CallOptions = {}): Promise<NotificationTriggerList> {
  return request('/api/v1/notifications/triggers', { signal: options.signal });
}

/** GET /api/v1/notifications/templates */
export function listNotificationTemplates(options: CallOptions = {}): Promise<NotificationTemplateList> {
  return request('/api/v1/notifications/templates', { signal: options.signal });
}

/**
 * POST /api/webhook[/{provider}] — called by SCM providers, never by the UI, and not part of the
 * published OpenAPI document (a hidden route). `provider` is a provider type as the server
 * names it (github, gitlab, gitea, bitbucket, bitbucketserver, azuredevops, gogs, ghcr; matched
 * case-insensitively); without it the server detects the provider from the headers. The UI only
 * shows the URL to register with the provider.
 */
export function webhookURL(provider?: string): string {
  return resolveServerURL(provider ? `/api/webhook/${seg(provider)}` : '/api/webhook').href;
}

export interface BadgeQuery {
  name?: string;
  appNamespace?: string;
  /** Aggregates every Application of these projects. */
  project?: string | string[];
  revision?: boolean;
  showAppName?: boolean;
  width?: number;
  keepFullRevision?: boolean;
}

/**
 * GET /api/badge — SVG status badge (public when statusbadge.enabled). `rootURL` overrides the
 * origin (settings.statusBadgeRootUrl).
 */
export function badgeURL(query: BadgeQuery, rootURL?: string): string {
  if (rootURL) {
    const url = new URL('api/badge', rootURL.endsWith('/') ? rootURL : `${rootURL}/`);
    for (const [k, v] of Object.entries(query)) {
      for (const item of Array.isArray(v) ? v : [v]) if (item !== undefined && item !== '') url.searchParams.append(k, String(item));
    }
    return url.href;
  }
  return buildURL('/api/badge', { ...query }).href;
}

export interface ExtensionCall extends Omit<RequestOptions, 'headers'> {
  /** Application the call is made for (Patrol-Application-Name: <namespace>:<name>). */
  application?: { namespace: string; name: string };
  /** Project the call is made for (Patrol-Project-Name). */
  project?: string;
  headers?: Readonly<Record<string, string>>;
}

/**
 * ANY /extensions/{extension}/{path} — proxy extension call. RBAC requires
 * `extensions, invoke, <extension>` plus access to the application named in the headers.
 */
export function invokeExtension<T = unknown>(extension: string, path: string, call: ExtensionCall = {}): Promise<T> {
  const headers: Record<string, string> = { ...call.headers };
  if (call.application) headers['Patrol-Application-Name'] = `${call.application.namespace}:${call.application.name}`;
  if (call.project) headers['Patrol-Project-Name'] = call.project;
  const { application: _application, project: _project, ...rest } = call;
  return request<T>(`/extensions/${seg(extension)}/${segs(path.replace(/^\/+/, ''))}`, { ...rest, headers });
}
