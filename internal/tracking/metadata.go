package tracking

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// metadataStringMap returns a copy of metadata.<field> (labels or annotations). A missing or
// null field yields a nil map; a value that is not a map of strings is an error.
func metadataStringMap(obj *unstructured.Unstructured, field string) (map[string]string, error) {
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, "metadata", field)
	if err != nil {
		return nil, err
	}
	if !found || v == nil {
		return nil, nil
	}
	m, _, err := unstructured.NestedStringMap(obj.Object, "metadata", field)
	return m, err
}

// hasOwnerReferences reports whether obj has ownerReferences. It fails closed: unreadable
// metadata, or an ownerReferences value that is neither null nor an empty list, counts as
// owned, whereas Unstructured.GetOwnerReferences returns nil for malformed values.
func hasOwnerReferences(obj *unstructured.Unstructured) bool {
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, "metadata", "ownerReferences")
	if err != nil {
		return true
	}
	if !found || v == nil {
		return false
	}
	refs, ok := v.([]any)
	return !ok || len(refs) > 0
}

// describe identifies obj in error messages, as upstream: "<gvk> <namespace>/<name>".
func describe(obj *unstructured.Unstructured) string {
	return fmt.Sprintf("%s %s/%s", obj.GroupVersionKind().String(), obj.GetNamespace(), obj.GetName())
}

func getAnnotation(obj *unstructured.Unstructured, key string) (string, error) {
	m, err := metadataStringMap(obj, "annotations")
	if err != nil {
		return "", fmt.Errorf("failed to get annotations from target object %s: %w", describe(obj), err)
	}
	return m[key], nil
}

func setAnnotation(obj *unstructured.Unstructured, key, val string) error {
	m, err := metadataStringMap(obj, "annotations")
	if err != nil {
		return fmt.Errorf("failed to get annotations from target object %s: %w", describe(obj), err)
	}
	if m == nil {
		m = make(map[string]string, 1)
	}
	m[key] = val
	obj.SetAnnotations(m)
	return nil
}

func removeAnnotation(obj *unstructured.Unstructured, key string) error {
	m, err := metadataStringMap(obj, "annotations")
	if err != nil {
		return fmt.Errorf("failed to get annotations for %s: %w", describe(obj), err)
	}
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	if len(m) == 0 {
		obj.SetAnnotations(nil)
	} else {
		obj.SetAnnotations(m)
	}
	return nil
}

func getLabel(obj *unstructured.Unstructured, key string) (string, error) {
	m, err := metadataStringMap(obj, "labels")
	if err != nil {
		return "", fmt.Errorf("failed to get labels for %s: %w", describe(obj), err)
	}
	return m[key], nil
}

func setLabel(obj *unstructured.Unstructured, key, val string) error {
	m, err := metadataStringMap(obj, "labels")
	if err != nil {
		return fmt.Errorf("failed to get labels from target object %s: %w", describe(obj), err)
	}
	if m == nil {
		m = make(map[string]string, 1)
	}
	m[key] = val
	obj.SetLabels(m)
	return nil
}

func removeLabel(obj *unstructured.Unstructured, key string) error {
	m, err := metadataStringMap(obj, "labels")
	if err != nil {
		return fmt.Errorf("failed to get labels for %s: %w", describe(obj), err)
	}
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	if len(m) == 0 {
		obj.SetLabels(nil)
	} else {
		obj.SetLabels(m)
	}
	return nil
}

// IsCRD reports whether obj is a CustomResourceDefinition. CRDs never receive tracking
// metadata from the repo-server and are skipped by [ResourceTracker.Normalize].
func IsCRD(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	gvk := obj.GroupVersionKind()
	return gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition"
}
