package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Route patterns of the application API (api-contract.md, "Applications"). Resource-level
// routes (resource tree, managed resources, live resources, actions, logs, events) are
// declared with their own service.
const (
	PathApplications                   = "/api/v1/applications"
	PathApplication                    = "/api/v1/applications/{name}"
	PathApplicationSpec                = "/api/v1/applications/{name}/spec"
	PathApplicationSync                = "/api/v1/applications/{name}/sync"
	PathApplicationRollback            = "/api/v1/applications/{name}/rollback"
	PathApplicationOperation           = "/api/v1/applications/{name}/operation"
	PathApplicationOperations          = "/api/v1/applications/{name}/operations"
	PathApplicationOperationByName     = "/api/v1/applications/{name}/operations/{op}"
	PathApplicationSyncWindows         = "/api/v1/applications/{name}/syncwindows"
	PathApplicationRevisionMetadata    = "/api/v1/applications/{name}/revisions/{revision}/metadata"
	PathApplicationRevisionChart       = "/api/v1/applications/{name}/revisions/{revision}/chartdetails"
	PathApplicationRevisionOCIMetadata = "/api/v1/applications/{name}/revisions/{revision}/ocimetadata"
	PathApplicationManifests           = "/api/v1/applications/{name}/manifests"
	PathApplicationManifestsWithFiles  = "/api/v1/applications/{name}/manifests-with-files"
	PathApplicationLinks               = "/api/v1/applications/{name}/links"
	PathApplicationResourceLinks       = "/api/v1/applications/{name}/resource/links"
	PathStreamApplications             = "/api/v1/stream/applications"
)

// ContentTypeGzip is the media type of the local source archive sent to
// POST /api/v1/applications/{name}/manifests-with-files.
const ContentTypeGzip = "application/gzip"

// ApplicationQuery selects Applications for GET /api/v1/applications and the watch stream.
type ApplicationQuery struct {
	// Name keeps only the Application with this name.
	Name string `query:"name" json:"-"`
	// Projects keeps only Applications of these projects.
	Projects []string `query:"projects" json:"-"`
	// Project is the legacy spelling of Projects; when set it wins (upstream).
	Project []string `query:"project" json:"-"`
	// Selector is a Kubernetes label selector.
	Selector string `query:"selector" json:"-"`
	// Repo keeps only Applications with a source of this repository URL.
	Repo string `query:"repo" json:"-"`
	// AppNamespace restricts the query to one namespace (default: every enabled namespace).
	AppNamespace string `query:"appNamespace" json:"-"`
	// ResourceVersion: the watch stream skips Applications older than it and, without Name,
	// sends no initial ADDED events.
	ResourceVersion string `query:"resourceVersion" json:"-"`
	// Limit bounds the number of items of a list response (0: all). The response's
	// metadata.continue resumes the listing.
	Limit int64 `query:"limit" json:"-"`
	// Continue is the opaque token of a previous list response.
	Continue string `query:"continue" json:"-"`
}

// ApplicationGetRequest addresses GET /api/v1/applications/{name}.
type ApplicationGetRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	// Project is the expected project (at most one value). With a project the answer for
	// an Application the caller may not see in that project is 404, without one 403.
	Project []string `query:"project" json:"-"`
	// Projects is the newer spelling of Project.
	Projects []string `query:"projects" json:"-"`
	// Refresh ("normal" or "hard") requests a refresh and waits (bounded) until the
	// controller reconciled the Application.
	Refresh string `query:"refresh" json:"-"`
	// ResourceVersion reads the Application at this resource version.
	ResourceVersion string `query:"resourceVersion" json:"-"`
}

// ApplicationCreateRequest is POST /api/v1/applications.
type ApplicationCreateRequest struct {
	// Upsert updates an existing Application whose spec differs (requires update).
	Upsert *bool `query:"upsert" json:"-"`
	// Validate checks repositories and manifest generation (default true).
	Validate *bool `query:"validate" json:"-"`
	// AppNamespace is used when the body has no metadata.namespace.
	AppNamespace string `query:"appNamespace" json:"-"`

	Application *v1alpha1.Application `body:"true" json:"-"`
}

