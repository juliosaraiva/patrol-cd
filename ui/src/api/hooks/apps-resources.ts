import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import * as api from '../endpoints/applications';
import { canonicalScope, controlPlaneNamespace, queryKeys } from '../queries/keys';
import { useCanI, useSettings } from '../queries/resources';
import type { Application, ApplicationResourcePatchRequest, AppScope, ResourceIdentifier } from '../types';

/*
 * Hooks for the application resource views (workstream apps-resources). They complement
 * src/api/queries (which is shared) with the per-resource routes the resource details drawer
 * needs. Keys live under queryKeys.applications.detail(name, scope) so one invalidation of the
 * application refreshes them together with the tree and diffs.
 */

/** Canonical scope (control-plane namespace dropped) and readiness, as the shared hooks do. */
function useScope(scopeArg: AppScope | undefined): { scope: AppScope | undefined; ready: boolean } {
  const settings = useSettings();
  return { scope: canonicalScope(scopeArg, settings.data?.controllerNamespace), ready: !settings.isPending };
}

/** Stable, order-independent identity of a resource for cache keys. */
function resourceKey(resource: ResourceIdentifier) {
  return {
    group: resource.group ?? '',
    version: resource.version ?? '',
    kind: resource.kind,
    namespace: resource.namespace ?? '',
    name: resource.name,
  };
}

export const appsResourcesKeys = {
  live: (name: string, scope: AppScope | undefined, resource: ResourceIdentifier) =>
    queryKeys.applications.sub(name, scope, 'live-resource', resourceKey(resource)),
};

/** GET /applications/{name}/resource: live manifest (JSON text, secrets masked by the server). */
export function useLiveResource(name: string, resource: ResourceIdentifier | null, scopeArg?: AppScope, options: { enabled?: boolean } = {}) {
  const { scope, ready } = useScope(scopeArg);
  return useQuery({
    queryKey: appsResourcesKeys.live(name, scope, resource ?? { kind: '', name: '' }),
    queryFn: ({ signal }) => api.getResource(name, resource ?? { kind: '', name: '' }, scope, { signal }),
    enabled: ready && name !== '' && resource !== null && (options.enabled ?? true),
  });
}

function useInvalidateApplication() {
  const queryClient = useQueryClient();
  return (name: string, scope: AppScope | undefined) =>
    queryClient.invalidateQueries({
      queryKey: queryKeys.applications.detail(name, canonicalScope(scope, controlPlaneNamespace(queryClient))),
    });
}

/** POST /applications/{name}/resource: patch the live object; refreshes the application's caches. */
export function usePatchResource() {
  const invalidate = useInvalidateApplication();
  return useMutation({
    mutationFn: ({ name, scope, resource, patch }: { name: string; scope?: AppScope; resource: ResourceIdentifier; patch: ApplicationResourcePatchRequest }) =>
      api.patchResource(name, resource, patch, scope),
    onSuccess: (_manifest, { name, scope }) => invalidate(name, scope),
  });
}

export interface DeleteResourceOptions {
  /** Delete immediately (grace period 0, background propagation). */
  force?: boolean;
  /** Leave dependents in place (orphan propagation). */
  orphan?: boolean;
}

/** DELETE /applications/{name}/resource */
export function useDeleteResource() {
  const invalidate = useInvalidateApplication();
  return useMutation({
    mutationFn: ({ name, scope, resource, force, orphan }: { name: string; scope?: AppScope; resource: ResourceIdentifier } & DeleteResourceOptions) =>
      api.deleteResource(name, resource, { force, orphan, ...scope }),
    onSuccess: (_r, { name, scope }) => invalidate(name, scope),
  });
}

/**
 * RBAC object of an application as the server checks it: "<project>/<name>", or
 * "<project>/<namespace>/<name>" when the application lives outside the control-plane namespace.
 */
export function applicationRbacObject(app: Application, controlPlane: string | undefined): string {
  const project = app.spec.project || 'default';
  const name = app.metadata.name ?? '';
  const namespace = app.metadata.namespace;
  return namespace && controlPlane && namespace !== controlPlane ? `${project}/${namespace}/${name}` : `${project}/${name}`;
}

/** can-i probe for an action on this application (e.g. ('logs', 'get'), ('exec', 'create')). */
export function useCanIOnApp(app: Application, resource: string, action: string, enabled = true) {
  const settings = useSettings();
  const object = applicationRbacObject(app, settings.data?.controllerNamespace);
  return useCanI(resource, action, object, enabled && !settings.isPending);
}

/**
 * Fine-grained application action the server checks to patch or delete one resource of an
 * application: "<verb>/<group>/<kind>/<namespace>/<name>" (empty group for core kinds, empty
 * namespace for cluster-scoped ones). The can-i endpoint takes it as one encoded path segment.
 */
export function resourceRbacAction(verb: 'update' | 'delete', resource: Pick<ResourceIdentifier, 'group' | 'kind' | 'namespace' | 'name'>): string {
  return [verb, resource.group ?? '', resource.kind, resource.namespace ?? '', resource.name].join('/');
}

/**
 * Result of a per-resource permission probe:
 * - `yes`: the fine-grained action is granted;
 * - `inherited`: only the plain verb on the application is granted, which the server accepts
 *   only when server.rbac.disableApplicationFineGrainedRBACInheritance is "false" (not the
 *   default, and not visible to the UI);
 * - `unknown`: a probe failed, so the server decides when the change is sent;
 * - `no`: neither is granted.
 */
export type ResourcePermission = 'yes' | 'inherited' | 'unknown' | 'no';

/**
 * Whether the caller may `verb` (update or delete) one resource of the application, probing the
 * fine-grained action the server enforces and the plain verb it may inherit from.
 */
export function useCanIOnAppResource(
  app: Application,
  verb: 'update' | 'delete',
  resource: Pick<ResourceIdentifier, 'group' | 'kind' | 'namespace' | 'name'>,
  enabled = true,
): { data: ResourcePermission | undefined; action: string; object: string } {
  const settings = useSettings();
  const object = applicationRbacObject(app, settings.data?.controllerNamespace);
  const action = resourceRbacAction(verb, resource);
  const fine = useCanI('applications', action, object, enabled && !settings.isPending);
  const coarse = useCanI('applications', verb, object, enabled && !settings.isPending);
  let data: ResourcePermission | undefined;
  if (fine.data === true) data = 'yes';
  else if (fine.isPending || coarse.isPending) data = undefined;
  else if (fine.data === false && coarse.data === true) data = 'inherited';
  else if (fine.data === false && coarse.data === false) data = 'no';
  else data = 'unknown';
  return { data, action, object };
}
