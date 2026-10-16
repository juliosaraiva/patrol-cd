package treeapi

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// Routes.
const (
	PathTree             = "/internal/v1/tree"
	PathManagedResources = "/internal/v1/managed-resources"
	PathServerSideDiff   = "/internal/v1/server-side-diff"
	PathResource         = "/internal/v1/resource"
	PathHealthz          = "/healthz"
)

// Defaults and limits.
const (
	// DefaultPort is the tree API port of controller pods.
	DefaultPort = 8090
	// DefaultMaxRequestBytes caps server-side diff request bodies.
	DefaultMaxRequestBytes = 32 << 20
	// DefaultMaxDiffResources caps the resources of one server-side diff request (the CLI
	// batches requests).
	DefaultMaxDiffResources = 5000
	// DefaultMaxResponseBytes caps responses read by the Client (upstream's default gRPC
	// message limit is 200 MiB).
	DefaultMaxResponseBytes = 200 << 20
	// HeaderShard carries the owning shard on 421 responses.
	HeaderShard = "Patrol-Shard"
)

// AppKey identifies an Application.
type AppKey struct {
	Namespace string
	Name      string
}

// String returns "namespace/name".
func (k AppKey) String() string { return k.Namespace + "/" + k.Name }

// StateProvider is implemented by the controller. Errors should wrap the sentinels of this
// package (ErrNotFound, ErrUnavailable, ErrInvalidArgument, ErrPermissionDenied) or be a
// *NotOwnedError; other errors are reported as internal errors. Error messages are returned
// to patrol-server and must not contain credentials or Secret data.
type StateProvider interface {
	// Tree returns the resource tree of the last comparison. ErrNotFound: unknown
	// Application; ErrUnavailable: not compared yet.
	Tree(ctx context.Context, app AppKey) (*compare.ApplicationTree, error)
	// ManagedResources returns the managed resources of the last comparison (hooks
	// included). They may be raw or already masked (compare.Result.MaskSecrets); this package
	// masks Secrets again before serving them.
	ManagedResources(ctx context.Context, app AppKey) ([]compare.ManagedResource, error)
	// ServerSideDiff runs server-side (dry-run apply) diffs of the index-aligned live and
	// target objects (either may be nil) with the Application's diff settings and returns one
	// result per index. Inputs were validated: every live object is a managed resource.
	ServerSideDiff(ctx context.Context, app AppKey, live, target []*unstructured.Unstructured) ([]diff.Result, error)
	// LiveResource fetches the live object of a node of the Application's tree from the
	// destination cluster (ref.Version may override the node's version).
	LiveResource(ctx context.Context, app AppKey, ref compare.ResourceRef) (*unstructured.Unstructured, error)
	// SensitiveAnnotations returns resource.sensitive.mask.annotations.
	SensitiveAnnotations() []string
}

// ResourceDiff is one managed resource with its desired and live states (upstream
// v1alpha1.ResourceDiff JSON).
type ResourceDiff struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	// TargetState is the desired object JSON ("null" when the resource is extraneous). In
	// server-side diff responses it is the predicted live state.
	TargetState string `json:"targetState,omitempty"`
	// LiveState is the live object JSON ("null" when missing). In server-side diff responses
	// it is the normalized live state.
	LiveState string `json:"liveState,omitempty"`
	// Diff is deprecated upstream and never set.
	Diff string `json:"diff,omitempty"`
	Hook bool   `json:"hook,omitempty"`
	// NormalizedLiveState and PredictedLiveState are the diff pair ("null" for absent sides).
	NormalizedLiveState string `json:"normalizedLiveState,omitempty"`
	PredictedLiveState  string `json:"predictedLiveState,omitempty"`
	ResourceVersion     string `json:"resourceVersion,omitempty"`
	Modified            bool   `json:"modified,omitempty"`
}

// ManagedResourcesQuery filters managed resources: every non-empty field must match exactly
// (upstream isMatchingResource; the version filter is accepted upstream but unused, so it is
// not offered).
type ManagedResourcesQuery struct {
	Group             string
	Kind              string
	ResourceNamespace string
	ResourceName      string
}

// ManagedResourcesResponse is the managed-resources response.
type ManagedResourcesResponse struct {
	Items []ResourceDiff `json:"items"`
}

// ServerSideDiffRequest is the server-side-diff request body: index-aligned live resources
// (their liveState empty or "null" for resources to be created) and target manifests (JSON,
// empty or "null" for resources to be pruned).
type ServerSideDiffRequest struct {
	Namespace       string         `json:"namespace"`
	Name            string         `json:"name"`
	LiveResources   []ResourceDiff `json:"liveResources,omitempty"`
	TargetManifests []string       `json:"targetManifests,omitempty"`
}

// ServerSideDiffResponse is the server-side-diff result.
type ServerSideDiffResponse struct {
	Items    []ResourceDiff `json:"items"`
	Modified bool           `json:"modified"`
}

// ResourceQuery identifies a live resource of an Application's tree.
type ResourceQuery struct {
	Group             string
	Version           string
	Kind              string
	ResourceNamespace string
	ResourceName      string
}

// ResourceResponse carries a live manifest (JSON, Secret data masked).
type ResourceResponse struct {
	Manifest string `json:"manifest"`
}
