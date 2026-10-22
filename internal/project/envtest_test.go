package project

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
)

// One envtest API server per test binary, started only when KUBEBUILDER_ASSETS is set (and
// required when PATROL_REQUIRE_ENVTEST=1, as `make test` sets it). The integration tests run
// the package against objects stored by a real kube-apiserver with the generated CRDs.

var envClient client.Client

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	// Invalid patterns and zones are logged once by design; keep test output readable.
	slog.SetDefault(slog.New(slog.DiscardHandler))
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := envtestx.Start(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		return 1
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		return 1
	}
	envClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		return 1
	}
	return m.Run()
}

func requireEnv(t *testing.T) context.Context {
	t.Helper()
	if envClient == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set: envtest integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func createNamespace(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	require.NoError(t, envClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
}

func create(ctx context.Context, t *testing.T, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		require.NoError(t, envClient.Create(ctx, o), "create %T %s", o, o.GetName())
	}
}

// TEN-037/038/039/040, APP-002..005, APP-068: project resolution and Application validation
// against objects stored in a real API server, read through KubeLookup.
func TestEnvtestValidateApplication(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "patrol-validate"
	createNamespace(ctx, t, ns)
	createNamespace(ctx, t, "team-a")

	create(ctx, t,
		&v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: ns, Labels: map[string]string{"tier": "apps"}}, Spec: v1alpha1.AppProjectSpec{
			SourceRepos:                     []string{"https://github.com/team/*"},
			SourceNamespaces:                []string{"team-*"},
			PermitOnlyProjectScopedClusters: true,
		}},
		&v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: ns}, Spec: v1alpha1.AppProjectSpec{
			SourceRepos: []string{"https://github.com/platform/*"},
			SyncWindows: v1alpha1.SyncWindows{{Kind: "deny", Schedule: "* * * * *", Duration: "1h", Namespaces: []string{"*"}}},
		}},
		&v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns}, Spec: v1alpha1.ClusterSpec{Name: "prod", Server: "https://prod.example.com", Project: "team", Namespaces: []string{"web", "api"}}},
		&v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}, Spec: v1alpha1.ClusterSpec{Name: "shared", Server: "https://shared.example.com"}},
		&v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "scoped", Namespace: ns}, Spec: v1alpha1.RepositorySpec{URL: "https://gitlab.com/scoped/charts.git", Project: "team"}},
		&v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "scoped-write", Namespace: ns}, Spec: v1alpha1.RepositorySpec{URL: "https://gitlab.com/scoped/hydrated.git", Project: "team", Purpose: v1alpha1.RepositoryPurposeWrite}},
	)
	globals, err := ParseGlobalProjectSettings("- projectName: platform\n  labelSelector:\n    matchLabels:\n      tier: apps\n")
	require.NoError(t, err)
	kl := NewKubeLookup(envClient, ns, true)
	l := kl.Lookups(globals)

	app := func(name, appNS string, mutate func(*v1alpha1.Application)) *v1alpha1.Application {
		a := &v1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS},
			Spec: v1alpha1.ApplicationSpec{
				Project:     "team",
				Source:      &v1alpha1.ApplicationSource{RepoURL: "https://github.com/team/web", Path: "deploy"},
				Destination: v1alpha1.ApplicationDestination{Name: "prod", Namespace: "web"},
			},
		}
		if mutate != nil {
			mutate(a)
		}
		create(ctx, t, a)
		stored := &v1alpha1.Application{}
		require.NoError(t, envClient.Get(ctx, client.ObjectKeyFromObject(a), stored))
		return stored
	}
	validate := func(a *v1alpha1.Application) []Issue {
		proj, _, err := ResolveAppProject(ctx, a, ns, l)
		if err != nil {
			return []Issue{ProjectErrorIssue(err, a)}
		}
		return ValidateApplication(ctx, a, proj, kl, ValidateOptions{ControlPlaneNamespace: ns})
	}

	assert.Empty(t, validate(app("ok", ns, nil)))
	assert.Empty(t, validate(app("ok-team-ns", "team-a", nil)), "sourceNamespaces glob")
	assert.Empty(t, validate(app("scoped-repo", ns, func(a *v1alpha1.Application) { a.Spec.Source.RepoURL = "https://gitlab.com/scoped/charts" })), "project-scoped repository")
	assert.Empty(t, validate(app("global-repo", ns, func(a *v1alpha1.Application) { a.Spec.Source.RepoURL = "https://github.com/platform/base" })), "inherited from the global project")
	assert.Equal(t, []string{"application repo https://gitlab.com/scoped/hydrated is not permitted in project 'team'"},
		messages(validate(app("write-repo", ns, func(a *v1alpha1.Application) { a.Spec.Source.RepoURL = "https://gitlab.com/scoped/hydrated" }))), "write repositories are not source repositories")
	assert.Equal(t, []string{"application destination server 'prod' and namespace 'db' do not match any of the allowed destinations in project 'team'"},
		messages(validate(app("bad-ns", ns, func(a *v1alpha1.Application) { a.Spec.Destination.Namespace = "db" }))))
	assert.Equal(t, []string{"application destination server 'https://shared.example.com' and namespace 'web' do not match any of the allowed destinations in project 'team'"},
		messages(validate(app("unscoped-cluster", ns, func(a *v1alpha1.Application) {
			a.Spec.Destination = v1alpha1.ApplicationDestination{Server: "https://shared.example.com", Namespace: "web"}
		}))))
	assert.Equal(t, []string{"there are no clusters with this name: nope"},
		messages(validate(app("unknown-cluster", ns, func(a *v1alpha1.Application) { a.Spec.Destination.Name = "nope" }))))
	missing := validate(app("missing-project", ns, func(a *v1alpha1.Application) { a.Spec.Project = "missing" }))
	assert.Equal(t, []Issue{{v1alpha1.ApplicationConditionInvalidSpecError, v1alpha1.ReasonProjectNotFound, "Application referencing project missing which does not exist"}}, missing)
	createNamespace(ctx, t, "elsewhere")
	assert.Equal(t, []string{"application 'denied-ns' in namespace 'elsewhere' is not permitted to use project 'team'"},
		messages(validate(app("denied-ns", "elsewhere", nil))))

	// Sync windows inherited from the global project apply to the team project.
	stored := app("windowed", ns, nil)
	proj, _, err := ResolveAppProject(ctx, stored, ns, l)
	require.NoError(t, err)
	ok, err := CanSync(proj, stored, false, nil, time.Now())
	require.NoError(t, err)
	assert.False(t, ok)
	raw, err := kl.GetProject(ctx, "team")
	require.NoError(t, err)
	ok, err = CanSync(raw, stored, false, nil, time.Now())
	require.NoError(t, err)
	assert.True(t, ok, "the raw project has no windows")

	_, err = kl.GetProject(ctx, "missing")
	assert.True(t, apierrors.IsNotFound(err))
	assert.True(t, IsNotFound(err))
}

