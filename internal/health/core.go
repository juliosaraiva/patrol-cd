package health

import (
	"encoding/json"
	"fmt"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func serviceHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != corev1.SchemeGroupVersion.WithKind("Service") {
		return nil, unsupportedGVK("Service", gvk)
	}
	var s corev1.Service
	if err := fromUnstructured(obj, "Service", &s); err != nil {
		return nil, err
	}
	// Only LoadBalancer services wait for an external address.
	if s.Spec.Type == corev1.ServiceTypeLoadBalancer && len(s.Status.LoadBalancer.Ingress) == 0 {
		return &HealthStatus{Status: StatusProgressing}, nil
	}
	return &HealthStatus{Status: StatusHealthy}, nil
}

func pvcHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim") {
		return nil, unsupportedGVK("PersistentVolumeClaim", gvk)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := fromUnstructured(obj, "PersistentVolumeClaim", &pvc); err != nil {
		return nil, err
	}
	switch pvc.Status.Phase {
	case corev1.ClaimLost:
		return &HealthStatus{Status: StatusDegraded}, nil
	case corev1.ClaimPending:
		return &HealthStatus{Status: StatusProgressing}, nil
	case corev1.ClaimBound:
		return &HealthStatus{Status: StatusHealthy}, nil
	}
	return &HealthStatus{Status: StatusUnknown}, nil
}

// ingressHealth handles extensions and networking.k8s.io Ingress of any version: healthy
// once the load balancer reports at least one ingress point.
func ingressHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	ingresses, _, _ := unstructured.NestedSlice(obj.Object, "status", "loadBalancer", "ingress")
	if len(ingresses) > 0 {
		return &HealthStatus{Status: StatusHealthy}, nil
	}
	return &HealthStatus{Status: StatusProgressing}, nil
}

// argoWorkflowHealth maps an Argo Workflows Workflow phase (any version).
func argoWorkflowHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	message, _, _ := unstructured.NestedString(obj.Object, "status", "message")
	switch phase {
	case "", "Pending", "Running":
		return &HealthStatus{Status: StatusProgressing, Message: message}, nil
	case "Succeeded":
		return &HealthStatus{Status: StatusHealthy, Message: message}, nil
	case "Failed", "Error":
		return &HealthStatus{Status: StatusDegraded, Message: message}, nil
	}
	return &HealthStatus{Status: StatusUnknown, Message: message}, nil
}

