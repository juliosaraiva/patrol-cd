package health

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// fromUnstructured decodes obj into out, reporting failures as ErrMalformedObject with the
// upstream message for kind.
func fromUnstructured(obj *unstructured.Unstructured, kind string, out any) error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, out); err != nil {
		return conversionError(kind, err)
	}
	return nil
}

func deploymentHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != appsv1.SchemeGroupVersion.WithKind("Deployment") {
		return nil, unsupportedGVK("Deployment", gvk)
	}
	var d appsv1.Deployment
	if err := fromUnstructured(obj, "Deployment", &d); err != nil {
		return nil, err
	}
	return assessDeployment(&d), nil
}

// assessDeployment follows kubectl's rollout status logic plus the paused and
// ProgressDeadlineExceeded rules.
func assessDeployment(d *appsv1.Deployment) *HealthStatus {
	if d.Spec.Paused {
		return &HealthStatus{Status: StatusSuspended, Message: "Deployment is paused"}
	}
	if d.Generation > d.Status.ObservedGeneration {
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: "Waiting for rollout to finish: observed deployment generation less than desired generation",
		}
	}
	st := d.Status
	var progressing *appsv1.DeploymentCondition
	for i := range st.Conditions {
		if st.Conditions[i].Type == appsv1.DeploymentProgressing {
			progressing = &st.Conditions[i]
			break
		}
	}
	switch {
	case progressing != nil && progressing.Reason == "ProgressDeadlineExceeded":
		return &HealthStatus{
			Status:  StatusDegraded,
			Message: fmt.Sprintf("Deployment %q exceeded its progress deadline", d.Name),
		}
	case d.Spec.Replicas != nil && st.UpdatedReplicas < *d.Spec.Replicas:
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: fmt.Sprintf("Waiting for rollout to finish: %d out of %d new replicas have been updated...", st.UpdatedReplicas, *d.Spec.Replicas),
		}
	case st.Replicas > st.UpdatedReplicas:
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: fmt.Sprintf("Waiting for rollout to finish: %d old replicas are pending termination...", st.Replicas-st.UpdatedReplicas),
		}
	case st.AvailableReplicas < st.UpdatedReplicas:
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: fmt.Sprintf("Waiting for rollout to finish: %d of %d updated replicas are available...", st.AvailableReplicas, st.UpdatedReplicas),
		}
	}
	return &HealthStatus{Status: StatusHealthy}
}

func statefulSetHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != appsv1.SchemeGroupVersion.WithKind("StatefulSet") {
		return nil, unsupportedGVK("StatefulSet", gvk)
	}
	var s appsv1.StatefulSet
	if err := fromUnstructured(obj, "StatefulSet", &s); err != nil {
		return nil, err
	}
	return assessStatefulSet(&s), nil
}

func assessStatefulSet(s *appsv1.StatefulSet) *HealthStatus {
	st := s.Status
	if st.ObservedGeneration == 0 || s.Generation > st.ObservedGeneration {
		return &HealthStatus{Status: StatusProgressing, Message: "Waiting for statefulset spec update to be observed..."}
	}
	if s.Spec.Replicas != nil && st.ReadyReplicas < *s.Spec.Replicas {
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: fmt.Sprintf("Waiting for %d pods to be ready...", *s.Spec.Replicas-st.ReadyReplicas),
		}
	}
	strategy := s.Spec.UpdateStrategy
	if strategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && strategy.RollingUpdate != nil {
		if s.Spec.Replicas != nil && strategy.RollingUpdate.Partition != nil {
			want := *s.Spec.Replicas - *strategy.RollingUpdate.Partition
			if st.UpdatedReplicas < want {
				return &HealthStatus{
					Status:  StatusProgressing,
					Message: fmt.Sprintf("Waiting for partitioned roll out to finish: %d out of %d new pods have been updated...", st.UpdatedReplicas, want),
				}
			}
		}
		return &HealthStatus{
			Status:  StatusHealthy,
			Message: fmt.Sprintf("partitioned roll out complete: %d new pods have been updated...", st.UpdatedReplicas),
		}
	}
	if strategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return &HealthStatus{Status: StatusHealthy, Message: fmt.Sprintf("statefulset has %d ready pods", st.ReadyReplicas)}
	}
	if st.UpdateRevision != st.CurrentRevision {
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: fmt.Sprintf("waiting for statefulset rolling update to complete %d pods at revision %s...", st.UpdatedReplicas, st.UpdateRevision),
		}
	}
	return &HealthStatus{
		Status:  StatusHealthy,
		Message: fmt.Sprintf("statefulset rolling update complete %d pods at revision %s...", st.CurrentReplicas, st.CurrentRevision),
	}
}

func daemonSetHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != appsv1.SchemeGroupVersion.WithKind("DaemonSet") {
		return nil, unsupportedGVK("DaemonSet", gvk)
	}
	var d appsv1.DaemonSet
	if err := fromUnstructured(obj, "DaemonSet", &d); err != nil {
		return nil, err
	}
	return assessDaemonSet(&d), nil
}

func assessDaemonSet(d *appsv1.DaemonSet) *HealthStatus {
	st := d.Status
	if d.Generation > st.ObservedGeneration {
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: "Waiting for rollout to finish: observed daemon set generation less than desired generation",
		}
	}
	if d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
		return &HealthStatus{
			Status:  StatusHealthy,
			Message: fmt.Sprintf("daemon set %d out of %d new pods have been updated", st.UpdatedNumberScheduled, st.DesiredNumberScheduled),
		}
	}
	if st.UpdatedNumberScheduled < st.DesiredNumberScheduled {
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: fmt.Sprintf("Waiting for daemon set %q rollout to finish: %d out of %d new pods have been updated...", d.Name, st.UpdatedNumberScheduled, st.DesiredNumberScheduled),
		}
	}
	if st.NumberAvailable < st.DesiredNumberScheduled {
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: fmt.Sprintf("Waiting for daemon set %q rollout to finish: %d of %d updated pods are available...", d.Name, st.NumberAvailable, st.DesiredNumberScheduled),
		}
	}
	return &HealthStatus{Status: StatusHealthy}
}

func replicaSetHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != appsv1.SchemeGroupVersion.WithKind("ReplicaSet") {
		return nil, unsupportedGVK("ReplicaSet", gvk)
	}
	var r appsv1.ReplicaSet
	if err := fromUnstructured(obj, "ReplicaSet", &r); err != nil {
		return nil, err
	}
	return assessReplicaSet(&r), nil
}

func assessReplicaSet(r *appsv1.ReplicaSet) *HealthStatus {
	st := r.Status
	if r.Generation > st.ObservedGeneration {
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: "Waiting for rollout to finish: observed replica set generation less than desired generation",
		}
	}
	for i := range st.Conditions {
		c := st.Conditions[i]
		if c.Type != appsv1.ReplicaSetReplicaFailure {
			continue
		}
		// Only the first ReplicaFailure condition is considered.
		if c.Status == corev1.ConditionTrue {
			return &HealthStatus{Status: StatusDegraded, Message: c.Message}
		}
		break
	}
	if r.Spec.Replicas != nil && st.AvailableReplicas < *r.Spec.Replicas {
		return &HealthStatus{
			Status:  StatusProgressing,
			Message: fmt.Sprintf("Waiting for rollout to finish: %d out of %d new replicas are available...", st.AvailableReplicas, *r.Spec.Replicas),
		}
	}
	return &HealthStatus{Status: StatusHealthy}
}