// ApplicationUpdateRequest is PUT /api/v1/applications/{name}: spec, labels, annotations
// and finalizers of the body replace the stored ones; status is ignored. A body
// metadata.resourceVersion makes the update conditional (409 when stale).
type ApplicationUpdateRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	// Project is the project the caller expects the Application to be in.
	Project  string `query:"project" json:"-"`
	Validate *bool  `query:"validate" json:"-"`

	Application *v1alpha1.Application `body:"true" json:"-"`
}

// ApplicationUpdateSpecRequest is PUT /api/v1/applications/{name}/spec.
type ApplicationUpdateSpecRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	Validate     *bool  `query:"validate" json:"-"`

	Spec *v1alpha1.ApplicationSpec `body:"true" json:"-"`
}

// Patch types of ApplicationPatchRequest.
const (
	PatchTypeJSON  = "json"
	PatchTypeMerge = "merge"
)

// ApplicationPatchRequest is PATCH /api/v1/applications/{name}. The Argo CD body fields
// name, appNamespace and project are accepted and must agree with the path and query.
type ApplicationPatchRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`

	// Patch is an RFC 6902 JSON patch (patchType json, the default) or an RFC 7386 merge
	// patch (patchType merge) applied to the Application JSON.
	Patch     string `json:"patch"`
	PatchType string `json:"patchType,omitempty"`

	BodyName         string `json:"name,omitempty"`
	BodyAppNamespace string `json:"appNamespace,omitempty"`
	BodyProject      string `json:"project,omitempty"`
}

// ApplicationDeleteRequest is DELETE /api/v1/applications/{name}.
type ApplicationDeleteRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	// Cascade deletes the managed resources (default true).
	Cascade *bool `query:"cascade" json:"-"`
	// PropagationPolicy is foreground (default) or background for cascading deletes.
	PropagationPolicy string `query:"propagationPolicy" json:"-"`
}

// ApplicationResponse is an empty acknowledgement.
type ApplicationResponse struct{}

// ApplicationSyncOptions wraps sync options as the Argo CD API does ({items: [...]}).
// When present in a sync request, Items replace the Application's sync options.
type ApplicationSyncOptions struct {
	Items []string `json:"items,omitempty"`
}

// ApplicationSyncRequest is POST /api/v1/applications/{name}/sync. Its fields mirror
// Operation spec.sync. The Argo CD body fields name, appNamespace and project are accepted
// and must agree with the path and query.
type ApplicationSyncRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`

	// Revision overrides the target revision of a single-source Application.
	Revision string `json:"revision,omitempty"`
	// Revisions override the target revision of the sources at SourcePositions (1-based)
	// of a multi-source Application.
	Revisions       []string `json:"revisions,omitempty"`
	SourcePositions []int64  `json:"sourcePositions,omitempty"`
	DryRun          bool     `json:"dryRun,omitempty"`
	Prune           bool     `json:"prune,omitempty"`
	// Strategy selects hook (default) or apply.
	Strategy *v1alpha1.SyncStrategy `json:"strategy,omitempty"`
	// Resources restricts the sync to these resources (selective sync).
	Resources []v1alpha1.SyncOperationResource `json:"resources,omitempty"`
	// Manifests are local manifests to sync instead of rendering (requires override; at
	// most 1 MiB in total).
	Manifests []string `json:"manifests,omitempty"`
	// Source and Sources override the Application's sources (Patrol addition; requires
	// override).
	Source  *v1alpha1.ApplicationSource `json:"source,omitempty"`
	Sources v1alpha1.ApplicationSources `json:"sources,omitempty"`
	// Infos are recorded on the Operation.
	Infos []v1alpha1.Info `json:"infos,omitempty"`
	// RetryStrategy replaces the Application's syncPolicy.retry for this operation.
	RetryStrategy *v1alpha1.RetryStrategy `json:"retryStrategy,omitempty"`
	// SyncOptions, when present, replace the Application's sync options.
	SyncOptions *ApplicationSyncOptions `json:"syncOptions,omitempty"`

	BodyName         string `json:"name,omitempty"`
	BodyAppNamespace string `json:"appNamespace,omitempty"`
	BodyProject      string `json:"project,omitempty"`
}

