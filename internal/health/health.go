// Package health assesses the health of Kubernetes resources and aggregates resource
// health into application health.
//
// Semantics follow Argo CD v3.5.5 (gitops-engine pkg/health and controller/health.go):
// built-in Go checks for core workload kinds, an [Overrides] hook that takes precedence
// over the built-in checks (implemented by internal/lua for Lua customizations), the
// health ordering used by [IsWorse], and the application aggregation rule in [Aggregate].
package health

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Status is a health status code.
type Status string

const (
	// StatusUnknown means health assessment failed and the actual health is unknown.
	StatusUnknown Status = "Unknown"
	// StatusProgressing means the resource is not healthy yet but may still become healthy.
	StatusProgressing Status = "Progressing"
	// StatusHealthy means the resource is healthy.
	StatusHealthy Status = "Healthy"
	// StatusSuspended means the resource is suspended or paused and waits for an external
	// event (for example a suspended CronJob or a paused Deployment).
	StatusSuspended Status = "Suspended"
	// StatusDegraded means the resource failed or could not reach a healthy state.
	StatusDegraded Status = "Degraded"
	// StatusMissing means the resource does not exist in the cluster.
	StatusMissing Status = "Missing"
)

// HealthStatus is the result of a health assessment.
type HealthStatus struct {
	Status  Status `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// HookFinalizer is the finalizer the sync engine places on hook resources so they are
// deleted only after their phase completes. A resource that is being deleted is reported as
// "Pending deletion" (Progressing) unless it carries this finalizer, in which case its own
// health is still assessed (mirrors upstream's argocd.argoproj.io/hook-finalizer).
//
// The value must stay equal to the hook finalizer declared in api/v1alpha1.
const HookFinalizer = "cd.patrol.example/hook-finalizer"

// Sentinel errors returned (wrapped) by [Assess]. The error text itself mirrors upstream so
// that the Unknown status message shown to users is identical.
var (
	// ErrNilObject is returned when Assess is called with a nil object.
	ErrNilObject = errors.New("health: nil object")
	// ErrUnsupportedVersion means a built-in check exists for the group/kind but not for the
	// object's API version (for example apps/v1beta2 Deployment).
	ErrUnsupportedVersion = errors.New("health: unsupported API version")
	// ErrMalformedObject means the object could not be decoded into the shape the built-in
	// check expects (for example a string where an integer is required).
	ErrMalformedObject = errors.New("health: malformed object")
	// ErrOverride means the configured health override (Lua script) failed.
	ErrOverride = errors.New("health: override failed")
)

// Overrides supplies custom health assessments that take precedence over the built-in
// checks. GetResourceHealth returns (nil, nil) when it has no opinion about obj.
// internal/lua.Runtime implements it.
type Overrides interface {
	GetResourceHealth(obj *unstructured.Unstructured) (*HealthStatus, error)
}

// healthOrder lists status codes from most to least healthy.
var healthOrder = [...]Status{
	StatusHealthy,
	StatusSuspended,
	StatusProgressing,
	StatusMissing,
	StatusDegraded,
	StatusUnknown,
}

// rank returns the position of s in healthOrder. Unrecognized codes rank as Healthy,
// matching upstream.
func rank(s Status) int {
	for i, c := range healthOrder {
		if c == s {
			return i
		}
	}
	return 0
}

// IsWorse reports whether candidate is a worse condition than current, using the order
// Healthy < Suspended < Progressing < Missing < Degraded < Unknown. Unrecognized codes
// are treated as Healthy.
func IsWorse(current, candidate Status) bool {
	return rank(candidate) > rank(current)
}

// Valid reports whether s is one of the six defined status codes.
func (s Status) Valid() bool {
	switch s {
	case StatusUnknown, StatusProgressing, StatusHealthy, StatusSuspended, StatusDegraded, StatusMissing:
		return true
	}
	return false
}

// Assess returns the health of obj.
//
// Order of evaluation (identical to upstream):
//  1. An object with a deletion timestamp and without [HookFinalizer] is Progressing with
//     message "Pending deletion".
//  2. overrides (if non-nil) is consulted; an error yields an Unknown status carrying the
//     error text plus a wrapped [ErrOverride]; a non-nil result is returned as is.
//  3. The built-in check for the object's group/kind, if any. A check error yields an
//     Unknown status carrying the error text plus the error.
//  4. Otherwise (nil, nil): the kind has no health.
func Assess(obj *unstructured.Unstructured, overrides Overrides) (*HealthStatus, error) {
	if obj == nil {
		return nil, ErrNilObject
	}
	if obj.GetDeletionTimestamp() != nil && !hasFinalizer(obj, HookFinalizer) {
		return &HealthStatus{Status: StatusProgressing, Message: "Pending deletion"}, nil
	}
	if overrides != nil {
		hs, err := overrides.GetResourceHealth(obj)
		if err != nil {
			return &HealthStatus{Status: StatusUnknown, Message: err.Error()},
				fmt.Errorf("%w: failed to get resource health for %s/%s: %w", ErrOverride, obj.GetNamespace(), obj.GetName(), err)
		}
		if hs != nil {
			return hs, nil
		}
	}
	check := builtinCheck(obj.GroupVersionKind().GroupKind())
	if check == nil {
		return nil, nil
	}
	hs, err := check(obj)
	if err != nil {
		return &HealthStatus{Status: StatusUnknown, Message: err.Error()}, err
	}
	return hs, nil
}

// HasBuiltinCheck reports whether a built-in Go health check exists for gk.
func HasBuiltinCheck(gk schema.GroupKind) bool {
	return builtinCheck(gk) != nil
}

type checkFunc func(obj *unstructured.Unstructured) (*HealthStatus, error)

// builtinCheck returns the built-in check for gk, or nil. Version-specific handling happens
// inside each check, as upstream.
func builtinCheck(gk schema.GroupKind) checkFunc {
	switch gk.Group {
	case "":
		switch gk.Kind {
		case "Service":
			return serviceHealth
		case "PersistentVolumeClaim":
			return pvcHealth
		case "Pod":
			return podHealth
		}
	case "apps":
		switch gk.Kind {
		case "Deployment":
			return deploymentHealth
		case "StatefulSet":
			return statefulSetHealth
		case "ReplicaSet":
			return replicaSetHealth
		case "DaemonSet":
			return daemonSetHealth
		}
	case "extensions", "networking.k8s.io":
		if gk.Kind == "Ingress" {
			return ingressHealth
		}
	case "argoproj.io":
		if gk.Kind == "Workflow" {
			return argoWorkflowHealth
		}
	case "apiregistration.k8s.io":
		if gk.Kind == "APIService" {
			return apiServiceHealth
		}
	case "batch":
		if gk.Kind == "Job" {
			return jobHealth
		}
	case "autoscaling":
		if gk.Kind == "HorizontalPodAutoscaler" {
			return hpaHealth
		}
	}
	return nil
}

func hasFinalizer(obj *unstructured.Unstructured, name string) bool {
	for _, f := range obj.GetFinalizers() {
		if f == name {
			return true
		}
	}
	return false
}

// checkError carries an upstream-identical message while still matching a sentinel with
// errors.Is and exposing the underlying cause with errors.Unwrap.
type checkError struct {
	msg      string
	sentinel error
	cause    error
}

func (e *checkError) Error() string        { return e.msg }
func (e *checkError) Is(target error) bool { return target == e.sentinel }
func (e *checkError) Unwrap() error        { return e.cause }

func unsupportedGVK(kind string, gvk schema.GroupVersionKind) error {
	return &checkError{
		msg:      fmt.Sprintf("unsupported %s GVK: %s", kind, gvk),
		sentinel: ErrUnsupportedVersion,
	}
}

func conversionError(kind string, cause error) error {
	return &checkError{
		msg:      fmt.Sprintf("failed to convert unstructured %s to typed: %v", kind, cause),
		sentinel: ErrMalformedObject,
		cause:    cause,
	}
}
