package apiclient

import (
	"context"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Route patterns of the cluster API. {server} is the URL-encoded server URL, or the cluster
// name with ?id.type=name (or id.type=name_escaped for a name that is URL-encoded once more).
const (
	PathClusters               = "/api/v1/clusters"
	PathCluster                = "/api/v1/clusters/{server}"
	PathClusterRotateAuth      = "/api/v1/clusters/{server}/rotate-auth"
	PathClusterInvalidateCache = "/api/v1/clusters/{server}/invalidate-cache"
)

// Cluster identifier types (?id.type=).
const (
	ClusterIDTypeServer      = "server"
	ClusterIDTypeName        = "name"
	ClusterIDTypeNameEscaped = "name_escaped"
)

// ClusterUpdatableFields are the paths accepted by ?updatedFields= on cluster updates.
var ClusterUpdatableFields = []string{"name", "namespaces", "config", "shard", "clusterResources", "labels", "annotations", "project"}

// RegisteredCluster is a registered destination cluster as returned by the API: the Cluster object
// with its connection configuration sanitized (spec.config.execProviderConfig removed, as
// its args and env may hold secrets) and flags telling which credentials are stored.
// Credential values are never returned.
type RegisteredCluster struct {
	metav1.TypeMeta `json:",inline"`
	// Metadata is the Cluster object's metadata (empty name for the implicit in-cluster
	// cluster). The kubectl last-applied-configuration annotation is removed.
	Metadata metav1.ObjectMeta `json:"metadata"`
	// Spec is the cluster configuration without spec.config.execProviderConfig.
	Spec v1alpha1.ClusterSpec `json:"spec"`
	// Status is the observed connection state, server version, cache information and
	// assigned shard.
	Status v1alpha1.ClusterStatus `json:"status,omitempty,omitzero"`
	// Credentials reports which credentials are configured.
	Credentials ClusterCredentialsInfo `json:"credentials"`
	// InCluster is true for the implicit in-cluster cluster that no Cluster object
	// registers (cluster.inClusterEnabled).
	InCluster bool `json:"inCluster,omitempty"`
}

// ClusterCredentialsInfo tells which credentials a cluster has without revealing them.
type ClusterCredentialsInfo struct {
	// HasBearerToken is true when a bearer token is stored.
	HasBearerToken bool `json:"hasBearerToken,omitempty"`
	// HasBasicAuth is true when a username or password is stored.
	HasBasicAuth bool `json:"hasBasicAuth,omitempty"`
	// HasCAData is true when a custom CA certificate is stored.
	HasCAData bool `json:"hasCAData,omitempty"`
	// HasClientCertificate is true when a client certificate and key are stored.
	HasClientCertificate bool `json:"hasClientCertificate,omitempty"`
	// HasExecProviderConfig is true when the cluster authenticates with an exec
	// credential plugin (its configuration is not returned).
	HasExecProviderConfig bool `json:"hasExecProviderConfig,omitempty"`
}

// RegisteredClusterList is the response of GET /api/v1/clusters.
type RegisteredClusterList struct {
	Metadata metav1.ListMeta     `json:"metadata"`
	Items    []RegisteredCluster `json:"items"`
}

// ClusterInput is the cluster of a create or update request (Argo CD's Cluster message).
// Its config is write-only: credentials are stored in a Secret owned by the Cluster object
// and are never returned.
type ClusterInput struct {
	// Server is the API server URL (http or https). It identifies the cluster and cannot
	// be changed by an update.
	Server string `json:"server"`
	// Name of the cluster; defaults to the server URL.
	Name string `json:"name,omitempty"`
	// Namespaces restricts management to these namespaces.
	Namespaces []string `json:"namespaces,omitempty"`
	// ClusterResources allows cluster-scoped resources when Namespaces is set.
	ClusterResources bool `json:"clusterResources,omitempty"`
	// Project scopes the cluster to an AppProject.
	Project string `json:"project,omitempty"`
	// Shard pins the cluster to a controller shard; a negative value means unset.
	Shard *int64 `json:"shard,omitempty"`
	// Labels and Annotations replace those of the Cluster object.
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// Config is the connection configuration (write-only credentials).
	Config ClusterConfigInput `json:"config,omitempty,omitzero"`
	// ResourceVersion, when set on an update, must match the stored Cluster (409
	// otherwise). The controller's status writes move it too: updatedFields confines a write
	// without it.
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// ClusterConfigInput is the connection configuration of a cluster create or update.
type ClusterConfigInput struct {
	// Username and Password are basic-authentication credentials (write-only).
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// BearerToken is a bearer token (write-only).
	BearerToken string `json:"bearerToken,omitempty"`
	// TLSClientConfig holds TLS settings; certificate and key data are write-only.
	TLSClientConfig ClusterTLSClientConfigInput `json:"tlsClientConfig,omitempty,omitzero"`
	// AWSAuthConfig authenticates with AWS IAM (EKS).
	AWSAuthConfig *v1alpha1.AWSAuthConfig `json:"awsAuthConfig,omitempty"`
	// ExecProviderConfig authenticates with an exec credential plugin. It is stored in the
	// Cluster object (readable by whoever may read Cluster objects) but never returned by
	// the API; do not put secrets in its args or env.
	ExecProviderConfig *v1alpha1.ExecProviderConfig `json:"execProviderConfig,omitempty"`
	// ProxyURL is an http, https or socks5 proxy for every request to the cluster. It must
	// not contain credentials.
	ProxyURL string `json:"proxyUrl,omitempty"`
	// DisableCompression disables response compression.
	DisableCompression bool `json:"disableCompression,omitempty"`
}

// ClusterTLSClientConfigInput holds the TLS settings of a cluster create or update.
type ClusterTLSClientConfigInput struct {
	// Insecure skips server certificate verification.
	Insecure bool `json:"insecure,omitempty"`
	// ServerName is used for SNI and certificate verification.
	ServerName string `json:"serverName,omitempty"`
	// CAData is the PEM CA bundle (base64 in JSON; write-only).
	CAData []byte `json:"caData,omitempty"`
	// CertData and KeyData are the PEM client certificate and key (base64 in JSON;
	// write-only).
	CertData []byte `json:"certData,omitempty"`
	KeyData  []byte `json:"keyData,omitempty"`
}

// ListClustersRequest is GET /api/v1/clusters. The server, name and id filters each keep
// only the first matching cluster (upstream); results are sorted by server.
type ListClustersRequest struct {
	Server  string `query:"server" json:"-"`
	Name    string `query:"name" json:"-"`
	IDType  string `query:"id.type" json:"-"`
	IDValue string `query:"id.value" json:"-"`
	// Selector is a label selector on the Cluster objects.
	Selector string `query:"selector" json:"-"`
	Limit    int64  `query:"limit" json:"-"`
	Continue string `query:"continue" json:"-"`
}

// ClusterIDRequest addresses one cluster: Server is the server URL, or the name when
// IDType is "name" ("name_escaped": the URL-encoded name).
type ClusterIDRequest struct {
	Server string `path:"server" json:"-"`
	IDType string `query:"id.type" json:"-"`
}

// CreateClusterRequest is POST /api/v1/clusters.
type CreateClusterRequest struct {
	// Upsert updates an existing cluster whose configuration differs.
	Upsert  bool         `query:"upsert" json:"-"`
	Cluster ClusterInput `body:"true" json:"-"`
}

// UpdateClusterRequest is PUT /api/v1/clusters/{server}.
type UpdateClusterRequest struct {
	Server string `path:"server" json:"-"`
	IDType string `query:"id.type" json:"-"`
	// UpdatedFields limits the update to these paths (ClusterUpdatableFields; repeated or
	// comma-separated). Empty replaces the whole cluster, including its credentials.
	UpdatedFields []string     `query:"updatedFields" json:"-"`
	Cluster       ClusterInput `body:"true" json:"-"`
}

// ClusterID identifies a cluster for the client methods.
type ClusterID struct {
	// Value is the server URL or, with Type "name", the cluster name.
	Value string
	// Type is "" or ClusterIDTypeServer, ClusterIDTypeName or ClusterIDTypeNameEscaped.
	Type string
}

// ClusterByServer identifies a cluster by its server URL.
func ClusterByServer(server string) ClusterID { return ClusterID{Value: server} }

// ClusterByName identifies a cluster by its name.
func ClusterByName(name string) ClusterID { return ClusterID{Value: name, Type: ClusterIDTypeName} }

func (id ClusterID) request() *ClusterIDRequest {
	return &ClusterIDRequest{Server: id.Value, IDType: id.Type}
}

// ListClusters lists the clusters the caller may get.
func (c *Client) ListClusters(ctx context.Context, req *ListClustersRequest) (*RegisteredClusterList, error) {
	var out RegisteredClusterList
	return &out, c.Do(ctx, http.MethodGet, PathClusters, req, &out)
}

// CreateCluster registers a cluster; with upsert an existing cluster is updated.
func (c *Client) CreateCluster(ctx context.Context, cluster *ClusterInput, upsert bool) (*RegisteredCluster, error) {
	var out RegisteredCluster
	return &out, c.Do(ctx, http.MethodPost, PathClusters, &CreateClusterRequest{Upsert: upsert, Cluster: *cluster}, &out)
}

// GetCluster returns one cluster.
func (c *Client) GetCluster(ctx context.Context, id ClusterID) (*RegisteredCluster, error) {
	var out RegisteredCluster
	return &out, c.Do(ctx, http.MethodGet, PathCluster, id.request(), &out)
}

// UpdateCluster updates a cluster; updatedFields limits the update to those paths.
func (c *Client) UpdateCluster(ctx context.Context, id ClusterID, cluster *ClusterInput, updatedFields ...string) (*RegisteredCluster, error) {
	var out RegisteredCluster
	req := &UpdateClusterRequest{Server: id.Value, IDType: id.Type, UpdatedFields: updatedFields, Cluster: *cluster}
	return &out, c.Do(ctx, http.MethodPut, PathCluster, req, &out)
}

// DeleteCluster removes a cluster (by name: every cluster with that name).
func (c *Client) DeleteCluster(ctx context.Context, id ClusterID) error {
	return c.Do(ctx, http.MethodDelete, PathCluster, id.request(), nil)
}

// RotateClusterAuth replaces the cluster's service-account bearer token.
func (c *Client) RotateClusterAuth(ctx context.Context, id ClusterID) error {
	return c.Do(ctx, http.MethodPost, PathClusterRotateAuth, id.request(), nil)
}

// InvalidateClusterCache requests a rebuild of the controller's cache of the cluster.
func (c *Client) InvalidateClusterCache(ctx context.Context, id ClusterID) (*RegisteredCluster, error) {
	var out RegisteredCluster
	return &out, c.Do(ctx, http.MethodPost, PathClusterInvalidateCache, id.request(), &out)
}

// ClusterRBACObject is the RBAC object of a cluster: "<project>/<server>" for a
// project-scoped cluster, else the server URL.
func ClusterRBACObject(project, server string) string {
	if project != "" {
		return project + "/" + server
	}
	return server
}

// SplitUpdatedFields flattens repeated and comma-separated updatedFields values.
func SplitUpdatedFields(values []string) []string {
	var out []string
	for _, v := range values {
		for f := range strings.SplitSeq(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
	}
	return out
}
