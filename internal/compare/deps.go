package compare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/util/managedfields"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// RepositoryResolver resolves repository access for an Application's project.
// *repository.Resolver implements it.
type RepositoryResolver interface {
	// GetRepository resolves read access to repoURL for an Application of project.
	GetRepository(ctx context.Context, repoURL, project string) (*apiclient.Repository, error)
	// ListDependencyRepositories returns the Helm (and with includeOCI, OCI) dependency
	// repositories and credential templates permitted by proj.
	ListDependencyRepositories(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]apiclient.Repository, []apiclient.RepositoryCredentialTemplate, error)
}

// ClusterResolver resolves Application destinations and project-scoped clusters.
// *clusters.Registry implements it.
type ClusterResolver interface {
	// DestinationCluster returns the cluster of an Application destination.
	DestinationCluster(dest v1alpha1.ApplicationDestination) (*v1alpha1.Cluster, error)
	// GetProjectClusters returns the clusters scoped to project.
	GetProjectClusters(ctx context.Context, project string) ([]*v1alpha1.Cluster, error)
}

// SettingsSource returns the current settings snapshot. *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// StaticSettings is a SettingsSource returning a fixed snapshot.
type StaticSettings struct{ Settings *settings.Settings }

// Current implements SettingsSource.
func (s StaticSettings) Current() *settings.Settings { return s.Settings }

// LiveStateProvider returns the synced live-state cache of a destination cluster. Caches it
// returns must populate Resource.Info with *ResourceInfo (see CacheSettings); LiveStateCache
// implements it.
type LiveStateProvider interface {
	ClusterCache(ctx context.Context, cluster *v1alpha1.Cluster) (cache.ClusterCache, error)
}

// ServerSideDiffProvider supplies what server-side and structured-merge diffs need for a
// destination cluster. KubeServerSideDiff implements it.
type ServerSideDiffProvider interface {
	// DryRunner returns a server-side apply dry runner for the cluster.
	DryRunner(ctx context.Context, cluster *v1alpha1.Cluster) (diff.DryRunner, error)
	// GVKParser returns the structured-merge type parser of the cluster's OpenAPI schema.
	GVKParser(ctx context.Context, cluster *v1alpha1.Cluster) (*managedfields.GvkParser, error)
}