// apiService mirrors the subset of apiregistration.k8s.io APIService (v1 and v1beta1 share
// the shape) that typed decoding validates. The kube-aggregator types are not a dependency.
type apiService struct {
	Spec struct {
		Service *struct {
			Namespace string `json:"namespace,omitempty"`
			Name      string `json:"name,omitempty"`
			Port      *int32 `json:"port,omitempty"`
		} `json:"service,omitempty"`
		Group                 string `json:"group,omitempty"`
		Version               string `json:"version,omitempty"`
		InsecureSkipTLSVerify bool   `json:"insecureSkipTLSVerify,omitempty"`
		CABundle              []byte `json:"caBundle,omitempty"`
		GroupPriorityMinimum  int32  `json:"groupPriorityMinimum"`
		VersionPriority       int32  `json:"versionPriority"`
	} `json:"spec,omitempty"`
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"conditions,omitempty"`
	} `json:"status,omitempty"`
}

var (
	apiServiceV1      = schema.GroupVersionKind{Group: "apiregistration.k8s.io", Version: "v1", Kind: "APIService"}
	apiServiceV1beta1 = schema.GroupVersionKind{Group: "apiregistration.k8s.io", Version: "v1beta1", Kind: "APIService"}
)

func apiServiceHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != apiServiceV1 && gvk != apiServiceV1beta1 {
		return nil, unsupportedGVK("APIService", gvk)
	}
	var svc apiService
	if err := fromUnstructured(obj, "APIService", &svc); err != nil {
		return nil, err
	}
	for _, c := range svc.Status.Conditions {
		if c.Type != "Available" {
			continue
		}
		msg := fmt.Sprintf("%s: %s", c.Reason, c.Message)
		if c.Status == string(corev1.ConditionTrue) {
			return &HealthStatus{Status: StatusHealthy, Message: msg}, nil
		}
		return &HealthStatus{Status: StatusProgressing, Message: msg}, nil
	}
	return &HealthStatus{Status: StatusProgressing, Message: "Waiting to be processed"}, nil
}

func jobHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if gvk := obj.GroupVersionKind(); gvk != batchv1.SchemeGroupVersion.WithKind("Job") {
		return nil, unsupportedGVK("Job", gvk)
	}
	var j batchv1.Job
	if err := fromUnstructured(obj, "Job", &j); err != nil {
		return nil, err
	}
	return assessJob(&j), nil
}

// assessJob mirrors upstream: the presence of a Failed, Complete or Suspended condition
// (regardless of its status, except that Suspended must be True to count as suspended)
// marks the job as finished; Failed wins over Suspended, which wins over Complete.
func assessJob(j *batchv1.Job) *HealthStatus {
	var (
		failed, complete, suspended bool
		failMsg, msg                string
	)
	for _, c := range j.Status.Conditions {
		switch c.Type {
		case batchv1.JobFailed:
			failed, complete = true, true
			failMsg = c.Message
		case batchv1.JobComplete:
			complete = true
			msg = c.Message
		case batchv1.JobSuspended:
			complete = true
			msg = c.Message
			if c.Status == corev1.ConditionTrue {
				suspended = true
			}
		}
	}
	switch {
	case !complete:
		return &HealthStatus{Status: StatusProgressing, Message: msg}
	case failed:
		return &HealthStatus{Status: StatusDegraded, Message: failMsg}
	case suspended:
		// Upstream reports the (normally empty) failure message for suspended jobs.
		return &HealthStatus{Status: StatusSuspended, Message: failMsg}
	}
	return &HealthStatus{Status: StatusHealthy, Message: msg}
}

// hpaCondition is the common shape of HPA conditions. For autoscaling/v1 they are read from
// the autoscaling.alpha.kubernetes.io/conditions annotation (JSON, case-insensitive keys).
type hpaCondition struct {
	Type    string
	Reason  string
	Message string
	Status  string
}

const hpaV1ConditionsAnnotation = "autoscaling.alpha.kubernetes.io/conditions"

func hpaWaiting() *HealthStatus {
	return &HealthStatus{Status: StatusProgressing, Message: "Waiting to Autoscale"}
}

func hpaHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	switch gvk := obj.GroupVersionKind(); gvk {
	case autoscalingv1.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"):
		var hpa autoscalingv1.HorizontalPodAutoscaler
		if err := fromUnstructured(obj, "HPA", &hpa); err != nil {
			return nil, err
		}
		raw, ok := hpa.Annotations[hpaV1ConditionsAnnotation]
		if !ok {
			return hpaWaiting(), nil
		}
		var conds []hpaCondition
		if err := json.Unmarshal([]byte(raw), &conds); err != nil {
			return nil, &checkError{
				msg:      fmt.Sprintf("failed to convert conditions annotation to typed: %v", err),
				sentinel: ErrMalformedObject,
				cause:    err,
			}
		}
		return assessHPA(conds), nil
	case autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"):
		var hpa autoscalingv2.HorizontalPodAutoscaler
		if err := fromUnstructured(obj, "HPA", &hpa); err != nil {
			return nil, err
		}
		conds := make([]hpaCondition, 0, len(hpa.Status.Conditions))
		for _, c := range hpa.Status.Conditions {
			conds = append(conds, hpaCondition{Type: string(c.Type), Reason: c.Reason, Message: c.Message, Status: string(c.Status)})
		}
		return assessHPA(conds), nil
	default:
		return nil, unsupportedGVK("HPA", gvk)
	}
}

// hpaDegradedReasons maps condition type to the reasons that mark an HPA Degraded.
var hpaDegradedReasons = map[string]map[string]bool{
	"AbleToScale": {"FailedGetScale": true, "FailedUpdateScale": true},
	"ScalingActive": {
		"FailedGetResourceMetric":      true,
		"FailedGetObjectMetric":        true,
		"FailedGetPodsMetric":          true,
		"FailedGetExternalMetric":      true,
		"FailedComputeMetricsReplicas": true,
		"InvalidSelector":              true,
	},
}

// assessHPA: any degraded condition wins (regardless of status); otherwise the first
// AbleToScale/ScalingLimited condition with status True is Healthy; otherwise Progressing.
func assessHPA(conds []hpaCondition) *HealthStatus {
	for _, c := range conds {
		if hpaDegradedReasons[c.Type][c.Reason] {
			return &HealthStatus{Status: StatusDegraded, Message: c.Message}
		}
	}
	for _, c := range conds {
		if (c.Type == "AbleToScale" || c.Type == "ScalingLimited") && c.Status == "True" {
			return &HealthStatus{Status: StatusHealthy, Message: c.Message}
		}
	}
	return hpaWaiting()
}
