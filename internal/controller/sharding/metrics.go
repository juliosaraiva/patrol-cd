package sharding

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// assignerMetrics are the cluster assignment instruments.
type assignerMetrics struct {
	clusterShard  *prometheus.GaugeVec
	shardClusters *prometheus.GaugeVec
	replicas      prometheus.Gauge
}

func newAssignerMetrics(reg prometheus.Registerer) (*assignerMetrics, error) {
	m := &assignerMetrics{
		clusterShard: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patrol_controller_cluster_shard",
			Help: "Shard assigned to each destination cluster (one series per cluster).",
		}, []string{"server", "name"}),
		shardClusters: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patrol_controller_shard_clusters",
			Help: "Number of destination clusters assigned to each shard.",
		}, []string{"shard"}),
		replicas: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "patrol_controller_shard_replicas",
			Help: "Number of controller replicas (shards) clusters are distributed over.",
		}),
	}
	if reg == nil {
		return m, nil
	}
	var err error
	if m.clusterShard, err = register(reg, m.clusterShard); err != nil {
		return nil, err
	}
	if m.shardClusters, err = register(reg, m.shardClusters); err != nil {
		return nil, err
	}
	if m.replicas, err = register(reg, m.replicas); err != nil {
		return nil, err
	}
	return m, nil
}

// leaseMetrics are the shard ownership instruments.
type leaseMetrics struct {
	owned       *prometheus.GaugeVec
	transitions *prometheus.CounterVec
	errors      *prometheus.CounterVec
}

// Lease transition events counted by patrol_controller_shard_lease_transitions_total.
const (
	eventAcquired = "acquired"
	eventLost     = "lost"
	eventReleased = "released"
	eventHandoff  = "handoff_requested"
)

// Reasons of patrol_controller_shard_heartbeat_errors_total: conflict is a lost
// optimistic-concurrency race (Conflict, AlreadyExists), error anything else.
const (
	reasonConflict = "conflict"
	reasonError    = "error"
)

func newLeaseMetrics(reg prometheus.Registerer) (*leaseMetrics, error) {
	m := &leaseMetrics{
		owned: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patrol_controller_shard_owned",
			Help: "1 when this replica holds the shard Lease, else 0.",
		}, []string{"shard"}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_controller_shard_lease_transitions_total",
			Help: "Shard Lease ownership transitions of this replica (acquired, lost, released, handoff_requested).",
		}, []string{"shard", "event"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patrol_controller_shard_heartbeat_errors_total",
			Help: "Failed heartbeat API calls (replica count, shard Leases), by operation and reason (conflict: lost a concurrent update race; error: anything else).",
		}, []string{"op", "reason"}),
	}
	if reg == nil {
		return m, nil
	}
	var err error
	if m.owned, err = register(reg, m.owned); err != nil {
		return nil, err
	}
	if m.transitions, err = register(reg, m.transitions); err != nil {
		return nil, err
	}
	if m.errors, err = register(reg, m.errors); err != nil {
		return nil, err
	}
	return m, nil
}

// register registers c, reusing an identical collector registered earlier.
func register[T prometheus.Collector](reg prometheus.Registerer, c T) (T, error) {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(T); ok {
				return existing, nil
			}
		}
		var zero T
		return zero, fmt.Errorf("register sharding metrics: %w", err)
	}
	return c, nil
}
