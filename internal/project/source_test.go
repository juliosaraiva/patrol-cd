package project

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func projWithSources(sources ...string) *v1alpha1.AppProject {
	return &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{SourceRepos: sources}}
}

func TestNormalizeGitURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"https://github.com/argoproj/test.git", "https://github.com/argoproj/test"},
		{"https://GITHUB.com/Argoproj/Test.git", "https://github.com/argoproj/test"},
		{"  https://github.com/org/repo  ", "https://github.com/org/repo"},
		{"git@github.com:argoproj/test.git", "git@github.com/argoproj/test"},
		{"ssh://git@github.com/argoproj/test.git", "git@github.com/argoproj/test"},
		{"ssh://git@github.com:22/argoproj/test", "git@github.com:22/argoproj/test"},
		// net/url rejects the non-numeric port: upstream normalizes to "".
		{"ssh://git@github.com:argoproj/test", ""},
		{"*", "*"},
		{"https://github.com/argoproj/*", "https://github.com/argoproj/*"},
		{"https://gitlab.com/group/**", "https://gitlab.com/group/**"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, NormalizeGitURL(tt.in), tt.in)
	}
	assert.True(t, SameRepoURL("https://github.com/Org/Repo.git", "https://github.com/org/repo"))
	assert.False(t, SameRepoURL("https://github.com/org/a", "https://github.com/org/b"))
	assert.False(t, SameRepoURL("", ""))
}

// Ports the semantics of upstream TestAppProject_IsSourcePermitted.
func TestIsSourcePermitted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sources []string
		repo    string
		want    bool
	}{
		{[]string{"*"}, "https://github.com/argoproj/test.git", true},
		{[]string{"https://github.com/argoproj/test.git"}, "https://github.com/argoproj/test.git", true},
		{[]string{"https://github.com/argoproj/test"}, "https://GitHub.com/argoproj/test.git", true},
		{[]string{"https://github.com/argoproj/test.git"}, "https://github.com/argoproj/test", true},
		// Both URLs fail to parse and normalize to "": upstream permits.
		{[]string{"ssh://git@GITHUB.com:argoproj/test"}, "ssh://git@github.com:argoproj/test", true},
		{[]string{"https://github.com/argoproj/*"}, "https://github.com/argoproj/argoproj.git", true},
		{[]string{"https://github.com/test1/test.git", "https://github.com/test2/test.git"}, "https://github.com/test2/test.git", true},
		{[]string{"https://github.com/argoproj/test1.git"}, "https://github.com/argoproj/test2.git", false},
		{[]string{"https://github.com/argoproj/*.git"}, "https://github.com/argoproj1/test2.git", false},
		{[]string{"https://github.com/argoproj/foo"}, "https://github.com/argoproj/foo1", false},
		{[]string{"https://gitlab.com/group/*"}, "https://gitlab.com/group/repo/owner", false},
		{[]string{"https://gitlab.com/group/*/*"}, "https://gitlab.com/group/repo/owner", true},
		{[]string{"https://gitlab.com/group/*/*/*"}, "https://gitlab.com/group/sub-group/repo/owner", true},
		{[]string{"https://gitlab.com/group/**"}, "https://gitlab.com/group/sub-group/repo/owner", true},
		// TEN-004 required: org/* permits org/a but not org/a/b; ** permits both.
		{[]string{"https://github.com/org/*"}, "https://github.com/org/a", true},
		{[]string{"https://github.com/org/*"}, "https://github.com/org/a/b", false},
		{[]string{"https://github.com/org/**"}, "https://github.com/org/a/b", true},
		// Empty list permits nothing.
		{nil, "https://github.com/org/a", false},
		{[]string{}, "https://github.com/org/a", false},
		// A malformed glob never matches.
		{[]string{"https://github.com/[org/a"}, "https://github.com/[org/a", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, IsSourcePermitted(projWithSources(tt.sources...), tt.repo), "%v vs %s", tt.sources, tt.repo)
	}
	assert.False(t, IsSourcePermitted(nil, "https://x"))
}

