package tracking

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Errors returned by [ResourceTracker.SetAppInstance] and [ResourceTracker.RemoveAppInstance].
var (
	// ErrNilObject is returned when the object to modify is nil.
	ErrNilObject = errors.New("object is nil")
	// ErrInvalidInstanceName is returned for an empty instance name, and for an instance name
	// containing ':' under annotation-based tracking (it would produce an unparseable id).
	ErrInvalidInstanceName = errors.New("invalid application instance name")
)

// Config holds the installation-wide settings a tracker needs. The tracking method and the
// installation ID are passed per call, as upstream reads them from live settings.
type Config struct {
	// InstanceLabelKey is the instance label (application.instanceLabelKey). Empty selects
	// v1alpha1.LabelKeyAppInstanceDefault ("app.kubernetes.io/instance").
	InstanceLabelKey string
	// ControlPlaneNamespace is the namespace Patrol CD is installed in. Applications in it
	// have unqualified instance names; Applications elsewhere use "<namespace>_<name>".
	ControlPlaneNamespace string
}

// Validate checks that the instance label key is a valid label key and the control-plane
// namespace (when set) a valid namespace name. [New] does not require a valid Config: an
// invalid label key is rejected by the API server when it is written.
func (c Config) Validate() error {
	var errs []error
	if c.InstanceLabelKey != "" {
		if msgs := validation.IsQualifiedName(c.InstanceLabelKey); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("invalid instance label key %q: %s", c.InstanceLabelKey, strings.Join(msgs, "; ")))
		}
	}
	if c.ControlPlaneNamespace != "" {
		if msgs := validation.IsDNS1123Label(c.ControlPlaneNamespace); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("invalid control-plane namespace %q: %s", c.ControlPlaneNamespace, strings.Join(msgs, "; ")))
		}
	}
	return errors.Join(errs...)
}

// Tracker writes and reads resource tracking metadata. method is the configured tracking
// method (see [EffectiveMethod]) and installationID the configured installation ID ("" when
// unset).
type Tracker interface {
	// SetAppInstance writes the tracking metadata of the Application with the given instance
	// name (see [InstanceName]) onto obj. defaultNamespace, the Application's destination
	// namespace, is recorded in the tracking id when obj has no namespace (cluster-scoped
	// resources).
	SetAppInstance(obj *unstructured.Unstructured, instanceName, defaultNamespace string, method Method, installationID string) error
	// RemoveAppInstance removes the tracking metadata written by method (used for hydrated
	// manifests).
	RemoveAppInstance(obj *unstructured.Unstructured, method Method) error
	// GetAppName returns the instance name obj is tracked by, or "" when it is not tracked
	// (missing or malformed metadata, or another installation ID).
	GetAppName(obj *unstructured.Unstructured, method Method, installationID string) string
	// GetAppInstance returns the parsed tracking id of obj under annotation-based methods,
	// or nil (label method, missing or malformed id, other installation ID).
	GetAppInstance(obj *unstructured.Unstructured, method Method, installationID string) *AppInstanceValue
	// IsSelfReferencedObject reports whether live may be treated as the resource its
	// tracking metadata names. target is the desired object paired with live, or nil.
	IsSelfReferencedObject(live, target *unstructured.Unstructured, method Method, installationID string) bool
	// Normalize adjusts live (a copy made for diffing) so that a change of tracking method
	// alone is not reported as a difference.
	Normalize(target, live *unstructured.Unstructured, method Method) error
}

// ResourceTracker is the [Tracker] implementation. It is immutable and safe for concurrent
// use; build a new one when settings change.
type ResourceTracker struct {
	labelKey              string
	controlPlaneNamespace string
}

var _ Tracker = (*ResourceTracker)(nil)

// New returns a tracker for cfg.
func New(cfg Config) *ResourceTracker {
	key := cfg.InstanceLabelKey
	if key == "" {
		key = v1alpha1.LabelKeyAppInstanceDefault
	}
	return &ResourceTracker{labelKey: key, controlPlaneNamespace: cfg.ControlPlaneNamespace}
}

// InstanceLabelKey returns the effective instance label key.
func (t *ResourceTracker) InstanceLabelKey() string { return t.labelKey }

