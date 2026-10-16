package apiclient

import (
	"context"
	"net/http"

	corev1 "k8s.io/api/core/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Route patterns of the ApplicationSet API (api-contract.md, "ApplicationSets").
const (
	PathApplicationSets            = "/api/v1/applicationsets"
	PathApplicationSet             = "/api/v1/applicationsets/{name}"
	PathApplicationSetGenerate     = "/api/v1/applicationsets/generate"
	PathApplicationSetResourceTree = "/api/v1/applicationsets/{name}/resource-tree"
	PathApplicationSetEvents       = "/api/v1/applicationsets/{name}/events"
	PathApplicationSetsStream      = "/api/v1/stream/applicationsets"
)

// ApplicationSetListRequest is GET /api/v1/applicationsets.
type ApplicationSetListRequest struct {
	// Projects keeps only ApplicationSets whose spec.template.spec.project is one of these
	// (an empty project is "default").
	Projects []string `query:"projects" json:"-"`
	// Selector is a Kubernetes label selector.
	Selector string `query:"selector" json:"-"`
	// AppsetNamespace restricts the list to one namespace (default: every enabled namespace).
	AppsetNamespace string `query:"appsetNamespace" json:"-"`
	// Limit bounds the number of items returned (0: no limit).
	Limit int64 `query:"limit" json:"-"`
	// Continue is the opaque token of a previous page's metadata.continue.
	Continue string `query:"continue" json:"-"`
}

// ApplicationSetGetRequest addresses one ApplicationSet (GET .../{name}, .../resource-tree,
// .../events).
type ApplicationSetGetRequest struct {
	Name string `path:"name" json:"-"`
	// AppsetNamespace is the ApplicationSet namespace (default: the control-plane namespace).
	AppsetNamespace string `query:"appsetNamespace" json:"-"`
}

// ApplicationSetCreateRequest is POST /api/v1/applicationsets.
type ApplicationSetCreateRequest struct {
	// Upsert updates an existing ApplicationSet whose spec differs (requires update).
	Upsert bool `query:"upsert" json:"-"`
	// DryRun renders the generated Applications into status.resources without persisting.
	DryRun bool `query:"dryRun" json:"-"`
	// ApplicationSet is the object to create; metadata.namespace selects the namespace
	// (default: the control-plane namespace). A metadata.resourceVersion makes an upsert
	// conditional (409 conflict when the stored object changed).
	ApplicationSet *v1alpha1.ApplicationSet `body:"true" json:"-"`
}

// ApplicationSetDeleteRequest is DELETE /api/v1/applicationsets/{name}.
type ApplicationSetDeleteRequest struct {
	Name            string `path:"name" json:"-"`
	AppsetNamespace string `query:"appsetNamespace" json:"-"`
	// Cascade (default true) deletes the generated Applications through their owner
	// references; false orphans them.
	Cascade *bool `query:"cascade" json:"-"`
	// PropagationPolicy is "foreground" or "background" (default) for cascading deletes.
	PropagationPolicy string `query:"propagationPolicy" json:"-"`
}

// ApplicationSetGenerateRequest is the body of POST /api/v1/applicationsets/generate.
type ApplicationSetGenerateRequest struct {
	ApplicationSet *v1alpha1.ApplicationSet `json:"applicationSet"`
}

// ApplicationSetGenerateResponse lists the Applications an ApplicationSet would generate.
type ApplicationSetGenerateResponse struct {
	Applications []v1alpha1.Application `json:"applications"`
}

// ApplicationSetTreeRef references a node's parent (the ApplicationSet).
type ApplicationSetTreeRef struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}

// ApplicationSetTreeInfo is a name/value pair shown with a node.
type ApplicationSetTreeInfo struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ApplicationSetTreeNode is one generated Application in the ApplicationSet resource tree.
type ApplicationSetTreeNode struct {
	Group      string                  `json:"group,omitempty"`
	Version    string                  `json:"version,omitempty"`
	Kind       string                  `json:"kind"`
	Namespace  string                  `json:"namespace,omitempty"`
	Name       string                  `json:"name"`
	ParentRefs []ApplicationSetTreeRef `json:"parentRefs,omitempty"`
	// Info carries the sync status of the Application ("Sync Status").
	Info   []ApplicationSetTreeInfo `json:"info,omitempty"`
	Health *v1alpha1.HealthStatus   `json:"health,omitempty"`
}

// FullName is the sort key of tree nodes: "group/kind/namespace/name".
func (n *ApplicationSetTreeNode) FullName() string {
	return n.Group + "/" + n.Kind + "/" + n.Namespace + "/" + n.Name
}

