package webhook

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// End to end against a real kube-apiserver with the generated CRDs (REPO-114, REPO-116,
// REPO-124..REPO-127, REPO-130, ASET-104..ASET-107, T9/T10): signed deliveries are
// verified with patrol-secret and SCMProvider secrets, matching Applications and
// ApplicationSets are annotated with JSON merge patches that keep unrelated annotations,
// non-matching objects and disallowed namespaces are untouched, and a replayed delivery
// is not processed twice.
func TestEnvtestWebhookRefresh(t *testing.T) {
	cfg := envConfig(t)
	c, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx := context.Background()

	// Unique namespaces: the API server is shared by the test binary (and -count reruns).
	suffix := fmt.Sprintf("-%d", rand.N(1_000_000))
	ns, teamA, teamB := "patrol-wh"+suffix, "wh-team-a"+suffix, "wh-team-b"+suffix
	for _, n := range []string{ns, teamA, teamB} {
		require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}))
	}
	template := v1alpha1.ApplicationSetTemplate{
		ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{Name: "{{path.basename}}"},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "default",
			Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "default"},
			Source:      &v1alpha1.ApplicationSource{RepoURL: platformRepo, TargetRevision: "HEAD", Path: "{{path}}"},
		},
	}
	appSet := func(name string, gens ...v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSet {
		as := appSetWith(ns, name, gens...)
		as.Spec.Template = template
		return as
	}

	web := gitApp(ns, "web", platformRepo, "HEAD", "apps/web")
	web.Annotations = map[string]string{"example.com/keep": "me"}
	hard := gitApp(ns, "hard", "git@github.com:acme/platform.git", "main", "apps/hard")
	hard.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: string(v1alpha1.RefreshTypeHard)}
	multi := multiSource(
		v1alpha1.ApplicationSource{RepoURL: "https://charts.example.com", Chart: "web", TargetRevision: "1.0.0"},
		v1alpha1.ApplicationSource{RepoURL: platformRepo + ".git", TargetRevision: "refs/heads/main", Ref: "values"},
	)
	multi.Namespace = ns
	hyd := hydrated(platformRepo, "main", "apps/web", "env/prod", "hydrated/web")
	hyd.Namespace = ns
	objs := []client.Object{
		web, hard, multi, hyd,
		withAnnotation(gitApp(ns, "filtered", platformRepo, "main", "apps/api"), "."),
		withAnnotation(gitApp(ns, "paths-match", platformRepo, "main", "apps/api"), "../web;/charts"),
		gitApp(ns, "other-branch", platformRepo, "stable", "apps/web"),
		gitApp(ns, "other-repo", "https://github.com/acme/other", "main", "apps/web"),
		gitApp(teamA, "tenant", platformRepo, "main", "apps/web"),
		gitApp(teamB, "not-allowed", platformRepo, "main", "apps/web"),
		appSet("git", v1alpha1.ApplicationSetGenerator{Git: &v1alpha1.GitGenerator{RepoURL: "git@github.com:acme/platform.git", Revision: "HEAD", Directories: []v1alpha1.GitDirectoryGeneratorItem{{Path: "apps/*"}}}}),
		appSet("merge", v1alpha1.ApplicationSetGenerator{Merge: &v1alpha1.MergeGenerator{MergeKeys: []string{"path"}, Generators: []v1alpha1.ApplicationSetNestedGenerator{
			{List: listGen()},
			{Git: &v1alpha1.GitGenerator{RepoURL: platformRepo, Revision: "main", Directories: []v1alpha1.GitDirectoryGeneratorItem{{Path: "apps/*"}}}},
		}}}),
		appSet("unrelated", v1alpha1.ApplicationSetGenerator{Git: &v1alpha1.GitGenerator{RepoURL: "https://github.com/acme/other", Revision: "HEAD", Directories: []v1alpha1.GitDirectoryGeneratorItem{{Path: "*"}}}}),
		&v1alpha1.SCMProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "github"},
			Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, Webhook: &v1alpha1.SCMProviderWebhook{
				SecretRef: v1alpha1.SecretKeyReference{Name: "github-webhook", Key: "secret"},
			}},
		},
		secretObj(ns, "github-webhook", "secret", "provider-secret"),
	}
	for _, o := range objs {
		require.NoError(t, c.Create(ctx, o), "create %T %s", o, o.GetName())
	}

	h := newTestHandler(t, Config{
		Namespace:             ns,
		Client:                c,
		ApplicationNamespaces: []string{teamA},
		Settings:              staticSettings(settings.Webhook{GitHubSecret: settings.NewSensitive(goodSecret)}),
	})
	h.start(t)

	body := fixture(t, "github_push.json")
	deliver := func(secret, id string) int {
		return h.serve(newRequest(t, "/api/webhook", body, map[string]string{
			"X-GitHub-Event": "push", "X-GitHub-Delivery": id, "X-Hub-Signature-256": "sha256=" + hmacHex(secret, body),
		})).Code
	}
	require.Equal(t, http.StatusBadRequest, deliver(wrongSecret, "env-0"), "wrong secret")
	// The SCMProvider secret (rotation candidate) verifies the delivery.
	require.Equal(t, http.StatusOK, deliver("provider-secret", "env-1"))

	app := func(ns, name string) map[string]string {
		return annotations(t, c, &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}})
	}
	set := func(name string) map[string]string {
		return annotations(t, c, &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}})
	}
	refreshed := []struct{ ns, name string }{{ns, "web"}, {ns, "multi"}, {ns, "hydrated"}, {ns, "paths-match"}, {teamA, "tenant"}}
	eventually(t, 20*time.Second, func() bool {
		for _, r := range refreshed {
			if app(r.ns, r.name)[v1alpha1.AnnotationKeyRefresh] != "normal" {
				return false
			}
		}
		return set("git")[v1alpha1.AnnotationKeyApplicationSetRefresh] == "true" &&
			set("merge")[v1alpha1.AnnotationKeyApplicationSetRefresh] == "true"
	}, "matching Applications and ApplicationSets annotated in the API server")
	eventually(t, 5*time.Second, func() bool {
		return containsAll(h.logs.String(), `"deliveryID":"env-1"`, `"applications":6`, `"applicationSets":2`)
	}, "audit log with counts")

	assert.Equal(t, "me", app(ns, "web")["example.com/keep"], "the merge patch keeps other annotations")
	assert.Equal(t, "hard", app(ns, "hard")[v1alpha1.AnnotationKeyRefresh], "a pending hard refresh is not downgraded")
	assert.Equal(t, "normal", app(ns, "hydrated")[v1alpha1.AnnotationKeyHydrate])
	assert.NotContains(t, app(ns, "web"), v1alpha1.AnnotationKeyHydrate)
	for _, n := range []struct{ ns, name string }{{ns, "filtered"}, {ns, "other-branch"}, {ns, "other-repo"}, {teamB, "not-allowed"}} {
		assert.NotContains(t, app(n.ns, n.name), v1alpha1.AnnotationKeyRefresh, n.name)
	}
	assert.NotContains(t, set("unrelated"), v1alpha1.AnnotationKeyApplicationSetRefresh)

	// The controller consumes the annotation; a replay of the same verified delivery is
	// acknowledged but not processed again.
	var consumed v1alpha1.Application
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web"}, &consumed))
	delete(consumed.Annotations, v1alpha1.AnnotationKeyRefresh)
	require.NoError(t, c.Update(ctx, &consumed))
	require.Equal(t, http.StatusOK, deliver(goodSecret, "env-1"))
	assert.Equal(t, 1.0, h.requests(ProviderGitHub, resultDuplicate))
	// A new delivery refreshes again (patrol-secret value this time).
	require.Equal(t, http.StatusOK, deliver(goodSecret, "env-2"))
	eventually(t, 20*time.Second, func() bool { return app(ns, "web")[v1alpha1.AnnotationKeyRefresh] == "normal" }, "new delivery refreshes")
	assert.Equal(t, 2.0, h.requests(ProviderGitHub, resultAccepted))
	assert.NotContains(t, h.logs.String(), "provider-secret")
	assert.NotContains(t, h.logs.String(), goodSecret)
}