// Ports the semantics of upstream TestAppProject_IsNegatedSourcePermitted and the
// sourceRepos documentation example.
func TestIsNegatedSourcePermitted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sources []string
		repo    string
		want    bool
	}{
		{[]string{"!https://github.com/argoproj/test.git"}, "https://github.com/argoproj/test.git", false},
		{[]string{"!ssh://git@GITHUB.com:argoproj/test"}, "ssh://git@github.com:argoproj/test", false},
		{[]string{"!https://github.com/argoproj/*"}, "https://github.com/argoproj/argoproj.git", false},
		{[]string{"https://github.com/test1/test.git", "!https://github.com/test2/test.git"}, "https://github.com/test2/test.git", false},
		{[]string{"!https://github.com/argoproj/foo*"}, "https://github.com/argoproj/foo1", false},
		{[]string{"!https://gitlab.com/group/*/*"}, "https://gitlab.com/group/repo/owner", false},
		{[]string{"!https://gitlab.com/group/*/*/*"}, "https://gitlab.com/group/sub-group/repo/owner", false},
		{[]string{"!https://gitlab.com/group/**"}, "https://gitlab.com/group/sub-group/repo/owner", false},
		{[]string{"*"}, "https://github.com/argoproj/test.git", true},
		{[]string{"https://github.com/argoproj/test1.git", "*"}, "https://github.com/argoproj/test2.git", true},
		{[]string{"!https://github.com/argoproj/*.git", "*"}, "https://github.com/argoproj1/test2.git", true},
		// TEN-005 doc example, both orders.
		{[]string{"!https://gitlab.com/group/**", "*"}, "https://gitlab.com/group/x/y", false},
		{[]string{"*", "!https://gitlab.com/group/**"}, "https://gitlab.com/group/x/y", false},
		{[]string{"!https://gitlab.com/group/**", "*"}, "https://github.com/x/y", true},
		// Deny wins over a more specific allow listed before or after.
		{[]string{"https://github.com/org/secret", "!https://github.com/org/*"}, "https://github.com/org/secret", false},
		{[]string{"!https://github.com/org/*", "https://github.com/org/secret"}, "https://github.com/org/secret", false},
		// Upstream code (not its documentation): a negated entry that does not match the
		// source counts as a match, so a deny-only list permits every other repository.
		{[]string{"!https://github.com/org/secret"}, "https://github.com/org/other", true},
		{[]string{"https://github.com/org/*", "!https://github.com/org/secret"}, "https://evil.example.com/x", true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, IsSourcePermitted(projWithSources(tt.sources...), tt.repo), "%v vs %s", tt.sources, tt.repo)
	}
}

// Ports the semantics of upstream TestAppProjectIsSourceNamespacePermitted plus regex
// entries (TEN-010, APP-068).
func TestIsAppNamespacePermitted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		namespaces []string
		appNS      string
		controlNS  string
		want       bool
	}{
		{"control-plane namespace", []string{"other-ns"}, "argocd", "argocd", true},
		{"control plane is the app namespace", []string{"other-ns"}, "some-ns", "some-ns", true},
		{"namespace not yet set", []string{"other-ns"}, "", "argocd", true},
		{"control plane with empty list", []string{}, "argocd", "argocd", true},
		{"empty list", []string{}, "some-ns", "argocd", false},
		{"listed", []string{"some-ns"}, "some-ns", "argocd", true},
		{"not listed", []string{"some-ns"}, "other-ns", "argocd", false},
		{"glob some-ns1", []string{"some-*"}, "some-ns1", "argocd", true},
		{"glob some-ns2", []string{"some-*"}, "some-ns2", "argocd", true},
		{"glob no match", []string{"some-*"}, "someotherns", "argocd", false},
		{"regex", []string{"/^team-(a|b)$/"}, "team-a", "argocd", true},
		{"regex no match", []string{"/^team-(a|b)$/"}, "team-c", "argocd", false},
		{"regex unanchored", []string{"/team/"}, "my-team-ns", "argocd", true},
		// RE2 has no lookahead: the upstream doc example never matches (fails closed).
		{"lookahead unsupported", []string{"/^((?!not-allowed).)*$/"}, "allowed", "argocd", false},
		// A lone "/" panics upstream; here it is a literal glob.
		{"lone slash", []string{"/"}, "ns", "argocd", false},
		{"star", []string{"*"}, "anything", "argocd", true},
	}
	for _, tt := range tests {
		proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: v1alpha1.AppProjectSpec{SourceNamespaces: tt.namespaces}}
		assert.Equal(t, tt.want, IsAppNamespacePermitted(proj, tt.appNS, tt.controlNS), tt.name)
	}
}

func TestCheckAppNamespace(t *testing.T) {
	t.Parallel()
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "proj"}, Spec: v1alpha1.AppProjectSpec{SourceNamespaces: []string{"ns-a"}}}
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns-b"}}
	err := CheckAppNamespace(proj, app, "patrol")
	require.Error(t, err)
	assert.EqualError(t, err, "application 'app' in namespace 'ns-b' is not permitted to use project 'proj'")
	var np *NotPermittedError
	require.True(t, errors.As(err, &np))
	assert.Equal(t, "application 'app' in namespace 'ns-b' is not allowed to use project proj", np.APIMessage())
	assert.Equal(t, `app is not allowed in project "proj", or the project does not exist`, AppNotAllowedMessage("proj"))

	app.Namespace = "ns-a"
	assert.NoError(t, CheckAppNamespace(proj, app, "patrol"))
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://user:xxxxx@github.com/org/repo", redactURL("https://user:secret@github.com/org/repo"))
	assert.Equal(t, "git@github.com:org/repo", redactURL("git@github.com:org/repo"))
	assert.Equal(t, "https://github.com/org/repo", redactURL("https://github.com/org/repo"))
}

// "!*" is rejected by ValidateProject but can be applied with kubectl. Negated entries use the
// plain glob with '/' as separator, so "*" does not match a URL containing '/': upstream then
// counts the entry as matching and the project permits every such repository.
func TestNegatedStarQuirk(t *testing.T) {
	t.Parallel()
	assert.True(t, IsSourcePermitted(projWithSources("!*"), "https://github.com/a/x"))
	assert.False(t, IsSourcePermitted(projWithSources("!*"), "no-slash"))
	requireValidation(t, ValidateProject(projWithSources("!*")), CodeInvalidArgument, "source repository has an invalid format, '!*'")
}
