package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestParseResourceSelectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want resourceSelector
		err  string
	}{
		{in: ":Service:web", want: resourceSelector{Kind: "Service", Name: "web"}},
		{in: "apps:Deployment:web", want: resourceSelector{Group: "apps", Kind: "Deployment", Name: "web"}},
		{in: "argoproj.io:Rollout:team/web", want: resourceSelector{Group: "argoproj.io", Kind: "Rollout", Namespace: "team", Name: "web"}},
		{in: "!apps:Deployment:web", want: resourceSelector{Group: "apps", Kind: "Deployment", Name: "web", Exclude: true}},
		{in: "!*:Service:*", want: resourceSelector{Group: "*", Kind: "Service", Name: "*", Exclude: true}},
		{in: "Deployment:web", err: "resource should have GROUP:KIND:NAME, but instead got: Deployment:web"},
		{in: "a:b:c:d", err: "resource should have GROUP:KIND:NAME"},
		{in: "apps:Deployment:a/b/c", err: "resource with namespace should have GROUP:KIND:NAMESPACE/NAME, but instead got: apps:Deployment:a/b/c"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseResourceSelectors([]string{tc.in})
			if tc.err != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.err)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, tc.want, got[0])
			assert.Equal(t, tc.in, got[0].String())
		})
	}
}

func TestIncludeResource(t *testing.T) {
	t.Parallel()
	parse := func(s ...string) []resourceSelector {
		out, err := parseResourceSelectors(s)
		require.NoError(t, err)
		return out
	}
	type res struct{ group, kind, ns, name string }
	deploy := res{"apps", "Deployment", "default", "web"}
	svc := res{"", "Service", "default", "web"}
	svcOther := res{"", "Service", "other", "api"}
	tests := []struct {
		name      string
		selectors []resourceSelector
		res       res
		want      bool
	}{
		{"no selectors include everything", nil, deploy, true},
		{"include match", parse("apps:Deployment:web"), deploy, true},
		{"include miss", parse("apps:Deployment:web"), svc, false},
		{"wildcards", parse("*:*:web"), svc, true},
		{"empty namespace matches any", parse(":Service:api"), svcOther, true},
		{"namespace must match", parse(":Service:default/api"), svcOther, false},
		{"only exclusions include the rest", parse("!:Service:*"), deploy, true},
		{"exclusion wins", parse("!:Service:*"), svc, false},
		{"exclusion wins over inclusion", parse("*:*:web", "!apps:Deployment:web"), deploy, false},
		{"inclusion with other exclusion", parse("*:*:web", "!apps:Deployment:web"), svc, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, includeResource(tc.selectors, tc.res.group, tc.res.kind, tc.res.ns, tc.res.name))
		})
	}
}

func TestSelectAppResources(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{Status: v1alpha1.ApplicationStatus{Resources: []v1alpha1.ResourceStatus{
		{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "web"},
		{Kind: "Service", Namespace: "default", Name: "web"},
		{Kind: "ConfigMap", Namespace: "default", Name: "cfg"},
	}}}
	sel, err := parseResourceSelectors([]string{"!:Service:*"})
	require.NoError(t, err)
	got := selectAppResources(app, sel)
	assert.Equal(t, []v1alpha1.SyncOperationResource{
		{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "web"},
		{Kind: "ConfigMap", Namespace: "default", Name: "cfg"},
	}, got)
	assert.Nil(t, selectAppResources(app, nil), "no selectors: the whole application is synced")
	none, err := parseResourceSelectors([]string{"x:Y:z"})
	require.NoError(t, err)
	assert.Empty(t, selectAppResources(app, none))
}
