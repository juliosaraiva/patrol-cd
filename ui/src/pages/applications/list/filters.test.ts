import { describe, expect, it } from 'vitest';
import { guestbookApp, paymentsApp, searchApp } from '../../../mocks/fixtures/apps-core';
import {
  applyFilters,
  dimensionCounts,
  EMPTY_FILTERS,
  filterChips,
  globToRegExp,
  hasActiveFilters,
  matchGlob,
  matchLabelSelector,
  parseFilters,
  parseSort,
  parseView,
  removeChip,
  sortApps,
  writeFilters,
} from './filters';
import { nextTileCursor, digitOf } from './keyboard';

const apps = [guestbookApp(), paymentsApp(), searchApp()];
const none = new Set<string>();

describe('URL filters (API-089)', () => {
  it('round-trips every filter through URL search params with upstream names', () => {
    const filters = {
      ...EMPTY_FILTERS,
      search: 'pay',
      projects: ['finance'],
      sync: ['OutOfSync'],
      health: ['Degraded', 'Missing'],
      operation: ['Failed'],
      labels: ['env in (prod,stage)', 'team=finance'],
      namespaces: ['pay*'],
      clusters: ['prod-*'],
      repos: ['https://charts.example.test'],
      annotations: ['owner'],
      targetRevisions: ['release-*'],
      autoSync: ['Disabled' as const],
      favorites: true,
    };
    const params = writeFilters(new URLSearchParams('view=list&page=2'), filters);
    expect(params.get('view')).toBe('list');
    expect(params.get('proj')).toBe('finance');
    expect(params.get('labels')).toBe('env%20in%20(prod%2Cstage),team%3Dfinance');
    expect(params.get('showFavorites')).toBe('true');
    expect(params.get('annotations')).toBe('owner');
    expect(params.get('targetRevision')).toBe('release-*');
    expect(parseFilters(params)).toEqual(filters);
    expect(writeFilters(params, EMPTY_FILTERS).toString()).toBe('view=list&page=2');
  });

  it('ignores unknown auto-sync values and reports active filters', () => {
    expect(parseFilters(new URLSearchParams('autoSync=Maybe,Enabled')).autoSync).toEqual(['Enabled']);
    expect(hasActiveFilters(EMPTY_FILTERS)).toBe(false);
    expect(hasActiveFilters({ ...EMPTY_FILTERS, favorites: true })).toBe(true);
  });

  it('builds and removes chips', () => {
    const filters = { ...EMPTY_FILTERS, search: 'x', sync: ['OutOfSync', 'Synced'] };
    const chips = filterChips(filters);
    expect(chips.map((c) => c.label)).toEqual(['Search: x', 'Sync: Out of sync', 'Sync: Synced']);
    const second = chips[1];
    expect(second && removeChip(filters, second.id).sync).toEqual(['Synced']);
    expect(removeChip(filters, 'search').search).toBe('');
  });

  it('parses view and sort with defaults', () => {
    expect(parseView(null)).toBe('tiles');
    expect(parseView('summary')).toBe('summary');
    expect(parseSort('synced')).toBe('synced');
    expect(parseSort('nope')).toBe('name');
  });
});

