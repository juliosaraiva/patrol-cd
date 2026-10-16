/** External URLs of an application (parity API-115, API-116), validated before rendering. */
import type { Application } from '../../../api/types';
import { isHttpURL } from '../../../lib/url';
import { ANNOTATION_LINK_PREFIX, ANNOTATION_MANAGED_BY_URL } from './app';

export interface ExternalLink {
  title: string | undefined;
  url: string;
}

/** Parses "URL" or "Title|URL"; returns null unless the URL is http(s). */
export function parseLinkValue(value: string): ExternalLink | null {
  const trimmed = value.trim();
  const bar = trimmed.indexOf('|');
  const title = bar >= 0 ? trimmed.slice(0, bar).trim() : undefined;
  const url = (bar >= 0 ? trimmed.slice(bar + 1) : trimmed).trim();
  if (!isHttpURL(url)) return null;
  return { title: title === '' ? undefined : title, url };
}

/** Titled links first (by title), then untitled ones (by URL); duplicates removed. */
export function sortLinks(links: readonly ExternalLink[]): ExternalLink[] {
  const seen = new Set<string>();
  const unique = links.filter((l) => {
    const key = `${l.title ?? ''}|${l.url}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  return unique.sort((a, b) => {
    if (a.title && !b.title) return -1;
    if (!a.title && b.title) return 1;
    return (a.title ?? a.url).localeCompare(b.title ?? b.url);
  });
}

/** status.summary.externalURLs plus link.cd.patrol.example/* annotations on the Application. */
export function applicationExternalLinks(app: Application): ExternalLink[] {
  const links: ExternalLink[] = [];
  for (const value of app.status?.summary?.externalURLs ?? []) {
    const link = parseLinkValue(value);
    if (link) links.push(link);
  }
  for (const [key, value] of Object.entries(app.metadata.annotations ?? {})) {
    if (!key.startsWith(ANNOTATION_LINK_PREFIX)) continue;
    const link = parseLinkValue(value);
    if (link) links.push(link);
  }
  return sortLinks(links);
}

export type ManagedByURL = { kind: 'none' } | { kind: 'valid'; url: string } | { kind: 'invalid'; value: string };

/** cd.patrol.example/managed-by-url: another Patrol instance manages this application. */
export function managedByURL(app: Application): ManagedByURL {
  const value = app.metadata.annotations?.[ANNOTATION_MANAGED_BY_URL]?.trim();
  if (!value) return { kind: 'none' };
  return isHttpURL(value) ? { kind: 'valid', url: value } : { kind: 'invalid', value };
}

/** Opens an external URL in a new tab without giving it access to this window. */
export function openExternal(url: string): void {
  if (!isHttpURL(url)) return;
  window.open(url, '_blank', 'noopener,noreferrer');
}
