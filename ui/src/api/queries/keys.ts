import type { QueryClient } from '@tanstack/react-query';
import type { AppScope, ResourceIdentifier, Settings } from '../types';

/**
 * Query key factory. Keys are hierarchical so invalidating a prefix (e.g.
 * queryKeys.applications.all) refreshes every dependent query.
 *
 * Namespaced keys (applications, ApplicationSets) must be built from a canonical namespace (see
 * canonicalNamespace/canonicalScope): the factories use whatever they are given.
 */
const scopeKey = (scope?: AppScope) => scope?.appNamespace ?? '';

/**
 * Objects in the control-plane namespace are addressed without a namespace (the default route
 * /applications/:name and the API default), while objects from the server always carry
 * metadata.namespace. Both must hit one cache entry, so the control-plane namespace (and an
 * absent one) is canonicalized to undefined, which keys as ''.
 */
export function canonicalNamespace(namespace: string | undefined, controlPlane: string | undefined): string | undefined {
  return !namespace || namespace === controlPlane ? undefined : namespace;
}

export function canonicalScope(scope: AppScope | undefined, controlPlane: string | undefined): AppScope | undefined {
  if (!scope) return scope;
  const appNamespace = canonicalNamespace(scope.appNamespace, controlPlane);
  return appNamespace === scope.appNamespace ? scope : { ...scope, appNamespace };
}

/** Control-plane namespace from the cached settings (undefined until settings load). */
export function controlPlaneNamespace(queryClient: QueryClient): string | undefined {
  return queryClient.getQueryData<Settings>(queryKeys.settings.all)?.controllerNamespace;
}

export const queryKeys = {
  session: {
    userInfo: ['session', 'userinfo'] as const,
    canI: (resource: string, action: string, subresource: string) => ['session', 'can-i', resource, action, subresource] as const,
  },
  settings: {
    all: ['settings'] as const,
    plugins: ['settings', 'plugins'] as const,
  },
  version: ['version'] as const,
  accounts: {
    all: ['accounts'] as const,
    detail: (name: string) => ['accounts', name] as const,
  },
  applications: {
    all: ['applications'] as const,
    lists: () => ['applications', 'list'] as const,
    list: (query: object) => ['applications', 'list', query] as const,
    detail: (name: string, scope?: AppScope) => ['applications', 'detail', scopeKey(scope), name] as const,
    sub: (name: string, scope: AppScope | undefined, ...rest: readonly unknown[]) =>
      ['applications', 'detail', scopeKey(scope), name, ...rest] as const,
    tree: (name: string, scope?: AppScope) => queryKeys.applications.sub(name, scope, 'tree'),
    managedResources: (name: string, scope?: AppScope, resource?: Partial<ResourceIdentifier>) =>
      queryKeys.applications.sub(name, scope, 'managed-resources', resource ?? {}),
    events: (name: string, scope?: AppScope, filter?: object) => queryKeys.applications.sub(name, scope, 'events', filter ?? {}),
    manifests: (name: string, scope?: AppScope, revision?: object) =>
      queryKeys.applications.sub(name, scope, 'manifests', revision ?? {}),
    revisionMetadata: (name: string, scope: AppScope | undefined, revision: string, sourceIndex?: number, versionId?: number) =>
      queryKeys.applications.sub(name, scope, 'revision', revision, sourceIndex ?? null, versionId ?? null),
    resourceActions: (name: string, scope: AppScope | undefined, resource: ResourceIdentifier) =>
      queryKeys.applications.sub(name, scope, 'actions', resource),
    links: (name: string, scope?: AppScope) => queryKeys.applications.sub(name, scope, 'links'),
    resourceLinks: (name: string, scope: AppScope | undefined, resource: ResourceIdentifier) =>
      queryKeys.applications.sub(name, scope, 'resource-links', resource),
    syncWindows: (name: string, scope?: AppScope) => queryKeys.applications.sub(name, scope, 'syncwindows'),
    operations: (name: string, scope?: AppScope) => queryKeys.applications.sub(name, scope, 'operations'),
  },
  applicationSets: {
    all: ['applicationsets'] as const,
    lists: () => ['applicationsets', 'list'] as const,
    list: (query: object) => ['applicationsets', 'list', query] as const,
    detail: (name: string, namespace?: string) => ['applicationsets', 'detail', namespace ?? '', name] as const,
    tree: (name: string, namespace?: string) => ['applicationsets', 'detail', namespace ?? '', name, 'tree'] as const,
    events: (name: string, namespace?: string) => ['applicationsets', 'detail', namespace ?? '', name, 'events'] as const,
  },
  projects: {
    all: ['projects'] as const,
    list: () => ['projects', 'list'] as const,
    detail: (name: string) => ['projects', 'detail', name] as const,
    detailed: (name: string) => ['projects', 'detail', name, 'detailed'] as const,
    events: (name: string) => ['projects', 'detail', name, 'events'] as const,
    syncWindows: (name: string) => ['projects', 'detail', name, 'syncwindows'] as const,
    links: (name: string) => ['projects', 'detail', name, 'links'] as const,
  },
  repositories: {
    all: ['repositories'] as const,
    list: (purpose: string, query: object) => ['repositories', 'list', purpose, query] as const,
    detail: (url: string, purpose: string) => ['repositories', 'detail', purpose, url] as const,
    refs: (url: string) => ['repositories', 'detail', 'read', url, 'refs'] as const,
    apps: (url: string, query: object) => ['repositories', 'detail', 'read', url, 'apps', query] as const,
    helmCharts: (url: string) => ['repositories', 'detail', 'read', url, 'helmcharts'] as const,
  },
  repositoryCredentials: {
    all: ['repocreds'] as const,
    list: (purpose: string) => ['repocreds', 'list', purpose] as const,
  },
  clusters: {
    all: ['clusters'] as const,
    list: () => ['clusters', 'list'] as const,
    detail: (id: string) => ['clusters', 'detail', id] as const,
  },
  certificates: { all: ['certificates'] as const, list: (query: object) => ['certificates', 'list', query] as const },
  gpgKeys: { all: ['gpgkeys'] as const, list: () => ['gpgkeys', 'list'] as const, detail: (id: string) => ['gpgkeys', 'detail', id] as const },
  scmProviders: {
    all: ['scmproviders'] as const,
    list: () => ['scmproviders', 'list'] as const,
    detail: (name: string) => ['scmproviders', 'detail', name] as const,
  },
  notifications: {
    services: ['notifications', 'services'] as const,
    triggers: ['notifications', 'triggers'] as const,
    templates: ['notifications', 'templates'] as const,
  },
} as const;
