/**
 * Repository discovery hooks for the application creation wizard (API-091, API-038): refs,
 * OCI tags, Helm charts, app paths and app details. One-shot queries (no watches); results are
 * cached under queryKeys.repositories so "Refresh" on the repositories page invalidates them.
 */
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import * as repos from '../endpoints/repositories';
import { queryKeys } from '../queries/keys';
import type { ApplicationSource } from '../types';

const DISCOVERY_STALE_MS = 60_000;

/** Branches and tags of a Git repository (ListRefs). */
export function useRepoRefs(url: string, appProject?: string) {
  return useQuery({
    queryKey: [...queryKeys.repositories.refs(url), appProject ?? ''],
    queryFn: ({ signal }) => repos.listRepositoryRefs(url, { appProject }, { signal }),
    enabled: url !== '',
    staleTime: DISCOVERY_STALE_MS,
  });
}

/** Tags of an OCI repository. */
export function useRepoOCITags(url: string, appProject?: string, enabled = true) {
  return useQuery({
    queryKey: ['repositories', 'detail', 'read', url, 'oci-tags', appProject ?? ''] as const,
    queryFn: ({ signal }) => repos.listRepositoryOCITags(url, { appProject }, { signal }),
    enabled: enabled && url !== '',
    staleTime: DISCOVERY_STALE_MS,
  });
}

/** Charts (with versions) of a Helm repository. */
export function useRepoHelmCharts(url: string, appProject?: string, enabled = true) {
  return useQuery({
    queryKey: [...queryKeys.repositories.helmCharts(url), appProject ?? ''],
    queryFn: ({ signal }) => repos.listRepositoryHelmCharts(url, { appProject }, { signal }),
    enabled: enabled && url !== '',
    staleTime: DISCOVERY_STALE_MS,
  });
}

/** Paths in a repository revision that look like applications (path autocompletion). */
export function useRepoApps(url: string, query: { revision?: string; appName?: string; appProject?: string }, enabled = true) {
  return useQuery({
    queryKey: queryKeys.repositories.apps(url, query),
    queryFn: ({ signal }) => repos.listRepositoryApps(url, query, { signal }),
    enabled: enabled && url !== '',
    staleTime: DISCOVERY_STALE_MS,
    placeholderData: keepPreviousData,
  });
}

/**
 * Detected source type and tool parameters for a source (GetAppDetails); used to prefill Helm,
 * Kustomize and plugin parameters. Enabled only once the source is complete enough to render.
 */
export function useRepoAppDetails(source: ApplicationSource | null, scope: { appName?: string; appProject?: string } = {}) {
  const ready = source !== null && source.repoURL !== '' && (Boolean(source.chart) || source.path !== undefined);
  return useQuery({
    queryKey: ['repositories', 'detail', 'read', source?.repoURL ?? '', 'appdetails', source ?? {}, scope] as const,
    queryFn: ({ signal }) =>
      repos.getRepositoryAppDetails(source?.repoURL ?? '', { source: source ?? { repoURL: '' }, ...scope }, { signal }),
    enabled: ready,
    staleTime: DISCOVERY_STALE_MS,
    retry: false,
  });
}
