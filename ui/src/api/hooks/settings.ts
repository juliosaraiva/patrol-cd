/**
 * Mutation and auxiliary query hooks for the Settings pages (repositories, credentials,
 * certificates, GnuPG keys, clusters, projects, accounts, SCM providers). Read hooks for lists
 * live in ../queries/resources.ts; these hooks reuse its keys so a mutation refreshes them.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import * as clusters from '../endpoints/clusters';
import * as projects from '../endpoints/projects';
import * as repos from '../endpoints/repositories';
import * as session from '../endpoints/session';
import * as trust from '../endpoints/trust';
import { queryKeys } from '../queries/keys';
import type {
  AppProjectInput,
  ClusterInput,
  CreateAccountTokenRequest,
  GnuPGPublicKey,
  ProjectTokenCreateRequest,
  RepositoryCertificateList,
  RepositoryCredentialWriteRequest,
  RepositoryWriteRequest,
  SCMProviderWriteRequest,
} from '../types';

type Purpose = repos.RepositoryPurpose;

// Repositories --------------------------------------------------------------------------------

export function useCreateRepository(purpose: Purpose = 'read') {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ body, upsert }: { body: RepositoryWriteRequest; upsert?: boolean }) =>
      repos.createRepository(body, { upsert }, { purpose }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.repositories.all }),
  });
}

export function useUpdateRepository(purpose: Purpose = 'read') {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ url, body, appProject }: { url: string; body: RepositoryWriteRequest; appProject?: string }) =>
      repos.updateRepository(url, body, { appProject }, { purpose }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.repositories.all }),
  });
}

export function useDeleteRepository(purpose: Purpose = 'read') {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ url, appProject }: { url: string; appProject?: string }) => repos.deleteRepository(url, { appProject }, { purpose }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.repositories.all }),
  });
}

/** POST .../validate: tests a connection without saving anything. */
export function useValidateRepository(purpose: Purpose = 'read') {
  return useMutation({
    mutationFn: ({ url, body }: { url: string; body: RepositoryWriteRequest }) => repos.validateRepository(url, body, { purpose }),
  });
}

// Repository credential templates ------------------------------------------------------------

export function useCreateRepositoryCredential(purpose: Purpose = 'read') {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ body, upsert }: { body: RepositoryCredentialWriteRequest; upsert?: boolean }) =>
      repos.createRepositoryCredential(body, { upsert }, { purpose }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.repositoryCredentials.all }),
  });
}

export function useDeleteRepositoryCredential(purpose: Purpose = 'read') {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (urlPrefix: string) => repos.deleteRepositoryCredential(urlPrefix, { purpose }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.repositoryCredentials.all }),
  });
}

// Certificates and GnuPG keys ----------------------------------------------------------------

export function useCreateCertificates() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ body, upsert }: { body: RepositoryCertificateList; upsert?: boolean }) => trust.createCertificates(body, { upsert }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.certificates.all }),
  });
}

export function useDeleteCertificates() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (query: trust.CertificateQuery) => trust.deleteCertificates(query),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.certificates.all }),
  });
}

export function useCreateGPGKey() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: GnuPGPublicKey) => trust.createGPGKey(body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.gpgKeys.all }),
  });
}

export function useDeleteGPGKey() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (keyID: string) => trust.deleteGPGKey(keyID),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.gpgKeys.all }),
  });
}

// Clusters -------------------------------------------------------------------------------------

/** PUT with `updatedFields`: only the named fields are changed on the server. */
export function useUpdateCluster() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ ref, body, updatedFields }: { ref: clusters.ClusterRef; body: ClusterInput; updatedFields: string[] }) =>
      clusters.updateCluster(ref, body, updatedFields),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.clusters.all }),
  });
}

export function useDeleteCluster() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (ref: clusters.ClusterRef) => clusters.deleteCluster(ref),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.clusters.all }),
  });
}

export function useRotateClusterAuth() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (ref: clusters.ClusterRef) => clusters.rotateClusterAuth(ref),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.clusters.all }),
  });
}

export function useInvalidateClusterCache() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (ref: clusters.ClusterRef) => clusters.invalidateClusterCache(ref),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.clusters.all }),
  });
}

// Projects -------------------------------------------------------------------------------------

export function useProjectSyncWindows(name: string) {
  return useQuery({
    queryKey: queryKeys.projects.syncWindows(name),
    queryFn: ({ signal }) => projects.getProjectSyncWindows(name, { signal }),
    enabled: name !== '',
  });
}

/** Deep links configured for the project (resource links of kind AppProject). */
export function useProjectLinks(name: string) {
  return useQuery({
    queryKey: queryKeys.projects.links(name),
    queryFn: ({ signal }) => projects.listProjectLinks(name, { signal }),
    enabled: name !== '',
  });
}

export function useCreateProject() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (project: AppProjectInput) => projects.createProject(project),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.projects.all }),
  });
}

/**
 * PUT of the whole project; the server rejects a stale metadata.resourceVersion with 409. On
 * success the fresh object replaces the cached detail so later edits carry the new version.
 */
export function useUpdateProject() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (project: AppProjectInput) => projects.updateProject(project),
    onSuccess: (updated) => {
      const name = updated.metadata.name ?? '';
      queryClient.setQueryData(queryKeys.projects.detail(name), updated);
      return queryClient.invalidateQueries({ queryKey: queryKeys.projects.all });
    },
  });
}

export function useDeleteProject() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => projects.deleteProject(name),
    onSuccess: (_void, name) => {
      queryClient.removeQueries({ queryKey: queryKeys.projects.detail(name) });
      return queryClient.invalidateQueries({ queryKey: queryKeys.projects.list() });
    },
  });
}

export function useCreateProjectRoleToken(project: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ role, body }: { role: string; body: ProjectTokenCreateRequest }) => projects.createProjectRoleToken(project, role, body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.projects.detail(project) }),
  });
}

export function useDeleteProjectRoleToken(project: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ role, issuedAt, id }: { role: string; issuedAt: number; id?: string }) =>
      projects.deleteProjectRoleToken(project, role, issuedAt, id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.projects.detail(project) }),
  });
}

// Accounts -------------------------------------------------------------------------------------

export function useCreateAccountToken(name: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateAccountTokenRequest) => session.createAccountToken(name, body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.accounts.all }),
  });
}

export function useDeleteAccountToken(name: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => session.deleteAccountToken(name, id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.accounts.all }),
  });
}

// SCM providers ------------------------------------------------------------------------------

export function useCreateSCMProvider() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: SCMProviderWriteRequest) => trust.createSCMProvider(body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.scmProviders.all }),
  });
}

export function useUpdateSCMProvider() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: SCMProviderWriteRequest }) => trust.updateSCMProvider(name, body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.scmProviders.all }),
  });
}

export function useDeleteSCMProvider() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => trust.deleteSCMProvider(name),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.scmProviders.all }),
  });
}
