package compare

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// swapSettings is a SettingsSource whose snapshot tests replace.
type swapSettings struct {
	p atomic.Pointer[settings.Settings]
}

func newSwapSettings(st *settings.Settings) *swapSettings {
	s := &swapSettings{}
	s.p.Store(st)
	return s
}

func (s *swapSettings) Current() *settings.Settings { return s.p.Load() }
func (s *swapSettings) set(st *settings.Settings)   { s.p.Store(st) }

func patrolCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-cm", Namespace: controlPlaneNS, Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}},
		Data:       data,
	}
}

var errSyncFailed = errors.New("cluster unreachable")

// fakeClusterCache counts in-place invalidations; other ClusterCache methods are unused.
type fakeClusterCache struct {
	cache.ClusterCache // unimplemented methods panic if called

	invalidations atomic.Int32
	syncs         atomic.Int32
}

func (f *fakeClusterCache) Invalidate(...cache.InvalidateOption) { f.invalidations.Add(1) }

func (f *fakeClusterCache) EnsureSynced(ctx context.Context) error {
	f.syncs.Add(1)
	return ctx.Err()
}

// fakeCacheManager holds at most one cache (the tests use a single cluster key).
type fakeCacheManager struct {
	mu            sync.Mutex
	cur           *fakeClusterCache
	created       []cache.Settings // settings of every cache built by Get
	invalidations int              // Manager.Invalidate calls
	// getErr, when set, decides the outcome of a Get for the cache it returns.
	getErr func(c *fakeClusterCache) error
	// hold, when set, makes the next Get signal held and wait for release.
	hold          bool
	held, release chan struct{}
}

func (m *fakeCacheManager) Get(ctx context.Context, _ string, _ *rest.Config, s cache.Settings) (cache.ClusterCache, error) {
	m.mu.Lock()
	if m.cur == nil {
		m.cur = &fakeClusterCache{}
		m.created = append(m.created, s)
	}
	c, getErr, hold := m.cur, m.getErr, m.hold
	m.hold = false
	m.mu.Unlock()
	if hold {
		close(m.held)
		<-m.release
	}
	if getErr != nil {
		if err := getErr(c); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

func (m *fakeCacheManager) Invalidate(string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur = nil
	m.invalidations++
}

func (m *fakeCacheManager) setGetErr(fn func(c *fakeClusterCache) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getErr = fn
}

func (m *fakeCacheManager) holdNextGet() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hold, m.held, m.release = true, make(chan struct{}), make(chan struct{})
}

func (m *fakeCacheManager) stats() (created []cache.Settings, invalidations int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.created), m.invalidations
}

func testCluster(namespaces ...string) *v1alpha1.Cluster {
	return &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://target.example.invalid", Namespaces: namespaces}}
}

func newFakeLiveState(t *testing.T, m CacheManager, src SettingsSource) *LiveStateCache {
	t.Helper()
	l, err := NewLiveStateCache(LiveStateCacheOptions{
		Manager: m,
		Config: func(context.Context, *v1alpha1.Cluster) (*rest.Config, error) {
			return &rest.Config{Host: "https://target.example.invalid"}, nil
		},
		Settings:              src,
		ControlPlaneNamespace: controlPlaneNS,
	})
	require.NoError(t, err)
	return l
}

func TestLiveStateCacheSettingsChangeSurvivesFailedCall(t *testing.T) {
	t.Parallel()
	failOnce := func(m *fakeCacheManager) {
		var failed atomic.Bool
		m.setGetErr(func(*fakeClusterCache) error {
			if failed.CompareAndSwap(false, true) {
				return errSyncFailed
			}
			return nil
		})
	}
	tests := []struct {
		name string
		// failingCall makes the first call after the settings change fail, or returns false
		// when that call is expected to succeed.
		failingCall func(m *fakeCacheManager) (context.Context, bool)
		// invalidatedByFailingCall is the in-place invalidation count after that call.
		invalidatedByFailingCall int32
	}{
		{
			name: "sync fails after the change was applied",
			failingCall: func(m *fakeCacheManager) (context.Context, bool) {
				failOnce(m)
				return t.Context(), true
			},
			invalidatedByFailingCall: 1,
		},
		{
			name: "context cancelled before the change was applied",
			failingCall: func(*fakeCacheManager) (context.Context, bool) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx, true
			},
			invalidatedByFailingCall: 0,
		},
		{
			// The cache never syncs with the old settings (for example a namespace the
			// credentials lost access to): the change must not wait for such a sync.
			name: "old settings never sync",
			failingCall: func(m *fakeCacheManager) (context.Context, bool) {
				m.setGetErr(func(c *fakeClusterCache) error {
					if c.invalidations.Load() == 0 {
						return errSyncFailed
					}
					return nil
				})
				return t.Context(), false
			},
			invalidatedByFailingCall: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &fakeCacheManager{}
			src := newSwapSettings(parseSettings(patrolCM(nil)))
			l := newFakeLiveState(t, m, src)
			cluster := testCluster()

			cc, err := l.ClusterCache(t.Context(), cluster)
			require.NoError(t, err)
			fc := cc.(*fakeClusterCache)

			src.set(parseSettings(patrolCM(map[string]string{"resource.customLabels": "team"})))
			ctx, fails := tc.failingCall(m)
			got, err := l.ClusterCache(ctx, cluster)
			if fails {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Same(t, fc, got)
			}
			assert.Equal(t, tc.invalidatedByFailingCall, fc.invalidations.Load())

			for range 3 {
				got, err = l.ClusterCache(t.Context(), cluster)
				require.NoError(t, err)
				assert.Same(t, fc, got, "the cache is reconfigured in place")
			}
			assert.Equal(t, int32(1), fc.invalidations.Load(), "the settings change is applied exactly once")
			created, mgrInvalidations := m.stats()
			assert.Len(t, created, 1)
			assert.Equal(t, 1, mgrInvalidations, "only the first use discards an unknown cache")
		})
	}
}

