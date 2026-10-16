/**
 * Extension host bootstrap (API-120, API-131, API-132): module globals for extension bundles
 * and the script that registers them.
 *
 * Contract (React 19 host, same as Argo CD 3.5):
 * - window.React, window.ReactDOM (the "react-dom" main entry, not "react-dom/client") and
 *   window.ReactJSXRuntime ("react/jsx-runtime") are the host's own module instances.
 *   Extensions must declare them as bundler externals
 *   ({react: 'React', 'react-dom': 'ReactDOM', 'react/jsx-runtime': 'ReactJSXRuntime'}) so
 *   hooks and context work; a bundled pre-19 jsx-runtime throws a TypeError at evaluation.
 *   window.Moment is not provided (Patrol does not ship moment); see ui.md.
 * - window.extensionsAPI and the deprecated window.extensions = {resources: {}} exist before
 *   any extension code runs.
 * - The server serves every `extension*.js` file of its extensions directory concatenated as
 *   `extensions.js` under the root path, each file wrapped in try/catch logging
 *   "Extension <file> failed to load:" (as upstream). The UI loads that script once with a
 *   classic <script> tag; when additional URLs are given each is its own tag, so one failing
 *   bundle never stops the others, and failures are logged with the same prefix.
 * - The server answers an empty script when no extension is installed. A server without the
 *   route (404, e.g. an older server or a dev proxy) or an unreachable one means the same thing
 *   for the default bundle: no extensions. That is not an error and is not logged; failures of
 *   explicitly requested bundles and exceptions while a bundle evaluates are.
 */
import * as React from 'react';
import * as ReactDOM from 'react-dom';
import * as ReactJSXRuntime from 'react/jsx-runtime';
import { appBaseURL } from '../api/basePath';
import { extensionsAPI } from './registry';
import type { ExtensionsAPI, LegacyExtensions } from './types';

declare global {
  interface Window {
    React?: typeof React;
    ReactDOM?: typeof ReactDOM;
    ReactJSXRuntime?: typeof ReactJSXRuntime;
    extensionsAPI?: ExtensionsAPI;
    extensions?: LegacyExtensions;
  }
}

/** Path (relative to the root path) of the concatenated bundle served by patrol-server. */
export const DEFAULT_EXTENSION_BUNDLES: readonly string[] = ['extensions.js'];

export type ExtensionsLoadState = 'idle' | 'loading' | 'done';

export interface ExtensionLoadResult {
  url: string;
  name: string;
  /** `unavailable`: the optional default bundle could not be fetched (no extensions). */
  status: 'loaded' | 'failed' | 'unavailable';
}

/** Defines the globals extensions rely on. Safe to call more than once. */
export function installExtensionGlobals(): void {
  window.React = React;
  window.ReactDOM = ReactDOM;
  window.ReactJSXRuntime = ReactJSXRuntime;
  window.extensionsAPI = extensionsAPI;
  // Deprecated, kept for extensions written against Argo CD < 2.6 (API-126).
  if (!window.extensions || typeof window.extensions !== 'object') window.extensions = { resources: {} };
}

/** Base file name used in log lines ("extensions.js", "extension-metrics.js"). */
export function bundleName(url: string): string {
  try {
    const path = new URL(url, appBaseURL()).pathname;
    return path.slice(path.lastIndexOf('/') + 1) || path;
  } catch {
    return url;
  }
}

function loadOne(src: string, optional: boolean): Promise<ExtensionLoadResult> {
  const url = new URL(src, appBaseURL()).href;
  const name = bundleName(url);
  return new Promise((resolve) => {
    const script = document.createElement('script');
    let evaluationFailed = false;
    // Uncaught exceptions while a separately loaded bundle evaluates surface as window errors
    // whose filename is the script URL.
    const onWindowError = (event: ErrorEvent) => {
      if (event.filename !== url) return;
      evaluationFailed = true;
      event.preventDefault();
      console.error(`Extension ${name} failed to load:`, event.error ?? event.message);
    };
    const finish = (status: ExtensionLoadResult['status']) => {
      window.removeEventListener('error', onWindowError);
      resolve({ url, name, status });
    };
    window.addEventListener('error', onWindowError);
    script.src = url;
    // Keep document order between bundles, like upstream's deferred script.
    script.async = false;
    script.dataset.patrolExtension = name;
    script.addEventListener('load', () => finish(evaluationFailed ? 'failed' : 'loaded'));
    script.addEventListener('error', () => {
      // A script element cannot see the HTTP status: 404, an empty answer from another server or
      // a network failure all mean "no extensions" for the optional default bundle.
      if (optional) {
        finish('unavailable');
        return;
      }
      console.error(`Extension ${name} failed to load:`, new Error(`could not fetch ${url}`));
      finish('failed');
    });
    document.head.appendChild(script);
  });
}

let state: ExtensionsLoadState = 'idle';
let pending: Promise<ExtensionLoadResult[]> | null = null;
const stateListeners = new Set<() => void>();

function setState(next: ExtensionsLoadState) {
  state = next;
  for (const listener of stateListeners) listener();
}

export function extensionsLoadState(): ExtensionsLoadState {
  return state;
}

export function subscribeExtensionsLoadState(listener: () => void): () => void {
  stateListeners.add(listener);
  return () => stateListeners.delete(listener);
}

/** How long a bundle may take before pages stop waiting for it (it can still register later). */
export const EXTENSIONS_LOAD_TIMEOUT_MS = 15_000;

/**
 * Installs the globals and loads the extension bundles once per page. Later calls return the
 * same promise. Never rejects: failures are logged per bundle.
 */
export function loadExtensions(urls: readonly string[] = DEFAULT_EXTENSION_BUNDLES): Promise<ExtensionLoadResult[]> {
  if (pending) return pending;
  installExtensionGlobals();
  setState('loading');
  const timeout = new Promise<null>((resolve) => setTimeout(() => resolve(null), EXTENSIONS_LOAD_TIMEOUT_MS));
  const all = Promise.all(urls.map((u) => loadOne(u, DEFAULT_EXTENSION_BUNDLES.includes(u))));
  void Promise.race([all, timeout]).then(() => setState('done'));
  pending = all;
  return all;
}

/**
 * Entry point for the app bootstrap (main.tsx): call once after the first render is scheduled.
 * Not called under vitest, where jsdom never executes external scripts.
 */
export function initExtensions(): Promise<ExtensionLoadResult[]> {
  return loadExtensions();
}

/** Test helper: forget the load state so loadExtensions() runs again. */
export function resetExtensionsLoader(): void {
  pending = null;
  setState('idle');
  document.head.querySelectorAll('script[data-patrol-extension]').forEach((s) => s.remove());
}