// ApplicationRollbackRequest is POST /api/v1/applications/{name}/rollback.
type ApplicationRollbackRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`

	// ID is the status.history entry to roll back to.
	ID     int64 `json:"id"`
	Prune  bool  `json:"prune,omitempty"`
	DryRun bool  `json:"dryRun,omitempty"`

	BodyName         string `json:"name,omitempty"`
	BodyAppNamespace string `json:"appNamespace,omitempty"`
	BodyProject      string `json:"project,omitempty"`
}

// ApplicationOperationResponse is the result of a sync, rollback or termination request:
// the Operation (created or cancelled) and the Application.
type ApplicationOperationResponse struct {
	Application *v1alpha1.Application `json:"application,omitempty"`
	Operation   *v1alpha1.Operation   `json:"operation,omitempty"`
}

// ApplicationRef addresses one Application (terminate, sync windows, links).
type ApplicationRef struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
}

// ApplicationOperationsRequest is GET /api/v1/applications/{name}/operations.
type ApplicationOperationsRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	// Limit bounds the number of Operations returned (default 100, at most 1000), newest
	// first.
	Limit int64 `query:"limit" json:"-"`
	// Continue is the opaque token of a previous response.
	Continue string `query:"continue" json:"-"`
}

// ApplicationOperationRequest is GET /api/v1/applications/{name}/operations/{op}.
type ApplicationOperationRequest struct {
	Name         string `path:"name" json:"-"`
	Operation    string `path:"op" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
}

// ApplicationSyncWindow is a project sync window that applies to the Application.
type ApplicationSyncWindow struct {
	Kind       string `json:"kind"`
	Schedule   string `json:"schedule"`
	Duration   string `json:"duration"`
	ManualSync bool   `json:"manualSync,omitempty"`
	TimeZone   string `json:"timeZone,omitempty"`
}

// ApplicationSyncWindowsResponse is GET /api/v1/applications/{name}/syncwindows.
type ApplicationSyncWindowsResponse struct {
	ActiveWindows   []ApplicationSyncWindow `json:"activeWindows,omitempty"`
	AssignedWindows []ApplicationSyncWindow `json:"assignedWindows,omitempty"`
	// CanSync is the manual sync decision now.
	CanSync bool `json:"canSync"`
	// Status is "Sync Allowed", "Manual Allowed" or "Sync Denied" (Patrol addition).
	Status string `json:"status,omitempty"`
	// NextTransition is the next instant an assigned window opens or closes (Patrol
	// addition; absent when none is known).
	NextTransition *metav1.Time `json:"nextTransition,omitempty"`
}