// ApplicationSetTree is the response of GET /api/v1/applicationsets/{name}/resource-tree.
type ApplicationSetTree struct {
	Nodes []ApplicationSetTreeNode `json:"nodes"`
}

// ApplicationSetWatchRequest is GET /api/v1/stream/applicationsets.
type ApplicationSetWatchRequest struct {
	// Name restricts the stream to one ApplicationSet and sends its current state first.
	Name     string   `query:"name" json:"-"`
	Projects []string `query:"projects" json:"-"`
	Selector string   `query:"selector" json:"-"`
	// AppsetNamespace restricts the stream to one namespace. AppSetNamespace is the upstream
	// spelling, accepted as an alias.
	AppsetNamespace string `query:"appsetNamespace" json:"-"`
	AppSetNamespace string `query:"appSetNamespace" json:"-"`
	// ResourceVersion skips events of objects whose resourceVersion is older.
	ResourceVersion string `query:"resourceVersion" json:"-"`
}

// ApplicationSetWatchEvent is the data of one event of GET /api/v1/stream/applicationsets.
type ApplicationSetWatchEvent struct {
	// Type is ADDED, MODIFIED or DELETED.
	Type   string                  `json:"type" jsonschema:"enum=ADDED,enum=MODIFIED,enum=DELETED"`
	Object v1alpha1.ApplicationSet `json:"object"`
}

// ListApplicationSets lists the ApplicationSets the caller may get.
func (c *Client) ListApplicationSets(ctx context.Context, req *ApplicationSetListRequest) (*v1alpha1.ApplicationSetList, error) {
	var out v1alpha1.ApplicationSetList
	return &out, c.Do(ctx, http.MethodGet, PathApplicationSets, req, &out)
}

// GetApplicationSet returns one ApplicationSet.
func (c *Client) GetApplicationSet(ctx context.Context, name, namespace string) (*v1alpha1.ApplicationSet, error) {
	var out v1alpha1.ApplicationSet
	return &out, c.Do(ctx, http.MethodGet, PathApplicationSet, &ApplicationSetGetRequest{Name: name, AppsetNamespace: namespace}, &out)
}

// CreateApplicationSet creates (or with Upsert updates, or with DryRun renders) an
// ApplicationSet.
func (c *Client) CreateApplicationSet(ctx context.Context, req *ApplicationSetCreateRequest) (*v1alpha1.ApplicationSet, error) {
	var out v1alpha1.ApplicationSet
	return &out, c.Do(ctx, http.MethodPost, PathApplicationSets, req, &out)
}

// DeleteApplicationSet deletes an ApplicationSet.
func (c *Client) DeleteApplicationSet(ctx context.Context, req *ApplicationSetDeleteRequest) error {
	return c.Do(ctx, http.MethodDelete, PathApplicationSet, req, nil)
}

// GenerateApplicationSet renders the Applications an ApplicationSet would generate without
// persisting anything.
func (c *Client) GenerateApplicationSet(ctx context.Context, appSet *v1alpha1.ApplicationSet) (*ApplicationSetGenerateResponse, error) {
	var out ApplicationSetGenerateResponse
	return &out, c.Do(ctx, http.MethodPost, PathApplicationSetGenerate, &ApplicationSetGenerateRequest{ApplicationSet: appSet}, &out)
}

// GetApplicationSetResourceTree returns the generated Applications of an ApplicationSet as
// a resource tree.
func (c *Client) GetApplicationSetResourceTree(ctx context.Context, name, namespace string) (*ApplicationSetTree, error) {
	var out ApplicationSetTree
	return &out, c.Do(ctx, http.MethodGet, PathApplicationSetResourceTree, &ApplicationSetGetRequest{Name: name, AppsetNamespace: namespace}, &out)
}

// ListApplicationSetEvents returns the Kubernetes Events of an ApplicationSet.
func (c *Client) ListApplicationSetEvents(ctx context.Context, name, namespace string) (*corev1.EventList, error) {
	var out corev1.EventList
	return &out, c.Do(ctx, http.MethodGet, PathApplicationSetEvents, &ApplicationSetGetRequest{Name: name, AppsetNamespace: namespace}, &out)
}

// WatchApplicationSets opens the ApplicationSet event stream; read it with
// Watch[v1alpha1.ApplicationSet] or EventStream.Next.
func (c *Client) WatchApplicationSets(ctx context.Context, req *ApplicationSetWatchRequest, opts *StreamOptions) (*EventStream, error) {
	return c.Stream(ctx, PathApplicationSetsStream, req, opts)
}
