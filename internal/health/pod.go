package health

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func podHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != corev1.SchemeGroupVersion.WithKind("Pod") {
		return nil, unsupportedGVK("Pod", gvk)
	}
	var p corev1.Pod
	if err := fromUnstructured(obj, "Pod", &p); err != nil {
		return nil, err
	}
	return assessPod(&p), nil
}

// isContainerErrorReason reports waiting reasons that indicate a failing container
// (ErrImagePull, CreateContainerConfigError, CrashLoopBackOff, ImagePullBackOff, ...).
func isContainerErrorReason(reason string) bool {
	return strings.HasPrefix(reason, "Err") ||
		strings.HasSuffix(reason, "Error") ||
		strings.HasSuffix(reason, "BackOff")
}

// assessPod mirrors upstream pod health:
//
//   - Waiting-container errors mark the pod Degraded only for restartPolicy Always. Pods
//     with OnFailure/Never are usually hooks; marking them Degraded on transient image pull
//     errors would fail the hook prematurely.
//   - Phase drives the rest: Pending=Progressing, Succeeded=Healthy, Failed=Degraded (with
//     the best available failure message, init containers first), Running depends on
//     readiness and restart policy, anything else is Unknown.
func assessPod(p *corev1.Pod) *HealthStatus {
	if p.Spec.RestartPolicy == corev1.RestartPolicyAlways {
		var msgs []string
		degraded := false
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && isContainerErrorReason(w.Reason) {
				degraded = true
				msgs = append(msgs, w.Message)
			}
		}
		if degraded {
			return &HealthStatus{Status: StatusDegraded, Message: strings.Join(msgs, ", ")}
		}
	}

	switch p.Status.Phase {
	case corev1.PodPending:
		return &HealthStatus{Status: StatusProgressing, Message: p.Status.Message}
	case corev1.PodSucceeded:
		return &HealthStatus{Status: StatusHealthy, Message: p.Status.Message}
	case corev1.PodFailed:
		if p.Status.Message != "" {
			return &HealthStatus{Status: StatusDegraded, Message: p.Status.Message}
		}
		for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
			for i := range list {
				if msg := terminatedMessage(&list[i]); msg != "" {
					return &HealthStatus{Status: StatusDegraded, Message: msg}
				}
			}
		}
		return &HealthStatus{Status: StatusDegraded}
	case corev1.PodRunning:
		switch p.Spec.RestartPolicy {
		case corev1.RestartPolicyAlways:
			if podReady(p) {
				return &HealthStatus{Status: StatusHealthy, Message: p.Status.Message}
			}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.LastTerminationState.Terminated != nil {
					return &HealthStatus{Status: StatusDegraded, Message: p.Status.Message}
				}
			}
			return &HealthStatus{Status: StatusProgressing, Message: p.Status.Message}
		case corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever:
			// Finite-lived pods (typically hooks) are Progressing until they complete.
			return &HealthStatus{Status: StatusProgressing, Message: p.Status.Message}
		}
	}
	return &HealthStatus{Status: StatusUnknown, Message: p.Status.Message}
}

// terminatedMessage returns the most specific failure description of a terminated
// container, or "" when the container did not fail.
func terminatedMessage(cs *corev1.ContainerStatus) string {
	t := cs.State.Terminated
	if t == nil {
		return ""
	}
	switch {
	case t.Message != "":
		return t.Message
	case t.Reason == "OOMKilled":
		return t.Reason
	case t.ExitCode != 0:
		return fmt.Sprintf("container %q failed with exit code %d", cs.Name, t.ExitCode)
	}
	return ""
}

// podReady reports whether the pod's Ready condition is True.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
