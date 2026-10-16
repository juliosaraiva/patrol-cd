/**
 * Types of the globals the Patrol CD UI provides to extensions (the Argo CD 3.5 extension
 * ABI). Only the parts this example uses are declared.
 */
import type { ComponentType } from 'react';

/** A live Kubernetes object as shown in the resource details panel (secrets masked). */
export interface KubernetesObject {
  apiVersion?: string;
  kind?: string;
  metadata?: {
    name?: string;
    namespace?: string;
    generation?: number;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
  };
  [key: string]: unknown;
}

/** Props of a resource tab component. application and tree are the Application and its
 * resource tree, typed loosely here. */
export interface ResourceTabProps {
  resource: KubernetesObject;
  application: unknown;
  tree: unknown;
}

export interface ExtensionsAPI {
  registerResourceExtension(
    component: ComponentType<ResourceTabProps>,
    group: string,
    kind: string,
    tabTitle: string,
    opts?: { icon?: string },
  ): void;
}

declare global {
  interface Window {
    extensionsAPI: ExtensionsAPI;
    /** The host's React 19 instance: extensions must not bundle their own React. */
    React: typeof import('react');
    ReactJSXRuntime: typeof import('react/jsx-runtime');
  }
}
