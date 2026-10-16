/**
 * In-memory registry behind window.extensionsAPI (API-120..126). Registrations are kept in
 * arrays (in registration order) and announced to listeners, so slots rendered before an
 * extension script ran update when it registers. The registry is per page load: nothing is
 * persisted.
 */
import type { Application } from '../api/types';
import { globMatch } from './glob';
import type {
  AppViewExtension,
  AppViewExtensionComponent,
  ExtensionComponent,
  ExtensionEventType,
  ExtensionsAPI,
  ExtensionsByType,
  FlyoutComponent,
  LegacyExtensions,
  ResourceTabExtension,
  StatusPanelExtension,
  StatusPanelExtensionComponent,
  SystemExtensionComponent,
  SystemLevelExtension,
  TopBarActionMenuExt,
  TopBarActionMenuExtComponent,
} from './types';

type Store = { [K in ExtensionEventType]: ExtensionsByType[K][] };

const store: Store = { resource: [], systemLevel: [], appView: [], statusPanel: [], topBar: [] };
const typedListeners = new Map<ExtensionEventType, Set<(ext: never) => void>>();
const changeListeners = new Set<() => void>();
let version = 0;
let legacyInitialized = false;

function isComponent(value: unknown): boolean {
  // Function and class components, and memo/forwardRef/lazy objects (they carry $$typeof).
  return typeof value === 'function' || (typeof value === 'object' && value !== null && '$$typeof' in value);
}

function reject(api: string, reason: string): void {
  console.error(`Extension registration ignored: ${api}: ${reason}`);
}

function add<K extends ExtensionEventType>(type: K, ext: ExtensionsByType[K]): void {
  store[type].push(ext);
  version += 1;
  for (const listener of typedListeners.get(type) ?? []) {
    try {
      (listener as (e: ExtensionsByType[K]) => void)(ext);
    } catch (err) {
      console.error('Extension listener failed:', err);
    }
  }
  for (const listener of changeListeners) listener();
}

export function registerResourceExtension(
  component: ExtensionComponent,
  group: string,
  kind: string,
  tabTitle: string,
  opts?: { icon?: string },
): void {
  if (!isComponent(component)) return reject('registerResourceExtension', 'component must be a React component');
  if (typeof tabTitle !== 'string' || tabTitle === '') return reject('registerResourceExtension', 'tabTitle must be a non-empty string');
  add('resource', { component, group: String(group ?? ''), kind: String(kind ?? ''), title: tabTitle, icon: opts?.icon });
}

/** Normalizes a system extension path to "/segment[/...]" without a trailing slash. */
export function normalizeExtensionPath(path: string): string {
  const trimmed = path.trim().replace(/\/+$/, '');
  return trimmed.startsWith('/') ? trimmed : `/${trimmed}`;
}

export function registerSystemLevelExtension(component: SystemExtensionComponent, title: string, path: string, icon?: string): void {
  if (!isComponent(component)) return reject('registerSystemLevelExtension', 'component must be a React component');
  if (typeof title !== 'string' || title === '') return reject('registerSystemLevelExtension', 'title must be a non-empty string');
  if (typeof path !== 'string' || path.trim().replace(/\/+/g, '') === '') {
    return reject('registerSystemLevelExtension', `path must be a non-empty string (extension "${title}")`);
  }
  add('systemLevel', { component, title, icon, path: normalizeExtensionPath(path) });
}

export function registerAppViewExtension(
  component: AppViewExtensionComponent,
  title: string,
  icon: string,
  shouldDisplay?: (app: Application) => boolean,
): void {
  if (!isComponent(component)) return reject('registerAppViewExtension', 'component must be a React component');
  if (typeof title !== 'string' || title === '') return reject('registerAppViewExtension', 'title must be a non-empty string');
  add('appView', { component, title, icon, shouldDisplay: typeof shouldDisplay === 'function' ? shouldDisplay : () => true });
}

