import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Application } from '../../../api/types';
import { guestbookApp, paymentsApp, searchApp } from '../../../mocks/fixtures/apps-core';
import {
  activeConditions,
  appDetailsPath,
  appSources,
  commitURL,
  compareURL,
  durationBetween,
  formatDuration,
  initiatorLabel,
  isAutoSyncEnabled,
  repoWebURL,
  resourceKey,
  shortRevision,
  sourceKind,
  syncedRevisions,
} from './app';
import { describeActionError } from './errors';
import { applicationExternalLinks, managedByURL, openExternal, parseLinkValue, sortLinks } from './links';
import { DISMISSED_NOTICES_KEY, dismissNotice, isNoticeDismissed, noticePreview, parseNotice, readDismissedNotices } from './notice';
import { ApiError } from '../../../api/errors';

function withAnnotations(app: Application, annotations: Record<string, string>): Application {
  return { ...app, metadata: { ...app.metadata, annotations } };
}

describe('application helpers', () => {
  it('builds namespaced detail paths', () => {
    expect(appDetailsPath(guestbookApp())).toBe('/applications/patrol/guestbook');
    expect(appDetailsPath(guestbookApp(), 'panel=sync')).toBe('/applications/patrol/guestbook?panel=sync');
    const bare = { ...guestbookApp(), metadata: { name: 'a b' } };
    expect(appDetailsPath(bare)).toBe('/applications/a%20b');
  });

  it('normalizes repository URLs and builds commit and compare links', () => {
    expect(repoWebURL('git@github.com:example/guestbook.git')).toBe('https://github.com/example/guestbook');
    expect(repoWebURL('ssh://git@gitlab.example.test/team/app.git')).toBe('https://gitlab.example.test/team/app');
    expect(repoWebURL('https://github.com/example/guestbook.git/')).toBe('https://github.com/example/guestbook');
    expect(repoWebURL('javascript:alert(1)')).toBeNull();
    const sha = 'a'.repeat(40);
    expect(commitURL('https://github.com/example/guestbook.git', sha)).toBe(`https://github.com/example/guestbook/commit/${sha}`);
    expect(commitURL('https://gitlab.example.test/team/app', sha)).toBe(`https://gitlab.example.test/team/app/-/commit/${sha}`);
    expect(commitURL('https://bitbucket.org/team/app', sha)).toBe(`https://bitbucket.org/team/app/commits/${sha}`);
    expect(commitURL('https://github.com/example/guestbook.git', 'main')).toBeNull();
    expect(compareURL('https://github.com/example/guestbook.git', 'abc', 'def')).toBe('https://github.com/example/guestbook/compare/abc...def');
    expect(compareURL('https://github.com/example/guestbook.git', 'abc', 'abc')).toBeNull();
  });

  it('shortens commit SHAs only', () => {
    expect(shortRevision('4f2c1a9d3e7b5c6a8f9e0d1c2b3a4f5e6d7c8b9a')).toBe('4f2c1a9');
    expect(shortRevision('v1.2.3')).toBe('v1.2.3');
    expect(shortRevision(undefined)).toBe('');
  });

  it('reads sources, revisions and auto-sync', () => {
    expect(appSources(paymentsApp())).toHaveLength(2);
    expect(appSources(guestbookApp())).toHaveLength(1);
    expect(syncedRevisions(paymentsApp())[1]).toMatch(/^1b2c/);
    expect(isAutoSyncEnabled(guestbookApp())).toBe(true);
    expect(isAutoSyncEnabled(searchApp())).toBe(false);
    const disabled = guestbookApp();
    if (disabled.spec.syncPolicy?.automated) disabled.spec.syncPolicy.automated.enabled = false;
    expect(isAutoSyncEnabled(disabled)).toBe(false);
    expect(sourceKind(paymentsApp().spec.sources?.[0] ?? { repoURL: '' })).toBe('Helm');
    expect(sourceKind(searchApp().spec.source ?? { repoURL: '' })).toBe('Kustomize');
    expect(sourceKind({ repoURL: 'x', ref: 'values' })).toBe('Reference');
  });

  it('orders active conditions errors first and ignores False ones', () => {
    const app = paymentsApp();
    app.status?.conditions?.push({ type: 'OrphanedResourceWarning', status: 'False', reason: 'Resolved', message: 'x', lastTransitionTime: '2026-10-16T00:00:00Z' });
    const types = activeConditions(app).map((c) => c.type);
    expect(types).toEqual(['ComparisonError', 'SharedResourceWarning']);
  });

  it('formats durations and initiators', () => {
    expect(formatDuration(45_000)).toBe('45s');
    expect(formatDuration(192_000)).toBe('3m 12s');
    expect(formatDuration(7_500_000)).toBe('2h 5m');
    expect(durationBetween('2026-10-15T07:58:00Z', '2026-10-15T07:59:30Z')).toBe('1m 30s');
    expect(initiatorLabel({ automated: true })).toBe('Automated sync');
    expect(initiatorLabel({ username: 'dana' })).toBe('dana');
    expect(resourceKey({ kind: 'Service', namespace: 'ns', name: 'svc' })).toBe('/Service/ns/svc');
  });
});

