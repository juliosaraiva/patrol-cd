package applications_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// onFirstRender runs fn once, from the repo-server hook, while a handler validates. The
// hook runs on the server's goroutine, so its error is returned by the wait function
// instead of failing the test there.
func (e *testEnv) onFirstRender(fn func() error) (wait func() error) {
	var (
		once sync.Once
		mu   sync.Mutex
		err  error
		ran  bool
	)
	e.repo.setHook(func(*reposerver.ManifestRequest) {
		once.Do(func() {
			hookErr := fn()
			mu.Lock()
			defer mu.Unlock()
			err, ran = hookErr, true
		})
	})
	return func() error {
		e.repo.setHook(nil)
		mu.Lock()
		defer mu.Unlock()
		require.True(e.t, ran, "the repo-server hook did not run")
		return err
	}
}

// mutateStored changes a stored Application with retries on conflict (as a controller or
// kubectl would), without failing the test from a non-test goroutine.
func (e *testEnv) mutateStored(ns, name string, mutate func(*v1alpha1.Application)) error {
	for {
		a := &v1alpha1.Application{}
		if err := e.kube.Get(e.ctx, client.ObjectKey{Namespace: ns, Name: name}, a); err != nil {
			return err
		}
		mutate(a)
		err := e.kube.Update(e.ctx, a)
		if !apierrors.IsConflict(err) {
			return err
		}
	}
}

// TestCreateApplicationUpsertRace covers an Application created concurrently with an
// upsert (Create returns AlreadyExists after the first read found nothing): the project
// change rules (update on the old project, TEN-043/APP-055) must still apply.
func TestCreateApplicationUpsertRace(t *testing.T) {
	// upserter: get in proj-a, create and update in proj-b (no update in proj-a).
	policy := defaultPolicy + `p, role:upserter, applications, get, proj-a/*, allow
p, role:upserter, applications, create, proj-b/*, allow
p, role:upserter, applications, update, proj-b/*, allow
g, upserter, role:upserter
`
	e := newEnv(t, envOptions{policy: policy})
	upsert := func(c *apiclient.Client, name, proj, revision string) (*v1alpha1.Application, error) {
		a := e.newApp(name, proj)
		a.Spec.Source.TargetRevision = revision
		return c.CreateApplication(e.ctx, &apiclient.ApplicationCreateRequest{Application: a, Upsert: ptrBool(true)})
	}
	// raceCreate stores app while the create request renders its sources.
	raceCreate := func(app *v1alpha1.Application) func() error {
		return e.onFirstRender(func() error { return e.kube.Create(e.ctx, app.DeepCopy()) })
	}

	t.Run("project move needs update on the old project", func(t *testing.T) {
		wait := raceCreate(e.newApp("victim", "proj-a"))
		_, err := upsert(e.client("upserter"), "victim", "proj-b", "main")
		require.NoError(t, wait())
		requireProblem(t, err, http.StatusForbidden, "permission denied: applications, update, proj-a/victim")
		assert.Equal(t, "proj-a", e.getApp(e.ns, "victim").Spec.Project, "the Application must not move")

		// Without the race the answer is the same.
		e.createApp(e.newApp("victim2", "proj-a"))
		_, err = upsert(e.client("upserter"), "victim2", "proj-b", "main")
		requireProblem(t, err, http.StatusForbidden, "permission denied: applications, update, proj-a/victim2")
		assert.Equal(t, "proj-a", e.getApp(e.ns, "victim2").Spec.Project)
	})

	t.Run("same project upsert still updates", func(t *testing.T) {
		wait := raceCreate(e.newApp("same", "proj-b"))
		got, err := upsert(e.client("bob"), "same", "proj-b", "v2")
		require.NoError(t, wait())
		require.NoError(t, err)
		assert.Equal(t, "v2", got.Spec.Source.TargetRevision)
		assert.Equal(t, "v2", e.getApp(e.ns, "same").Spec.Source.TargetRevision)
	})

	t.Run("a role allowed on both projects may move it", func(t *testing.T) {
		wait := raceCreate(e.newApp("movable", "proj-a"))
		got, err := upsert(e.client("admin"), "movable", "proj-b", "main")
		require.NoError(t, wait())
		require.NoError(t, err)
		assert.Equal(t, "proj-b", got.Spec.Project)
	})

	t.Run("an Application the caller may not see is reported as existing", func(t *testing.T) {
		wait := raceCreate(e.newApp("hidden", "proj-a"))
		_, err := upsert(e.client("bob"), "hidden", "proj-b", "main")
		require.NoError(t, wait())
		requireProblem(t, err, http.StatusConflict, `application "hidden" already exists`)
		assert.Equal(t, "proj-a", e.getApp(e.ns, "hidden").Spec.Project)
	})
}

