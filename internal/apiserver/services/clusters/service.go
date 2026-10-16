// Package clusters implements the cluster API of patrol-server (api-contract.md,
// "Clusters"; upstream ClusterService): registering destination clusters as Cluster objects
// whose credentials live in a Secret owned by the Cluster and written only by this API,
// listing them with their connection state, updating them field by field, deleting them,
// rotating their service-account token and requesting a cache invalidation.
//
// Every route is authorized in the handler with RBAC resource "clusters" and object
// "<project>/<server>" for project-scoped clusters, else the server URL. A missing cluster
// and a forbidden one produce the same 403 "permission denied", so the API is no oracle for
// registered servers (upstream behavior). Credentials are write-only: responses carry
// presence flags, and spec.config.execProviderConfig is removed from them.
package clusters

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Client reads and writes Cluster objects, their credential Secrets and AppProjects of
// the control-plane namespace; a controller-runtime client.Client implements it.
type Client interface {
	client.Reader
	client.Writer
}

// SettingsSource returns the current settings snapshot; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// StatusRecorder persists the result of a connection check in Cluster status;
// *clusters.StatusWriter implements it.
type StatusRecorder interface {
	RecordProbe(ctx context.Context, c *v1alpha1.Cluster, version string, probeErr error) (*v1alpha1.Cluster, error)
}

// InClusterSource reports the implicit in-cluster cluster with its probed status;
// *clusters.Registry implements it (ByServer of the in-cluster server).
type InClusterSource interface {
	ByServer(server string) (*v1alpha1.Cluster, error)
}

// Connector reaches destination clusters. NewConnector returns the default
// implementation (internal/clusters configuration with SSRF guardrails).
type Connector interface {
	// Probe checks connectivity to c authenticated with the credentials Secret data and
	// returns the normalized server version. It returns ErrExecNotAllowed when c would
	// run an exec credential plugin that the connector may not run.
	Probe(ctx context.Context, c *v1alpha1.Cluster, secretData map[string][]byte) (string, error)
	// Kubernetes returns a client for c (token rotation).
	Kubernetes(ctx context.Context, c *v1alpha1.Cluster, secretData map[string][]byte) (kubernetes.Interface, error)
}

// Config is the static configuration of the service.
type Config struct {
	// Namespace is the control-plane namespace holding Clusters and their Secrets.
	Namespace string
	// AllowLinkLocal accepts server and proxy URLs that designate link-local or cloud
	// metadata addresses (rejected by default, SSRF guard). It is also passed to the
	// default Connector.
	AllowLinkLocal bool
	// Bootstrap configures token rotation (token wait timeout and poll interval).
	Bootstrap *clusterlib.BootstrapOptions
}

// Deps are the collaborators of the service. With zero Deps the routes register (OpenAPI
// generation) and answer 503.
type Deps struct {
	Config   Config
	Client   Client
	Settings SettingsSource
	// Connector probes clusters; nil disables connection checks (503 on writes).
	Connector Connector
	// Status records connection checks in Cluster status; nil skips it.
	Status StatusRecorder
	// InCluster provides the status of the implicit in-cluster cluster; nil lists it
	// without status.
	InCluster InClusterSource
	Logger    *slog.Logger
	Now       func() time.Time
}

// Service serves the cluster routes.
type Service struct {
	d Deps
}

// New returns the cluster service.
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{d: d}
}

const tag = "clusters"

// Register adds the cluster routes to t.
func (s *Service) Register(t *rest.Table) {
	obj := "<project>/<server> (project-scoped) or <server>"
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathClusters, OperationID: "listClusters",
		Summary: "List clusters", Tags: []string{tag},
		Access:      withNote(rest.RBACInHandler(rbac.ResourceClusters, rbac.ActionGet, obj), "items filtered per cluster"),
		Description: "Lists the registered clusters the caller may get, including the implicit in-cluster cluster when cluster.inClusterEnabled is not \"false\". The server, name and id filters each keep only the first match. Credentials are never returned.",
	}, s.list)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathClusters, OperationID: "createCluster",
		Summary: "Register a cluster", Tags: []string{tag},
		Access:      withNote(rest.RBACInHandler(rbac.ResourceClusters, rbac.ActionCreate, obj), "awsAuthConfig/execProviderConfig also need create on <server>; registering the in-cluster server needs update on it"),
		Description: "Validates the configuration, checks connectivity with GET /version, then stores the Cluster object and its credentials Secret. An identical existing cluster is returned unchanged; a different one is an error unless upsert is set (then the update rules apply). The implicit in-cluster cluster counts as existing: registering its server follows the update rules even without upsert. awsAuthConfig and execProviderConfig need the permission on the unscoped server object and are not probed by the API server.",
	}, s.create)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathCluster, OperationID: "getCluster",
		Summary: "Get a cluster", Tags: []string{tag},
		Access: withNote(rest.RBACInHandler(rbac.ResourceClusters, rbac.ActionGet, obj), "missing and forbidden clusters both return 403"),
	}, s.get)
	rest.Register(t, rest.Route{
		Method: http.MethodPut, Path: apiclient.PathCluster, OperationID: "updateCluster",
		Summary: "Update a cluster", Tags: []string{tag},
		Access:      withNote(rest.RBACInHandler(rbac.ResourceClusters, rbac.ActionUpdate, obj), "also update on the new project's object when the project changes, and on <server> when the config sets awsAuthConfig/execProviderConfig"),
		Description: "With updatedFields only the listed paths change (applied to the latest stored object, so concurrent changes of other fields are kept); without it the cluster (including its credentials) is replaced. The server cannot change. Connectivity is checked before saving.",
	}, s.update)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathCluster, OperationID: "deleteCluster",
		Summary: "Delete a cluster (by name: every cluster with the name)", Tags: []string{tag},
		Access: rest.RBACInHandler(rbac.ResourceClusters, rbac.ActionDelete, obj),
	}, s.delete)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathClusterRotateAuth, OperationID: "rotateClusterAuth",
		Summary: "Rotate the cluster's service-account token", Tags: []string{tag},
		Access:      rest.RBACInHandler(rbac.ResourceClusters, rbac.ActionUpdate, obj),
		Description: "For clusters authenticated with a legacy service-account bearer token: creates a new token Secret in the destination cluster, verifies and stores the new token, then deletes the old token Secret.",
	}, s.rotateAuth)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathClusterInvalidateCache, OperationID: "invalidateClusterCache",
		Summary: "Request a rebuild of the controller's cache of the cluster", Tags: []string{tag},
		Access: rest.RBACInHandler(rbac.ResourceClusters, rbac.ActionUpdate, obj),
	}, s.invalidateCache)
}

func withNote(a rest.Access, note string) rest.Access {
	a.Note = note
	return a
}
