package controller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

func TestConfigCompleteDefaults(t *testing.T) {
	t.Parallel()
	in := &rest.Config{Host: "https://example.invalid"}
	c, err := Config{RestConfig: in, Namespace: "patrol"}.complete()
	require.NoError(t, err)
	assert.Equal(t, DefaultConfigMapName, c.ConfigMapName)
	assert.Equal(t, DefaultSecretName, c.SecretName)
	assert.Equal(t, DefaultProcessors, c.Processors)
	assert.Equal(t, DefaultResyncPeriod, c.ResyncPeriod)
	assert.Equal(t, DefaultLeaderElectionID, c.LeaderElectionID)
	assert.Equal(t, "patrol", c.LeaderElectionNamespace)
	assert.NotNil(t, c.Logger)
	assert.NotNil(t, c.Now)
	assert.NotNil(t, c.Network.DialContext, "the SSRF guard is installed by default")
	assert.True(t, c.selector.Empty())
	// NOTIF-009: client defaults on a copy of the REST config.
	assert.Equal(t, DefaultUserAgent, c.RestConfig.UserAgent)
	assert.InDelta(t, float32(DefaultQPS), c.RestConfig.QPS, 0)
	assert.Equal(t, DefaultBurst, c.RestConfig.Burst)
	assert.Empty(t, in.UserAgent, "the caller's REST config is not modified")
	assert.Zero(t, in.QPS)

	// Explicit values are kept.
	c, err = Config{
		RestConfig:         &rest.Config{UserAgent: "custom", QPS: 5, Burst: 7},
		Namespace:          "patrol",
		ConfigMapName:      "my-cm",
		SecretName:         "my-secret",
		Processors:         4,
		ResyncPeriod:       2 * time.Minute,
		AppLabelSelector:   "notify=true",
		AllowPrivateEgress: true,
	}.complete()
	require.NoError(t, err)
	assert.Equal(t, "custom", c.RestConfig.UserAgent)
	assert.InDelta(t, float32(5), c.RestConfig.QPS, 0)
	assert.Equal(t, 7, c.RestConfig.Burst)
	assert.Equal(t, "my-cm", c.ConfigMapName)
	assert.Equal(t, 4, c.Processors)
	assert.Nil(t, c.Network.DialContext, "AllowPrivateEgress leaves the default dialer")
	assert.False(t, c.selector.Empty())
}

// NOTIF-003/004/006: invalid settings fail the start instead of running half configured.
func TestConfigCompleteValidation(t *testing.T) {
	t.Parallel()
	rc := &rest.Config{}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "no rest config", cfg: Config{Namespace: "patrol"}, want: "rest config is required"},
		{name: "no namespace", cfg: Config{RestConfig: rc}, want: "namespace is required"},
		{name: "invalid namespace", cfg: Config{RestConfig: rc, Namespace: "Patrol_NS"}, want: "invalid namespace"},
		{name: "invalid ConfigMap name", cfg: Config{RestConfig: rc, Namespace: "patrol", ConfigMapName: "Bad Name"}, want: "invalid object name"},
		{name: "invalid selector", cfg: Config{RestConfig: rc, Namespace: "patrol", AppLabelSelector: "notify in (a"}, want: "invalid application label selector"},
		{name: "too many processors", cfg: Config{RestConfig: rc, Namespace: "patrol", Processors: maxProcessors + 1}, want: "processors must be at most"},
		{name: "resync too short", cfg: Config{RestConfig: rc, Namespace: "patrol", ResyncPeriod: time.Millisecond}, want: "resync period must be at least"},
		{name: "negative send timeout", cfg: Config{RestConfig: rc, Namespace: "patrol", SendTimeout: -time.Second}, want: "send timeout must not be negative"},
		{name: "too many namespace patterns", cfg: Config{RestConfig: rc, Namespace: "patrol", ApplicationNamespaces: make([]string, maxNamespacePatterns+1)}, want: "application namespace patterns"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.cfg.complete()
			require.ErrorContains(t, err, tc.want)
			assert.True(t, strings.HasPrefix(err.Error(), "notifications controller configuration: "))
		})
	}
	// Errors are reported together.
	_, err := Config{Processors: maxProcessors + 1}.complete()
	require.ErrorContains(t, err, "rest config is required")
	require.ErrorContains(t, err, "namespace is required")
	require.ErrorContains(t, err, "processors must be at most")
}

