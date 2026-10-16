/**
 * Pure helpers for the help page (API-109, API-111): CLI downloads from settings
 * help.download.<os>-<arch> (sent as settings.help.binaryUrls) and the chat link.
 */
import type { Settings, Version } from '../../api/types';
import { isHttpOrRelativeURL, isHttpURL } from '../../lib/url';

/** help.download.<os>-<arch> suffixes the server accepts, in upstream order. */
export const DOWNLOAD_PLATFORMS = [
  'darwin-amd64',
  'darwin-arm64',
  'windows-amd64',
  'linux-amd64',
  'linux-arm64',
  'linux-ppc64le',
  'linux-s390x',
] as const;

export type DownloadPlatform = (typeof DOWNLOAD_PLATFORMS)[number];

const OS_LABELS: Record<string, string> = { darwin: 'macOS', windows: 'Windows', linux: 'Linux' };
const ARCH_LABELS: Record<string, string> = {
  amd64: 'x86-64',
  arm64: 'ARM64',
  ppc64le: 'ppc64le',
  s390x: 's390x',
};

export function platformLabel(os: string, arch: string): string {
  return `${OS_LABELS[os] ?? os} (${ARCH_LABELS[arch] ?? arch})`;
}

export interface CLIDownload {
  id: string;
  label: string;
  href: string;
  /** Served by this Patrol server (download/patrol-linux) rather than an operator-provided URL. */
  local: boolean;
}

/** Server architecture from the version's platform ("linux/arm64" → "arm64"). */
export function serverArch(version: Version | undefined): string {
  const platform = version?.platform ?? '';
  const arch = platform.split('/')[1];
  return arch && /^[a-z0-9]+$/.test(arch) ? arch : 'amd64';
}

/**
 * Download entries: the binary served by patrol-server for its own architecture, then one per
 * configured help.download key that is a known platform with an http(s) (or root-relative)
 * URL. Unknown keys and unsafe URLs (javascript:, data:, ...) are dropped.
 */
export function cliDownloads(settings: Settings | undefined, version: Version | undefined): CLIDownload[] {
  const arch = serverArch(version);
  const entries: CLIDownload[] = [
    { id: 'server', label: `${platformLabel('linux', arch)} from this server`, href: 'download/patrol-linux', local: true },
  ];
  const urls = settings?.help?.binaryUrls ?? {};
  for (const platform of DOWNLOAD_PLATFORMS) {
    const href = urls[platform]?.trim();
    if (!href || !isHttpOrRelativeURL(href)) continue;
    const [os = '', a = ''] = platform.split('-');
    entries.push({ id: platform, label: platformLabel(os, a), href, local: false });
  }
  return entries;
}

export interface ChatLink {
  text: string;
  /** Validated http(s) URL, or null when the configured value is not one. */
  href: string | null;
  raw: string;
}

export const DEFAULT_CHAT_TEXT = 'Chat now!';

/**
 * help.chatUrl may be "<url>" or "<title>|<url>"; help.chatText labels the link (default
 * "Chat now!"). Returns null when no chat URL is configured.
 */
export function chatLink(settings: Settings | undefined): ChatLink | null {
  const raw = settings?.help?.chatUrl?.trim() ?? '';
  if (raw === '') return null;
  const sep = raw.indexOf('|');
  const title = sep >= 0 ? raw.slice(0, sep).trim() : '';
  const url = (sep >= 0 ? raw.slice(sep + 1) : raw).trim();
  const configured = settings?.help?.chatText?.trim() ?? '';
  const text = configured !== '' ? configured : title !== '' ? title : DEFAULT_CHAT_TEXT;
  return { text, href: isHttpURL(url) ? url : null, raw };
}

/** Optional documentation site for this distribution, set at build time (VITE_PATROL_DOCS_URL). */
export function documentationURL(): string | null {
  const value: unknown = import.meta.env.VITE_PATROL_DOCS_URL;
  return typeof value === 'string' && isHttpURL(value) ? value.replace(/\/?$/, '/') : null;
}