// ControlPlaneNamespace returns the configured control-plane namespace.
func (t *ResourceTracker) ControlPlaneNamespace() string { return t.controlPlaneNamespace }

// InstanceName returns the instance name of app (see [InstanceName]).
func (t *ResourceTracker) InstanceName(app App) string {
	return InstanceName(app.Name, app.Namespace, t.controlPlaneNamespace)
}

// TrackingLabelKeys returns the label keys tracking reads, for live-state caches that keep
// only tracking metadata.
func (t *ResourceTracker) TrackingLabelKeys() []string { return []string{t.labelKey} }

// TrackingAnnotationKeys returns the annotation keys tracking reads.
func (t *ResourceTracker) TrackingAnnotationKeys() []string {
	return []string{v1alpha1.AnnotationKeyTrackingID, v1alpha1.AnnotationKeyInstallationID}
}

// DiffTrackingKeys returns the keys for internal/diff's TrackingOptions under method: the
// instance label key and the tracking-id annotation key, or two empty strings (normalization
// disabled) under label tracking. Passing them to diff.Options.Tracking is equivalent to
// calling [ResourceTracker.Normalize] on copies before diffing.
func (t *ResourceTracker) DiffTrackingKeys(method Method) (labelKey, annotationKey string) {
	if !UsesAnnotation(method) {
		return "", ""
	}
	return t.labelKey, v1alpha1.AnnotationKeyTrackingID
}

// SetAppInstance implements [Tracker].
//
//   - label: the instance label is set to instanceName, untruncated (the API server rejects
//     values longer than 63 bytes, as upstream).
//   - annotation: the installation-id annotation is set to installationID, or removed when
//     it is empty; the tracking-id annotation is set to
//     "<instanceName>:<group>/<kind>:<namespace>/<name>".
//   - annotation+label: as annotation, plus the instance label truncated with
//     [TruncateLabel].
//
// Everything is validated before obj is modified, so an error leaves obj unchanged. Errors:
// [ErrNilObject], [ErrInvalidInstanceName], [ErrLabelTruncation] (wrapped as "failed to set
// app instance label: ...") and labels or annotations that are not string maps.
func (t *ResourceTracker) SetAppInstance(obj *unstructured.Unstructured, instanceName, defaultNamespace string, method Method, installationID string) error {
	if obj == nil {
		return ErrNilObject
	}
	if instanceName == "" {
		return fmt.Errorf("%w: empty", ErrInvalidInstanceName)
	}
	m := EffectiveMethod(method)
	if m == MethodLabel {
		if err := setLabel(obj, t.labelKey, instanceName); err != nil {
			return fmt.Errorf("failed to set app instance label: %w", err)
		}
		return nil
	}
	if strings.Contains(instanceName, ":") {
		return fmt.Errorf("%w: %q contains ':'", ErrInvalidInstanceName, instanceName)
	}
	annotations, err := metadataStringMap(obj, "annotations")
	if err != nil {
		return fmt.Errorf("failed to get annotations from target object %s: %w", describe(obj), err)
	}
	var labels map[string]string
	var labelVal string
	if m == MethodAnnotationAndLabel {
		if labelVal, err = TruncateLabel(instanceName); err != nil {
			return fmt.Errorf("failed to set app instance label: %w", err)
		}
		if labels, err = metadataStringMap(obj, "labels"); err != nil {
			return fmt.Errorf("failed to set app instance label: failed to get labels from target object %s: %w", describe(obj), err)
		}
	}

	// All inputs are valid: commit. The maps are copies, so nothing was modified so far.
	if annotations == nil {
		annotations = make(map[string]string, 2)
	}
	if installationID != "" {
		annotations[v1alpha1.AnnotationKeyInstallationID] = installationID
	} else {
		delete(annotations, v1alpha1.AnnotationKeyInstallationID)
	}
	annotations[v1alpha1.AnnotationKeyTrackingID] = BuildTrackingID(NewAppInstanceValue(obj, instanceName, defaultNamespace))
	obj.SetAnnotations(annotations)
	if m == MethodAnnotationAndLabel {
		if labels == nil {
			labels = make(map[string]string, 1)
		}
		labels[t.labelKey] = labelVal
		obj.SetLabels(labels)
	}
	return nil
}

