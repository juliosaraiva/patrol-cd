package health

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Patrol Application identity. These must match api/v1alpha1 (GroupName and the
// Application kind); this package deliberately does not import the API types.
const (
	ApplicationGroup = "cd.patrol.example"
	ApplicationKind  = "Application"
)

// invalidHealthStatusMessage is reported when a customization yields an unrecognized
// status code. The text matches upstream's Lua runtime so UIs and alerts behave the same.
const invalidHealthStatusMessage = "Lua returned an invalid health status"

// IsApplication reports whether obj is a Patrol Application (any version).
func IsApplication(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	gk := obj.GroupVersionKind().GroupKind()
	return gk.Group == ApplicationGroup && gk.Kind == ApplicationKind
}

// ApplicationHealth reports the health a Patrol Application records in status.health.
// It returns nil when obj is not a Patrol Application.
//
// The result is identical to running the Lua customization that upstream documents for
// restoring Application health (resource.customizations.health.argoproj.io_Application):
// Progressing with an empty message until status.health is set, then status.health.status
// and status.health.message; an unrecognized status code becomes Unknown with
// "Lua returned an invalid health status".
//
// Application health is not part of the built-in checks (upstream removed it in v1.8 to
// avoid app-of-apps deadlocks). Enable it explicitly with [WithApplicationHealth], or with
// the equivalent Lua customization keyed cd.patrol.example/Application.
func ApplicationHealth(obj *unstructured.Unstructured) *HealthStatus {
	if !IsApplication(obj) {
		return nil
	}
	h, ok, _ := unstructured.NestedMap(obj.Object, "status", "health")
	if !ok || h == nil {
		return &HealthStatus{Status: StatusProgressing}
	}
	status, _ := h["status"].(string)
	message, _ := h["message"].(string)
	if !Status(status).Valid() {
		return &HealthStatus{Status: StatusUnknown, Message: invalidHealthStatusMessage}
	}
	return &HealthStatus{Status: Status(status), Message: message}
}

// WithApplicationHealth returns Overrides that consult next first and, when next has no
// opinion, report Patrol Application health via [ApplicationHealth]. next may be nil.
func WithApplicationHealth(next Overrides) Overrides {
	return applicationOverrides{next: next}
}

type applicationOverrides struct{ next Overrides }

func (a applicationOverrides) GetResourceHealth(obj *unstructured.Unstructured) (*HealthStatus, error) {
	if a.next != nil {
		hs, err := a.next.GetResourceHealth(obj)
		if err != nil || hs != nil {
			return hs, err
		}
	}
	return ApplicationHealth(obj), nil
}