// ApplicationRevisionRequest addresses the revision metadata routes.
type ApplicationRevisionRequest struct {
	Name         string `path:"name" json:"-"`
	Revision     string `path:"revision" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	// SourceIndex selects the source (0-based; default 0).
	SourceIndex *int32 `query:"sourceIndex" json:"-"`
	// VersionID selects the sources of a status.history entry instead of the spec.
	VersionID *int32 `query:"versionId" json:"-"`
}

// ApplicationRevisionReference is a commit referenced by a revision (hydrated commits).
type ApplicationRevisionReference struct {
	RepoURL string       `json:"repoURL,omitempty"`
	SHA     string       `json:"sha,omitempty"`
	Author  string       `json:"author,omitempty"`
	Date    *metav1.Time `json:"date,omitempty"`
	Subject string       `json:"subject,omitempty"`
	Body    string       `json:"body,omitempty"`
	Path    string       `json:"path,omitempty"`
}

// ApplicationSourceIntegrityCheck is one source integrity check and its problems.
type ApplicationSourceIntegrityCheck struct {
	Name     string   `json:"name"`
	Problems []string `json:"problems,omitempty"`
}

// ApplicationSourceIntegrityResult is the source integrity verification of a revision.
type ApplicationSourceIntegrityResult struct {
	Checks []ApplicationSourceIntegrityCheck `json:"checks,omitempty"`
}

// ApplicationRevisionMetadata is Git commit metadata.
type ApplicationRevisionMetadata struct {
	Author                string                            `json:"author,omitempty"`
	Date                  *metav1.Time                      `json:"date,omitempty"`
	Tags                  []string                          `json:"tags,omitempty"`
	Message               string                            `json:"message,omitempty"`
	SignatureInfo         string                            `json:"signatureInfo,omitempty"`
	References            []ApplicationRevisionReference    `json:"references,omitempty"`
	SourceIntegrityResult *ApplicationSourceIntegrityResult `json:"sourceIntegrityResult,omitempty"`
}

// ApplicationChartDetails is Helm chart metadata.
type ApplicationChartDetails struct {
	Description string   `json:"description,omitempty"`
	Home        string   `json:"home,omitempty"`
	Maintainers []string `json:"maintainers,omitempty"`
}

// ApplicationOCIMetadata is the annotation metadata of an OCI artifact.
type ApplicationOCIMetadata struct {
	CreatedAt   string `json:"createdAt,omitempty"`
	Authors     string `json:"authors,omitempty"`
	ImageURL    string `json:"imageUrl,omitempty"`
	DocsURL     string `json:"docsUrl,omitempty"`
	SourceURL   string `json:"sourceUrl,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// ApplicationManifestQuery is GET /api/v1/applications/{name}/manifests.
type ApplicationManifestQuery struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	// Revision renders a single-source Application at this revision.
	Revision string `query:"revision" json:"-"`
	// Revisions render the sources at SourcePositions (1-based) of a multi-source
	// Application at these revisions.
	Revisions       []string `query:"revisions" json:"-"`
	SourcePositions []int64  `query:"sourcePositions" json:"-"`
	// NoCache bypasses the repo-server manifest cache.
	NoCache bool `query:"noCache" json:"-"`
}

// ApplicationManifestsWithFilesQuery documents the query of
// POST /api/v1/applications/{name}/manifests-with-files (body: gzipped tar of the source
// directory, Content-Type application/gzip).
type ApplicationManifestsWithFilesQuery struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	// Checksum is the hex SHA-256 of the body; a mismatch is rejected.
	Checksum string `query:"checksum" json:"-"`
}

// ApplicationManifestResponse holds rendered manifests (JSON documents; Secret data
// masked).
type ApplicationManifestResponse struct {
	Manifests []string `json:"manifests"`
	// Revision is the rendered revision of a single-source Application.
	Revision string `json:"revision,omitempty"`
	// Revisions are the rendered revisions per source of a multi-source Application.
	Revisions []string `json:"revisions,omitempty"`
	// SourceType(s) are the detected rendering tools.
	SourceType  string   `json:"sourceType,omitempty"`
	SourceTypes []string `json:"sourceTypes,omitempty"`
	// Namespace and Server are the destination of the rendering.
	Namespace string `json:"namespace,omitempty"`
	Server    string `json:"server,omitempty"`
}

// ApplicationLinksRequest is GET /api/v1/applications/{name}/links. Namespace is the
// Argo CD name of the application namespace parameter of this route; AppNamespace wins.
type ApplicationLinksRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	Project      string `query:"project" json:"-"`
}

// ApplicationResourceRequest identifies a resource of an Application
// (GET /api/v1/applications/{name}/resource/links).
type ApplicationResourceRequest struct {
	Name         string `path:"name" json:"-"`
	AppNamespace string `query:"appNamespace" json:"-"`
	Project      string `query:"project" json:"-"`
	ResourceName string `query:"resourceName" json:"-"`
	Namespace    string `query:"namespace" json:"-"`
	Version      string `query:"version" json:"-"`
	Group        string `query:"group" json:"-"`
	Kind         string `query:"kind" json:"-"`
}

