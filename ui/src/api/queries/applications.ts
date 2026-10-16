import { keepPreviousData, useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import * as api from '../endpoints/applications';
import type { StreamState } from '../sse';
import type {
  Application,
  ApplicationInput,
  ApplicationList,
  ApplicationOperationResponse,
  ApplicationRollbackRequest,
  ApplicationSyncRequest,
  AppScope,
  ResourceActionRunRequest,
  ResourceIdentifier,
} from '../types';
import { canonicalNamespace, canonicalScope, controlPlaneNamespace, queryKeys } from './keys';
import { useSettings } from './resources';
import { nonEmpty, useListWatch, watchedListOptions } from './watch';

/** Detail key of an object received from the server (watch event, mutation result). */
function detailKeyOf(queryClient: QueryClient, app: Application) {
  return queryKeys.applications.detail(app.metadata.name ?? '', {
    appNamespace: canonicalNamespace(app.metadata.namespace, controlPlaneNamespace(queryClient)),
  });
}

/**
 * Canonical form of a caller's scope for cache keys and requests: the control-plane namespace
 * is dropped (the API default), so `/applications/guestbook` and `/applications/<cp-ns>/guestbook`
 * share cache entries with watch events and mutation results. Queries wait for `ready` (settings
 * resolved) so nothing is cached under a key that becomes non-canonical once settings load; in
 * the shell, settings are always loaded first.
 */
function useCanonicalScope(scope: AppScope | undefined): { scope: AppScope | undefined; ready: boolean } {
  const settings = useSettings();
  return { scope: canonicalScope(scope, settings.data?.controllerNamespace), ready: !settings.isPending };
}

/**
 * Application list, kept live through /api/v1/stream/applications (events merged into the
 * cached list every 500 ms). Disable `watch` for one-shot lists (pickers, exports).
 */
export function useApplications(query: api.ListApplicationsQuery = {}, options: { watch?: boolean; enabled?: boolean } = {}) {
  const queryClient = useQueryClient();
  // Watch events are cached under canonical detail keys, which need the settings.
  const settingsReady = !useSettings().isPending;
  const enabled = options.enabled ?? true;
  const watch = options.watch ?? true;
  const listKey = queryKeys.applications.list(query);
  const result = useQuery({
    queryKey: listKey,
    queryFn: ({ signal }) => api.listApplications(query, { signal }),
    placeholderData: keepPreviousData,
    enabled,
    ...(watch ? watchedListOptions : {}),
  });
  useListWatch<Application>({
    listKey,
    // Wait for the real list (not placeholder data from a previous query) so the watch starts at its resourceVersion.
    enabled: enabled && watch && settingsReady && result.isSuccess && !result.isPlaceholderData,
    resourceVersion: result.data?.metadata?.resourceVersion ?? (result.isSuccess ? '' : undefined),
    detailKey: (app) => detailKeyOf(queryClient, app),
    open: ({ resourceVersion, resume, onEvent }) =>
      api.watchApplications(
        {
          projects: query.projects,
          selector: query.selector,
          repo: query.repo,
          appNamespace: query.appNamespace,
          name: query.name,
          resourceVersion: nonEmpty(resourceVersion),
        },
        { onEvent, resumeQuery: () => ({ resourceVersion: nonEmpty(resume()) }) },
      ),
  });
  return result;
}

/** One application; `watch` subscribes to the stream filtered by name. */
export function useApplication(name: string, scopeArg?: AppScope, options: { watch?: boolean } = {}) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  const queryClient = useQueryClient();
  const key = queryKeys.applications.detail(name, scope);
  const result = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => api.getApplication(name, scope, { signal }),
    enabled: ready && name !== '',
  });
  const watch = (options.watch ?? false) && result.isSuccess;
  const keyHash = JSON.stringify(key);
  useEffect(() => {
    if (!watch) return undefined;
    const handle = api.watchApplications(
      { name, appNamespace: scope?.appNamespace },
      {
        onEvent: (event) => {
          if (event.type === 'DELETED') queryClient.removeQueries({ queryKey: key, exact: true });
          else queryClient.setQueryData(key, event.object);
        },
      },
    );
    return () => handle.close();
    // keyHash identifies `key`, which is a new array every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [watch, keyHash, queryClient]);
  return result;
}

/** Resource tree from GET .../resource-tree, then live from the SSE tree stream. */
export function useResourceTree(name: string, scopeArg?: AppScope, options: { watch?: boolean } = {}) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  const queryClient = useQueryClient();
  const key = queryKeys.applications.tree(name, scope);
  const [streamState, setStreamState] = useState<StreamState>('closed');
  const result = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => api.getResourceTree(name, scope, { signal }),
    enabled: ready && name !== '',
  });
  const watch = (options.watch ?? true) && result.isSuccess;
  const keyHash = JSON.stringify(key);
  useEffect(() => {
    if (!watch) return undefined;
    const handle = api.watchResourceTree(name, scope, {
      onEvent: (event) => {
        if (event.type !== 'DELETED') queryClient.setQueryData(key, event.object);
      },
      onStateChange: setStreamState,
    });
    return () => handle.close();
    // keyHash identifies `key` and `scope`.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [watch, keyHash, queryClient]);
  return { ...result, streamState };
}

export function useManagedResources(name: string, scopeArg?: AppScope, resource?: Partial<ResourceIdentifier>) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.managedResources(name, scope, resource),
    queryFn: ({ signal }) => api.getManagedResources(name, { ...resource, ...scope }, { signal }),
    enabled: ready && name !== '',
  });
}

