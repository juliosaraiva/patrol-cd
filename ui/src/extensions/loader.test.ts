import { afterEach, describe, expect, it, vi } from 'vitest';
import { bundleName, extensionsLoadState, installExtensionGlobals, loadExtensions, resetExtensionsLoader } from './loader';
import { getResourceTabs, resetExtensionRegistry } from './registry';

afterEach(() => {
  resetExtensionsLoader();
  resetExtensionRegistry();
  delete window.extensions;
  delete window.extensionsAPI;
  for (const key of ['React', 'ReactDOM', 'ReactJSXRuntime']) Reflect.deleteProperty(window, key);
});

function scripts(): HTMLScriptElement[] {
  return Array.from(document.head.querySelectorAll<HTMLScriptElement>('script[data-patrol-extension]'));
}

describe('extension host globals (API-131, API-132)', () => {
  it('exposes the host React 19 instance, react-dom and the automatic JSX runtime', () => {
    installExtensionGlobals();
    expect(window.React?.version.startsWith('19.')).toBe(true);
    expect(typeof window.React?.createElement).toBe('function');
    expect(typeof window.ReactDOM?.createPortal).toBe('function');
    expect(typeof window.ReactJSXRuntime?.jsx).toBe('function');
    expect(typeof window.ReactJSXRuntime?.jsxs).toBe('function');
    expect(window.ReactJSXRuntime?.Fragment).toBeDefined();
    expect(Object.keys(window.extensionsAPI ?? {}).sort()).toEqual([
      'registerAppViewExtension',
      'registerResourceExtension',
      'registerStatusPanelExtension',
      'registerSystemLevelExtension',
      'registerTopBarActionMenuExt',
    ]);
    expect(window.extensions).toEqual({ resources: {} });
  });

  it('keeps an existing legacy window.extensions object', () => {
    const legacy = { resources: {} };
    window.extensions = legacy;
    installExtensionGlobals();
    expect(window.extensions).toBe(legacy);
  });

  it('lets extension code that only uses the globals register at top level', () => {
    installExtensionGlobals();
    // What a bundle built with externals {react: 'React'} does when it evaluates.
    const bundle = () => {
      const React = window.React;
      const component = () => React.createElement('div', null, 'Hello');
      window.extensionsAPI!.registerResourceExtension(component, '*', '*', 'Nice extension');
    };
    expect(bundle).not.toThrow();
    expect(getResourceTabs('apps', 'Deployment').map((t) => t.title)).toEqual(['Nice extension']);
  });
});

describe('loadExtensions', () => {
  it('loads extensions.js from the root path once, in document order', async () => {
    const pending = loadExtensions();
    expect(loadExtensions()).toBe(pending);
    expect(extensionsLoadState()).toBe('loading');
    const [script] = scripts();
    expect(script?.src).toBe(new URL('extensions.js', document.baseURI).href);
    expect(script?.async).toBe(false);
    script?.dispatchEvent(new Event('load'));
    await expect(pending).resolves.toEqual([{ url: script?.src, name: 'extensions.js', status: 'loaded' }]);
    await vi.waitFor(() => expect(extensionsLoadState()).toBe('done'));
  });

  it('treats a missing or unreachable extensions.js (404, no route) as no extensions, without console errors', async () => {
    const error = vi.spyOn(console, 'error');
    const warn = vi.spyOn(console, 'warn');
    const pending = loadExtensions();
    const [script] = scripts();
    script?.dispatchEvent(new Event('error'));
    await expect(pending).resolves.toEqual([{ url: script?.src, name: 'extensions.js', status: 'unavailable' }]);
    await vi.waitFor(() => expect(extensionsLoadState()).toBe('done'));
    expect(error).not.toHaveBeenCalled();
    expect(warn).not.toHaveBeenCalled();
    // The globals stay installed, so a later page still renders its slots.
    expect(typeof window.extensionsAPI?.registerResourceExtension).toBe('function');
  });

  it('accepts the empty bundle the server sends when no extension is installed', async () => {
    const error = vi.spyOn(console, 'error');
    const pending = loadExtensions();
    scripts()[0]?.dispatchEvent(new Event('load'));
    expect((await pending).map((r) => r.status)).toEqual(['loaded']);
    expect(error).not.toHaveBeenCalled();
  });

  it('logs "Extension <name> failed to load:" when a bundle cannot be fetched and keeps loading the others', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const pending = loadExtensions(['extensions/extension-a.js', 'extensions/extension-b.js']);
    const [a, b] = scripts();
    a?.dispatchEvent(new Event('error'));
    b?.dispatchEvent(new Event('load'));
    const results = await pending;
    expect(results.map((r) => [r.name, r.status])).toEqual([
      ['extension-a.js', 'failed'],
      ['extension-b.js', 'loaded'],
    ]);
    expect(error).toHaveBeenCalledWith('Extension extension-a.js failed to load:', expect.any(Error));
  });

  it('isolates an exception thrown while one bundle evaluates', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const pending = loadExtensions(['extensions/extension-a.js', 'extensions/extension-b.js']);
    const [a, b] = scripts();
    const failure = new TypeError("Cannot read properties of undefined (reading 'ReactCurrentOwner')");
    window.dispatchEvent(new ErrorEvent('error', { error: failure, filename: a?.src, cancelable: true }));
    a?.dispatchEvent(new Event('load'));
    b?.dispatchEvent(new Event('load'));
    const results = await pending;
    expect(results.map((r) => r.status)).toEqual(['failed', 'loaded']);
    expect(error).toHaveBeenCalledWith('Extension extension-a.js failed to load:', failure);
  });

  it('names bundles by their base file name', () => {
    expect(bundleName('extensions.js')).toBe('extensions.js');
    expect(bundleName('https://patrol.example.test/root/extensions/extension-x.js')).toBe('extension-x.js');
  });
});