// ApplicationWatchEvent is one event of GET /api/v1/stream/applications.
type ApplicationWatchEvent = WatchEvent[v1alpha1.Application]

// ListApplications lists the Applications the caller may get.
func (c *Client) ListApplications(ctx context.Context, q *ApplicationQuery) (*v1alpha1.ApplicationList, error) {
	var out v1alpha1.ApplicationList
	return &out, c.Do(ctx, http.MethodGet, PathApplications, q, &out)
}

// WatchApplications opens the Application event stream; read it with Watch.
func (c *Client) WatchApplications(ctx context.Context, q *ApplicationQuery, opts *StreamOptions) (*EventStream, error) {
	return c.Stream(ctx, PathStreamApplications, q, opts)
}

// GetApplication returns one Application (refreshing it first when req.Refresh is set).
func (c *Client) GetApplication(ctx context.Context, req *ApplicationGetRequest) (*v1alpha1.Application, error) {
	var out v1alpha1.Application
	return &out, c.Do(ctx, http.MethodGet, PathApplication, req, &out)
}

// CreateApplication creates an Application.
func (c *Client) CreateApplication(ctx context.Context, req *ApplicationCreateRequest) (*v1alpha1.Application, error) {
	var out v1alpha1.Application
	return &out, c.Do(ctx, http.MethodPost, PathApplications, req, &out)
}

// UpdateApplication replaces the spec, labels, annotations and finalizers of an
// Application. req.Name defaults to the body's metadata.name.
func (c *Client) UpdateApplication(ctx context.Context, req *ApplicationUpdateRequest) (*v1alpha1.Application, error) {
	if req != nil && req.Name == "" && req.Application != nil {
		r := *req
		r.Name = req.Application.Name
		req = &r
	}
	var out v1alpha1.Application
	return &out, c.Do(ctx, http.MethodPut, PathApplication, req, &out)
}

// UpdateApplicationSpec replaces the spec of an Application and returns the stored spec.
func (c *Client) UpdateApplicationSpec(ctx context.Context, req *ApplicationUpdateSpecRequest) (*v1alpha1.ApplicationSpec, error) {
	var out v1alpha1.ApplicationSpec
	return &out, c.Do(ctx, http.MethodPut, PathApplicationSpec, req, &out)
}

// PatchApplication applies a JSON or merge patch to an Application.
func (c *Client) PatchApplication(ctx context.Context, req *ApplicationPatchRequest) (*v1alpha1.Application, error) {
	var out v1alpha1.Application
	return &out, c.Do(ctx, http.MethodPatch, PathApplication, req, &out)
}

// DeleteApplication deletes an Application.
func (c *Client) DeleteApplication(ctx context.Context, req *ApplicationDeleteRequest) error {
	return c.Do(ctx, http.MethodDelete, PathApplication, req, nil)
}

// SyncApplication requests a sync (creates an Operation).
func (c *Client) SyncApplication(ctx context.Context, req *ApplicationSyncRequest) (*ApplicationOperationResponse, error) {
	var out ApplicationOperationResponse
	return &out, c.Do(ctx, http.MethodPost, PathApplicationSync, req, &out)
}

// RollbackApplication requests a rollback to a history entry (creates an Operation).
func (c *Client) RollbackApplication(ctx context.Context, req *ApplicationRollbackRequest) (*ApplicationOperationResponse, error) {
	var out ApplicationOperationResponse
	return &out, c.Do(ctx, http.MethodPost, PathApplicationRollback, req, &out)
}

// TerminateApplicationOperation requests cancellation of the running Operation.
func (c *Client) TerminateApplicationOperation(ctx context.Context, req *ApplicationRef) (*ApplicationOperationResponse, error) {
	var out ApplicationOperationResponse
	return &out, c.Do(ctx, http.MethodDelete, PathApplicationOperation, req, &out)
}

