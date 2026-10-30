package applications

import (
	"context"
	"errors"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	apirest "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Defaults of ResourceDeps.
const (
	// DefaultTreePollInterval is how often the resource tree stream asks the controller
	// whether the tree changed (besides Application events).
	DefaultTreePollInterval = 3 * time.Second
	// DefaultClusterCallTimeout bounds one non-streaming call to a destination cluster.
	DefaultClusterCallTimeout = time.Minute
	// DefaultResourceHealthTimeout bounds the tree read that fills resource health in
	// Application responses.
	DefaultResourceHealthTimeout = 5 * time.Second
	// DefaultMaxEvents caps the Events returned by the events route.
	DefaultMaxEvents = 1000
	// MaxServerSideDiffResources caps the resources of one server-side diff request (the
	// controller's limit; the CLI batches requests).
	MaxServerSideDiffResources = treeapi.DefaultMaxDiffResources
	// serverSideDiffMaxBodyBytes bounds server-side diff bodies (the controller's limit).
	serverSideDiffMaxBodyBytes = treeapi.DefaultMaxRequestBytes
	// MaxActionParameters caps the parameters of a resource action request.
	MaxActionParameters = 32
	// MaxActionParameterBytes caps one action parameter value.
	MaxActionParameterBytes = 64 << 10
	// actionMaxBodyBytes bounds resource action request bodies.
	actionMaxBodyBytes = 1 << 20
)

// TreeClient reads the controller-owned state of an Application from the replica serving
// its destination cluster; *treeapi.Client implements it. Errors are *treeapi.Error values
// (matching the treeapi sentinels with errors.Is) or transport errors wrapping
// treeapi.ErrUnavailable.
type TreeClient interface {
	Tree(ctx context.Context, app *v1alpha1.Application) (*compare.ApplicationTree, error)
	TreeIfChanged(ctx context.Context, app *v1alpha1.Application, etag string) (tree *compare.ApplicationTree, newETag string, changed bool, err error)
	ManagedResources(ctx context.Context, app *v1alpha1.Application, q treeapi.ManagedResourcesQuery) ([]treeapi.ResourceDiff, error)
	ServerSideDiff(ctx context.Context, app *v1alpha1.Application, live []treeapi.ResourceDiff, targets []string) (*treeapi.ServerSideDiffResponse, error)
}

var _ TreeClient = (*treeapi.Client)(nil)

// ClusterConfigs returns the REST configuration of a destination cluster with the
// cluster's own credentials (no impersonation; the handlers add it when the impersonation
// settings require it). The production adapter wraps internal/clusters:
//
//	applications.ClusterConfigsFunc(func(ctx context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
//		return registry.RestConfig(ctx, c, clusters.Options{})
//	})
type ClusterConfigs interface {
	RESTConfig(ctx context.Context, cluster *v1alpha1.Cluster) (*rest.Config, error)
}

// ClusterConfigsFunc adapts a function to ClusterConfigs.
type ClusterConfigsFunc func(ctx context.Context, cluster *v1alpha1.Cluster) (*rest.Config, error)

// RESTConfig calls f.
func (f ClusterConfigsFunc) RESTConfig(ctx context.Context, cluster *v1alpha1.Cluster) (*rest.Config, error) {
	return f(ctx, cluster)
}

// ActionRuntime discovers and runs resource actions; *lua.Runtime implements it.
type ActionRuntime interface {
	ListActions(ctx context.Context, obj *unstructured.Unstructured) ([]lua.Action, error)
	ExecuteAction(ctx context.Context, obj *unstructured.Unstructured, name string, params []lua.ActionParameter) ([]lua.ImpactedResource, error)
}

var _ ActionRuntime = (*lua.Runtime)(nil)

// LuaRuntimes returns the action runtime of a settings snapshot (built-in and configured
// resource customizations).
type LuaRuntimes interface {
	ActionRuntime(st *settings.Settings) (ActionRuntime, error)
}

// settingsLua builds lua.New(st.ResourceOverrides()) once per settings fingerprint.
type settingsLua struct {
	mu          sync.Mutex
	fingerprint string
	rt          *lua.Runtime
}

func (l *settingsLua) ActionRuntime(st *settings.Settings) (ActionRuntime, error) {
	if st == nil {
		return nil, errors.New("settings are not loaded")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rt == nil || l.fingerprint != st.Fingerprint() {
		l.rt = lua.New(st.ResourceOverrides())
		l.fingerprint = st.Fingerprint()
	}
	return l.rt, nil
}

// LogsConfig bounds pod log streams.
type LogsConfig struct {
	// MaxLineBytes truncates longer log lines (default 64 KiB).
	MaxLineBytes int
	// MaxBytes ends a stream (with a resource_exhausted error event) after that many bytes
	// of log content (default 256 MiB).
	MaxBytes int64
	// LinesPerSecond and Burst rate-limit the lines sent to one client (default 2000/s,
	// burst 10000); reading from the pods is paused, not dropped, when the limit is hit.
	LinesPerSecond float64
	Burst          int
	// MergeWindow is how long lines of several pods are buffered to be merged by timestamp
	// (default 100 ms, upstream).
	MergeWindow time.Duration
	// Heartbeat is the interval of SSE heartbeat comments (default 15 s).
	Heartbeat time.Duration
	// RecheckInterval is how often a stream re-verifies the caller's session and
	// permissions (default 1 min).
	RecheckInterval time.Duration
	// WriteTimeout bounds one write to the client (default 30 s).
	WriteTimeout time.Duration
}

// TerminalConfig bounds web terminal sessions.
type TerminalConfig struct {
	// IdleTimeout closes a session without client input for that long (default 30 min).
	IdleTimeout time.Duration
	// MaxMessageBytes caps one client message (default 64 KiB).
	MaxMessageBytes int64
	// PingInterval is the WebSocket keepalive interval (default 15 s).
	PingInterval time.Duration
	// RecheckInterval is how often the session re-verifies the caller's session,
	// permissions and the Application (default 30 s); permissions are also checked on
	// every client message.
	RecheckInterval time.Duration
	// HandshakeTimeout bounds the exec handshake with the destination cluster (default
	// 30 s).
	HandshakeTimeout time.Duration
	// MaxSessionDuration ends sessions after that long (0: no limit besides the session
	// token).
	MaxSessionDuration time.Duration
}

// ResourceDeps are the collaborators of the resource-level routes (resource tree,
// managed resources, server-side diff, events, live resources, actions, logs, terminal).
// Nil collaborators make the routes that need them answer 503.
type ResourceDeps struct {
	// Tree reads resource trees, managed resources and server-side diffs from the
	// controller (*treeapi.Client with a treeapi.ShardRouter).
	Tree TreeClient
	// Clusters builds destination cluster configurations (credentials of the Cluster's
	// Secret, or the pod identity for the in-cluster destination).
	Clusters ClusterConfigs
	// Lua returns the resource action runtime (default: built from the settings snapshot,
	// cached per settings fingerprint).
	Lua LuaRuntimes
	// Auditor records web terminal session start and end (default: an audit log line on
	// Deps.Logger).
	Auditor apirest.Auditor
	// TreePollInterval is the poll interval of the resource tree stream
	// (DefaultTreePollInterval).
	TreePollInterval time.Duration
	// ClusterCallTimeout bounds non-streaming destination cluster calls
	// (DefaultClusterCallTimeout).
	ClusterCallTimeout time.Duration
	// ResourceHealthTimeout bounds the tree read that fills status.resources[].health of
	// Applications whose resource health lives in the tree (resourceHealthSource appTree)
	// in get, watch, sync, rollback and terminate responses (DefaultResourceHealthTimeout).
	ResourceHealthTimeout time.Duration
	// MaxEvents caps listed Events (DefaultMaxEvents).
	MaxEvents int64
	Logs      LogsConfig
	Terminal  TerminalConfig
}

func (r *ResourceDeps) normalize() {
	if r.Lua == nil {
		r.Lua = &settingsLua{}
	}
	if r.TreePollInterval <= 0 {
		r.TreePollInterval = DefaultTreePollInterval
	}
	if r.ClusterCallTimeout <= 0 {
		r.ClusterCallTimeout = DefaultClusterCallTimeout
	}
	if r.ResourceHealthTimeout <= 0 {
		r.ResourceHealthTimeout = DefaultResourceHealthTimeout
	}
	if r.MaxEvents <= 0 {
		r.MaxEvents = DefaultMaxEvents
	}
	l := &r.Logs
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = 64 << 10
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 256 << 20
	}
	if l.LinesPerSecond <= 0 {
		l.LinesPerSecond = 2000
	}
	if l.Burst <= 0 {
		l.Burst = 10000
	}
	if l.MergeWindow <= 0 {
		l.MergeWindow = 100 * time.Millisecond
	}
	if l.Heartbeat <= 0 {
		l.Heartbeat = apirest.DefaultHeartbeat
	}
	if l.RecheckInterval <= 0 {
		l.RecheckInterval = apirest.DefaultReverifyInterval
	}
	if l.WriteTimeout <= 0 {
		l.WriteTimeout = apirest.DefaultStreamWrite
	}
	t := &r.Terminal
	if t.IdleTimeout <= 0 {
		t.IdleTimeout = 30 * time.Minute
	}
	if t.MaxMessageBytes <= 0 {
		t.MaxMessageBytes = 64 << 10
	}
	if t.PingInterval <= 0 {
		t.PingInterval = 15 * time.Second
	}
	if t.RecheckInterval <= 0 {
		t.RecheckInterval = 30 * time.Second
	}
	if t.HandshakeTimeout <= 0 {
		t.HandshakeTimeout = 30 * time.Second
	}
}

// initResources completes the resource dependencies (called by New): defaults, and the
// LiveResources implementation used by the resource links route when the wiring did not
// provide one and the tree API and cluster configurations are available.
func (s *Service) initResources() {
	s.d.Resources.normalize()
	if s.d.Resources.Auditor == nil {
		s.d.Resources.Auditor = &apirest.LogAuditor{Logger: s.d.Logger}
	}
	if s.d.LiveResources == nil && s.d.Resources.Tree != nil && s.d.Resources.Clusters != nil {
		s.d.LiveResources = &treeLiveResources{s: s}
	}
}

func (s *Service) treeClient() (TreeClient, error) {
	if s.d.Resources.Tree == nil {
		return nil, unavailable("the application controller tree API")
	}
	return s.d.Resources.Tree, nil
}

func (s *Service) clusterConfigs() (ClusterConfigs, error) {
	if s.d.Resources.Clusters == nil {
		return nil, unavailable("destination cluster access")
	}
	return s.d.Resources.Clusters, nil
}

// treeLiveResources implements LiveResources over the controller tree (membership) and
// the destination cluster (the live object, read with the Application's cluster
// configuration, impersonated when configured).
type treeLiveResources struct {
	s *Service
}

// GetManagedResource implements LiveResources.
func (l *treeLiveResources) GetManagedResource(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, ref ResourceRef) (*unstructured.Unstructured, error) {
	tree, err := l.s.appTree(ctx, app, proj)
	if err != nil {
		return nil, err
	}
	node := findTreeNode(tree, ref.Group, ref.Kind, ref.Namespace, ref.Name)
	if node == nil {
		return nil, ErrResourceNotInTree
	}
	st, err := l.s.settings()
	if err != nil {
		return nil, err
	}
	cc, err := l.s.appClusterConfig(ctx, app, proj, st, true)
	if err != nil {
		return nil, err
	}
	version := node.Version
	if ref.Version != "" {
		version = ref.Version
	}
	return l.s.getLiveObject(ctx, cc.config, node.Group, version, node.Kind, node.Namespace, node.Name)
}
