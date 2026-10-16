package operation

import (
	"context"
	"fmt"
	"log/slog"
	gosync "sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/health"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

// EngineFactory returns the sync engine used for a destination cluster, optionally
// impersonating a user (application.sync.impersonation, SYNC-093).
type EngineFactory interface {
	Engine(ctx context.Context, cluster *v1alpha1.Cluster, impersonate string, st *settings.Settings) (*syncpkg.Engine, error)
}

// KubeEnginesOptions configures KubeEngines.
type KubeEnginesOptions struct {
	// Config returns the controller's REST config of a destination cluster. Required.
	Config core.ClusterConfigFunc
	// Engine is the engine configuration (zero value: syncpkg.DefaultConfig()).
	Engine *syncpkg.Config
	// Metrics counts destination requests (nil: not counted).
	Metrics *core.Metrics
	// TTL bounds how long an engine (and the credentials of its REST config) is reused
	// (default 10m).
	TTL time.Duration
	// Logger receives engine logs.
	Logger *slog.Logger
	// Clock is the time source (default real clock).
	Clock clock.PassiveClock
}

// KubeEngines builds sync engines over sync.KubeClient and reuses them per destination
// server, impersonated user and settings fingerprint (the health overrides) for TTL, so the
// per-engine discovery cache survives across steps. It is safe for concurrent use.
type KubeEngines struct {
	o KubeEnginesOptions

	mu      gosync.Mutex
	engines map[string]engineEntry
}

type engineEntry struct {
	engine  *syncpkg.Engine
	created time.Time
}

// maxCachedEngines bounds the engine cache.
const maxCachedEngines = 1024

// NewKubeEngines returns a KubeEngines.
func NewKubeEngines(o KubeEnginesOptions) (*KubeEngines, error) {
	if o.Config == nil {
		return nil, fmt.Errorf("operation: KubeEnginesOptions.Config is required")
	}
	if o.TTL <= 0 {
		o.TTL = 10 * time.Minute
	}
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &KubeEngines{o: o, engines: map[string]engineEntry{}}, nil
}

// Engine implements EngineFactory.
func (k *KubeEngines) Engine(ctx context.Context, cluster *v1alpha1.Cluster, impersonate string, st *settings.Settings) (*syncpkg.Engine, error) {
	fp := ""
	if st != nil {
		fp = st.Fingerprint()
	}
	key := clusters.NormalizeServer(cluster.Spec.Server) + "\x00" + impersonate + "\x00" + fp + "\x00" + cluster.ResourceVersion
	now := k.o.Clock.Now()
	k.mu.Lock()
	if e, ok := k.engines[key]; ok && now.Sub(e.created) < k.o.TTL {
		k.mu.Unlock()
		return e.engine, nil
	}
	k.mu.Unlock()

	cfg, err := k.o.Config(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get REST config of cluster %q: %w", cluster.Spec.Server, err)
	}
	cfg = k.o.Metrics.WrapConfig(cfg, cluster.Spec.Server)
	if impersonate != "" {
		if cfg, err = clusters.ImpersonatingConfig(cfg, impersonate); err != nil {
			return nil, err
		}
	}
	engine, err := newEngine(cfg, st, k.o)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.engines) >= maxCachedEngines {
		for key, e := range k.engines {
			if now.Sub(e.created) >= k.o.TTL || len(k.engines) >= maxCachedEngines {
				delete(k.engines, key)
			}
		}
	}
	k.engines[key] = engineEntry{engine: engine, created: now}
	return engine, nil
}

// Forget drops every cached engine of server (credentials or server changed).
func (k *KubeEngines) Forget(server string) {
	prefix := clusters.NormalizeServer(server) + "\x00"
	k.mu.Lock()
	defer k.mu.Unlock()
	for key := range k.engines {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(k.engines, key)
		}
	}
}

func newEngine(cfg *rest.Config, st *settings.Settings, o KubeEnginesOptions) (*syncpkg.Engine, error) {
	ecfg := syncpkg.DefaultConfig()
	if o.Engine != nil {
		ecfg = *o.Engine
	}
	if ecfg.Logger == nil {
		ecfg.Logger = o.Logger
	}
	kube, err := syncpkg.NewKubeClient(cfg, syncpkg.KubeClientOptions{Manager: ecfg.Manager, Logger: o.Logger})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize sync context: %w", err)
	}
	return syncpkg.NewEngine(kube, HealthFunc(st, o.Logger), ecfg)
}

// HealthFunc returns the health assessment of the sync engine: built-in checks with the Lua
// customizations of st.
func HealthFunc(st *settings.Settings, log *slog.Logger) syncpkg.HealthFunc {
	var rt *lua.Runtime
	if st != nil {
		rt = lua.New(st.ResourceOverrides(), lua.WithLogger(log))
	}
	return func(obj *unstructured.Unstructured) (*health.HealthStatus, error) {
		if rt == nil {
			return health.Assess(obj, nil)
		}
		return health.Assess(obj, rt)
	}
}