// Admission (CRD schema) versus project validation: values the CRD admits but the upstream
// semantics reject, and the empty-whitelist round trip of TEN-013.
func TestEnvtestCRDAdmissionVersusValidation(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "patrol-admission"
	createNamespace(ctx, t, ns)

	descriptor := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "descriptor", Namespace: ns}, Spec: v1alpha1.AppProjectSpec{
		SourceRepos: []string{"!*"},
		SyncWindows: v1alpha1.SyncWindows{{Kind: "deny", Schedule: "@daily", Duration: "1h", Applications: []string{"*"}}},
	}}
	require.NoError(t, envClient.Create(ctx, descriptor), "the CRD pattern admits descriptors and !* (kubectl path)")
	stored := &v1alpha1.AppProject{}
	require.NoError(t, envClient.Get(ctx, client.ObjectKeyFromObject(descriptor), stored))
	err := ValidateProject(stored)
	requireValidation(t, err, CodeInvalidArgument, "source repository has an invalid format, '!*'")
	stored.Spec.SourceRepos = []string{"*"}
	err = ValidateProject(stored)
	requireValidation(t, err, CodeInvalidArgument, "cannot parse schedule '@daily': parser does not accept descriptors: @daily")
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	_, err = CanSync(stored, app, true, nil, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sync windows: cannot parse schedule '@daily'")

	// The CRD pattern requires exactly five fields, so a TZ= prefix (accepted upstream) is
	// rejected at admission.
	tz := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "tz", Namespace: ns}, Spec: v1alpha1.AppProjectSpec{
		SyncWindows: v1alpha1.SyncWindows{{Kind: "allow", Schedule: "TZ=Europe/Paris 0 8 * * *", Duration: "1h", Applications: []string{"*"}}},
	}}
	err = envClient.Create(ctx, tz)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), err)

	// A window without kind passes the enum (omitted field) but not validation.
	noKind := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "nokind", Namespace: ns}, Spec: v1alpha1.AppProjectSpec{
		SyncWindows: v1alpha1.SyncWindows{{Schedule: "0 8 * * *", Duration: "1h", Applications: []string{"*"}}},
	}}
	require.NoError(t, envClient.Create(ctx, noKind))
	require.NoError(t, envClient.Get(ctx, client.ObjectKeyFromObject(noKind), stored))
	requireValidation(t, ValidateProject(stored), CodeInvalidArgument, "kind '' mismatch: can only be allow or deny")

	// TEN-013: an explicit empty namespaceResourceWhitelist (kubectl/YAML) survives storage and
	// whitelists nothing; the omitted field whitelists everything.
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "AppProject",
		"metadata":   map[string]any{"name": "empty-whitelist", "namespace": ns},
		"spec":       map[string]any{"namespaceResourceWhitelist": []any{}},
	}}
	require.NoError(t, envClient.Create(ctx, u))
	require.NoError(t, envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "empty-whitelist"}, stored))
	assert.NotNil(t, stored.Spec.NamespaceResourceWhitelist)
	assert.False(t, IsGroupKindPermitted(stored, schema.GroupKind{Kind: "ConfigMap"}, true))
	omitted := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "omitted-whitelist", Namespace: ns}}
	require.NoError(t, envClient.Create(ctx, omitted))
	require.NoError(t, envClient.Get(ctx, client.ObjectKeyFromObject(omitted), stored))
	assert.True(t, IsGroupKindPermitted(stored, schema.GroupKind{Kind: "ConfigMap"}, true))
}

