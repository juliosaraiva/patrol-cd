/**
 * Public ABI of Patrol UI extensions (window.extensionsAPI). It mirrors the Argo CD v3.5.5
 * extensions API so extensions built for Argo CD 3.5 load unmodified: same registration
 * functions, argument order and component props (API-120..126).
 */
import type { ComponentType } from 'react';
import type { Application, ApplicationTree } from '../api/types';

/** A live Kubernetes object as returned by GET /applications/{name}/resource (secrets masked). */
export interface KubernetesObject {
  apiVersion?: string;
  kind?: string;
  metadata?: {
    name?: string;
    namespace?: string;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
    [key: string]: unknown;
  };
  [key: string]: unknown;
}

/** Props of a resource tab (and of every flyout, which also receives `resource` when known). */
export interface ExtensionComponentProps {
  resource: KubernetesObject;
  tree: ApplicationTree;
  application: Application;
}

export interface AppViewComponentProps {
  application: Application;
  tree: ApplicationTree;
}

export interface StatusPanelComponentProps {
  application: Application;
  openFlyout: () => void;
}

export interface FlyoutComponentProps {
  application: Application;
  tree: ApplicationTree;
}

export interface TopBarActionMenuExtComponentProps {
  application: Application;
  tree: ApplicationTree;
  openFlyout: () => void;
}

export type ExtensionComponent = ComponentType<ExtensionComponentProps>;
export type SystemExtensionComponent = ComponentType;
export type AppViewExtensionComponent = ComponentType<AppViewComponentProps>;
export type StatusPanelExtensionComponent = ComponentType<StatusPanelComponentProps>;
export type FlyoutComponent = ComponentType<FlyoutComponentProps>;
export type TopBarActionMenuExtComponent = ComponentType<TopBarActionMenuExtComponentProps>;

export interface ResourceTabExtension {
  title: string;
  /** Glob matched against the resource group ("**" also matches the empty core group). */
  group: string;
  /** Glob matched against the resource kind. */
  kind: string;
  component: ExtensionComponent;
  /** Font Awesome class upstream (e.g. "fa-calendar-alt"); Patrol shows a generic glyph. */
  icon?: string | undefined;
}

export interface SystemLevelExtension {
  title: string;
  component: SystemExtensionComponent;
  icon?: string | undefined;
  /** Router path of the page, e.g. "/hello". */
  path?: string | undefined;
}

export interface AppViewExtension {
  component: AppViewExtensionComponent;
  title: string;
  icon?: string | undefined;
  shouldDisplay: (app: Application) => boolean;
}

export interface StatusPanelExtension {
  component: StatusPanelExtensionComponent;
  flyout?: FlyoutComponent | undefined;
  title: string;
  id: string;
}

export interface TopBarActionMenuExt {
  component: TopBarActionMenuExtComponent;
  flyout?: FlyoutComponent | undefined;
  shouldDisplay: (app?: Application) => boolean;
  title: string;
  id: string;
  iconClassName?: string | undefined;
  isMiddle: boolean;
}

export type ExtensionEventType = 'resource' | 'systemLevel' | 'appView' | 'statusPanel' | 'topBar';

export interface ExtensionsByType {
  resource: ResourceTabExtension;
  systemLevel: SystemLevelExtension;
  appView: AppViewExtension;
  statusPanel: StatusPanelExtension;
  topBar: TopBarActionMenuExt;
}

/** window.extensionsAPI */
export interface ExtensionsAPI {
  registerResourceExtension(component: ExtensionComponent, group: string, kind: string, tabTitle: string, opts?: { icon?: string }): void;
  registerSystemLevelExtension(component: SystemExtensionComponent, title: string, path: string, icon?: string): void;
  registerAppViewExtension(
    component: AppViewExtensionComponent,
    title: string,
    icon: string,
    shouldDisplay?: (app: Application) => boolean,
  ): void;
  registerStatusPanelExtension(component: StatusPanelExtensionComponent, title: string, id: string, flyout?: FlyoutComponent): void;
  registerTopBarActionMenuExt(
    component: TopBarActionMenuExtComponent,
    title: string,
    id: string,
    flyout?: FlyoutComponent,
    shouldDisplay?: (app?: Application) => boolean,
    iconClassName?: string,
    isMiddle?: boolean,
  ): void;
}

/** Deprecated window.extensions (API-126): `resources["<group>/<kind>"] = {component}`. */
export interface LegacyExtensions {
  resources: Record<string, { component: ExtensionComponent } | undefined>;
}