describe('action errors', () => {
  it('maps API error codes to guidance and recovery', () => {
    expect(describeActionError(new ApiError({ status: 409, code: 'conflict', detail: 'modified' }), 'save').recovery).toBe('reload');
    const precondition = describeActionError(new ApiError({ status: 412, code: 'failed_precondition', detail: 'sync window' }), 'sync');
    expect(precondition.title).toBe('Cannot sync right now');
    expect(precondition.nextStep).toMatch(/sync window/);
    expect(describeActionError(new ApiError({ status: 403, code: 'permission_denied', detail: 'no' }), 'delete').nextStep).toMatch(/RBAC/);
    expect(describeActionError(new ApiError({ status: 503 }), 'refresh').recovery).toBe('retry');
    expect(describeActionError(new Error('boom'), 'sync').message).toBe('boom');
  });
});

describe('notices (API-117)', () => {
  afterEach(() => window.localStorage.clear());

  it('requires content, strips bidi controls and truncates', () => {
    expect(parseNotice(guestbookApp())).toBeNull();
    expect(parseNotice(withAnnotations(guestbookApp(), { 'notice.cd.patrol.example/content': '   ' }))).toBeNull();
    const notice = parseNotice(withAnnotations(guestbookApp(), { 'notice.cd.patrol.example/content': ' a‮b ' }));
    expect(notice?.content).toBe('ab');
    const long = parseNotice(withAnnotations(guestbookApp(), { 'notice.cd.patrol.example/content': 'x'.repeat(600) }));
    expect(long?.content).toHaveLength(501);
    expect(long?.content.endsWith('…')).toBe(true);
    expect(long ? noticePreview(long) : '').toHaveLength(201);
  });

  it('parses severity, url, permanent, icon and scope with safe defaults', () => {
    const base = { 'notice.cd.patrol.example/content': 'Maintenance' };
    const n = (extra: Record<string, string>) => parseNotice(withAnnotations(guestbookApp(), { ...base, ...extra }));
    expect(n({})).toMatchObject({ severity: 'info', icon: 'info', scope: 'both', permanent: false, url: undefined });
    expect(n({ 'notice.cd.patrol.example/severity': 'critical' })).toMatchObject({ severity: 'critical', icon: 'error' });
    expect(n({ 'notice.cd.patrol.example/severity': 'bogus' })?.severity).toBe('info');
    expect(n({ 'notice.cd.patrol.example/url': 'javascript:alert(1)' })?.url).toBeUndefined();
    expect(n({ 'notice.cd.patrol.example/url': 'https://status.example.test/a b' })?.url).toBe('https://status.example.test/a%20b');
    expect(n({ 'notice.cd.patrol.example/permanent': 'true' })?.permanent).toBe(true);
    expect(n({ 'notice.cd.patrol.example/icon': 'fa-exclamation-triangle' })?.icon).toBe('warning');
    expect(n({ 'notice.cd.patrol.example/icon': 'fa-rocket' })?.icon).toBe('info');
    expect(n({ 'notice.cd.patrol.example/scope': 'icon' })?.scope).toBe('icon');
  });

  it('remembers dismissals per content and keeps at most 200', () => {
    const notice = parseNotice(withAnnotations(guestbookApp(), { 'notice.cd.patrol.example/content': 'Freeze' }));
    if (!notice) throw new Error('notice expected');
    expect(isNoticeDismissed(notice)).toBe(false);
    dismissNotice(notice.dismissKey);
    expect(isNoticeDismissed(notice)).toBe(true);
    const changed = parseNotice(withAnnotations(guestbookApp(), { 'notice.cd.patrol.example/content': 'Freeze extended' }));
    expect(changed && isNoticeDismissed(changed)).toBe(false);
    for (let i = 0; i < 250; i += 1) dismissNotice(`ns/app:${i}`);
    expect(readDismissedNotices()).toHaveLength(200);
    expect(readDismissedNotices()[199]).toBe('ns/app:249');
    window.localStorage.setItem(DISMISSED_NOTICES_KEY, '{broken');
    expect(readDismissedNotices()).toEqual([]);
  });
});

describe('external links (API-115, API-116)', () => {
  it('parses titled links and rejects unsafe schemes', () => {
    expect(parseLinkValue('Docs|https://docs.example.test')).toEqual({ title: 'Docs', url: 'https://docs.example.test' });
    expect(parseLinkValue('https://a.example.test')).toEqual({ title: undefined, url: 'https://a.example.test' });
    expect(parseLinkValue('x|javascript:alert(1)')).toBeNull();
    expect(sortLinks([{ title: undefined, url: 'https://b' }, { title: 'Zed', url: 'https://z' }, { title: 'Alpha', url: 'https://a' }]).map((l) => l.title ?? l.url)).toEqual([
      'Alpha',
      'Zed',
      'https://b',
    ]);
  });

  it('merges summary URLs and link annotations', () => {
    expect(applicationExternalLinks(guestbookApp())).toEqual([
      { title: 'Dashboard', url: 'https://grafana.example.test/d/guestbook' },
      { title: undefined, url: 'https://guestbook.example.test' },
    ]);
  });

  it('validates the managed-by URL', () => {
    expect(managedByURL(guestbookApp())).toEqual({ kind: 'none' });
    expect(managedByURL(withAnnotations(guestbookApp(), { 'cd.patrol.example/managed-by-url': 'https://other.example.test' }))).toEqual({
      kind: 'valid',
      url: 'https://other.example.test',
    });
    expect(managedByURL(withAnnotations(guestbookApp(), { 'cd.patrol.example/managed-by-url': 'ftp://x' })).kind).toBe('invalid');
  });

  it('opens only http(s) URLs, without opener access', () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    openExternal('javascript:alert(1)');
    expect(open).not.toHaveBeenCalled();
    openExternal('https://a.example.test');
    expect(open).toHaveBeenCalledWith('https://a.example.test', '_blank', 'noopener,noreferrer');
  });
});
