package project

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func boolPtr(b bool) *bool { return &b }

// TEN-015 / TEN-016 / APP-041.
func TestOrphanedResources(t *testing.T) {
	t.Parallel()
	none := &v1alpha1.AppProject{}
	empty := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{OrphanedResources: &v1alpha1.OrphanedResourcesMonitorSettings{}}}
	warn := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{OrphanedResources: &v1alpha1.OrphanedResourcesMonitorSettings{Warn: boolPtr(true)}}}
	assert.False(t, OrphanedResourcesEnabled(none))
	assert.False(t, OrphanedResourcesWarn(none))
	assert.True(t, OrphanedResourcesEnabled(empty))
	assert.False(t, OrphanedResourcesWarn(empty))
	assert.True(t, OrphanedResourcesWarn(warn))
	assert.Equal(t, "Application has 1 orphaned resources", OrphanedResourceWarningMessage(1))

	ignore := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{
		OrphanedResources: &v1alpha1.OrphanedResourcesMonitorSettings{Ignore: []v1alpha1.OrphanedResourceKey{
			{Kind: "Secret", Name: "*.example.com"},
			{Kind: "Deployment", Name: "legacy-*"},
			{Group: "*", Kind: "StatefulSet"},
		}},
		NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "", Kind: "ResourceQuota"}},
	}}
	tests := []struct {
		key      v1alpha1.ResourceKey
		excluded bool
	}{
		{v1alpha1.ResourceKey{Kind: "Service", Namespace: "default", Name: "kubernetes"}, true},
		{v1alpha1.ResourceKey{Kind: "Service", Namespace: "other", Name: "kubernetes"}, false},
		{v1alpha1.ResourceKey{Kind: "ServiceAccount", Namespace: "x", Name: "default"}, true},
		{v1alpha1.ResourceKey{Kind: "ConfigMap", Namespace: "x", Name: "kube-root-ca.crt"}, true},
		{v1alpha1.ResourceKey{Kind: "Secret", Namespace: "x", Name: "tls.example.com"}, true},
		{v1alpha1.ResourceKey{Kind: "Secret", Namespace: "x", Name: "token"}, false},
		// Group omitted in the ignore entry only matches the core group.
		{v1alpha1.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "x", Name: "legacy-a"}, false},
		{v1alpha1.ResourceKey{Group: "apps", Kind: "StatefulSet", Namespace: "x", Name: "db"}, true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.excluded, IsKnownOrphanedResourceExclusion(ignore, tt.key), "%+v", tt.key)
	}
	// Empty kind matches every kind.
	anyKind := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{OrphanedResources: &v1alpha1.OrphanedResourcesMonitorSettings{Ignore: []v1alpha1.OrphanedResourceKey{{Name: "keep-*"}}}}}
	assert.True(t, IsKnownOrphanedResourceExclusion(anyKind, v1alpha1.ResourceKey{Kind: "ConfigMap", Namespace: "x", Name: "keep-me"}))
	assert.False(t, IsKnownOrphanedResourceExclusion(anyKind, v1alpha1.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "x", Name: "keep-me"}))

	assert.True(t, IsOrphanCandidate(ignore, v1alpha1.ResourceKey{Kind: "ConfigMap", Namespace: "x", Name: "stray"}))
	assert.False(t, IsOrphanCandidate(ignore, v1alpha1.ResourceKey{Kind: "ResourceQuota", Namespace: "x", Name: "q"}), "denied kind")
	assert.False(t, IsOrphanCandidate(ignore, v1alpha1.ResourceKey{Kind: "Namespace", Name: "x"}), "cluster-scoped")
	assert.False(t, IsOrphanCandidate(ignore, v1alpha1.ResourceKey{Kind: "ServiceAccount", Namespace: "x", Name: "default"}), "built-in exclusion")
}

func gitPolicy(mode v1alpha1.SourceIntegrityGitPolicyGPGMode, urls ...string) v1alpha1.SourceIntegrityGitPolicy {
	var repos []v1alpha1.SourceIntegrityGitPolicyRepo
	for _, u := range urls {
		repos = append(repos, v1alpha1.SourceIntegrityGitPolicyRepo{URL: u})
	}
	return v1alpha1.SourceIntegrityGitPolicy{Repos: repos, GPG: &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: mode, Keys: []string{"KEY"}}}
}