// NOTIF-002: the upstream-documented lookahead example (app-any-namespace.md) does not stop
// the controller from starting: it is logged and never matches (RE2 has no lookaround).
func TestConfigCompleteWarnsAboutUnsupportedNamespaceExpressions(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	c, err := Config{
		RestConfig:            &rest.Config{},
		Namespace:             "patrol",
		ApplicationNamespaces: []string{"/^((?!not-allowed).)*$/", "team-*", "team-[a"},
		Logger:                slog.New(slog.NewTextHandler(&logs, nil)),
	}.complete()
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "ignoring application namespace entry")
	assert.Contains(t, logs.String(), "lookaround is not supported")
	assert.Contains(t, logs.String(), "team-[a")
	assert.True(t, c.scope.matches("team-a"), "valid entries keep working")
	assert.False(t, c.scope.matches("allowed"))
}

// T8: namespace (self-service) configurations always get the guarded dialer, whatever the
// control-plane egress policy; the other network settings are shared.
func TestConfigCompleteTenantNetwork(t *testing.T) {
	t.Parallel()
	custom := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("custom dialer") }
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "default guard", cfg: Config{}},
		{name: "private egress allowed for the control plane", cfg: Config{AllowPrivateEgress: true}},
		{name: "control-plane prefixes", cfg: Config{EgressAllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}},
		{name: "custom control-plane dialer", cfg: Config{Network: services.Network{DialContext: custom}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			cfg.RestConfig, cfg.Namespace = &rest.Config{}, "patrol"
			cfg.Network.Timeout = 7 * time.Second
			c, err := cfg.complete()
			require.NoError(t, err)
			require.NotNil(t, c.tenantNetwork.DialContext)
			_, err = c.tenantNetwork.DialContext(context.Background(), "tcp", "127.0.0.1:1")
			require.ErrorIs(t, err, services.ErrURLNotAllowed, "loopback is refused for tenants")
			_, err = c.tenantNetwork.DialContext(context.Background(), "tcp", "10.1.2.3:443")
			require.ErrorIs(t, err, services.ErrURLNotAllowed, "private ranges are refused for tenants")
			assert.Equal(t, 7*time.Second, c.tenantNetwork.Timeout, "non-dialer settings are shared")
			assert.Equal(t, "patrol-notifications", c.tenantNetwork.UserAgent)
		})
	}

	// TenantEgressAllowedPrefixes opens a range for tenants only.
	c, err := Config{
		RestConfig: &rest.Config{}, Namespace: "patrol",
		TenantEgressAllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}.complete()
	require.NoError(t, err)
	_, err = c.Network.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	require.ErrorIs(t, err, services.ErrURLNotAllowed, "tenant prefixes do not open the control plane")
	conn, err := c.tenantNetwork.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if conn != nil {
		_ = conn.Close()
	}
	require.NotErrorIs(t, err, services.ErrURLNotAllowed, "the tenant prefix is allowed (connection refused is fine)")
}

func TestCacheNamespaces(t *testing.T) {
	t.Parallel()
	mk := func(patterns ...string) *completed {
		c, err := Config{RestConfig: &rest.Config{}, Namespace: "patrol", ApplicationNamespaces: patterns}.complete()
		require.NoError(t, err)
		return c
	}
	assert.Equal(t, map[string]cache.Config{"patrol": {}}, cacheNamespaces(mk(), false))
	assert.Equal(t, map[string]cache.Config{"patrol": {}}, cacheNamespaces(mk("a"), false), "multi=false caches only the control plane")
	assert.Equal(t, map[string]cache.Config{"patrol": {}, "a": {}, "b": {}}, cacheNamespaces(mk("a", "b"), true))
	assert.Equal(t, map[string]cache.Config{cache.AllNamespaces: {}}, cacheNamespaces(mk("team-*"), true))
}