// TestUpdateApplicationSpecConcurrentMetadata covers PUT /spec racing with metadata
// changes: only the spec is written, so labels, annotations and finalizers set
// concurrently survive (APP-051), including the cascade finalizer of an in-flight delete.
func TestUpdateApplicationSpecConcurrentMetadata(t *testing.T) {
	e := newEnv(t, envOptions{})
	alice := e.client("alice")
	updateSpec := func(name string) (*v1alpha1.ApplicationSpec, error) {
		spec := e.getApp(e.ns, name).Spec
		spec.Source.TargetRevision = "v2"
		return alice.UpdateApplicationSpec(e.ctx, &apiclient.ApplicationUpdateSpecRequest{Name: name, Spec: &spec})
	}

	t.Run("labels annotations and finalizers survive", func(t *testing.T) {
		e.createApp(e.newApp("meta", "proj-a"))
		wait := e.onFirstRender(func() error {
			return e.mutateStored(e.ns, "meta", func(a *v1alpha1.Application) {
				a.Finalizers = append(a.Finalizers, v1alpha1.FinalizerResources)
				a.Labels = map[string]string{"concurrent": "1"}
				a.Annotations = map[string]string{"note": "concurrent"}
			})
		})
		got, err := updateSpec("meta")
		require.NoError(t, wait())
		require.NoError(t, err)
		assert.Equal(t, "v2", got.Source.TargetRevision)
		stored := e.getApp(e.ns, "meta")
		assert.Equal(t, "v2", stored.Spec.Source.TargetRevision)
		assert.Equal(t, []string{v1alpha1.FinalizerResources}, stored.Finalizers)
		assert.Equal(t, map[string]string{"concurrent": "1"}, stored.Labels)
		assert.Equal(t, map[string]string{"note": "concurrent"}, stored.Annotations)
	})

	t.Run("an in-flight cascading delete keeps its finalizer", func(t *testing.T) {
		e.createApp(e.newApp("cascade", "proj-a"))
		wait := e.onFirstRender(func() error {
			if err := e.mutateStored(e.ns, "cascade", func(a *v1alpha1.Application) {
				a.Finalizers = append(a.Finalizers, v1alpha1.FinalizerResources)
			}); err != nil {
				return err
			}
			return e.kube.Delete(e.ctx, e.appRef(e.ns, "cascade"))
		})
		_, err := updateSpec("cascade")
		require.NoError(t, wait())
		require.NoError(t, err)
		stored := &v1alpha1.Application{}
		require.NoError(t, e.kube.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: "cascade"}, stored),
			"the Application must wait for the cascade (its finalizer must not be stripped)")
		assert.NotNil(t, stored.DeletionTimestamp)
		assert.Equal(t, []string{v1alpha1.FinalizerResources}, stored.Finalizers)
		// Release it.
		require.NoError(t, e.mutateStored(e.ns, "cascade", func(a *v1alpha1.Application) { a.Finalizers = nil }))
	})
}

// appRef returns a reference to an Application (name and namespace only).
func (e *testEnv) appRef(ns, name string) *v1alpha1.Application {
	a := &v1alpha1.Application{}
	a.Namespace, a.Name = ns, name
	return a
}

// TestPatchApplicationLimits covers JSON patches whose effect is unbounded by the body
// size: self-copies double the document per operation.
func TestPatchApplicationLimits(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createApp(e.newApp("guestbook", "proj-a"))
	before := e.getApp(e.ns, "guestbook")
	alice := e.client("alice")

	// Each copy of /spec into a new member of /spec doubles the document.
	ops := make([]string, 40)
	for i := range ops {
		ops[i] = fmt.Sprintf(`{"op":"copy","from":"/spec","path":"/spec/x%d"}`, i)
	}
	_, err := alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook", Patch: "[" + strings.Join(ops, ",") + "]"})
	p := requireProblem(t, err, http.StatusRequestEntityTooLarge, "the accumulated size increase of copy")
	assert.Equal(t, apiclient.CodeResourceExhausted, p.Code)

	many := make([]string, 1001)
	for i := range many {
		many[i] = `{"op":"test","path":"/spec/project","value":"proj-a"}`
	}
	_, err = alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook", Patch: "[" + strings.Join(many, ",") + "]"})
	requireProblem(t, err, http.StatusRequestEntityTooLarge, "the allowed maximum operations in a JSON patch is 1000, got 1001")

	assert.Equal(t, before.ResourceVersion, e.getApp(e.ns, "guestbook").ResourceVersion, "nothing written")

	// A legitimate copy still works.
	got, err := alice.PatchApplication(e.ctx, &apiclient.ApplicationPatchRequest{Name: "guestbook",
		Patch: `[{"op":"add","path":"/metadata/annotations","value":{"a":"yes"}},{"op":"copy","from":"/metadata/annotations/a","path":"/metadata/annotations/b"}]`})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "yes", "b": "yes"}, got.Annotations)
}