// Deps are the dependencies and parameters of a Manager.
type Deps struct {
	// RepoServer renders manifests. Required.
	RepoServer apiclient.Client
	// Repositories resolves repository credentials. Required.
	Repositories RepositoryResolver
	// Settings provides patrol-cm settings. Required.
	Settings SettingsSource
	// Clusters resolves destinations. Required.
	Clusters ClusterResolver
	// LiveState provides per-cluster live-state caches. Required.
	LiveState LiveStateProvider
	// ServerSideDiff provides dry runners and type parsers. Optional: without it server-side
	// diff fails with an UnknownError condition, and structured-merge diff and
	// managedFieldsManagers rules use deduced (atomic) schemas.
	ServerSideDiff ServerSideDiffProvider
	// ApplicationExists reports whether an Application with the given instance name exists
	// (orphaned-resource detection skips resources of other Applications). Nil treats every
	// tracked instance name as an existing Application.
	ApplicationExists func(instanceName string) bool

	// ControlPlaneNamespace is the namespace Patrol CD runs in (instance names). Required.
	ControlPlaneNamespace string
	// ServerSideDiffEnabled enables server-side diff for every Application
	// (controller.diff.server.side); the compare-options annotation overrides it per app.
	ServerSideDiffEnabled bool
	// PersistResourceHealth stores per-resource health in status.resources
	// (controller.resource.health.persist); otherwise health is served from the tree.
	PersistResourceHealth bool
	// RepoErrorGracePeriod defers the first manifest generation error of an Application for
	// this long (ErrRepoErrorDeferred). Zero or negative reports errors at once.
	RepoErrorGracePeriod time.Duration
	// MaxResources bounds status.resources (default DefaultMaxResources; negative means
	// unbounded).
	MaxResources int
	// GPGEnabled reports whether GnuPG source verification is enabled (local manifests are
	// refused when the project enforces it).
	GPGEnabled bool
	// JQTimeout bounds each jq ignore rule (zero: diff.DefaultJQTimeout).
	JQTimeout time.Duration

	// Registerer registers the comparison metrics (nil: not registered).
	Registerer prometheus.Registerer
	// Logger is the comparison logger (default slog.Default()).
	Logger *slog.Logger
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Manager compares Applications with their live state. It is safe for concurrent use.
type Manager struct {
	d       Deps
	log     *slog.Logger
	metrics *metrics

	// lua caches the Lua runtime of the current settings fingerprint.
	lua atomic.Pointer[luaEntry]

	repoErrMu sync.Mutex
	repoErrs  map[string]time.Time
}

type luaEntry struct {
	fingerprint string
	runtime     *lua.Runtime
}

// maxRepoErrorEntries bounds the repo error grace bookkeeping.
const maxRepoErrorEntries = 10000

// New returns a Manager.
func New(d Deps) (*Manager, error) {
	var missing []error
	for name, ok := range map[string]bool{
		"RepoServer":            d.RepoServer != nil,
		"Repositories":          d.Repositories != nil,
		"Settings":              d.Settings != nil,
		"Clusters":              d.Clusters != nil,
		"LiveState":             d.LiveState != nil,
		"ControlPlaneNamespace": d.ControlPlaneNamespace != "",
	} {
		if !ok {
			missing = append(missing, fmt.Errorf("compare: %s is required", name))
		}
	}
	if err := errors.Join(missing...); err != nil {
		return nil, err
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.MaxResources == 0 {
		d.MaxResources = DefaultMaxResources
	}
	m := &Manager{d: d, log: d.Logger, repoErrs: map[string]time.Time{}}
	met, err := newMetrics(d.Registerer)
	if err != nil {
		return nil, err
	}
	m.metrics = met
	return m, nil
}

// luaRuntime returns the Lua runtime for the settings snapshot, rebuilt when the settings
// fingerprint changes.
func (m *Manager) luaRuntime(st *settings.Settings) *lua.Runtime {
	fp := st.Fingerprint()
	if e := m.lua.Load(); e != nil && e.fingerprint == fp {
		return e.runtime
	}
	rt := lua.New(st.ResourceOverrides(), lua.WithLogger(m.log))
	m.lua.Store(&luaEntry{fingerprint: fp, runtime: rt})
	return rt
}

// tracker returns the resource tracker for the settings snapshot.
func (m *Manager) tracker(st *settings.Settings) *tracking.ResourceTracker {
	return tracking.New(tracking.Config{InstanceLabelKey: st.InstanceLabelKey(), ControlPlaneNamespace: m.d.ControlPlaneNamespace})
}

// repoErrorDeferred records a manifest generation error of appKey and reports whether it is
// still within the grace period (upstream repoErrorCache).
func (m *Manager) repoErrorDeferred(appKey string, noRevisionCache bool) bool {
	if m.d.RepoErrorGracePeriod <= 0 || noRevisionCache {
		return false
	}
	now := m.d.Now()
	m.repoErrMu.Lock()
	defer m.repoErrMu.Unlock()
	first, ok := m.repoErrs[appKey]
	if !ok {
		if len(m.repoErrs) >= maxRepoErrorEntries {
			m.pruneRepoErrorsLocked(now)
		}
		m.repoErrs[appKey] = now
		return true
	}
	return now.Sub(first) <= m.d.RepoErrorGracePeriod
}

func (m *Manager) clearRepoError(appKey string) {
	m.repoErrMu.Lock()
	delete(m.repoErrs, appKey)
	m.repoErrMu.Unlock()
}

// pruneRepoErrorsLocked drops expired entries, then the oldest half if still full.
func (m *Manager) pruneRepoErrorsLocked(now time.Time) {
	for k, t := range m.repoErrs {
		if now.Sub(t) > m.d.RepoErrorGracePeriod {
			delete(m.repoErrs, k)
		}
	}
	if len(m.repoErrs) < maxRepoErrorEntries {
		return
	}
	var oldest time.Time
	for _, t := range m.repoErrs {
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	cut := oldest.Add(m.d.RepoErrorGracePeriod / 2)
	for k, t := range m.repoErrs {
		if !t.After(cut) {
			delete(m.repoErrs, k)
		}
	}
}