// REPO-127, SEC-WH-6 against a real kube-apiserver: the refresh merge patch carries the
// resourceVersion read, so a hard refresh requested between the read and the patch makes
// the API server answer 409 and the retry keeps "hard"; a stale API reader never
// overwrites the newer value (the refresh fails with a conflict instead).
func TestEnvtestRefreshConditionalPatch(t *testing.T) {
	cfg := envConfig(t)
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx := context.Background()
	ns := fmt.Sprintf("patrol-wh-rv-%d", rand.N(1_000_000))
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	key := client.ObjectKey{Namespace: ns, Name: "web"}
	get := func() *v1alpha1.Application {
		var app v1alpha1.Application
		require.NoError(t, c.Get(ctx, key, &app))
		return &app
	}
	setRefresh := func(cl client.Client, value string) error {
		var app v1alpha1.Application
		if err := cl.Get(ctx, key, &app); err != nil {
			return err
		}
		if app.Annotations == nil {
			app.Annotations = map[string]string{}
		}
		if value == "" {
			delete(app.Annotations, v1alpha1.AnnotationKeyRefresh)
		} else {
			app.Annotations[v1alpha1.AnnotationKeyRefresh] = value
		}
		return cl.Update(ctx, &app)
	}
	require.NoError(t, c.Create(ctx, gitApp(ns, "web", platformRepo, "main", "apps/web")))
	req := refreshRequest{kind: kindApplication, namespace: ns, name: "web"}

	t.Run("hard requested between read and patch is kept", func(t *testing.T) {
		var (
			mu        sync.Mutex
			patches   int
			conflicts int
		)
		ic := interceptor.NewClient(c, interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				mu.Lock()
				patches++
				first := patches == 1
				mu.Unlock()
				if first {
					// A user requests a hard refresh after the handler read the object.
					if err := setRefresh(cl, "hard"); err != nil {
						return err
					}
				}
				err := cl.Patch(ctx, obj, patch, opts...)
				if apierrors.IsConflict(err) {
					mu.Lock()
					conflicts++
					mu.Unlock()
				}
				return err
			},
		})
		h := newTestHandler(t, Config{Namespace: ns, Client: ic, APIReader: c, Settings: staticSettings(settings.Webhook{})})
		require.NoError(t, h.refreshApplication(ctx, req))
		assert.Equal(t, 1, conflicts, "the API server rejected the stale patch")
		assert.Equal(t, 2, patches)
		assert.Equal(t, "hard", get().Annotations[v1alpha1.AnnotationKeyRefresh])
	})

	t.Run("stale API reader never downgrades", func(t *testing.T) {
		require.NoError(t, setRefresh(c, ""))
		snapshot := get() // read before the hard request: a lagging cache's view
		require.NoError(t, setRefresh(c, "hard"))
		stale := interceptor.NewClient(fakeClient(t), interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				snapshot.DeepCopyInto(obj.(*v1alpha1.Application))
				return nil
			},
		})
		h := newTestHandler(t, Config{Namespace: ns, Client: c, APIReader: stale, Settings: staticSettings(settings.Webhook{})})
		err := h.refreshApplication(ctx, req)
		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err), "%v", err)
		assert.Equal(t, "hard", get().Annotations[v1alpha1.AnnotationKeyRefresh])
	})

	t.Run("consumed request is re-requested as normal", func(t *testing.T) {
		require.NoError(t, setRefresh(c, "hard"))
		var (
			mu      sync.Mutex
			patches int
		)
		ic := interceptor.NewClient(c, interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				mu.Lock()
				patches++
				first := patches == 1
				mu.Unlock()
				if first {
					// The controller consumes the hard refresh after the handler read it.
					if err := setRefresh(cl, ""); err != nil {
						return err
					}
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		})
		h := newTestHandler(t, Config{Namespace: ns, Client: ic, APIReader: c, Settings: staticSettings(settings.Webhook{})})
		require.NoError(t, h.refreshApplication(ctx, req))
		assert.Equal(t, 2, patches)
		assert.Equal(t, "normal", get().Annotations[v1alpha1.AnnotationKeyRefresh])
	})
}
