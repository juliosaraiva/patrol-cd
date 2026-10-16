package tracking

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const (
	testControlPlane = "patrol-cd"
	keyTracking      = v1alpha1.AnnotationKeyTrackingID
	keyInstallation  = v1alpha1.AnnotationKeyInstallationID
	keyInstance      = v1alpha1.LabelKeyAppInstanceDefault
)

var allMethods = []Method{"", MethodAnnotation, MethodLabel, MethodAnnotationAndLabel}

// newObj returns a minimal object; an empty namespace leaves metadata.namespace unset.
func newObj(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	md := map[string]any{"name": name}
	if namespace != "" {
		md["namespace"] = namespace
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   md,
	}}
}

func configMap(namespace, name string) *unstructured.Unstructured {
	return newObj("v1", "ConfigMap", namespace, name)
}

func withAnnotations(obj *unstructured.Unstructured, kv ...string) *unstructured.Unstructured {
	m := obj.GetAnnotations()
	if m == nil {
		m = map[string]string{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	obj.SetAnnotations(m)
	return obj
}

func withLabels(obj *unstructured.Unstructured, kv ...string) *unstructured.Unstructured {
	m := obj.GetLabels()
	if m == nil {
		m = map[string]string{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	obj.SetLabels(m)
	return obj
}

func newTestTracker() *ResourceTracker {
	return New(Config{ControlPlaneNamespace: testControlPlane})
}
