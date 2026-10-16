import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  addExtensionListener,
  getActionMenuExtensions,
  getAppViewExtensions,
  getResourceTabs,
  getStatusPanelExtensions,
  getSystemExtensions,
  registerAppViewExtension,
  registerResourceExtension,
  registerStatusPanelExtension,
  registerSystemLevelExtension,
  registerTopBarActionMenuExt,
  resetExtensionRegistry,
} from './registry';
import { installExtensionGlobals } from './loader';
import type { Application } from '../api/types';
import { applicationFixture } from '../mocks/fixtures';

const A = () => null;
const B = () => null;

afterEach(() => {
  resetExtensionRegistry();
  delete window.extensions;
  delete window.extensionsAPI;
});

describe('extension registry', () => {
  it('matches resource tabs by group and kind globs and sorts them by title', () => {
    registerResourceExtension(A, '**', '*', 'Zeta');
    registerResourceExtension(B, 'apps', 'Deployment', 'Alpha', { icon: 'fa-flask' });
    registerResourceExtension(A, '*', 'Pod', 'Pods only');

    expect(getResourceTabs('apps', 'Deployment').map((t) => t.title)).toEqual(['Alpha', 'Zeta']);
    // "**" also matches the empty core group; "*" does not.
    expect(getResourceTabs('', 'Pod').map((t) => t.title)).toEqual(['Zeta']);
    expect(getResourceTabs('batch', 'Pod').map((t) => t.title)).toEqual(['Pods only', 'Zeta']);
    expect(getResourceTabs('apps', 'Deployment')[0]?.icon).toBe('fa-flask');
  });

  it('keeps every registration type with upstream defaults', () => {
    registerSystemLevelExtension(A, 'Metrics', 'metrics/', 'fa-chart');
    registerAppViewExtension(A, 'Cost', 'fa-dollar');
    registerStatusPanelExtension(A, 'Rollout', 'rollout', B);
    registerTopBarActionMenuExt(A, 'Promote', 'promote', B);

    expect(getSystemExtensions()).toEqual([{ component: A, title: 'Metrics', path: '/metrics', icon: 'fa-chart' }]);
    const app: Application = applicationFixture('guestbook');
    expect(getAppViewExtensions()[0]?.shouldDisplay(app)).toBe(true);
    expect(getStatusPanelExtensions()[0]).toMatchObject({ title: 'Rollout', id: 'rollout', flyout: B });
    const top = getActionMenuExtensions()[0];
    expect(top).toMatchObject({ title: 'Promote', id: 'promote', isMiddle: false });
    expect(top?.shouldDisplay(app)).toBe(true);
  });

  it('notifies typed listeners of registrations made after they subscribed', () => {
    const seen: string[] = [];
    const stop = addExtensionListener('systemLevel', (ext) => seen.push(ext.title));
    registerSystemLevelExtension(A, 'One', '/one');
    stop();
    registerSystemLevelExtension(A, 'Two', '/two');
    expect(seen).toEqual(['One']);
  });

  it('ignores malformed registrations with a console error instead of throwing', () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    registerResourceExtension('not a component' as never, '*', '*', 'Broken');
    registerSystemLevelExtension(A, 'No path', '');
    registerStatusPanelExtension(A, 'No id', '');
    expect(getResourceTabs('apps', 'Deployment')).toEqual([]);
    expect(getSystemExtensions()).toEqual([]);
    expect(getStatusPanelExtensions()).toEqual([]);
    expect(error).toHaveBeenCalledTimes(3);
    expect(error.mock.calls[0]?.[0]).toMatch(/^Extension registration ignored: registerResourceExtension/);
  });

  it('converts legacy window.extensions.resources entries into "More" tabs on first lookup', () => {
    installExtensionGlobals();
    window.extensions!.resources['apps/Deployment'] = { component: A };
    const tabs = getResourceTabs('apps', 'Deployment');
    expect(tabs).toHaveLength(1);
    expect(tabs[0]).toMatchObject({ title: 'More', group: 'apps', kind: 'Deployment', component: A });
    // Converted once, as upstream.
    expect(getResourceTabs('apps', 'Deployment')).toHaveLength(1);
  });

  it('exposes the same functions on window.extensionsAPI', () => {
    installExtensionGlobals();
    window.extensionsAPI?.registerResourceExtension(A, '*', '*', 'Via window');
    expect(getResourceTabs('apps', 'Deployment').map((t) => t.title)).toEqual(['Via window']);
  });
});