func TestLiveStateCacheConcurrentCallersWaitForSettingsChange(t *testing.T) {
	t.Parallel()
	m := &fakeCacheManager{}
	src := newSwapSettings(parseSettings(patrolCM(nil)))
	l := newFakeLiveState(t, m, src)
	cluster := testCluster()
	cc, err := l.ClusterCache(t.Context(), cluster)
	require.NoError(t, err)
	fc := cc.(*fakeClusterCache)

	src.set(parseSettings(patrolCM(map[string]string{"resource.customLabels": "team"})))
	m.holdNextGet()
	const callers = 8
	type outcome struct {
		cc          cache.ClusterCache
		invalidated int32
		err         error
	}
	results := make(chan outcome, callers)
	call := func() {
		cc, err := l.ClusterCache(t.Context(), cluster)
		o := outcome{cc: cc, err: err}
		if f, ok := cc.(*fakeClusterCache); ok {
			o.invalidated = f.invalidations.Load()
		}
		results <- o
	}
	go call()
	<-m.held // the first caller observed the change and is inside Manager.Get
	for range callers - 1 {
		go call()
	}
	time.Sleep(50 * time.Millisecond) // let the other callers reach the cache
	close(m.release)
	for range callers {
		o := <-results
		require.NoError(t, o.err)
		assert.Same(t, fc, o.cc)
		assert.Equal(t, int32(1), o.invalidated, "no caller with the new settings gets the cache before the change was applied")
	}
	assert.Equal(t, int32(1), fc.invalidations.Load())
}

func TestLiveStateCacheUnverifiedCacheRebuiltOnSettingsChange(t *testing.T) {
	t.Parallel()
	m := &fakeCacheManager{}
	m.setGetErr(func(*fakeClusterCache) error { return errSyncFailed })
	l := newFakeLiveState(t, m, newSwapSettings(parseSettings(patrolCM(nil))))

	// The cluster is unreachable: retries with unchanged settings reuse the cache being
	// built instead of discarding it.
	for range 2 {
		_, err := l.ClusterCache(t.Context(), testCluster("team-a"))
		require.ErrorIs(t, err, errSyncFailed)
	}
	created, mgrInvalidations := m.stats()
	require.Len(t, created, 1)
	assert.Equal(t, []string{"team-a"}, created[0].Namespaces)
	assert.Equal(t, 1, mgrInvalidations)

	// The cluster's namespaces change before any sync succeeded: the never-verified cache
	// built with the old namespaces is replaced by one built with the new ones.
	m.setGetErr(nil)
	cc, err := l.ClusterCache(t.Context(), testCluster("team-b"))
	require.NoError(t, err)
	created, mgrInvalidations = m.stats()
	require.Len(t, created, 2)
	assert.Equal(t, []string{"team-b"}, created[1].Namespaces)
	assert.Equal(t, 2, mgrInvalidations)
	assert.Equal(t, int32(0), cc.(*fakeClusterCache).invalidations.Load(), "a cache built with the current settings is not invalidated again")

	again, err := l.ClusterCache(t.Context(), testCluster("team-b"))
	require.NoError(t, err)
	assert.Same(t, cc, again)
	_, mgrInvalidations = m.stats()
	assert.Equal(t, 2, mgrInvalidations)
}

func TestLiveStateCacheVerifiesRebuiltCache(t *testing.T) {
	t.Parallel()
	m := &fakeCacheManager{}
	l := newFakeLiveState(t, m, newSwapSettings(parseSettings(patrolCM(nil))))
	cluster := testCluster()
	first, err := l.ClusterCache(t.Context(), cluster)
	require.NoError(t, err)

	t.Run("rebuilt without Forget", func(t *testing.T) {
		m.Invalidate("") // the controller invalidated the cache through the Manager
		rebuilt, err := l.ClusterCache(t.Context(), cluster)
		require.NoError(t, err)
		require.NotSame(t, first, rebuilt)
		fc := rebuilt.(*fakeClusterCache)
		assert.Equal(t, int32(1), fc.invalidations.Load(), "settings of a cache built elsewhere are applied")
		again, err := l.ClusterCache(t.Context(), cluster)
		require.NoError(t, err)
		assert.Same(t, rebuilt, again)
		assert.Equal(t, int32(1), fc.invalidations.Load())
	})
	t.Run("rebuilt with Forget", func(t *testing.T) {
		m.Invalidate("")
		l.Forget(cluster)
		rebuilt, err := l.ClusterCache(t.Context(), cluster)
		require.NoError(t, err)
		assert.Equal(t, int32(0), rebuilt.(*fakeClusterCache).invalidations.Load(), "a cache built with the current settings is used as is")
	})
}