// Ports the semantics of upstream TestAppProject_EffectiveSourceIntegrity (TEN-026, REPO-072).
func TestEffectiveSourceIntegrity(t *testing.T) {
	t.Parallel()
	si := func(repo string, mode v1alpha1.SourceIntegrityGitPolicyGPGMode, keys ...string) *v1alpha1.SourceIntegrity {
		return &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
			Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: repo}},
			GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: mode, Keys: keys},
		}}}}
	}
	keys := []v1alpha1.SignatureKey{{KeyID: "LEGACY"}, {KeyID: "KEYS"}, {KeyID: "FOUND"}}
	tests := []struct {
		name      string
		spec      v1alpha1.AppProjectSpec
		want      *v1alpha1.SourceIntegrity
		migration SignatureKeysMigration
	}{
		{"no old, no new", v1alpha1.AppProjectSpec{}, nil, SignatureKeysNone},
		{"no old, new unchanged", v1alpha1.AppProjectSpec{SourceIntegrity: si("https://github.com/*", "strict", "FOO")}, si("https://github.com/*", "strict", "FOO"), SignatureKeysNone},
		{"old, no new", v1alpha1.AppProjectSpec{SignatureKeys: keys}, si("*", "head", "LEGACY", "KEYS", "FOUND"), SignatureKeysCreated},                                                                          //nolint:staticcheck // deprecated field exercised for migration
		{"old ignored", v1alpha1.AppProjectSpec{SignatureKeys: keys, SourceIntegrity: si("https://github.com/*", "strict", "NEW_KEY")}, si("https://github.com/*", "strict", "NEW_KEY"), SignatureKeysIgnored},   //nolint:staticcheck // deprecated field exercised for migration
		{"old merged", v1alpha1.AppProjectSpec{SignatureKeys: []v1alpha1.SignatureKey{{KeyID: "LEGACY_KEY"}}, SourceIntegrity: &v1alpha1.SourceIntegrity{}}, si("*", "head", "LEGACY_KEY"), SignatureKeysMerged}, //nolint:staticcheck // deprecated field exercised for migration
	}
	for _, tt := range tests {
		p := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "sut"}, Spec: tt.spec}
		got, migration := EffectiveSourceIntegrity(p)
		assert.Equal(t, tt.want, got, tt.name)
		assert.Equal(t, tt.migration, migration, tt.name)
	}
	merged := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SignatureKeys: keys, SourceIntegrity: &v1alpha1.SourceIntegrity{}}} //nolint:staticcheck // deprecated field exercised for migration
	_, _ = EffectiveSourceIntegrity(merged)
	assert.Nil(t, merged.Spec.SourceIntegrity.Git, "project not modified")
	assert.Equal(t, []string{"LEGACY", "KEYS", "FOUND"}, RequiredSignatureKeys(merged))
}

// REPO-068: policy selection with negated URLs; zero/one/multiple matches; non-git sources.
func TestGitVerificationFor(t *testing.T) {
	t.Parallel()
	si := &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{
		gitPolicy("head", "https://github.com/org/*", "!https://github.com/org/unsigned"),
		gitPolicy("strict", "https://gitlab.com/**"),
		gitPolicy("none", "https://example.com/none"),
		{Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "https://example.com/nogpg"}}},
		gitPolicy("head", "https://dup.example.com/*"),
		gitPolicy("strict", "https://dup.example.com/x"),
		gitPolicy("head"), // no repos: never applies
	}}}
	src := func(url string) v1alpha1.ApplicationSource {
		return v1alpha1.ApplicationSource{RepoURL: url, Path: "."}
	}
	tests := []struct {
		source v1alpha1.ApplicationSource
		mode   v1alpha1.SourceIntegrityGitPolicyGPGMode
		err    string
	}{
		{src("https://github.com/org/app"), "head", ""},
		{src("https://github.com/org/unsigned"), "", ""},
		{src("https://gitlab.com/a/b/c"), "strict", ""},
		{src("https://example.com/none"), "", ""},
		{src("https://example.com/nogpg"), "", ""},
		{src("https://unmatched.example.com/x"), "", ""},
		{src("https://dup.example.com/x"), "", "multiple (2) git source integrity policies found for repo URL: https://dup.example.com/x"},
		{v1alpha1.ApplicationSource{RepoURL: "https://github.com/org/app", Chart: "c", TargetRevision: "1.0.0"}, "", ""},
		{v1alpha1.ApplicationSource{RepoURL: "oci://github.com/org/app"}, "", ""},
		{v1alpha1.ApplicationSource{}, "", ""},
	}
	for _, tt := range tests {
		v, err := GitVerificationFor(si, tt.source, true)
		if tt.err != "" {
			require.EqualError(t, err, tt.err)
			assert.True(t, HasSourceIntegrityCriteria(si, true, tt.source), "ambiguous policies are criteria")
			continue
		}
		require.NoError(t, err, tt.source.RepoURL)
		if tt.mode == "" {
			assert.Nil(t, v, tt.source.RepoURL)
			assert.False(t, HasSourceIntegrityCriteria(si, true, tt.source), tt.source.RepoURL)
			continue
		}
		require.NotNil(t, v, tt.source.RepoURL)
		assert.Equal(t, tt.mode, v.Mode)
		assert.Equal(t, []string{"KEY"}, v.Keys)
		assert.True(t, HasSourceIntegrityCriteria(si, true, tt.source))
	}
	// GPG disabled: no verification.
	v, err := GitVerificationFor(si, src("https://github.com/org/app"), false)
	require.NoError(t, err)
	assert.Nil(t, v)
	// Multi-source: each source independently.
	assert.True(t, HasSourceIntegrityCriteria(si, true, src("https://unmatched.example.com/x"), src("https://gitlab.com/a")))
	assert.False(t, HasSourceIntegrityCriteria(nil, true, src("https://gitlab.com/a")))
	assert.EqualError(t, UnknownGPGModeError("bogus"), `unknown GPG mode "bogus" configured for GIT source integrity`)
}
