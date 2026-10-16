/** UI extensions host: ABI types, registry, loader, mount points and proxy helper. */
export type * from './types';
export {
  addExtensionListener,
  extensionsAPI,
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
export {
  DEFAULT_EXTENSION_BUNDLES,
  EXTENSIONS_LOAD_TIMEOUT_MS,
  initExtensions,
  installExtensionGlobals,
  loadExtensions,
  resetExtensionsLoader,
} from './loader';
export {
  AppViewExtensions,
  EXTENSION_FLYOUT_PARAM,
  ResourceExtensionTabs,
  StatusPanelExtensions,
  SystemExtensionRoutes,
  TopBarExtensionActions,
  findSystemExtension,
  useAppViewExtensions,
  useResourceExtensionTabs,
  useSystemExtensionNavItems,
} from './slots';
export type { ExtensionNavItem, ResourceExtensionContext, TopBarPlacement } from './slots';
export { ExtensionBoundary } from './ExtensionBoundary';
export { IconExtension } from './icon';
export { APPLICATION_HEADER, PROJECT_HEADER, callProxyExtension, extensionProxyHeaders } from './proxy';
export { globMatch } from './glob';