// TEN-032..034: token metadata written through the API server (spec and status subresource)
// and read back.
func TestEnvtestJWTTokenStorage(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "patrol-tokens"
	createNamespace(ctx, t, ns)
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns}, Spec: v1alpha1.AppProjectSpec{
		Roles: []v1alpha1.ProjectRole{{Name: "ci", JWTTokens: []v1alpha1.JWTToken{{IssuedAt: 10}}}},
	}}
	create(ctx, t, proj)

	stored := &v1alpha1.AppProject{}
	require.NoError(t, envClient.Get(ctx, client.ObjectKeyFromObject(proj), stored))
	require.NoError(t, ValidateProject(stored))
	require.NoError(t, AddJWTToken(stored, "ci", v1alpha1.JWTToken{IssuedAt: 20, ExpiresAt: 30, ID: "new"}))
	status := stored.Status.DeepCopy()
	require.NoError(t, envClient.Update(ctx, stored))
	stored.Status = *status
	require.NoError(t, envClient.Status().Update(ctx, stored))

	reread := &v1alpha1.AppProject{}
	require.NoError(t, envClient.Get(ctx, client.ObjectKeyFromObject(proj), reread))
	want := []v1alpha1.JWTToken{{IssuedAt: 20, ExpiresAt: 30, ID: "new"}, {IssuedAt: 10, ID: "10"}}
	assert.Equal(t, want, reread.Status.JWTTokensByRole["ci"].Items)
	assert.Equal(t, want, reread.Spec.Roles[0].JWTTokens) //nolint:staticcheck // deprecated field exercised for migration
	assert.False(t, NormalizeJWTTokens(reread), "stored state is normalized")
	tok, _, err := GetJWTToken(reread, "ci", 10, "")
	require.NoError(t, err)
	assert.Equal(t, "10", tok.ID, "legacy token migrated with an iat-based id")
}

// Security, TEN-038/039: the CRDs admit a Cluster server "https://*" and a Repository URL
// "!x" (reported to the api/v1alpha1 owner); scoped to a project, they must not widen it
// beyond themselves.
func TestEnvtestScopedResourcesAreExact(t *testing.T) {
	ctx := requireEnv(t)
	const ns = "patrol-scoped-exact"
	createNamespace(ctx, t, ns)
	create(ctx, t,
		&v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: ns}, Spec: v1alpha1.AppProjectSpec{
			SourceRepos: []string{"https://github.com/team/*"},
		}},
		&v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "star", Namespace: ns}, Spec: v1alpha1.ClusterSpec{Name: "star", Server: "https://*", Project: "team"}},
		&v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: ns}, Spec: v1alpha1.ClusterSpec{Name: "prod", Server: "https://prod.example.com"}},
		&v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "negated", Namespace: ns}, Spec: v1alpha1.RepositorySpec{URL: "!x", Project: "team"}},
		&v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "glob", Namespace: ns}, Spec: v1alpha1.RepositorySpec{URL: "https://github.com/*/*", Project: "team"}},
	)
	kl := NewKubeLookup(envClient, ns, true)
	raw, err := kl.GetProject(ctx, "team")
	require.NoError(t, err)
	eff, gerr, err := EffectiveProject(ctx, raw, kl.Lookups(nil))
	require.NoError(t, err)
	require.NoError(t, gerr)

	assert.False(t, IsSourcePermitted(eff, "https://github.com/evil/repo"))
	assert.True(t, IsSourcePermitted(eff, "https://github.com/team/web"))
	for _, tt := range []struct {
		name string
		want bool
	}{
		{"prod", false},
		{"star", true},
	} {
		ok, _, err := IsAppDestinationPermitted(ctx, eff, v1alpha1.ApplicationDestination{Name: tt.name, Namespace: "kube-system"}, kl)
		require.NoError(t, err)
		assert.Equal(t, tt.want, ok, tt.name)
	}
}
