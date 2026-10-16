package apiclient

import (
	"context"
	"net/http"

	corev1 "k8s.io/api/core/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Route patterns of the project API.
const (
	PathProjects              = "/api/v1/projects"
	PathProject               = "/api/v1/projects/{name}"
	PathProjectDetailed       = "/api/v1/projects/{name}/detailed"
	PathProjectGlobalProjects = "/api/v1/projects/{name}/globalprojects"
	PathProjectEvents         = "/api/v1/projects/{name}/events"
	PathProjectSyncWindows    = "/api/v1/projects/{name}/syncwindows"
	PathProjectLinks          = "/api/v1/projects/{name}/links"
	PathProjectRoleToken      = "/api/v1/projects/{project}/roles/{role}/token"
	PathProjectRoleTokenIAT   = "/api/v1/projects/{project}/roles/{role}/token/{iat}"
)

// ProjectListRequest is GET /api/v1/projects; the response is an AppProjectList of the
// projects the caller may get, sorted by name.
type ProjectListRequest struct {
	// Selector is a label selector on the AppProjects.
	Selector string `query:"selector" json:"-"`
	Limit    int64  `query:"limit" json:"-"`
	Continue string `query:"continue" json:"-"`
}

// ProjectRequest addresses one project.
type ProjectRequest struct {
	Name string `path:"name" json:"-"`
}

// ProjectCreateRequest is the body of POST /api/v1/projects (Argo CD's
// ProjectCreateRequest). status is ignored, and so is the deprecated
// spec.roles[].jwtTokens: tokens are created with the token route only.
type ProjectCreateRequest struct {
	Project *v1alpha1.AppProject `json:"project"`
	// Upsert replaces the spec of an existing project (needs projects, update).
	Upsert bool `json:"upsert,omitempty"`
}

// ProjectUpdateRequest is PUT /api/v1/projects/{name}. metadata.name of the project must
// be empty or the path name; metadata.resourceVersion, when set, must match (409
// otherwise). Only the spec, labels and annotations are written: status and the
// deprecated spec.roles[].jwtTokens keep their stored values.
type ProjectUpdateRequest struct {
	Name    string               `path:"name" json:"-"`
	Project *v1alpha1.AppProject `json:"project"`
}

// ProjectDetailed is the response of GET /api/v1/projects/{name}/detailed.
type ProjectDetailed struct {
	// Project is the effective project: the stored project with the lists of its global
	// projects appended.
	Project *v1alpha1.AppProject `json:"project"`
	// GlobalProjects are the global projects it inherits from (status removed).
	GlobalProjects []v1alpha1.AppProject `json:"globalProjects"`
	// Repositories are the repositories scoped to the project that the caller may get.
	Repositories []v1alpha1.Repository `json:"repositories"`
	// Clusters are the clusters scoped to the project that the caller may get.
	Clusters []RegisteredCluster `json:"clusters"`
}

// ProjectGlobalProjectsResponse is the response of GET
// /api/v1/projects/{name}/globalprojects.
type ProjectGlobalProjectsResponse struct {
	Items []v1alpha1.AppProject `json:"items"`
}

// ProjectSyncWindowsResponse is the response of GET /api/v1/projects/{name}/syncwindows:
// the project's windows active now (its own windows, without global projects).
type ProjectSyncWindowsResponse struct {
	Windows []v1alpha1.SyncWindow `json:"windows"`
}

// ProjectTokenCreateRequest is POST /api/v1/projects/{project}/roles/{role}/token.
type ProjectTokenCreateRequest struct {
	Project string `path:"project" json:"-"`
	Role    string `path:"role" json:"-"`

	// Description is accepted for compatibility and not stored.
	Description string `json:"description,omitempty"`
	// ExpiresIn is the lifetime in seconds; 0 creates a token without expiry.
	ExpiresIn int64 `json:"expiresIn,omitempty"`
	// ID is the token ID (jti); empty generates a UUID. It must be unique per role.
	ID string `json:"id,omitempty"`
}

// ProjectTokenResponse carries a new project role token. The token is returned only by
// this response; the project records its ID and times only.
type ProjectTokenResponse struct {
	Token string `json:"token"`
	// ID is the token's jti.
	ID string `json:"id,omitempty"`
	// IssuedAt and ExpiresAt are Unix timestamps (seconds); ExpiresAt 0 means no expiry.
	IssuedAt  int64 `json:"issuedAt,omitempty"`
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

// ProjectTokenDeleteRequest is DELETE /api/v1/projects/{project}/roles/{role}/token/{iat}.
// The token is located by ID when given, else by issued-at time. Deleting an unknown token
// or role succeeds.
type ProjectTokenDeleteRequest struct {
	Project string `path:"project" json:"-"`
	Role    string `path:"role" json:"-"`
	IAT     int64  `path:"iat" json:"-"`
	ID      string `query:"id" json:"-"`
}

// ListProjects lists the projects the caller may get.
func (c *Client) ListProjects(ctx context.Context, req *ProjectListRequest) (*v1alpha1.AppProjectList, error) {
	var out v1alpha1.AppProjectList
	return &out, c.Do(ctx, http.MethodGet, PathProjects, req, &out)
}

// CreateProject creates a project (with upsert: replaces the spec of an existing one).
func (c *Client) CreateProject(ctx context.Context, proj *v1alpha1.AppProject, upsert bool) (*v1alpha1.AppProject, error) {
	var out v1alpha1.AppProject
	return &out, c.Do(ctx, http.MethodPost, PathProjects, &ProjectCreateRequest{Project: proj, Upsert: upsert}, &out)
}

// GetProject returns a project.
func (c *Client) GetProject(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
	var out v1alpha1.AppProject
	return &out, c.Do(ctx, http.MethodGet, PathProject, &ProjectRequest{Name: name}, &out)
}

// UpdateProject updates a project's spec, labels and annotations.
func (c *Client) UpdateProject(ctx context.Context, proj *v1alpha1.AppProject) (*v1alpha1.AppProject, error) {
	var out v1alpha1.AppProject
	return &out, c.Do(ctx, http.MethodPut, PathProject, &ProjectUpdateRequest{Name: proj.Name, Project: proj}, &out)
}

// DeleteProject deletes a project that no Application or ApplicationSet references.
func (c *Client) DeleteProject(ctx context.Context, name string) error {
	return c.Do(ctx, http.MethodDelete, PathProject, &ProjectRequest{Name: name}, nil)
}

// GetDetailedProject returns the effective project with its global projects and the
// scoped repositories and clusters visible to the caller.
func (c *Client) GetDetailedProject(ctx context.Context, name string) (*ProjectDetailed, error) {
	var out ProjectDetailed
	return &out, c.Do(ctx, http.MethodGet, PathProjectDetailed, &ProjectRequest{Name: name}, &out)
}

// GetGlobalProjects returns the global projects a project inherits from.
func (c *Client) GetGlobalProjects(ctx context.Context, name string) (*ProjectGlobalProjectsResponse, error) {
	var out ProjectGlobalProjectsResponse
	return &out, c.Do(ctx, http.MethodGet, PathProjectGlobalProjects, &ProjectRequest{Name: name}, &out)
}

// ListProjectEvents lists the Kubernetes Events of a project.
func (c *Client) ListProjectEvents(ctx context.Context, name string) (*corev1.EventList, error) {
	var out corev1.EventList
	return &out, c.Do(ctx, http.MethodGet, PathProjectEvents, &ProjectRequest{Name: name}, &out)
}

// GetProjectSyncWindows returns the project's sync windows active now.
func (c *Client) GetProjectSyncWindows(ctx context.Context, name string) (*ProjectSyncWindowsResponse, error) {
	var out ProjectSyncWindowsResponse
	return &out, c.Do(ctx, http.MethodGet, PathProjectSyncWindows, &ProjectRequest{Name: name}, &out)
}

// ListProjectLinks evaluates the project deep links (project.links).
func (c *Client) ListProjectLinks(ctx context.Context, name string) (*LinksResponse, error) {
	var out LinksResponse
	return &out, c.Do(ctx, http.MethodGet, PathProjectLinks, &ProjectRequest{Name: name}, &out)
}

// CreateProjectToken creates a project role token.
func (c *Client) CreateProjectToken(ctx context.Context, req *ProjectTokenCreateRequest) (*ProjectTokenResponse, error) {
	var out ProjectTokenResponse
	return &out, c.Do(ctx, http.MethodPost, PathProjectRoleToken, req, &out)
}

// DeleteProjectToken deletes (revokes) a project role token, by id when given, else by
// issued-at time.
func (c *Client) DeleteProjectToken(ctx context.Context, project, role string, issuedAt int64, id string) error {
	return c.Do(ctx, http.MethodDelete, PathProjectRoleTokenIAT, &ProjectTokenDeleteRequest{Project: project, Role: role, IAT: issuedAt, ID: id}, nil)
}
