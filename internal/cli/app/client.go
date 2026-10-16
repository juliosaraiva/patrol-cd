package app

import (
	"context"
	"io"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Client is the API surface used by the app and appset commands. Its methods mirror
// *apiclient.Client (same names and request types); only the streams are narrowed to small
// interfaces so tests can fake them. NewAPIClient adapts the real client.
type Client interface {
	ApplicationClient
	ResourceClient
	ApplicationSetClient
	// GetSettings returns the server settings (URL, tracking, resource overrides).
	GetSettings(ctx context.Context) (*apiclient.Settings, error)
	// GetCluster returns a registered cluster (version and API versions for local rendering).
	GetCluster(ctx context.Context, id apiclient.ClusterID) (*apiclient.RegisteredCluster, error)
	// BaseURL is the server base URL (default Application URLs when settings have none).
	BaseURL() string
}

// ApplicationClient covers the Application routes (api-contract.md, "Applications").
type ApplicationClient interface {
	ListApplications(ctx context.Context, q *apiclient.ApplicationQuery) (*v1alpha1.ApplicationList, error)
	GetApplication(ctx context.Context, req *apiclient.ApplicationGetRequest) (*v1alpha1.Application, error)
	CreateApplication(ctx context.Context, req *apiclient.ApplicationCreateRequest) (*v1alpha1.Application, error)
	UpdateApplication(ctx context.Context, req *apiclient.ApplicationUpdateRequest) (*v1alpha1.Application, error)
	UpdateApplicationSpec(ctx context.Context, req *apiclient.ApplicationUpdateSpecRequest) (*v1alpha1.ApplicationSpec, error)
	PatchApplication(ctx context.Context, req *apiclient.ApplicationPatchRequest) (*v1alpha1.Application, error)
	DeleteApplication(ctx context.Context, req *apiclient.ApplicationDeleteRequest) error
	SyncApplication(ctx context.Context, req *apiclient.ApplicationSyncRequest) (*apiclient.ApplicationOperationResponse, error)
	RollbackApplication(ctx context.Context, req *apiclient.ApplicationRollbackRequest) (*apiclient.ApplicationOperationResponse, error)
	TerminateApplicationOperation(ctx context.Context, req *apiclient.ApplicationRef) (*apiclient.ApplicationOperationResponse, error)
	ListApplicationOperations(ctx context.Context, req *apiclient.ApplicationOperationsRequest) (*v1alpha1.OperationList, error)
	GetApplicationOperation(ctx context.Context, req *apiclient.ApplicationOperationRequest) (*v1alpha1.Operation, error)
	GetApplicationSyncWindows(ctx context.Context, req *apiclient.ApplicationRef) (*apiclient.ApplicationSyncWindowsResponse, error)
	GetApplicationManifests(ctx context.Context, req *apiclient.ApplicationManifestQuery) (*apiclient.ApplicationManifestResponse, error)
	GetApplicationManifestsWithFiles(ctx context.Context, req *apiclient.ApplicationManifestsWithFilesQuery, tarGz io.Reader) (*apiclient.ApplicationManifestResponse, error)
	// WatchApplications opens the Application event stream (GET /api/v1/stream/applications).
	WatchApplications(ctx context.Context, q *apiclient.ApplicationQuery, opts *apiclient.StreamOptions) (ApplicationEventStream, error)
}

// ApplicationEventStream yields Application watch events. Next returns io.EOF when the
// server ends the stream.
type ApplicationEventStream interface {
	Next() (*apiclient.ApplicationWatchEvent, error)
	Close() error
}

// ResourceClient covers the resource-level Application routes (resource tree, managed
// resources, server-side diff, live resources, actions, logs).
type ResourceClient interface {
	GetApplicationResourceTree(ctx context.Context, q *apiclient.ApplicationTreeQuery) (*apiclient.ApplicationTree, error)
	GetApplicationManagedResources(ctx context.Context, q *apiclient.ApplicationManagedResourcesQuery) (*apiclient.ManagedResourcesResponse, error)
	ApplicationServerSideDiff(ctx context.Context, req *apiclient.ApplicationServerSideDiffRequest) (*apiclient.ServerSideDiffResponse, error)
	GetApplicationResource(ctx context.Context, q *apiclient.ApplicationResourceRequest) (*apiclient.ApplicationResourceResponse, error)
	PatchApplicationResource(ctx context.Context, req *apiclient.ApplicationResourcePatchRequest) (*apiclient.ApplicationResourceResponse, error)
	DeleteApplicationResource(ctx context.Context, req *apiclient.ApplicationResourceDeleteRequest) error
	ListApplicationResourceActions(ctx context.Context, q *apiclient.ApplicationResourceRequest) (*apiclient.ResourceActionsListResponse, error)
	RunApplicationResourceAction(ctx context.Context, req *apiclient.ResourceActionRunRequest) error
	// StreamApplicationLogs opens the log stream of an Application's pods.
	StreamApplicationLogs(ctx context.Context, q *apiclient.ApplicationLogsQuery) (LogStream, error)
}

// LogStream yields log entries; Next returns io.EOF at the end of the stream.
type LogStream interface {
	Next() (*apiclient.LogEntry, error)
	Close() error
}

// ApplicationSetClient covers the ApplicationSet routes.
type ApplicationSetClient interface {
	ListApplicationSets(ctx context.Context, req *apiclient.ApplicationSetListRequest) (*v1alpha1.ApplicationSetList, error)
	GetApplicationSet(ctx context.Context, name, namespace string) (*v1alpha1.ApplicationSet, error)
	CreateApplicationSet(ctx context.Context, req *apiclient.ApplicationSetCreateRequest) (*v1alpha1.ApplicationSet, error)
	DeleteApplicationSet(ctx context.Context, req *apiclient.ApplicationSetDeleteRequest) error
	GenerateApplicationSet(ctx context.Context, appSet *v1alpha1.ApplicationSet) (*apiclient.ApplicationSetGenerateResponse, error)
	WatchApplicationSets(ctx context.Context, req *apiclient.ApplicationSetWatchRequest, opts *apiclient.StreamOptions) (ApplicationSetEventStream, error)
}

// ApplicationSetEventStream yields ApplicationSet watch events.
type ApplicationSetEventStream interface {
	Next() (*apiclient.ApplicationSetWatchEvent, error)
	Close() error
}

// AppRef addresses an Application.
type AppRef struct {
	Name      string
	Namespace string
	// Project, when set, turns "permission denied" for a missing Application into
	// "not found" (upstream --project on resource commands).
	Project string
}

// resourceTree returns the resource tree of an Application.
func resourceTree(ctx context.Context, cl Client, app AppRef) (*apiclient.ApplicationTree, error) {
	return cl.GetApplicationResourceTree(ctx, &apiclient.ApplicationTreeQuery{Name: app.Name, AppNamespace: app.Namespace, Project: app.Project})
}

// managedResources returns the managed resources (diff data) of an Application.
func managedResources(ctx context.Context, cl Client, app AppRef) ([]apiclient.ResourceDiff, error) {
	res, err := cl.GetApplicationManagedResources(ctx, &apiclient.ApplicationManagedResourcesQuery{Name: app.Name, AppNamespace: app.Namespace, Project: app.Project})
	if err != nil {
		return nil, err
	}
	return res.Items, nil
}

// resourceRequest addresses a live resource of an Application.
func resourceRequest(app AppRef, group, version, kind, namespace, name string) *apiclient.ApplicationResourceRequest {
	return &apiclient.ApplicationResourceRequest{
		Name: app.Name, AppNamespace: app.Namespace, Project: app.Project,
		Group: group, Version: version, Kind: kind, Namespace: namespace, ResourceName: name,
	}
}
