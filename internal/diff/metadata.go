package diff

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TrackingOptions configures the tracking-metadata pre-normalization that Diff runs on
// copies of both objects (baseline: resource tracking Normalize, called before every diff).
// The zero value disables it, which is the behavior of label-only tracking.
type TrackingOptions struct {
	// LabelKey is the application instance label (application.instanceLabelKey).
	LabelKey string
	// AnnotationKey is the tracking-id annotation written by annotation-based tracking.
	AnnotationKey string
}

func (t TrackingOptions) enabled() bool { return t.LabelKey != "" && t.AnnotationKey != "" }

// NormalizeTracking reconciles tracking metadata on live (modified in place) so that a change
// of tracking method is not reported as drift:
//   - when live carries the instance label but no tracking annotation (it was last synced
//     with label tracking) and is not a CustomResourceDefinition, the desired tracking
//     annotation is copied from config onto live;
//   - when config has no instance label, the label is removed from live.
//
// A live tracking annotation that differs from the desired one is kept, so a resource that
// moved between applications still shows as a difference. Labels or annotations that are not
// string maps are reported as errors and leave live unchanged.
func NormalizeTracking(config, live *unstructured.Unstructured, t TrackingOptions) error {
	if config == nil || live == nil || !t.enabled() {
		return nil
	}
	liveLabels, err := stringMap(live, "labels")
	if err != nil {
		return err
	}
	if liveLabels[t.LabelKey] == "" {
		return nil
	}
	if gvk := live.GroupVersionKind(); gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition" {
		return nil // CRDs never get tracking annotations
	}
	liveAnnotations, err := stringMap(live, "annotations")
	if err != nil {
		return err
	}
	configLabels, err := stringMap(config, "labels")
	if err != nil {
		return err
	}
	if liveAnnotations[t.AnnotationKey] == "" {
		configAnnotations, err := stringMap(config, "annotations")
		if err != nil {
			return err
		}
		if liveAnnotations == nil {
			liveAnnotations = map[string]string{}
		}
		liveAnnotations[t.AnnotationKey] = configAnnotations[t.AnnotationKey]
		live.SetAnnotations(liveAnnotations)
	}
	if configLabels[t.LabelKey] == "" {
		delete(liveLabels, t.LabelKey)
		if len(liveLabels) == 0 {
			live.SetLabels(nil)
		} else {
			live.SetLabels(liveLabels)
		}
	}
	return nil
}

// stringMap returns a copy of metadata.<field> (labels or annotations). A missing or null
// field is a nil map; a value that is not a map of strings is an error.
func stringMap(obj *unstructured.Unstructured, field string) (map[string]string, error) {
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, "metadata", field)
	if err != nil || !found || v == nil {
		if err != nil {
			return nil, fmt.Errorf("metadata.%s of %s: %w", field, describe(obj), err)
		}
		return nil, nil
	}
	m, _, err := unstructured.NestedStringMap(obj.Object, "metadata", field)
	if err != nil {
		return nil, fmt.Errorf("metadata.%s of %s: %w", field, describe(obj), err)
	}
	return m, nil
}

// metadataNoiseFields are metadata fields populated by the API server for its own
// bookkeeping.
var metadataNoiseFields = []string{"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields", "selfLink"}

// StripMetadataNoise removes server bookkeeping from un in place: metadata.resourceVersion,
// uid, generation, creationTimestamp, managedFields and selfLink, the
// last-applied-configuration annotation and any further annotation keys given (for example
// the tracking annotation). An annotations map left empty is removed.
//
// It is meant for presenting and hashing objects. Diff does not apply it: like the baseline,
// the legacy strategy already ignores these fields unless a manifest sets them, and
// server-side diff strips managedFields, resourceVersion and the last-applied annotation
// itself.
func StripMetadataNoise(un *unstructured.Unstructured, annotations ...string) {
	if un == nil || un.Object == nil {
		return
	}
	md, ok := un.Object["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, f := range metadataNoiseFields {
		delete(md, f)
	}
	ann, ok := md["annotations"].(map[string]any)
	if !ok {
		return
	}
	delete(ann, AnnotationLastAppliedConfig)
	for _, k := range annotations {
		delete(ann, k)
	}
	if len(ann) == 0 {
		delete(md, "annotations")
	}
}