func TestLiveStateCacheForgetDuringTransition(t *testing.T) {
	t.Parallel()
	m := &fakeCacheManager{}
	l := newFakeLiveState(t, m, newSwapSettings(parseSettings(patrolCM(nil))))
	cluster := testCluster()
	m.holdNextGet()
	done := make(chan error, 1)
	go func() {
		_, err := l.ClusterCache(t.Context(), cluster)
		done <- err
	}()
	<-m.held
	m.Invalidate("") // credentials changed while the first build was in flight
	l.Forget(cluster)
	close(m.release)
	require.NoError(t, <-done)

	_, before := m.stats()
	_, err := l.ClusterCache(t.Context(), cluster)
	require.NoError(t, err)
	_, after := m.stats()
	assert.Equal(t, before+1, after, "the outcome of a transition that Forget superseded is not trusted")
}

func TestSameCache(t *testing.T) {
	t.Parallel()
	a, b := &fakeClusterCache{}, &fakeClusterCache{}
	assert.True(t, sameCache(a, a))
	assert.False(t, sameCache(a, b))
	assert.False(t, sameCache(a, nil))
	assert.True(t, sameCache(nil, nil))
	type nonComparable struct {
		cache.ClusterCache
		_ []int
	}
	assert.False(t, sameCache(nonComparable{}, nonComparable{}), "non-comparable implementations do not panic")
}

// failingCacheManager fails the next Get when armed, then delegates.
type failingCacheManager struct {
	CacheManager
	failNext atomic.Bool
}

func (f *failingCacheManager) Get(ctx context.Context, key string, cfg *rest.Config, s cache.Settings) (cache.ClusterCache, error) {
	if f.failNext.CompareAndSwap(true, false) {
		return nil, errSyncFailed
	}
	return f.CacheManager.Get(ctx, key, cfg, s)
}

func TestEnvSettingsChangeAppliedAfterFailedCall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fail func(t *testing.T, h *harness, m *failingCacheManager, l *LiveStateCache)
	}{
		{"cancelled context", func(t *testing.T, h *harness, _ *failingCacheManager, l *LiveStateCache) {
			ctx, cancel := context.WithCancel(h.ctx)
			cancel()
			_, err := l.ClusterCache(ctx, h.cluster)
			require.ErrorIs(t, err, context.Canceled)
		}},
		{"cache sync error", func(t *testing.T, h *harness, m *failingCacheManager, l *LiveStateCache) {
			m.failNext.Store(true)
			_, err := l.ClusterCache(h.ctx, h.cluster)
			require.ErrorIs(t, err, errSyncFailed)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			m := &failingCacheManager{CacheManager: h.cacheMgr}
			src := newSwapSettings(h.settings)
			l, err := NewLiveStateCache(LiveStateCacheOptions{
				Manager: m, Config: h.live.o.Config, Settings: src,
				ControlPlaneNamespace: controlPlaneNS, Base: h.live.o.Base, Logger: h.live.o.Logger,
			})
			require.NoError(t, err)

			cm := configMap("labelled", h.ns, nil)
			cm.SetLabels(map[string]string{"team": "a"})
			h.create(cm)
			key := ResourceKey{Kind: "ConfigMap", Namespace: h.ns, Name: "labelled"}
			info := func(cc cache.ClusterCache) ([]InfoItem, bool) {
				r := cc.FindResources(h.ns, func(r *cache.Resource) bool { return r.ResourceKey() == key })[key]
				if r == nil {
					return nil, false
				}
				return InfoOf(r).Info, true
			}
			cc, err := l.ClusterCache(h.ctx, h.cluster)
			require.NoError(t, err)
			require.Eventually(t, func() bool { _, ok := info(cc); return ok }, 30*time.Second, 20*time.Millisecond)
			items, _ := info(cc)
			require.NotContains(t, items, InfoItem{Name: "team", Value: "a"})

			src.set(parseSettings(patrolCM(map[string]string{"resource.customLabels": "team"})))
			tc.fail(t, h, m, l)

			// The next successful call returns the cache synced with the new settings.
			cc, err = l.ClusterCache(h.ctx, h.cluster)
			require.NoError(t, err)
			items, ok := info(cc)
			require.True(t, ok)
			assert.Contains(t, items, InfoItem{Name: "team", Value: "a"}, "new settings (customLabels) are applied to the cache")
		})
	}
}
