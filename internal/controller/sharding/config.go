package sharding

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
)

// Environment variables read by ConfigFromEnv. They are Argo CD's ARGOCD_* variables with the
// PATROL_ prefix (internal/settings PatrolEnv).
const (
	// EnvControllerReplicas is the number of controller replicas (upstream
	// ARGOCD_CONTROLLER_REPLICAS). Ignored with dynamic cluster distribution.
	EnvControllerReplicas = "PATROL_CONTROLLER_REPLICAS"
	// EnvControllerShard pins the replica's preferred shard (upstream ARGOCD_CONTROLLER_SHARD).
	// Unset or negative: inferred from the hostname (StatefulSet ordinal).
	EnvControllerShard = "PATROL_CONTROLLER_SHARD"
	// EnvShardingAlgorithm selects the cluster distribution algorithm (upstream
	// ARGOCD_CONTROLLER_SHARDING_ALGORITHM, cmd-params key controller.sharding.algorithm).
	EnvShardingAlgorithm = "PATROL_CONTROLLER_SHARDING_ALGORITHM"
	// EnvDynamicClusterDistribution enables dynamic cluster distribution (upstream
	// ARGOCD_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION): the replica count is read from the controller
	// workload and changes rebalance clusters without restarts.
	EnvDynamicClusterDistribution = "PATROL_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION"
	// EnvHeartbeatTime is the shard Lease renewal period in seconds (upstream
	// ARGOCD_CONTROLLER_HEARTBEAT_TIME): default 10, allowed 10..60. Leases expire after three
	// heartbeats.
	EnvHeartbeatTime = "PATROL_CONTROLLER_HEARTBEAT_TIME"
	// EnvApplicationControllerName names the controller workload whose replica count is used
	// with dynamic cluster distribution (upstream ARGOCD_APPLICATION_CONTROLLER_NAME).
	EnvApplicationControllerName = "PATROL_APPLICATION_CONTROLLER_NAME"
)

// Defaults.
const (
	// DefaultApplicationControllerName is the controller StatefulSet name; pods are named
	// <name>-<ordinal>.
	DefaultApplicationControllerName = "patrol-controller"
	// DefaultHeartbeat is the default Lease renewal period.
	DefaultHeartbeat = 10 * time.Second
	// MinHeartbeatSeconds and MaxHeartbeatSeconds bound EnvHeartbeatTime.
	MinHeartbeatSeconds = 10
	MaxHeartbeatSeconds = 60
	// HeartbeatTimeoutFactor: a shard Lease expires after this many missed heartbeats
	// (upstream HeartbeatTimeout = 3 × HeartbeatDuration).
	HeartbeatTimeoutFactor = 3
	// LeaseNamePrefix prefixes the shard Lease names: patrol-controller-shard-<n> (ADR-0008).
	LeaseNamePrefix = "patrol-controller-shard-"
	// MaxReplicas bounds replica counts accepted from the environment or the workload.
	MaxReplicas = 1024
)

// Config is the sharding configuration of one controller replica.
type Config struct {
	// Replicas is the static replica count (EnvControllerReplicas). Values below 1 mean one
	// replica processing all clusters ("Processing all cluster shards").
	Replicas int
	// Shard is the explicitly configured preferred shard (EnvControllerShard); -1 infers it
	// from Hostname.
	Shard int
	// Algorithm is the cluster distribution algorithm.
	Algorithm Algorithm
	// DynamicDistribution reads the replica count from the controller workload and rebalances
	// on changes.
	DynamicDistribution bool
	// Heartbeat is the Lease renewal period (DefaultHeartbeat).
	Heartbeat time.Duration
	// ControllerName is the controller workload name (DefaultApplicationControllerName).
	ControllerName string
	// Hostname is the pod hostname, used as the Lease holder identity and to infer the ordinal.
	Hostname string
}

// LeaseDuration is how long a shard Lease stays valid without renewal (3 × Heartbeat).
func (c Config) LeaseDuration() time.Duration {
	hb := c.Heartbeat
	if hb <= 0 {
		hb = DefaultHeartbeat
	}
	return HeartbeatTimeoutFactor * hb
}

