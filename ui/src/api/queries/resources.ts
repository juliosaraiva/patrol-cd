import { useQuery, useQueryClient } from '@tanstack/react-query';
import * as appsets from '../endpoints/applicationsets';
import * as clusters from '../endpoints/clusters';
import * as misc from '../endpoints/misc';
import * as projects from '../endpoints/projects';
import * as repos from '../endpoints/repositories';
import * as session from '../endpoints/session';
import * as trust from '../endpoints/trust';
import { isApiError } from '../errors';
import type { ApplicationSet, UserInfo } from '../types';
import { canonicalNamespace, controlPlaneNamespace, queryKeys } from './keys';
import { nonEmpty, useListWatch, watchedListOptions } from './watch';

const LOGGED_OUT: UserInfo = { loggedIn: false };

/** Current user. A 401 is "logged out", not an error. */
export function useUserInfo() {
  return useQuery({
    queryKey: queryKeys.session.userInfo,
    queryFn: async ({ signal }) => {
      try {
        return await session.getUserInfo({ signal });
      } catch (err) {
        if (isApiError(err) && err.status === 401) return LOGGED_OUT;
        throw err;
      }
    },
    staleTime: 60_000,
  });
}

export function useSettings() {
  return useQuery({
    queryKey: queryKeys.settings.all,
    queryFn: ({ signal }) => session.getSettings({ signal }),
    staleTime: 5 * 60_000,
  });
}

export function usePluginSettings() {
  return useQuery({ queryKey: queryKeys.settings.plugins, queryFn: ({ signal }) => session.getPluginSettings({ signal }) });
}

export function useVersion() {
  return useQuery({ queryKey: queryKeys.version, queryFn: ({ signal }) => session.getVersion({ signal }), staleTime: Infinity });
}

/** RBAC probe for the caller; `data` is true when allowed. */
export function useCanI(resource: string, action: string, subresource: string, enabled = true) {
  return useQuery({
    queryKey: queryKeys.session.canI(resource, action, subresource),
    queryFn: async ({ signal }) => (await session.canI(resource, action, subresource, { signal })).value === 'yes',
    enabled,
    staleTime: 60_000,
  });
}

export function useAccounts() {
  return useQuery({ queryKey: queryKeys.accounts.all, queryFn: ({ signal }) => session.listAccounts({ signal }) });
}

export function useAccount(name: string) {
  return useQuery({
    queryKey: queryKeys.accounts.detail(name),
    queryFn: ({ signal }) => session.getAccount(name, { signal }),
    enabled: name !== '',
  });
}

export function useProjects() {
  return useQuery({ queryKey: queryKeys.projects.list(), queryFn: ({ signal }) => projects.listProjects({}, { signal }) });
}

export function useProject(name: string) {
  return useQuery({
    queryKey: queryKeys.projects.detail(name),
    queryFn: ({ signal }) => projects.getProject(name, { signal }),
    enabled: name !== '',
  });
}

export function useDetailedProject(name: string) {
  return useQuery({
    queryKey: queryKeys.projects.detailed(name),
    queryFn: ({ signal }) => projects.getDetailedProject(name, { signal }),
    enabled: name !== '',
  });
}

export function useProjectEvents(name: string) {
  return useQuery({
    queryKey: queryKeys.projects.events(name),
    queryFn: ({ signal }) => projects.listProjectEvents(name, { signal }),
    enabled: name !== '',
  });
}

export function useRepositories(query: { forceRefresh?: boolean; appProject?: string } = {}, purpose: repos.RepositoryPurpose = 'read') {
  return useQuery({
    queryKey: queryKeys.repositories.list(purpose, query),
    queryFn: ({ signal }) => repos.listRepositories(query, { signal, purpose }),
  });
}

export function useRepositoryCredentials(purpose: repos.RepositoryPurpose = 'read') {
  return useQuery({
    queryKey: queryKeys.repositoryCredentials.list(purpose),
    queryFn: ({ signal }) => repos.listRepositoryCredentials({ signal, purpose }),
  });
}

export function useRepositoryRefs(url: string) {
  return useQuery({
    queryKey: queryKeys.repositories.refs(url),
    queryFn: ({ signal }) => repos.listRepositoryRefs(url, {}, { signal }),
    enabled: url !== '',
  });
}

export function useClusters() {
  return useQuery({ queryKey: queryKeys.clusters.list(), queryFn: ({ signal }) => clusters.listClusters({}, { signal }) });
}