// RemoveAppInstance implements [Tracker]. Annotation-based methods remove the tracking-id
// and installation-id annotations. The label and annotation+label methods remove the label
// v1alpha1.LabelKeyAppInstanceDefault — not a custom InstanceLabelKey — reproducing the
// upstream behavior used for hydrated manifests (APP-073 edge case).
func (t *ResourceTracker) RemoveAppInstance(obj *unstructured.Unstructured, method Method) error {
	if obj == nil {
		return ErrNilObject
	}
	m := EffectiveMethod(method)
	if m != MethodLabel {
		if err := removeAnnotation(obj, v1alpha1.AnnotationKeyTrackingID); err != nil {
			return err
		}
		if err := removeAnnotation(obj, v1alpha1.AnnotationKeyInstallationID); err != nil {
			return err
		}
	}
	if m != MethodAnnotation {
		if err := removeLabel(obj, v1alpha1.LabelKeyAppInstanceDefault); err != nil {
			return err
		}
	}
	return nil
}

// GetAppName implements [Tracker]. Under label tracking it returns the instance label
// (the installation ID is not consulted). Under annotation-based methods it returns the
// application name of the tracking id when the installation-id annotation equals
// installationID exactly; the instance label is ignored.
func (t *ResourceTracker) GetAppName(obj *unstructured.Unstructured, method Method, installationID string) string {
	if obj == nil {
		return ""
	}
	if EffectiveMethod(method) == MethodLabel {
		v, err := getLabel(obj, t.labelKey)
		if err != nil {
			return ""
		}
		return v
	}
	if v := appInstanceValue(obj, installationID); v != nil {
		return v.ApplicationName
	}
	return ""
}

// GetAppInstance implements [Tracker].
func (t *ResourceTracker) GetAppInstance(obj *unstructured.Unstructured, method Method, installationID string) *AppInstanceValue {
	if obj == nil || EffectiveMethod(method) == MethodLabel {
		return nil
	}
	return appInstanceValue(obj, installationID)
}

// appInstanceValue returns the parsed tracking id of obj, or nil when the installation-id
// annotation differs from installationID or the id is missing or malformed.
func appInstanceValue(obj *unstructured.Unstructured, installationID string) *AppInstanceValue {
	ann, err := metadataStringMap(obj, "annotations")
	if err != nil || ann[v1alpha1.AnnotationKeyInstallationID] != installationID {
		return nil
	}
	v, err := ParseTrackingID(ann[v1alpha1.AnnotationKeyTrackingID])
	if err != nil {
		return nil
	}
	return v
}

// IsSelfReferencedObject implements [Tracker], as upstream's isSelfReferencedObj:
//
//   - a nil live object, and every object under label tracking, is self-referenced (label
//     tracking carries no resource identity);
//   - when target is given, the identity expected from target is compared with live (this
//     keeps API group migrations, e.g. extensions -> networking.k8s.io Ingress, managed);
//   - otherwise the live tracking id is compared with live; a missing or unreadable id
//     (including another installation ID) is treated as self-referenced.
//
// The comparison is [AppInstanceValue.References]: group, kind and name equal, namespace
// equal unless live is cluster-scoped. The application name is not compared.
func (t *ResourceTracker) IsSelfReferencedObject(live, target *unstructured.Unstructured, method Method, installationID string) bool {
	if live == nil || EffectiveMethod(method) == MethodLabel {
		return true
	}
	if target != nil {
		return NewAppInstanceValue(target, "", "").References(live)
	}
	v := appInstanceValue(live, installationID)
	if v == nil {
		return true
	}
	return v.References(live)
}