// ConfigFromEnv reads the sharding configuration from lookup (os.LookupEnv in production)
// with upstream's parsing rules: unparsable or out-of-range numbers keep the default with a
// warning, booleans accept only true/false (any case), unknown algorithms fall back to legacy.
func ConfigFromEnv(lookup func(string) (string, bool), hostname string, logger *slog.Logger) Config {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	get := func(k string) string {
		if lookup == nil {
			return ""
		}
		v, _ := lookup(k)
		return v
	}
	name := strings.TrimSpace(get(EnvApplicationControllerName))
	if name == "" {
		name = DefaultApplicationControllerName
	}
	return Config{
		Replicas:            parseNum(logger, EnvControllerReplicas, get(EnvControllerReplicas), 0, 0, MaxReplicas),
		Shard:               parseNum(logger, EnvControllerShard, get(EnvControllerShard), -1, -math.MaxInt32, math.MaxInt32),
		Algorithm:           ParseAlgorithm(get(EnvShardingAlgorithm), logger),
		DynamicDistribution: parseBool(get(EnvDynamicClusterDistribution), false),
		Heartbeat:           time.Duration(parseNum(logger, EnvHeartbeatTime, get(EnvHeartbeatTime), int(DefaultHeartbeat/time.Second), MinHeartbeatSeconds, MaxHeartbeatSeconds)) * time.Second,
		ControllerName:      name,
		Hostname:            hostname,
	}
}

// parseNum follows upstream env.ParseNumFromEnv: empty → default; unparsable, below minimum
// or above maximum → default with a warning.
func parseNum(logger *slog.Logger, env, raw string, def, minimum, maximum int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 0)
	if err != nil {
		logger.Warn(fmt.Sprintf("Could not parse '%s' as a number from environment %s", raw, env), slog.String("env", env))
		return def
	}
	if int(n) < minimum {
		logger.Warn(fmt.Sprintf("Value in %s is %d, which is less than minimum %d allowed", env, n, minimum), slog.String("env", env))
		return def
	}
	if int(n) > maximum {
		logger.Warn(fmt.Sprintf("Value in %s is %d, which is greater than maximum %d allowed", env, n, maximum), slog.String("env", env))
		return def
	}
	return int(n)
}

// parseBool follows upstream env.ParseBoolFromEnv: only "true"/"false" (any case) count.
func parseBool(raw string, def bool) bool {
	switch {
	case strings.EqualFold(raw, "true"):
		return true
	case strings.EqualFold(raw, "false"):
		return false
	}
	return def
}

// InferShard returns the StatefulSet ordinal at the end of hostname (the part after the last
// "-"). ok is false when that part is not a number (upstream logs "hostname should end with
// shard number separated by '-' but got: <h>" and uses shard 0).
func InferShard(hostname string) (shard int, ok bool) {
	i := strings.LastIndex(hostname, "-")
	n, err := strconv.Atoi(hostname[i+1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// PreferredShard returns the shard this replica tries to own for the given replica count:
//
//   - replicas ≤ 1: shard 0 (one replica processes all clusters);
//   - an explicit Shard ≥ 0, else the hostname ordinal; a hostname without ordinal gives shard
//     0 with a warning, or -1 (claim any free shard) with dynamic distribution, where pods of a
//     Deployment have random names as upstream;
//   - a preferred shard ≥ replicas gives -1 with a warning: the replica runs as a standby.
//     Upstream falls back to shard 0 when the shard is greater than the replica count (and
//     keeps a shard equal to it); with Leases a standby is the safe equivalent.
func (c Config) PreferredShard(replicas int, logger *slog.Logger) int {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if replicas <= 1 {
		return 0
	}
	shard := c.Shard
	if shard < 0 {
		n, ok := InferShard(c.Hostname)
		switch {
		case ok:
			shard = n
		case c.DynamicDistribution:
			return -1
		default:
			logger.Warn(fmt.Sprintf("hostname should end with shard number separated by '-' but got: %s", c.Hostname))
			shard = 0
		}
	}
	if shard >= replicas {
		logger.Warn(fmt.Sprintf("Calculated shard number %d is not lower than the number of replicas (%d); running as a standby", shard, replicas),
			slog.Int("shard", shard), slog.Int("replicas", replicas))
		return -1
	}
	return shard
}

// LeaseName returns the name of the Lease of shard.
func LeaseName(shard int) string {
	return LeaseNamePrefix + strconv.Itoa(shard)
}

// ShardFromLeaseName parses a shard Lease name.
func ShardFromLeaseName(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, LeaseNamePrefix)
	if !ok || rest == "" || (len(rest) > 1 && rest[0] == '0') {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