describe('matching', () => {
  it('matches globs on whole values', () => {
    expect(matchGlob('prod-eu', 'prod-*')).toBe(true);
    expect(matchGlob('preprod-eu', 'prod-*')).toBe(false);
    expect(matchGlob('a.b', 'a?b')).toBe(true);
    expect(globToRegExp('a.b').test('axb')).toBe(false);
  });

  it('matches label selector expressions', () => {
    const labels = { env: 'prod', team: 'web' };
    expect(matchLabelSelector(labels, 'env')).toBe(true);
    expect(matchLabelSelector(labels, '!tier')).toBe(true);
    expect(matchLabelSelector(labels, 'env=prod')).toBe(true);
    expect(matchLabelSelector(labels, 'env==prod')).toBe(true);
    expect(matchLabelSelector(labels, 'env!=prod')).toBe(false);
    expect(matchLabelSelector(labels, 'env in (stage, prod)')).toBe(true);
    expect(matchLabelSelector(labels, 'team notin (web)')).toBe(false);
    expect(matchLabelSelector(labels, 'env=prod,team=ops')).toBe(false);
  });

  it('filters by every dimension, glob dimensions included', () => {
    const names = (f: Partial<typeof EMPTY_FILTERS>) => applyFilters(apps, { ...EMPTY_FILTERS, ...f }, none).map((a) => a.metadata.name);
    expect(names({ search: 'GUEST' })).toEqual(['guestbook']);
    expect(names({ search: 'patrol' })).toHaveLength(3);
    expect(names({ projects: ['finance'] })).toEqual(['payments']);
    expect(names({ sync: ['OutOfSync'] })).toEqual(['payments']);
    expect(names({ health: ['Progressing', 'Healthy'] })).toEqual(['guestbook', 'search']);
    expect(names({ operation: ['Running'] })).toEqual(['search']);
    expect(names({ labels: ['team=web'] })).toEqual(['guestbook']);
    expect(names({ namespaces: ['pay*'] })).toEqual(['payments']);
    expect(names({ clusters: ['prod-*'] })).toEqual(['payments']);
    expect(names({ clusters: ['https://kubernetes.default.svc'] })).toEqual(['guestbook', 'search']);
    expect(names({ repos: ['https://charts.*'] })).toEqual(['payments']);
    expect(names({ autoSync: ['Enabled'] })).toEqual(['guestbook']);
    expect(names({ targetRevisions: ['1.4.*'] })).toEqual(['payments']);
    expect(names({ targetRevisions: ['main'] })).toEqual(['guestbook', 'payments', 'search']);
    expect(names({ annotations: ['notice.cd.patrol.example/severity=warning'] })).toEqual(['payments']);
    expect(applyFilters(apps, { ...EMPTY_FILTERS, favorites: true }, new Set(['patrol/search'])).map((a) => a.metadata.name)).toEqual(['search']);
  });

  it('counts options against the other active filters', () => {
    const filters = { ...EMPTY_FILTERS, sync: ['Synced'], projects: ['default'] };
    const sync = dimensionCounts(apps, filters, none, 'sync');
    expect(sync.get('Synced')).toBe(2);
    expect(sync.get('OutOfSync')).toBeUndefined();
    const projects = dimensionCounts(apps, filters, none, 'projects');
    expect(projects.get('default')).toBe(2);
    expect(projects.get('finance')).toBeUndefined();
    expect(dimensionCounts(apps, EMPTY_FILTERS, none, 'clusters').get('prod-eu')).toBe(1);
  });

  it('sorts by name, creation and last sync', () => {
    const older = { ...searchApp(), metadata: { ...searchApp().metadata, name: 'app10', creationTimestamp: '2026-01-01T00:00:00Z' } };
    const newer = { ...searchApp(), metadata: { ...searchApp().metadata, name: 'app9', creationTimestamp: '2026-09-01T00:00:00Z' } };
    expect(sortApps([older, newer], 'name').map((a) => a.metadata.name)).toEqual(['app9', 'app10']);
    expect(sortApps([older, newer], 'created').map((a) => a.metadata.name)).toEqual(['app9', 'app10']);
    expect(sortApps(apps, 'synced').map((a) => a.metadata.name)).toEqual(['payments', 'guestbook', 'search']);
  });
});

describe('tile keyboard cursor (API-134)', () => {
  it('moves within bounds without wrapping', () => {
    expect(nextTileCursor('ArrowRight', -1, 6, 3)).toBe(0);
    expect(nextTileCursor('ArrowRight', 0, 6, 3)).toBe(1);
    expect(nextTileCursor('ArrowDown', 2, 6, 3)).toBe(5);
    expect(nextTileCursor('ArrowDown', 4, 6, 3)).toBe(4);
    expect(nextTileCursor('ArrowLeft', 0, 6, 3)).toBe(0);
    expect(nextTileCursor('ArrowLeft', -1, 6, 3)).toBeNull();
    expect(nextTileCursor('ArrowUp', 4, 6, 3)).toBe(1);
    expect(nextTileCursor('Tab', 0, 6, 3)).toBeNull();
  });

  it('reads digits from the top row and the numpad', () => {
    expect(digitOf(new KeyboardEvent('keydown', { key: '3', code: 'Digit3' }))).toBe(3);
    expect(digitOf(new KeyboardEvent('keydown', { key: 'End', code: 'Numpad1' }))).toBe(1);
    expect(digitOf(new KeyboardEvent('keydown', { key: 'a', code: 'KeyA' }))).toBeNull();
  });
});
