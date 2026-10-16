/**
 * Application notice annotations (parity API-117), with Patrol's annotation prefix:
 * notice.cd.patrol.example/{content,severity,url,permanent,icon,scope}.
 */
import type { Application } from '../../../api/types';
import { readJSON, writeJSON } from '../../../lib/storage';
import { GROUP } from './app';

export const NOTICE_PREFIX = `notice.${GROUP}/`;
export const NOTICE_MAX_LENGTH = 500;
export const NOTICE_PREVIEW_LENGTH = 200;
export const DISMISSED_NOTICES_KEY = 'patrol.apps.dismissedNotices';
const MAX_DISMISSED = 200;

export type NoticeSeverity = 'info' | 'warning' | 'critical';
export type NoticeScope = 'banner' | 'icon' | 'both';
export type NoticeIcon = 'info' | 'warning' | 'error';

export interface AppNotice {
  content: string;
  severity: NoticeSeverity;
  url: string | undefined;
  permanent: boolean;
  icon: NoticeIcon;
  scope: NoticeScope;
  /** Dismissal key: "<namespace>/<name>:<hash(content)>". */
  dismissKey: string;
}

// Bidirectional override and isolate characters that could disguise the text.
const BIDI = /[‪-‮⁦-⁩]/g;

/**
 * Icon allow-list. Upstream names (Font Awesome) are accepted for compatibility and mapped to the
 * glyphs Patrol has; anything else falls back to the severity's default icon.
 */
const ICONS: Record<string, NoticeIcon> = {
  info: 'info',
  warning: 'warning',
  error: 'error',
  'fa-info-circle': 'info',
  'fa-exclamation-triangle': 'warning',
  'fa-exclamation-circle': 'error',
};

const DEFAULT_ICON: Record<NoticeSeverity, NoticeIcon> = { info: 'info', warning: 'warning', critical: 'error' };

/** FNV-1a 32-bit hash as 8 hex characters. */
export function hashContent(text: string): string {
  let hash = 0x811c9dc5;
  for (let i = 0; i < text.length; i += 1) {
    hash ^= text.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, '0');
}

function canonicalURL(value: string | undefined): string | undefined {
  if (!value) return undefined;
  try {
    const url = new URL(value.trim());
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : undefined;
  } catch {
    return undefined;
  }
}

export function parseNotice(app: Application): AppNotice | null {
  const annotations = app.metadata.annotations ?? {};
  const get = (key: string) => annotations[`${NOTICE_PREFIX}${key}`];
  const raw = (get('content') ?? '').replace(BIDI, '').trim();
  if (raw === '') return null;
  const content = raw.length > NOTICE_MAX_LENGTH ? `${raw.slice(0, NOTICE_MAX_LENGTH)}…` : raw;
  const severityRaw = (get('severity') ?? '').trim().toLowerCase();
  const severity: NoticeSeverity = severityRaw === 'warning' || severityRaw === 'critical' ? severityRaw : 'info';
  const scopeRaw = (get('scope') ?? '').trim().toLowerCase();
  const scope: NoticeScope = scopeRaw === 'banner' || scopeRaw === 'icon' ? scopeRaw : 'both';
  const iconRaw = (get('icon') ?? '').trim();
  return {
    content,
    severity,
    url: canonicalURL(get('url')),
    permanent: (get('permanent') ?? '').trim().toLowerCase() === 'true',
    icon: ICONS[iconRaw] ?? DEFAULT_ICON[severity],
    scope,
    dismissKey: `${app.metadata.namespace ?? ''}/${app.metadata.name ?? ''}:${hashContent(content)}`,
  };
}

export function noticePreview(notice: AppNotice): string {
  return notice.content.length > NOTICE_PREVIEW_LENGTH ? `${notice.content.slice(0, NOTICE_PREVIEW_LENGTH)}…` : notice.content;
}

export function readDismissedNotices(): string[] {
  const value = readJSON<unknown>(DISMISSED_NOTICES_KEY, []);
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === 'string') : [];
}

/** Records a dismissal; keeps the newest 200 (oldest dropped). */
export function dismissNotice(key: string): void {
  const list = readDismissedNotices().filter((k) => k !== key);
  list.push(key);
  writeJSON(DISMISSED_NOTICES_KEY, list.slice(-MAX_DISMISSED));
}

export function isNoticeDismissed(notice: AppNotice): boolean {
  return !notice.permanent && readDismissedNotices().includes(notice.dismissKey);
}
