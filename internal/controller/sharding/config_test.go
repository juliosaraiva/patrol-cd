package sharding

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func envLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    Config
		warning string
	}{
		{
			name: "defaults",
			want: Config{Replicas: 0, Shard: -1, Algorithm: AlgorithmLegacy, Heartbeat: 10 * time.Second, ControllerName: "patrol-controller", Hostname: "patrol-controller-1"},
		},
		{
			name: "all set",
			env: map[string]string{
				EnvControllerReplicas: "3", EnvControllerShard: "2", EnvShardingAlgorithm: "round-robin",
				EnvDynamicClusterDistribution: "TRUE", EnvHeartbeatTime: "30", EnvApplicationControllerName: "ctrl",
			},
			want: Config{Replicas: 3, Shard: 2, Algorithm: AlgorithmRoundRobin, DynamicDistribution: true, Heartbeat: 30 * time.Second, ControllerName: "ctrl", Hostname: "patrol-controller-1"},
		},
		{
			name:    "heartbeat below range keeps the default",
			env:     map[string]string{EnvHeartbeatTime: "5"},
			want:    Config{Shard: -1, Algorithm: AlgorithmLegacy, Heartbeat: 10 * time.Second, ControllerName: "patrol-controller", Hostname: "patrol-controller-1"},
			warning: "Value in PATROL_CONTROLLER_HEARTBEAT_TIME is 5, which is less than minimum 10 allowed",
		},
		{
			name:    "heartbeat above range keeps the default",
			env:     map[string]string{EnvHeartbeatTime: "61"},
			want:    Config{Shard: -1, Algorithm: AlgorithmLegacy, Heartbeat: 10 * time.Second, ControllerName: "patrol-controller", Hostname: "patrol-controller-1"},
			warning: "which is greater than maximum 60 allowed",
		},
		{
			name:    "unparsable replicas",
			env:     map[string]string{EnvControllerReplicas: "two"},
			want:    Config{Shard: -1, Algorithm: AlgorithmLegacy, Heartbeat: 10 * time.Second, ControllerName: "patrol-controller", Hostname: "patrol-controller-1"},
			warning: "Could not parse 'two' as a number from environment PATROL_CONTROLLER_REPLICAS",
		},
		{
			name:    "unknown algorithm and non-boolean flag",
			env:     map[string]string{EnvShardingAlgorithm: "foo", EnvDynamicClusterDistribution: "yes"},
			want:    Config{Shard: -1, Algorithm: AlgorithmLegacy, Heartbeat: 10 * time.Second, ControllerName: "patrol-controller", Hostname: "patrol-controller-1"},
			warning: "distribution type foo is not supported, defaulting to legacy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			got := ConfigFromEnv(envLookup(tc.env), "patrol-controller-1", slog.New(slog.NewTextHandler(&buf, nil)))
			assert.Equal(t, tc.want, got)
			if tc.warning == "" {
				assert.Empty(t, buf.String())
			} else {
				assert.Contains(t, buf.String(), tc.warning)
			}
		})
	}
	assert.Equal(t, 30*time.Second, Config{}.LeaseDuration())
	assert.Equal(t, 90*time.Second, Config{Heartbeat: 30 * time.Second}.LeaseDuration())
}

func TestInferShard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		host string
		want int
		ok   bool
	}{
		{"patrol-controller-0", 0, true},
		{"patrol-controller-12", 12, true},
		{"7", 7, true},
		{"controller-abc", 0, false},
		{"controller-", 0, false},
		{"controller", 0, false},
	} {
		got, ok := InferShard(tc.host)
		assert.Equal(t, tc.want, got, tc.host)
		assert.Equal(t, tc.ok, ok, tc.host)
	}
}

func TestPreferredShard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		cfg      Config
		replicas int
		want     int
		warning  string
	}{
		{"single replica processes all shards", Config{Shard: -1, Hostname: "x-5"}, 1, 0, ""},
		{"ordinal", Config{Shard: -1, Hostname: "patrol-controller-1"}, 3, 1, ""},
		{"explicit shard overrides the hostname", Config{Shard: 1, Hostname: "patrol-controller-0"}, 2, 1, ""},
		{"hostname without ordinal", Config{Shard: -1, Hostname: "controller-abc"}, 2, 0, "hostname should end with shard number separated by '-' but got: controller-abc"},
		{"hostname without ordinal, dynamic", Config{Shard: -1, Hostname: "controller-abc", DynamicDistribution: true}, 2, -1, ""},
		{"ordinal beyond replicas is a standby", Config{Shard: -1, Hostname: "patrol-controller-2"}, 2, -1, "running as a standby"},
		{"explicit shard beyond replicas is a standby", Config{Shard: 5, Hostname: "patrol-controller-0"}, 2, -1, "running as a standby"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			assert.Equal(t, tc.want, tc.cfg.PreferredShard(tc.replicas, slog.New(slog.NewTextHandler(&buf, nil))))
			if tc.warning != "" {
				assert.Contains(t, buf.String(), tc.warning)
			} else {
				assert.Empty(t, buf.String())
			}
		})
	}
}

func TestLeaseNames(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "patrol-controller-shard-3", LeaseName(3))
	for name, want := range map[string]int{"patrol-controller-shard-0": 0, "patrol-controller-shard-12": 12} {
		got, ok := ShardFromLeaseName(name)
		assert.True(t, ok, name)
		assert.Equal(t, want, got)
	}
	for _, name := range []string{"patrol-controller-shard-", "patrol-controller-shard-01", "patrol-controller-shard--1", "other-lease", "patrol-controller-shard-x"} {
		_, ok := ShardFromLeaseName(name)
		assert.False(t, ok, name)
	}
}
