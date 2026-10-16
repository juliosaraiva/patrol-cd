package projects

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

func TestRequiredGrants(t *testing.T) {
	t.Parallel()
	resolve := func(_ context.Context, name string) ([]string, error) {
		return map[string][]string{
			"dev":  {"https://dev.example.com"},
			"dup":  {"https://a.example.com", "https://b.example.com"},
			"boom": nil,
		}[name], nil
	}
	dst := func(server, name, ns string) v1alpha1.ApplicationDestination {
		return v1alpha1.ApplicationDestination{Server: server, Name: name, Namespace: ns}
	}
	cl := func(objs ...string) []grant {
		out := make([]grant, 0, len(objs))
		for _, o := range objs {
			out = append(out, grant{resource: rbac.ResourceClusters, object: o})
		}
		return out
	}
	base := v1alpha1.AppProjectSpec{
		SourceRepos:  []string{"https://github.com/org/app"},
		Destinations: []v1alpha1.ApplicationDestination{dst("https://kube.example.com", "", "apps"), dst("", "dev", "apps")},
	}
	for _, tc := range []struct {
		name   string
		mutate func(s *v1alpha1.AppProjectSpec)
		want   []grant
	}{
		{"unchanged", func(*v1alpha1.AppProjectSpec) {}, nil},
		{"reordered destinations", func(s *v1alpha1.AppProjectSpec) {
			s.Destinations[0], s.Destinations[1] = s.Destinations[1], s.Destinations[0]
		}, nil},
		{"namespace widened on a server destination", func(s *v1alpha1.AppProjectSpec) { s.Destinations[0].Namespace = "*" }, cl("https://kube.example.com")},
		{"namespace widened on a name destination", func(s *v1alpha1.AppProjectSpec) { s.Destinations[1].Namespace = "*" }, cl("https://dev.example.com")},
		{"name glob added", func(s *v1alpha1.AppProjectSpec) { s.Destinations = append(s.Destinations, dst("", "*", "*")) }, cl("*")},
		{"negated name added", func(s *v1alpha1.AppProjectSpec) { s.Destinations = append(s.Destinations, dst("", "!dev", "*")) }, cl("*")},
		{"unregistered name added", func(s *v1alpha1.AppProjectSpec) { s.Destinations = append(s.Destinations, dst("", "boom", "apps")) }, cl("*")},
		{"name shared by two clusters", func(s *v1alpha1.AppProjectSpec) { s.Destinations = append(s.Destinations, dst("", "dup", "apps")) },
			cl("https://a.example.com", "https://b.example.com")},
		{"server glob added: the pattern is the object (upstream)", func(s *v1alpha1.AppProjectSpec) {
			s.Destinations = append(s.Destinations, dst("https://*.example.com", "", "*"))
		}, cl("https://*.example.com")},
		{"server and name entry", func(s *v1alpha1.AppProjectSpec) {
			s.Destinations = append(s.Destinations, dst("https://x.example.com", "dev", "apps"))
		}, cl("https://dev.example.com", "https://x.example.com")},
		{"entry without server and name", func(s *v1alpha1.AppProjectSpec) { s.Destinations = append(s.Destinations, dst("", "", "apps")) }, cl("")},
		{"destination removed", func(s *v1alpha1.AppProjectSpec) { s.Destinations = s.Destinations[1:] }, cl("https://kube.example.com")},
		{"source added and removed", func(s *v1alpha1.AppProjectSpec) { s.SourceRepos = []string{"https://github.com/evil/*"} },
			[]grant{{rbac.ResourceRepositories, "https://github.com/evil/*"}, {rbac.ResourceRepositories, "https://github.com/org/app"}}},
		{"resource list changed: every destination of the result", func(s *v1alpha1.AppProjectSpec) {
			s.ClusterResourceWhitelist = []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "*"}}
		}, cl("https://dev.example.com", "https://kube.example.com")},
		{"service account added", func(s *v1alpha1.AppProjectSpec) {
			s.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: "https://kube.example.com", Namespace: "apps", DefaultServiceAccount: "admin"}}
		}, cl("https://kube.example.com")},
		{"destination and list change: deduplicated, categories in order", func(s *v1alpha1.AppProjectSpec) {
			s.Destinations[0].Namespace = "*"
			s.ClusterResourceBlacklist = []v1alpha1.ClusterResourceRestrictionItem{{Group: "", Kind: "Namespace"}}
		}, cl("https://kube.example.com", "https://dev.example.com")},
	} {
		next := base.DeepCopy()
		tc.mutate(next)
		got, err := requiredGrants(context.Background(), &base, next, resolve)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}

	// Service account entries are compared whole: changing the impersonated account counts.
	old := &v1alpha1.AppProjectSpec{DestinationServiceAccounts: []v1alpha1.ApplicationDestinationServiceAccount{{Server: "https://kube.example.com", Namespace: "apps", DefaultServiceAccount: "deployer"}}}
	next := old.DeepCopy()
	next.DestinationServiceAccounts[0].DefaultServiceAccount = "kube-system:admin"
	got, err := requiredGrants(context.Background(), old, next, resolve)
	require.NoError(t, err)
	assert.Equal(t, cl("https://kube.example.com"), got)

	// Removing a deny list widens the project like adding to an allow list.
	withDeny := base.DeepCopy()
	withDeny.NamespaceResourceBlacklist = []metav1.GroupKind{{Group: "", Kind: "Secret"}}
	got, err = requiredGrants(context.Background(), withDeny, &base, resolve)
	require.NoError(t, err)
	assert.Equal(t, cl("https://dev.example.com", "https://kube.example.com"), got)

	// Lookup failures are returned.
	boom := errors.New("lookup failed")
	_, err = requiredGrants(context.Background(), &v1alpha1.AppProjectSpec{}, &base, func(context.Context, string) ([]string, error) { return nil, boom })
	require.ErrorIs(t, err, boom)
}

func TestIsLiteralName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{"dev": true, "in-cluster": true, "*": false, "dev-*": false, "d?v": false, "[a]": false, "{a,b}": false, "!dev": false} {
		assert.Equal(t, want, isLiteralName(name), name)
	}
}
