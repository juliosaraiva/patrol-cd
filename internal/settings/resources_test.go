package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
)

// The settings filter plugs into the cluster cache.
var _ cache.ResourceFilter = (*ResourcesFilter)(nil)

func TestFilteredResourceMatch(t *testing.T) {
	t.Parallel()
	const g, k, c = "foo.com", "bar", "baz.com"
	tests := []struct {
		name string
		r    FilteredResource
		want bool
	}{
		{"missing groups", FilteredResource{Kinds: []string{k}, Clusters: []string{c}}, true},
		{"missing kinds", FilteredResource{APIGroups: []string{g}, Clusters: []string{c}}, true},
		{"missing clusters, kind *", FilteredResource{APIGroups: []string{g}, Kinds: []string{"*"}}, true},
		{"exact", FilteredResource{APIGroups: []string{g}, Kinds: []string{k}, Clusters: []string{c}}, true},
		{"group glob", FilteredResource{APIGroups: []string{"*.com"}, Kinds: []string{k}, Clusters: []string{c}}, true},
		{"cluster glob", FilteredResource{APIGroups: []string{g}, Kinds: []string{k}, Clusters: []string{"*.com"}}, true},
		{"kinds are not globbed", FilteredResource{APIGroups: []string{g}, Kinds: []string{"b*"}}, false},
		{"empty group pattern", FilteredResource{APIGroups: []string{""}, Kinds: []string{k}, Clusters: []string{c}}, false},
		{"empty kind", FilteredResource{APIGroups: []string{g}, Kinds: []string{""}, Clusters: []string{c}}, false},
		{"empty cluster pattern", FilteredResource{APIGroups: []string{g}, Kinds: []string{k}, Clusters: []string{""}}, false},
		{"duplicates", FilteredResource{APIGroups: []string{g, g}, Kinds: []string{k, k}, Clusters: []string{c, c}}, true},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, tc.r.Match(g, k, c), "literal: %s", tc.name)
		compiled := NewFilteredResource(tc.r.APIGroups, tc.r.Kinds, tc.r.Clusters)
		assert.Equal(t, tc.want, compiled.Match(g, k, c), "compiled: %s", tc.name)
	}
	// Invalid globs never match.
	assert.False(t, NewFilteredResource([]string{"["}, []string{""}, []string{""}).Match("", "", ""))
	assert.False(t, NewFilteredResource([]string{""}, []string{""}, []string{"["}).Match("", "", ""))
	assert.False(t, FilteredResource{APIGroups: []string{"["}}.Match("[", "", ""))
}

func TestResourcesFilterTruthTable(t *testing.T) {
	t.Parallel()
	in := NewFilteredResource([]string{"incl.io"}, nil, []string{"inc-cluster"})
	ex := NewFilteredResource([]string{"excl.io"}, nil, []string{"inc-cluster"})
	both := NewFilteredResource([]string{"both.io"}, nil, nil)
	tests := []struct {
		name                string
		inclusions, exclude []FilteredResource
		group, cluster      string
		want                bool
	}{
		{"no inclusions, no exclusions → allowed", nil, nil, "x.io", "c", false},
		{"inclusion present → allowed", []FilteredResource{in}, nil, "incl.io", "inc-cluster", false},
		{"inclusion not present → not allowed", []FilteredResource{in}, nil, "other.io", "inc-cluster", true},
		{"exclusion present → not allowed", nil, []FilteredResource{ex}, "excl.io", "inc-cluster", true},
		{"exclusion not present → allowed", nil, []FilteredResource{ex}, "other.io", "inc-cluster", false},
		{"inclusion present, exclusion not present → allowed", []FilteredResource{in}, []FilteredResource{ex}, "incl.io", "inc-cluster", false},
		{"inclusion not present, exclusion present → not allowed", []FilteredResource{in}, []FilteredResource{ex}, "excl.io", "inc-cluster", true},
		{"neither present, inclusions apply → not allowed", []FilteredResource{in}, []FilteredResource{ex}, "other.io", "inc-cluster", true},
		{"both present → not allowed", []FilteredResource{both}, []FilteredResource{both}, "both.io", "any", true},
		{"inclusions for another cluster → allowed", []FilteredResource{in}, nil, "other.io", "other-cluster", false},
	}
	for _, tc := range tests {
		rf := &ResourcesFilter{ResourceInclusions: tc.inclusions, ResourceExclusions: tc.exclude}
		assert.Equal(t, tc.want, rf.IsExcluded(tc.group, "Kind", tc.cluster), tc.name)
	}
}