// Normalize implements [Tracker] (upstream ResourceTracking.Normalize, APP-076/DIFF-026). It
// is a no-op under label tracking, when either object is nil, when live has no instance
// label, or when live is a CustomResourceDefinition. Otherwise, on live only:
//
//   - if live has no tracking-id annotation, target's tracking-id annotation is copied onto
//     it (an empty value when target has none, as upstream), so resources last synced with
//     label tracking do not show as OutOfSync;
//   - if target has no instance label, the label is removed from live.
//
// A live tracking id that differs from target's is kept: a resource that moved to another
// Application must still show a difference. target is never modified. Call it on copies.
func (t *ResourceTracker) Normalize(target, live *unstructured.Unstructured, method Method) error {
	if !UsesAnnotation(method) || target == nil || live == nil {
		return nil
	}
	label, err := getLabel(live, t.labelKey)
	if err != nil {
		return fmt.Errorf("failed to get app instance label: %w", err)
	}
	if label == "" || IsCRD(live) {
		return nil
	}
	liveID, err := getAnnotation(live, v1alpha1.AnnotationKeyTrackingID)
	if err != nil {
		return err
	}
	if liveID == "" {
		targetID, err := getAnnotation(target, v1alpha1.AnnotationKeyTrackingID)
		if err != nil {
			return err
		}
		if err := setAnnotation(live, v1alpha1.AnnotationKeyTrackingID, targetID); err != nil {
			return err
		}
	}
	targetLabel, err := getLabel(target, t.labelKey)
	if err != nil {
		return fmt.Errorf("failed to get app instance label: %w", err)
	}
	if targetLabel == "" {
		if err := removeLabel(live, t.labelKey); err != nil {
			return fmt.Errorf("failed to remove app instance label: %w", err)
		}
	}
	return nil
}

// IsManagedBy reports whether obj is a managed root of app: it is tracked by app's instance
// name under method and installationID, it has no ownerReferences, and its tracking metadata
// refers to obj itself. These are the conditions under which upstream reports a live object
// without a desired counterpart as requiring pruning (managed live set of
// gitops-engine/pkg/cache/cluster.go GetManagedLiveObjs, then isSelfReferencedObj in
// controller/state.go). Untracked objects, objects tracked by another Application or
// installation, owned children that inherited the tracking label or annotation (for example
// Pods and ReplicaSets carrying a pod-template instance label under label tracking), and
// copies carrying another object's tracking id are not managed. An ownerReferences field
// that cannot be read as a list counts as owned.
//
// Only ownerReferences present on obj are considered; owners the live-state cache
// synthesizes (for example Endpoints -> Service) must be checked by the caller.
//
// IsManagedBy reads only obj's own metadata, which anyone allowed to write obj can set, so a
// self-consistent forged tracking id is accepted. It is a necessary condition for pruning,
// not an authorization check: callers must also enforce AppProject destination and resource
// restrictions before deleting.
func (t *ResourceTracker) IsManagedBy(obj *unstructured.Unstructured, app App, method Method, installationID string) bool {
	if obj == nil || hasOwnerReferences(obj) {
		return false
	}
	name := t.GetAppName(obj, method, installationID)
	return name != "" && name == t.InstanceName(app) && t.IsSelfReferencedObject(obj, nil, method, installationID)
}

// DetectSharedResource returns the instance name of the other Application that live is
// tracked by, and true, when live is tracked by an Application other than app (upstream
// SharedResourceWarning detection; self-reference is not considered, as upstream). It
// returns "", false for untracked objects and objects tracked by app.
func (t *ResourceTracker) DetectSharedResource(live *unstructured.Unstructured, app App, method Method, installationID string) (otherInstanceName string, shared bool) {
	name := t.GetAppName(live, method, installationID)
	if name == "" || name == t.InstanceName(app) {
		return "", false
	}
	return name, true
}

// SharedResourceMessage returns the SharedResourceWarning condition message for live,
// selected for app but tracked by otherInstanceName:
// "<Kind>/<name> is part of applications <namespace>/<app> and <other>", where other is the
// instance name with '_' replaced by '/' (upstream wording).
func SharedResourceMessage(live *unstructured.Unstructured, app App, otherInstanceName string) string {
	var kind, name string
	if live != nil {
		kind, name = live.GetKind(), live.GetName()
	}
	return fmt.Sprintf("%s/%s is part of applications %s and %s", kind, name, app.QualifiedName(), DisplayName(otherInstanceName))
}
