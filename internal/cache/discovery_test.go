package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type excludeFilter map[schema.GroupKind]bool

func (f excludeFilter) IsExcluded(group, kind, _ string) bool {
	return f[schema.GroupKind{Group: group, Kind: kind}]
}

func TestBuildDiscovery(t *testing.T) {
	t.Parallel()
	lw := []string{"get", "list", "watch"}
	groups := []*metav1.APIGroup{
		{Name: "", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "v1", Version: "v1"}},
		{Name: "autoscaling", Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "autoscaling/v1", Version: "v1"}, {GroupVersion: "autoscaling/v2", Version: "v2"},
		}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "autoscaling/v2", Version: "v2"}},
		{Name: "coordination.k8s.io", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "coordination.k8s.io/v1", Version: "v1"}},
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "coordination.k8s.io/v1", Version: "v1"}},
		{Name: "events.k8s.io", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "events.k8s.io/v1", Version: "v1"}},
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "events.k8s.io/v1", Version: "v1"}},
		{Name: "example.com", Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "example.com/v2", Version: "v2"}, {GroupVersion: "example.com/v1", Version: "v1"},
		}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "example.com/v2", Version: "v2"}},
	}
	lists := []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: lw},
			{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: lw},
			{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
			{Name: "namespaces", Kind: "Namespace", Verbs: lw},
			{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: []string{"create"}},
			{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: lw},
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "coordination.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "leases", Kind: "Lease", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "autoscaling/v1", APIResources: []metav1.APIResource{
			{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "autoscaling/v2", APIResources: []metav1.APIResource{
			{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "example.com/v2", APIResources: []metav1.APIResource{
			{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: lw},
			{Name: "gadgets", Kind: "Gadget", Verbs: lw}, // only served in a non-preferred version
		}},
	}
	s := Settings{ResourceFilter: excludeFilter{{Kind: "Secret"}: true}}
	d := buildDiscovery(groups, lists, &s, "https://cluster")

	var watched []string
	for _, a := range d.watch {
		watched = append(watched, a.GroupVersionResource.String())
	}
	assert.Equal(t, []string{
		"/v1, Resource=configmaps",
		"/v1, Resource=namespaces",
		"/v1, Resource=pods",
		"autoscaling/v2, Resource=horizontalpodautoscalers",
		"example.com/v1, Resource=gadgets",
		"example.com/v2, Resource=widgets",
	}, watched, "preferred versions, list+watch only, exclusions (configured and always-on) applied, subresources skipped")

	assert.Len(t, d.all, 12, "GetAPIResources lists every served version with list+watch, including excluded kinds")
	assert.True(t, d.served(schema.GroupKind{Group: "events.k8s.io"}), "group-level query")
	assert.False(t, d.served(schema.GroupKind{Group: "absent.example.com"}))
	assert.True(t, d.namespaced[schema.GroupKind{Kind: "Secret"}], "excluded kinds still have known scope")
	assert.False(t, d.namespaced[schema.GroupKind{Group: "example.com", Kind: "Gadget"}])
	assert.True(t, d.served(schema.GroupKind{Kind: "Binding"}))
	assert.False(t, d.served(schema.GroupKind{Group: "nope", Kind: "X"}))
	_, ok := d.byGVK[schema.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "HorizontalPodAutoscaler"}]
	assert.True(t, ok, "non-preferred versions are resolvable for GET")
	assert.Equal(t, "autoscaling", d.byGVK[schema.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "HorizontalPodAutoscaler"}].Meta.Group)
}

func TestNormalizeServerVersion(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"v1.36.0":          "v1.36.0",
		"v1.30.2-eks-1234": "v1.30.2",
		"v1.29.4+k3s1":     "v1.29.4",
		"v1.28.3-gke.100":  "v1.28.3",
		"not-a-version":    "not-a-version",
	} {
		assert.Equal(t, want, normalizeServerVersion(in), in)
	}
}