func TestResourcesFilterUpstreamCases(t *testing.T) {
	t.Parallel()
	f := &ResourcesFilter{
		ResourceInclusions: []FilteredResource{{APIGroups: []string{"whitelisted-resource"}}},
		ResourceExclusions: []FilteredResource{{APIGroups: []string{"whitelisted-resource"}, Kinds: []string{"blacklisted-kind"}}},
	}
	assert.True(t, f.IsExcludedResource("whitelisted-resource", "blacklisted-kind", ""))
	assert.False(t, f.IsExcludedResource("whitelisted-resource", "", ""))
	assert.False(t, f.IsExcludedResource("whitelisted-resource", "non-blacklisted-kind", ""))

	multi := &ResourcesFilter{
		ResourceInclusions: []FilteredResource{{APIGroups: []string{"w"}, Clusters: []string{"cluster-one"}}},
		ResourceExclusions: []FilteredResource{{APIGroups: []string{"w"}, Clusters: []string{"cluster-two"}}},
	}
	assert.False(t, multi.IsExcluded("w", "", "cluster-one"))
	assert.True(t, multi.IsExcluded("w", "", "cluster-two"))
	assert.False(t, multi.IsExcluded("w", "", "cluster-three"))
}

func TestCoreExclusionsCannotBeIncluded(t *testing.T) {
	t.Parallel()
	var nilFilter *ResourcesFilter
	for _, gk := range [][2]string{{"events.k8s.io", "Event"}, {"metrics.k8s.io", "PodMetrics"}, {"", "Event"}, {"coordination.k8s.io", "Lease"}} {
		assert.True(t, nilFilter.IsExcluded(gk[0], gk[1], "c"), gk)
		incl := &ResourcesFilter{ResourceInclusions: []FilteredResource{NewFilteredResource([]string{gk[0]}, []string{gk[1]}, nil)}}
		assert.True(t, incl.IsExcluded(gk[0], gk[1], "c"), "DIFF-069: %v cannot be re-included", gk)
	}
	assert.False(t, nilFilter.IsExcluded("rubbish.io", "", ""))
	assert.False(t, nilFilter.IsExcluded("coordination.k8s.io", "Other", ""))
}

func TestDefaultResourceExclusionsMatchUpstream(t *testing.T) {
	t.Parallel()
	got := DefaultResourceExclusions()
	want := []struct{ groups, kinds []string }{
		{[]string{"", "discovery.k8s.io"}, []string{"Endpoints", "EndpointSlice"}},
		{[]string{"coordination.k8s.io"}, []string{"Lease"}},
		{[]string{"authentication.k8s.io", "authorization.k8s.io"}, []string{"SelfSubjectReview", "TokenReview", "LocalSubjectAccessReview", "SelfSubjectAccessReview", "SelfSubjectRulesReview", "SubjectAccessReview"}},
		{[]string{"certificates.k8s.io"}, []string{"CertificateSigningRequest"}},
		{[]string{"cert-manager.io"}, []string{"CertificateRequest"}},
		{[]string{"cilium.io"}, []string{"CiliumIdentity", "CiliumEndpoint", "CiliumEndpointSlice"}},
		{[]string{"kyverno.io", "reports.kyverno.io", "wgpolicyk8s.io"}, []string{"PolicyReport", "ClusterPolicyReport", "EphemeralReport", "ClusterEphemeralReport", "AdmissionReport", "ClusterAdmissionReport", "BackgroundScanReport", "ClusterBackgroundScanReport", "UpdateRequest"}},
	}
	require.Len(t, got, len(want))
	for i, w := range want {
		assert.Equal(t, w.groups, got[i].APIGroups, i)
		assert.Equal(t, w.kinds, got[i].Kinds, i)
		assert.Empty(t, got[i].Clusters, i)
	}
	// The shipped value parses through patrol-cm without issues.
	s := Parse(in(map[string]string{KeyResourceExclusions: DefaultResourceExclusionsYAML}))
	requireNoIssues(t, s, DefaultConfigMapName)
	assert.True(t, s.IsExcludedResource("discovery.k8s.io", "EndpointSlice", "https://kubernetes.default.svc"))
	assert.False(t, s.IsExcludedResource("apps", "Deployment", "https://kubernetes.default.svc"))
}

func TestDefaultIgnoreResourceUpdatesParse(t *testing.T) {
	t.Parallel()
	ov, err := lua.ParseResourceOverrides(DefaultIgnoreResourceUpdates())
	require.NoError(t, err)
	assert.Contains(t, ov, "*/*")
	assert.Contains(t, ov, "cd.patrol.example/Application")
	assert.Contains(t, ov, "argoproj.io/Rollout")
	assert.Contains(t, ov, "Endpoints")
	s := Parse(in(DefaultIgnoreResourceUpdates()))
	requireNoIssues(t, s, DefaultConfigMapName)
}