// ListApplicationOperations lists the Operations of an Application, newest first.
func (c *Client) ListApplicationOperations(ctx context.Context, req *ApplicationOperationsRequest) (*v1alpha1.OperationList, error) {
	var out v1alpha1.OperationList
	return &out, c.Do(ctx, http.MethodGet, PathApplicationOperations, req, &out)
}

// GetApplicationOperation returns one Operation of an Application.
func (c *Client) GetApplicationOperation(ctx context.Context, req *ApplicationOperationRequest) (*v1alpha1.Operation, error) {
	var out v1alpha1.Operation
	return &out, c.Do(ctx, http.MethodGet, PathApplicationOperationByName, req, &out)
}

// GetApplicationSyncWindows returns the sync window state of an Application.
func (c *Client) GetApplicationSyncWindows(ctx context.Context, req *ApplicationRef) (*ApplicationSyncWindowsResponse, error) {
	var out ApplicationSyncWindowsResponse
	return &out, c.Do(ctx, http.MethodGet, PathApplicationSyncWindows, req, &out)
}

// GetApplicationRevisionMetadata returns Git commit metadata of a revision of a source.
func (c *Client) GetApplicationRevisionMetadata(ctx context.Context, req *ApplicationRevisionRequest) (*ApplicationRevisionMetadata, error) {
	var out ApplicationRevisionMetadata
	return &out, c.Do(ctx, http.MethodGet, PathApplicationRevisionMetadata, req, &out)
}

// GetApplicationRevisionChartDetails returns Helm chart metadata of a revision of a source.
func (c *Client) GetApplicationRevisionChartDetails(ctx context.Context, req *ApplicationRevisionRequest) (*ApplicationChartDetails, error) {
	var out ApplicationChartDetails
	return &out, c.Do(ctx, http.MethodGet, PathApplicationRevisionChart, req, &out)
}

// GetApplicationRevisionOCIMetadata returns OCI metadata of a revision of a source.
func (c *Client) GetApplicationRevisionOCIMetadata(ctx context.Context, req *ApplicationRevisionRequest) (*ApplicationOCIMetadata, error) {
	var out ApplicationOCIMetadata
	return &out, c.Do(ctx, http.MethodGet, PathApplicationRevisionOCIMetadata, req, &out)
}

// GetApplicationManifests renders the target manifests of an Application.
func (c *Client) GetApplicationManifests(ctx context.Context, req *ApplicationManifestQuery) (*ApplicationManifestResponse, error) {
	var out ApplicationManifestResponse
	return &out, c.Do(ctx, http.MethodGet, PathApplicationManifests, req, &out)
}

// GetApplicationManifestsWithFiles renders the Application's source from a gzipped tar of
// a local directory.
func (c *Client) GetApplicationManifestsWithFiles(ctx context.Context, req *ApplicationManifestsWithFilesQuery, tarGz io.Reader) (*ApplicationManifestResponse, error) {
	resp, err := c.DoRaw(ctx, http.MethodPost, PathApplicationManifestsWithFiles, req, tarGz, ContentTypeGzip)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out ApplicationManifestResponse
	if err := decodeApplicationJSON(resp.Body, c.maxBody, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListApplicationLinks evaluates application.links for an Application.
func (c *Client) ListApplicationLinks(ctx context.Context, req *ApplicationLinksRequest) (*LinksResponse, error) {
	var out LinksResponse
	return &out, c.Do(ctx, http.MethodGet, PathApplicationLinks, req, &out)
}

// ListApplicationResourceLinks evaluates resource.links for a resource of an Application
// (or the Application itself).
func (c *Client) ListApplicationResourceLinks(ctx context.Context, req *ApplicationResourceRequest) (*LinksResponse, error) {
	var out LinksResponse
	return &out, c.Do(ctx, http.MethodGet, PathApplicationResourceLinks, req, &out)
}

// decodeApplicationJSON decodes a bounded JSON response body.
func decodeApplicationJSON(body io.Reader, limit int64, out any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("response exceeds %d bytes", limit)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