export function useApplicationEvents(
  name: string,
  scopeArg?: AppScope,
  filter: { resourceNamespace?: string; resourceName?: string; resourceUID?: string } = {},
) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.events(name, scope, filter),
    queryFn: ({ signal }) => api.listApplicationEvents(name, { ...filter, ...scope }, { signal }),
    enabled: ready && name !== '',
  });
}

export function useManifests(name: string, scopeArg?: AppScope, revision: { revision?: string; sourcePositions?: number[]; revisions?: string[] } = {}) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.manifests(name, scope, revision),
    queryFn: ({ signal }) => api.getManifests(name, { ...revision, ...scope }, { signal }),
    enabled: ready && name !== '',
  });
}

export function useRevisionMetadata(name: string, revision: string, scopeArg?: AppScope, source: { sourceIndex?: number; versionId?: number } = {}) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.revisionMetadata(name, scope, revision, source.sourceIndex, source.versionId),
    queryFn: ({ signal }) => api.getRevisionMetadata(name, revision, { ...source, ...scope }, { signal }),
    enabled: ready && name !== '' && revision !== '',
    staleTime: Infinity, // a commit's metadata never changes
  });
}

export function useResourceActions(name: string, resource: ResourceIdentifier | null, scopeArg?: AppScope) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.resourceActions(name, scope, resource ?? { kind: '', name: '' }),
    queryFn: ({ signal }) => api.listResourceActions(name, resource ?? { kind: '', name: '' }, scope, { signal }),
    enabled: ready && name !== '' && resource !== null,
  });
}

export function useApplicationLinks(name: string, scopeArg?: AppScope) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.links(name, scope),
    queryFn: ({ signal }) => api.listApplicationLinks(name, scope, { signal }),
    enabled: ready && name !== '',
  });
}

export function useResourceLinks(name: string, resource: ResourceIdentifier | null, scopeArg?: AppScope) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.resourceLinks(name, scope, resource ?? { kind: '', name: '' }),
    queryFn: ({ signal }) => api.listResourceLinks(name, resource ?? { kind: '', name: '' }, scope, { signal }),
    enabled: ready && name !== '' && resource !== null,
  });
}

export function useApplicationSyncWindows(name: string, scopeArg?: AppScope) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.syncWindows(name, scope),
    queryFn: ({ signal }) => api.getApplicationSyncWindows(name, scope, { signal }),
    enabled: ready && name !== '',
  });
}

export function useApplicationOperations(name: string, scopeArg?: AppScope) {
  const { scope, ready } = useCanonicalScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.operations(name, scope),
    queryFn: ({ signal }) => api.listApplicationOperations(name, scope, { signal }),
    enabled: ready && name !== '',
  });
}

