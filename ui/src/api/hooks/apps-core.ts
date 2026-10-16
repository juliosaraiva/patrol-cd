/**
 * Application hooks used by the apps-core pages (list, details shell, sync, history, operations,
 * delete, parameters) that src/api/queries does not provide. Keys stay under
 * queryKeys.applications.detail(...) so the shared mutations' invalidation refreshes them.
 */
import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import * as api from '../endpoints/applications';
import * as repos from '../endpoints/repositories';
import { canonicalNamespace, canonicalScope, controlPlaneNamespace, queryKeys } from '../queries/keys';
import { useSettings } from '../queries/resources';
import type { Application, ApplicationPatchRequest, ApplicationSource, ApplicationSpec, AppScope, Operation, OperationList } from '../types';

function useScope(scopeArg: AppScope | undefined): { scope: AppScope | undefined; ready: boolean } {
  const settings = useSettings();
  return { scope: canonicalScope(scopeArg, settings.data?.controllerNamespace), ready: !settings.isPending };
}

function detailKeyOf(queryClient: QueryClient, app: Application) {
  return queryKeys.applications.detail(app.metadata.name ?? '', {
    appNamespace: canonicalNamespace(app.metadata.namespace, controlPlaneNamespace(queryClient)),
  });
}

/** Polling interval while an operation runs (Operations have no watch stream). */
export const OPERATION_POLL_MS = 2000;

/**
 * Operation objects of one application (GET .../operations), newest first. Polls while `poll` is
 * set (a sync or rollback is running) because the Operation list has no stream.
 */
export function useApplicationOperationsPolling(name: string, scopeArg: AppScope | undefined, options: { poll?: boolean; enabled?: boolean } = {}) {
  const { scope, ready } = useScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.operations(name, scope),
    queryFn: ({ signal }) => api.listApplicationOperations(name, scope, { signal }),
    enabled: ready && name !== '' && (options.enabled ?? true),
    refetchInterval: options.poll ? OPERATION_POLL_MS : false,
    select: sortOperations,
  });
}

/** Newest Operation first (creationTimestamp, then name). */
export function sortOperations(list: OperationList): OperationList {
  const items = [...list.items].sort((a, b) => {
    const at = a.metadata.creationTimestamp ?? '';
    const bt = b.metadata.creationTimestamp ?? '';
    if (at !== bt) return at < bt ? 1 : -1;
    return (b.metadata.name ?? '').localeCompare(a.metadata.name ?? '');
  });
  return { ...list, items };
}

/** A list continue token, or undefined when there is no further page ("" or absent). */
const nonEmptyToken = (token: string | undefined) => (token === '' ? undefined : token);

/**
 * Older Operations beyond the first page. GET .../operations returns at most `limit` (server
 * default 100) newest first, with `metadata.continue` when there are more; `loadMore` follows
 * the token (of the first page, then of the last loaded page) and appends the results.
 */
export function useOlderOperations(name: string, scopeArg: AppScope | undefined, firstPageContinue: string | undefined) {
  const { scope } = useScope(scopeArg);
  const [pages, setPages] = useState<{ key: string; items: Operation[]; next: string | undefined }>({ key: '', items: [], next: undefined });
  const key = `${name}|${scope?.appNamespace ?? ''}`;
  const current = pages.key === key ? pages : { key, items: [], next: undefined };
  const next = current.items.length > 0 ? current.next : nonEmptyToken(firstPageContinue);
  const load = useMutation({
    mutationFn: (token: string) => api.listApplicationOperations(name, { ...scope, continue: token }),
    onSuccess: (page) =>
      setPages((prev) => {
        const base = prev.key === key ? prev.items : [];
        return { key, items: [...base, ...page.items], next: nonEmptyToken(page.metadata?.continue) };
      }),
  });
  return {
    items: current.items,
    hasMore: next !== undefined,
    loadMore: () => {
      if (next !== undefined) load.mutate(next);
    },
    isLoading: load.isPending,
    error: load.error,
  };
}

/** One Operation (GET .../operations/{op}); polls while `poll` is set. */
export function useApplicationOperation(name: string, op: string, scopeArg: AppScope | undefined, options: { poll?: boolean } = {}) {
  const { scope, ready } = useScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.sub(name, scope, 'operation', op),
    queryFn: ({ signal }) => api.getApplicationOperation(name, op, scope, { signal }),
    enabled: ready && name !== '' && op !== '',
    refetchInterval: options.poll ? OPERATION_POLL_MS : false,
  });
}

/** Helm chart metadata for a chart revision (GET .../revisions/{revision}/chartdetails). */
export function useChartDetails(name: string, revision: string, scopeArg: AppScope | undefined, sourceIndex?: number, enabled = true) {
  const { scope, ready } = useScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.sub(name, scope, 'chartdetails', revision, sourceIndex ?? null),
    queryFn: ({ signal }) => api.getRevisionChartDetails(name, revision, { sourceIndex, ...scope }, { signal }),
    enabled: ready && enabled && name !== '' && revision !== '',
    staleTime: Infinity,
  });
}

/** PUT .../spec. The spec route carries no resourceVersion; prefer useUpdateApplication for edits. */
export function useUpdateApplicationSpec() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ name, scope, spec, validate }: { name: string; scope?: AppScope; spec: ApplicationSpec; validate?: boolean }) =>
      api.updateApplicationSpec(name, spec, { validate, ...scope }),
    onSuccess: (_spec, { name, scope }) =>
      queryClient.invalidateQueries({ queryKey: queryKeys.applications.detail(name, canonicalScope(scope, controlPlaneNamespace(queryClient))) }),
  });
}

/** PATCH /applications/{name} (merge or JSON patch); stores the returned object. */
export function usePatchApplication() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ name, scope, patch }: { name: string; scope?: AppScope; patch: ApplicationPatchRequest }) =>
      api.patchApplication(name, patch, scope),
    onSuccess: (app) => {
      queryClient.setQueryData(detailKeyOf(queryClient, app), app);
    },
  });
}

/**
 * Tool details for one source from the repo server (POST /repositories/{repo}/appdetails): Helm
 * parameter defaults and value files, Kustomize images, plugin parameter announcements.
 */
export function useSourceAppDetails(
  source: ApplicationSource | undefined,
  context: { appName?: string; appProject?: string; sourceIndex?: number; versionId?: number },
  enabled = true,
) {
  const url = source?.repoURL ?? '';
  return useQuery({
    queryKey: [...queryKeys.repositories.detail(url, 'read'), 'appdetails', source ?? null, context] as const,
    queryFn: ({ signal }) => {
      if (!source) throw new Error('no source to inspect');
      return repos.getRepositoryAppDetails(
        url,
        { source, appName: context.appName, appProject: context.appProject, sourceIndex: context.sourceIndex, versionId: context.versionId },
        { signal },
      );
    },
    enabled: enabled && url !== '',
    staleTime: 60_000,
    retry: false,
  });
}

/**
 * The resource tree as cached by whoever fetches it (the resource views use useResourceTree with
 * the same key); never fetches or opens a stream by itself. For consumers that only need the
 * tree when it is already there (extension flyouts).
 */
export function useCachedResourceTree(name: string, scopeArg: AppScope | undefined) {
  const { scope } = useScope(scopeArg);
  return useQuery({
    queryKey: queryKeys.applications.tree(name, scope),
    queryFn: ({ signal }) => api.getResourceTree(name, scope, { signal }),
    enabled: false,
  });
}