export function registerStatusPanelExtension(
  component: StatusPanelExtensionComponent,
  title: string,
  id: string,
  flyout?: FlyoutComponent,
): void {
  if (!isComponent(component)) return reject('registerStatusPanelExtension', 'component must be a React component');
  if (typeof id !== 'string' || id === '') return reject('registerStatusPanelExtension', 'id must be a non-empty string');
  add('statusPanel', { component, title: String(title ?? id), id, flyout: isComponent(flyout) ? flyout : undefined });
}

export function registerTopBarActionMenuExt(
  component: TopBarActionMenuExtComponent,
  title: string,
  id: string,
  flyout?: FlyoutComponent,
  shouldDisplay: (app?: Application) => boolean = () => true,
  iconClassName?: string,
  isMiddle = false,
): void {
  if (!isComponent(component)) return reject('registerTopBarActionMenuExt', 'component must be a React component');
  if (typeof id !== 'string' || id === '') return reject('registerTopBarActionMenuExt', 'id must be a non-empty string');
  add('topBar', {
    component,
    title: String(title ?? id),
    id,
    flyout: isComponent(flyout) ? flyout : undefined,
    shouldDisplay: typeof shouldDisplay === 'function' ? shouldDisplay : () => true,
    iconClassName,
    isMiddle: Boolean(isMiddle),
  });
}

export const extensionsAPI: ExtensionsAPI = {
  registerResourceExtension,
  registerSystemLevelExtension,
  registerAppViewExtension,
  registerStatusPanelExtension,
  registerTopBarActionMenuExt,
};

/** The deprecated window.extensions object; created by installExtensionGlobals(). */
export function legacyExtensions(): LegacyExtensions | undefined {
  const value = (window as unknown as { extensions?: LegacyExtensions }).extensions;
  return value && typeof value === 'object' && value.resources && typeof value.resources === 'object' ? value : undefined;
}

/**
 * Converts window.extensions.resources entries ("<group>/<kind>": {component}) into resource
 * tabs titled "More", once, on the first resource-tab lookup (upstream behavior).
 */
function initLegacyExtensions(): void {
  if (legacyInitialized) return;
  legacyInitialized = true;
  const legacy = legacyExtensions();
  if (!legacy) return;
  for (const [key, entry] of Object.entries(legacy.resources)) {
    if (!entry) continue;
    const [group = '', kind = ''] = key.split('/');
    registerResourceExtension(entry.component, group, kind, 'More');
  }
}

/** Resource tabs whose group and kind globs match, sorted by title. */
export function getResourceTabs(group: string, kind: string): ResourceTabExtension[] {
  initLegacyExtensions();
  return store.resource
    .filter((ext) => globMatch(group, ext.group) && globMatch(kind, ext.kind))
    .sort((a, b) => a.title.localeCompare(b.title));
}

export function getSystemExtensions(): SystemLevelExtension[] {
  return store.systemLevel.slice();
}

export function getAppViewExtensions(): AppViewExtension[] {
  return store.appView.slice();
}

export function getStatusPanelExtensions(): StatusPanelExtension[] {
  return store.statusPanel.slice();
}

export function getActionMenuExtensions(): TopBarActionMenuExt[] {
  return store.topBar.slice();
}

/** Typed event listener (upstream ExtensionsService.addEventListener). */
export function addExtensionListener<K extends ExtensionEventType>(type: K, listener: (ext: ExtensionsByType[K]) => void): () => void {
  let set = typedListeners.get(type);
  if (!set) {
    set = new Set();
    typedListeners.set(type, set);
  }
  set.add(listener);
  return () => set.delete(listener);
}

/** Change subscription for useSyncExternalStore. */
export function subscribeExtensions(listener: () => void): () => void {
  changeListeners.add(listener);
  return () => changeListeners.delete(listener);
}

/** Monotonic counter bumped on every registration (useSyncExternalStore snapshot). */
export function extensionsVersion(): number {
  return version;
}

/** Test helper: forget every registration. */
export function resetExtensionRegistry(): void {
  for (const key of Object.keys(store) as ExtensionEventType[]) store[key].length = 0;
  legacyInitialized = false;
  version += 1;
  for (const listener of changeListeners) listener();
}