export function useCluster(ref: clusters.ClusterRef, options: { refetchInterval?: number } = {}) {
  const id = ref.server ?? `name:${ref.name ?? ''}`;
  return useQuery({
    queryKey: queryKeys.clusters.detail(id),
    queryFn: ({ signal }) => clusters.getCluster(ref, { signal }),
    enabled: Boolean(ref.server ?? ref.name),
    refetchInterval: options.refetchInterval,
  });
}

/** ApplicationSet list, live through /api/v1/stream/applicationsets. */
export function useApplicationSets(query: appsets.ListApplicationSetsQuery = {}, options: { watch?: boolean } = {}) {
  const queryClient = useQueryClient();
  const settingsReady = !useSettings().isPending;
  const watch = options.watch ?? true;
  const listKey = queryKeys.applicationSets.list(query);
  const result = useQuery({
    queryKey: listKey,
    queryFn: ({ signal }) => appsets.listApplicationSets(query, { signal }),
    ...(watch ? watchedListOptions : {}),
  });
  useListWatch<ApplicationSet>({
    listKey,
    enabled: watch && settingsReady && result.isSuccess,
    resourceVersion: result.data?.metadata?.resourceVersion ?? (result.isSuccess ? '' : undefined),
    detailKey: (set) =>
      queryKeys.applicationSets.detail(set.metadata.name ?? '', canonicalNamespace(set.metadata.namespace, controlPlaneNamespace(queryClient))),
    open: ({ resourceVersion, resume, onEvent }) =>
      appsets.watchApplicationSets(
        { projects: query.projects, appsetNamespace: query.appsetNamespace, selector: query.selector, resourceVersion: nonEmpty(resourceVersion) },
        { onEvent, resumeQuery: () => ({ resourceVersion: nonEmpty(resume()) }) },
      ),
  });
  return result;
}

/**
 * Canonical ApplicationSet namespace for keys and requests (see canonicalNamespace); queries wait
 * for `ready` (settings resolved).
 */
function useCanonicalNamespace(namespace: string | undefined): { namespace: string | undefined; ready: boolean } {
  const settings = useSettings();
  return { namespace: canonicalNamespace(namespace, settings.data?.controllerNamespace), ready: !settings.isPending };
}

export function useApplicationSet(name: string, namespaceArg?: string) {
  const { namespace, ready } = useCanonicalNamespace(namespaceArg);
  return useQuery({
    queryKey: queryKeys.applicationSets.detail(name, namespace),
    queryFn: ({ signal }) => appsets.getApplicationSet(name, namespace, { signal }),
    enabled: ready && name !== '',
  });
}

export function useApplicationSetTree(name: string, namespaceArg?: string) {
  const { namespace, ready } = useCanonicalNamespace(namespaceArg);
  return useQuery({
    queryKey: queryKeys.applicationSets.tree(name, namespace),
    queryFn: ({ signal }) => appsets.getApplicationSetResourceTree(name, namespace, { signal }),
    enabled: ready && name !== '',
  });
}

export function useApplicationSetEvents(name: string, namespaceArg?: string) {
  const { namespace, ready } = useCanonicalNamespace(namespaceArg);
  return useQuery({
    queryKey: queryKeys.applicationSets.events(name, namespace),
    queryFn: ({ signal }) => appsets.listApplicationSetEvents(name, namespace, { signal }),
    enabled: ready && name !== '',
  });
}

export function useNotificationServices() {
  return useQuery({ queryKey: queryKeys.notifications.services, queryFn: ({ signal }) => misc.listNotificationServices({ signal }) });
}

export function useNotificationTriggers() {
  return useQuery({ queryKey: queryKeys.notifications.triggers, queryFn: ({ signal }) => misc.listNotificationTriggers({ signal }) });
}

export function useNotificationTemplates() {
  return useQuery({ queryKey: queryKeys.notifications.templates, queryFn: ({ signal }) => misc.listNotificationTemplates({ signal }) });
}

export function useCertificates(query: trust.CertificateQuery = {}) {
  return useQuery({
    queryKey: queryKeys.certificates.list(query),
    queryFn: ({ signal }) => trust.listCertificates(query, { signal }),
  });
}

export function useGPGKeys() {
  return useQuery({ queryKey: queryKeys.gpgKeys.list(), queryFn: ({ signal }) => trust.listGPGKeys({ signal }) });
}

export function useSCMProviders() {
  return useQuery({ queryKey: queryKeys.scmProviders.list(), queryFn: ({ signal }) => trust.listSCMProviders({ signal }) });
}

export function useSCMProvider(name: string) {
  return useQuery({
    queryKey: queryKeys.scmProviders.detail(name),
    queryFn: ({ signal }) => trust.getSCMProvider(name, { signal }),
    enabled: name !== '',
  });
}
