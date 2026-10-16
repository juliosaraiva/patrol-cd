package sharding

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ReplicaSource returns the number of controller replicas (shards).
type ReplicaSource interface {
	Replicas(ctx context.Context) (int, error)
}

// StaticReplicas is a fixed replica count (EnvControllerReplicas). Values below 1 mean one
// replica.
type StaticReplicas int

// Replicas implements ReplicaSource.
func (s StaticReplicas) Replicas(context.Context) (int, error) { return max(int(s), 1), nil }

// WorkloadKind is the kind of the controller workload read with dynamic cluster distribution.
type WorkloadKind string

// Workload kinds.
const (
	WorkloadStatefulSet WorkloadKind = "StatefulSet"
	WorkloadDeployment  WorkloadKind = "Deployment"
)

// WorkloadReplicas reads spec.replicas of the controller workload (dynamic cluster
// distribution, OPS-045): scaling the workload changes the shard count without restarting
// the running replicas.
type WorkloadReplicas struct {
	Client    kubernetes.Interface
	Namespace string
	// Name is the workload name (Config.ControllerName).
	Name string
	// Kind is the workload kind (StatefulSet when empty).
	Kind WorkloadKind
}

// ErrInvalidReplicas reports a controller workload without a usable replica count.
var ErrInvalidReplicas = errors.New("invalid controller replica count")

// Replicas implements ReplicaSource with upstream's messages (Deployment replaced by the
// configured kind): "(dynamic cluster distribution) failed to get app controller <kind>: …",
// "… failed to get app controller <kind> replica count" and "application controller <kind>
// replicas is not set or is less than 0, replicas: <n>".
func (w WorkloadReplicas) Replicas(ctx context.Context) (int, error) {
	kind := w.Kind
	if kind == "" {
		kind = WorkloadStatefulSet
	}
	var replicas *int32
	switch kind {
	case WorkloadDeployment:
		d, err := w.Client.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return 0, fmt.Errorf("(dynamic cluster distribution) failed to get app controller deployment: %w", err)
		}
		replicas = d.Spec.Replicas
	default:
		s, err := w.Client.AppsV1().StatefulSets(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return 0, fmt.Errorf("(dynamic cluster distribution) failed to get app controller statefulset: %w", err)
		}
		replicas = s.Spec.Replicas
	}
	lower := map[WorkloadKind]string{WorkloadDeployment: "deployment", WorkloadStatefulSet: "statefulset"}[kind]
	if replicas == nil {
		return 0, fmt.Errorf("(dynamic cluster distribution) failed to get app controller %s replica count: %w", lower, ErrInvalidReplicas)
	}
	if *replicas <= 0 {
		return 0, fmt.Errorf("application controller %s replicas is not set or is less than 0, replicas: %d: %w", lower, *replicas, ErrInvalidReplicas)
	}
	if *replicas > MaxReplicas {
		return 0, fmt.Errorf("application controller %s replicas %d exceeds the maximum %d: %w", lower, *replicas, MaxReplicas, ErrInvalidReplicas)
	}
	return int(*replicas), nil
}