/** Invalidate everything cached for one application (detail, tree, diffs, events...). */
function useInvalidateApp() {
  const queryClient = useQueryClient();
  return (name: string, scope?: AppScope) =>
    queryClient.invalidateQueries({
      queryKey: queryKeys.applications.detail(name, canonicalScope(scope, controlPlaneNamespace(queryClient))),
    });
}

export function useCreateApplication() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ app, upsert, validate }: { app: ApplicationInput; upsert?: boolean; validate?: boolean }) =>
      api.createApplication(app, { upsert, validate }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.applications.lists() }),
  });
}

export function useUpdateApplication() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ app, validate }: { app: ApplicationInput; validate?: boolean }) => api.updateApplication(app, { validate }),
    onSuccess: (app) => {
      queryClient.setQueryData(detailKeyOf(queryClient, app), app);
    },
  });
}

/**
 * Seeds the caches from a sync/rollback/terminate response ({application, operation}) so the
 * operation panel shows the new Operation before the next poll, then refreshes the app.
 */
function useApplyOperationResponse() {
  const queryClient = useQueryClient();
  const invalidate = useInvalidateApp();
  return (res: ApplicationOperationResponse | undefined, name: string, scope: AppScope | undefined) => {
    const opName = res?.operation?.metadata.name;
    if (res?.operation && opName) {
      const canonical = canonicalScope(scope, controlPlaneNamespace(queryClient));
      queryClient.setQueryData(queryKeys.applications.sub(name, canonical, 'operation', opName), res.operation);
    }
    if (res?.application) queryClient.setQueryData(detailKeyOf(queryClient, res.application), res.application);
    return invalidate(name, scope);
  };
}

/** POST .../sync: creates an Operation; the response carries it (and the Application). */
export function useSyncApplication() {
  const apply = useApplyOperationResponse();
  return useMutation({
    mutationFn: ({ name, scope, request }: { name: string; scope?: AppScope; request: ApplicationSyncRequest }) =>
      api.syncApplication(name, request, scope),
    onSuccess: (res, { name, scope }) => apply(res, name, scope),
  });
}

/** POST .../rollback: creates a rollback Operation. */
export function useRollbackApplication() {
  const apply = useApplyOperationResponse();
  return useMutation({
    mutationFn: ({ name, scope, request }: { name: string; scope?: AppScope; request: ApplicationRollbackRequest }) =>
      api.rollbackApplication(name, request, scope),
    onSuccess: (res, { name, scope }) => apply(res, name, scope),
  });
}

/** DELETE .../operation: asks the running Operation to stop (spec.cancel). */
export function useTerminateOperation() {
  const apply = useApplyOperationResponse();
  return useMutation({
    mutationFn: ({ name, scope }: { name: string; scope?: AppScope }) => api.terminateOperation(name, scope),
    onSuccess: (res, { name, scope }) => apply(res, name, scope),
  });
}

export function useDeleteApplication() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ name, scope, cascade, propagationPolicy }: { name: string; scope?: AppScope } & api.DeleteApplicationQuery) =>
      api.deleteApplication(name, { cascade, propagationPolicy, ...scope }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.applications.lists() }),
  });
}

/** Normal or hard refresh: GET with ?refresh=, then store the refreshed object. */
export function useRefreshApplication() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ name, scope, type = 'normal' }: { name: string; scope?: AppScope; type?: api.RefreshType }) =>
      api.getApplication(name, { ...scope, refresh: type }),
    onSuccess: (app) => {
      queryClient.setQueryData(detailKeyOf(queryClient, app), app);
      queryClient.setQueriesData<ApplicationList>({ queryKey: queryKeys.applications.lists() }, (old) =>
        old
          ? {
              ...old,
              items: old.items.map((item) =>
                item.metadata.name === app.metadata.name && item.metadata.namespace === app.metadata.namespace ? app : item,
              ),
            }
          : old,
      );
    },
  });
}

export function useRunResourceAction() {
  const invalidate = useInvalidateApp();
  return useMutation({
    mutationFn: ({ name, scope, resource, request }: { name: string; scope?: AppScope; resource: ResourceIdentifier; request: ResourceActionRunRequest }) =>
      api.runResourceAction(name, resource, request, scope),
    onSuccess: (_r, { name, scope }) => invalidate(name, scope),
  });
}
