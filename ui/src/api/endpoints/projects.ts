import { request, seg, type CallOptions } from '../client';
import type {
  AppProject,
  AppProjectInput,
  AppProjectList,
  EventList,
  LinksResponse,
  ProjectCreateRequest,
  ProjectDetailed,
  ProjectGlobalProjectsResponse,
  ProjectSyncWindowsResponse,
  ProjectTokenCreateRequest,
  ProjectTokenResponse,
  ProjectUpdateRequest,
  QueryOf,
} from '../types';

export type ListProjectsQuery = QueryOf<'listProjects'>;

const base = (name: string) => `/api/v1/projects/${seg(name)}`;

/** GET /api/v1/projects */
export function listProjects(query: ListProjectsQuery = {}, options: CallOptions = {}): Promise<AppProjectList> {
  return request('/api/v1/projects', { query, signal: options.signal });
}

/** POST /api/v1/projects — body `{project, upsert}` (ProjectCreateRequest). */
export function createProject(project: AppProjectInput, query: { upsert?: boolean } = {}, options: CallOptions = {}): Promise<AppProject> {
  return request('/api/v1/projects', { method: 'POST', body: { project, upsert: query.upsert } satisfies ProjectCreateRequest, signal: options.signal });
}

/** GET /api/v1/projects/{name} */
export function getProject(name: string, options: CallOptions = {}): Promise<AppProject> {
  return request(base(name), { signal: options.signal });
}

/**
 * PUT /api/v1/projects/{name} — body `{project}` (ProjectUpdateRequest);
 * metadata.resourceVersion guards concurrent edits.
 */
export function updateProject(project: AppProjectInput, options: CallOptions = {}): Promise<AppProject> {
  return request(base(project.metadata?.name ?? ''), { method: 'PUT', body: { project } satisfies ProjectUpdateRequest, signal: options.signal });
}

/** DELETE /api/v1/projects/{name} */
export async function deleteProject(name: string, options: CallOptions = {}): Promise<void> {
  await request(base(name), { method: 'DELETE', signal: options.signal });
}

/** GET /api/v1/projects/{name}/detailed */
export function getDetailedProject(name: string, options: CallOptions = {}): Promise<ProjectDetailed> {
  return request(`${base(name)}/detailed`, { signal: options.signal });
}

/** GET /api/v1/projects/{name}/globalprojects */
export function getGlobalProjects(name: string, options: CallOptions = {}): Promise<ProjectGlobalProjectsResponse> {
  return request(`${base(name)}/globalprojects`, { signal: options.signal });
}

/** GET /api/v1/projects/{name}/events */
export function listProjectEvents(name: string, options: CallOptions = {}): Promise<EventList> {
  return request(`${base(name)}/events`, { signal: options.signal });
}

/** GET /api/v1/projects/{name}/syncwindows */
export function getProjectSyncWindows(name: string, options: CallOptions = {}): Promise<ProjectSyncWindowsResponse> {
  return request(`${base(name)}/syncwindows`, { signal: options.signal });
}

/** GET /api/v1/projects/{name}/links */
export function listProjectLinks(name: string, options: CallOptions = {}): Promise<LinksResponse> {
  return request(`${base(name)}/links`, { signal: options.signal });
}

/** POST /api/v1/projects/{project}/roles/{role}/token */
export function createProjectRoleToken(
  project: string,
  role: string,
  body: ProjectTokenCreateRequest,
  options: CallOptions = {},
): Promise<ProjectTokenResponse> {
  return request(`${base(project)}/roles/${seg(role)}/token`, { method: 'POST', body, signal: options.signal });
}

/** DELETE /api/v1/projects/{project}/roles/{role}/token/{iat} */
export async function deleteProjectRoleToken(
  project: string,
  role: string,
  issuedAt: number,
  id?: string,
  options: CallOptions = {},
): Promise<void> {
  await request(`${base(project)}/roles/${seg(role)}/token/${issuedAt}`, { method: 'DELETE', query: { id }, signal: options.signal });
}
