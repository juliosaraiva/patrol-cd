package appset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
)

func TestParsePolicy(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]v1alpha1.ApplicationsSyncPolicy{
		"":              v1alpha1.ApplicationsSyncPolicySync,
		"sync":          v1alpha1.ApplicationsSyncPolicySync,
		"create-only":   v1alpha1.ApplicationsSyncPolicyCreateOnly,
		"create-update": v1alpha1.ApplicationsSyncPolicyCreateUpdate,
		"create-delete": v1alpha1.ApplicationsSyncPolicyCreateDelete,
	} {
		got, err := ParsePolicy(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := ParsePolicy("update-only")
	require.ErrorContains(t, err, "Policy value can be: sync, create-only, create-update, create-delete, default value: sync")
}

func TestEffectivePolicy(t *testing.T) {
	t.Parallel()
	withPolicy := func(p v1alpha1.ApplicationsSyncPolicy) *v1alpha1.ApplicationSet {
		return &v1alpha1.ApplicationSet{Spec: v1alpha1.ApplicationSetSpec{SyncPolicy: &v1alpha1.ApplicationSetSyncPolicy{ApplicationsSync: ptr.To(p)}}}
	}
	tests := []struct {
		name       string
		appSet     *v1alpha1.ApplicationSet
		controller v1alpha1.ApplicationsSyncPolicy
		override   bool
		want       v1alpha1.ApplicationsSyncPolicy
	}{
		{"default", &v1alpha1.ApplicationSet{}, "", true, v1alpha1.ApplicationsSyncPolicySync},
		{"controller only", &v1alpha1.ApplicationSet{}, v1alpha1.ApplicationsSyncPolicyCreateOnly, true, v1alpha1.ApplicationsSyncPolicyCreateOnly},
		{"override enabled", withPolicy(v1alpha1.ApplicationsSyncPolicySync), v1alpha1.ApplicationsSyncPolicyCreateOnly, true, v1alpha1.ApplicationsSyncPolicySync},
		{"override disabled", withPolicy(v1alpha1.ApplicationsSyncPolicySync), v1alpha1.ApplicationsSyncPolicyCreateOnly, false, v1alpha1.ApplicationsSyncPolicyCreateOnly},
		{"appset create-delete", withPolicy(v1alpha1.ApplicationsSyncPolicyCreateDelete), "", true, v1alpha1.ApplicationsSyncPolicyCreateDelete},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, EffectivePolicy(tt.appSet, tt.controller, tt.override), tt.name)
	}
	assert.True(t, DefaultEnablePolicyOverride(""))
	assert.False(t, DefaultEnablePolicyOverride("create-only"))
}

func TestPolicyTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		policy         v1alpha1.ApplicationsSyncPolicy
		update, delete bool
	}{
		{v1alpha1.ApplicationsSyncPolicyCreateOnly, false, false},
		{v1alpha1.ApplicationsSyncPolicyCreateUpdate, true, false},
		{v1alpha1.ApplicationsSyncPolicyCreateDelete, false, true},
		{v1alpha1.ApplicationsSyncPolicySync, true, true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.update, tt.policy.AllowUpdate(), tt.policy)
		assert.Equal(t, tt.delete, tt.policy.AllowDelete(), tt.policy)
	}
}

func TestNamespaceMatcher(t *testing.T) {
	t.Parallel()
	m, err := NewNamespaceMatcher(controlNS, []string{"team-*", "exact", "/^ns-[0-9]+$/", " "})
	require.NoError(t, err)
	for ns, want := range map[string]bool{
		controlNS: true, "team-a": true, "team-b": true, "exact": true, "ns-12": true,
		"other": false, "ns-x": false, "exactly": false,
	} {
		assert.Equal(t, want, m.Allowed(ns), ns)
	}
	assert.True(t, m.MultiNamespace())
	_, literal := m.LiteralNamespaces()
	assert.False(t, literal, "globs need a cluster-wide cache")

	only, err := NewNamespaceMatcher(controlNS, nil)
	require.NoError(t, err)
	assert.False(t, only.MultiNamespace())
	assert.False(t, only.Allowed("team-a"))
	lits, literal := only.LiteralNamespaces()
	assert.True(t, literal)
	assert.Equal(t, []string{controlNS}, lits)

	all, err := NewNamespaceMatcher(controlNS, []string{"*"})
	require.NoError(t, err)
	assert.True(t, all.Allowed("anything"))

	_, err = NewNamespaceMatcher(controlNS, []string{"/(/"})
	require.Error(t, err)
	// RE2 has no look-around: the upstream documentation example cannot compile in Go (upstream
	// logs and never matches; Patrol rejects the configuration).
	_, err = NewNamespaceMatcher(controlNS, []string{"/^((?!not-allowed).)*$/"})
	require.Error(t, err)
}

func TestConfigComplete(t *testing.T) {
	t.Parallel()
	_, err := Config{}.complete()
	require.Error(t, err)

	_, err = Config{Namespace: controlNS, ApplicationSetNamespaces: []string{"team-*"}, Generators: generators.Config{SCM: generators.SCMConfig{Enabled: true}}}.complete()
	require.ErrorIs(t, err, ErrSCMProvidersRequireAllowList)

	c, err := Config{Namespace: controlNS, ApplicationSetNamespaces: []string{"team-*"},
		Generators:                   generators.Config{SCM: generators.SCMConfig{Enabled: true, AllowedProviders: []string{"https://git.example"}}},
		ConcurrentApplicationUpdates: 1000, MaxResourcesStatusCount: -1}.complete()
	require.NoError(t, err)
	assert.Equal(t, MaxConcurrentApplicationUpdates, c.ConcurrentApplicationUpdates)
	assert.Equal(t, DefaultConcurrentReconciliations, c.ConcurrentReconciliations)
	assert.Equal(t, 0, c.MaxResourcesStatusCount)
	assert.Equal(t, v1alpha1.ApplicationsSyncPolicySync, c.Policy)
	assert.Equal(t, controlNS, c.Generators.Namespace)

	_, err = Config{Namespace: controlNS, Policy: "bogus"}.complete()
	require.Error(t, err)
}
