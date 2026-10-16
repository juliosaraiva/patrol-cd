package applications_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestResourceTree(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")

	tree, err := re.rclient("reader").GetApplicationResourceTree(re.ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook"})
	require.NoError(t, err)
	require.Len(t, tree.Nodes, 7)
	require.Len(t, tree.OrphanedNodes, 1)
	pod := tree.FindNode("", "Pod", re.appNS, "web-abc-1")
	require.NotNil(t, pod)
	assert.Equal(t, "ReplicaSet", pod.ParentRefs[0].Kind)
	assert.Equal(t, "Running", pod.Info[0].Value)

	// Existence hiding: another tenant, an unknown application, no credentials.
	_, err = re.rclient("bob").GetApplicationResourceTree(re.ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook"})
	requireProblem(t, err, 403, "permission denied")
	_, err = re.rclient("reader").GetApplicationResourceTree(re.ctx, &apiclient.ApplicationTreeQuery{Name: "missing"})
	requireProblem(t, err, 403, "permission denied")
	_, err = re.rclient("reader").GetApplicationResourceTree(re.ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook", Project: "proj-b"})
	requireProblem(t, err, 403, "")
	_, err = re.rclient("alice").GetApplicationResourceTree(re.ctx, &apiclient.ApplicationTreeQuery{Name: "missing", Project: "proj-a"})
	requireProblem(t, err, 404, "not found")
	_, err = re.rclient("").GetApplicationResourceTree(re.ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook"})
	requireProblem(t, err, 401, "")
}

// A missing controller state triggers a refresh (upstream getCachedAppState); a comparison
// error is reported instead.
func TestResourceTreeCacheMiss(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	app := re.fixture("guestbook", "proj-a")
	key := treeapi.AppKey{Namespace: app.Namespace, Name: app.Name}
	re.state.setMissing(key, true)

	// Stand-in controller: reconciles the refresh and computes the state. Like the real
	// controller it stores the state before it clears the refresh annotation (the API
	// server treats the refresh as done as soon as the annotation is gone).
	ctx, cancel := context.WithCancel(re.ctx)
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			cur := &v1alpha1.Application{}
			if re.kube.Get(ctx, client.ObjectKeyFromObject(app), cur) == nil {
				if _, ok := cur.Annotations[v1alpha1.AnnotationKeyRefresh]; ok {
					re.state.setMissing(key, false)
					delete(cur.Annotations, v1alpha1.AnnotationKeyRefresh)
					if re.kube.Update(ctx, cur) != nil {
						re.state.setMissing(key, true)
					}
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	tree, err := re.rclient("reader").GetApplicationResourceTree(re.ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook"})
	require.NoError(t, err)
	assert.Len(t, tree.Nodes, 7)
	cancel()

	re.state.setMissing(key, true)
	cur := re.getApp(app.Namespace, app.Name)
	cur.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ApplicationConditionComparisonError, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ReasonComparisonFailed, Message: "repository not accessible", LastTransitionTime: metav1.Now()}}
	require.NoError(t, re.kube.Status().Update(re.ctx, cur))
	_, err = re.rclient("reader").GetApplicationResourceTree(re.ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook"})
	requireProblem(t, err, 412, "ComparisonError: repository not accessible")
}

func TestWatchResourceTree(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	app := re.fixture("guestbook", "proj-a")
	key := treeapi.AppKey{Namespace: app.Namespace, Name: app.Name}

	ctx, cancel := context.WithTimeout(re.ctx, time.Minute)
	defer cancel()
	s, err := re.rclient("reader").WatchApplicationResourceTree(ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook"}, nil)
	require.NoError(t, err)
	events := make(chan apiclient.ApplicationTreeEvent, 16)
	errs := make(chan error, 1)
	go func() {
		errs <- apiclient.Watch(s, func(ev apiclient.ApplicationTreeEvent) error { events <- ev; return nil })
	}()
	next := func() apiclient.ApplicationTreeEvent {
		t.Helper()
		select {
		case ev := <-events:
			return ev
		case err := <-errs:
			t.Fatalf("stream ended: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatal("no tree event")
		}
		return apiclient.ApplicationTreeEvent{}
	}
	ev := next()
	assert.Equal(t, apiclient.EventAdded, ev.Type)
	assert.Len(t, ev.Object.Nodes, 7)

	// The controller's tree changes: MODIFIED (polling).
	tree, err := re.state.Tree(re.ctx, key)
	require.NoError(t, err)
	tree.Nodes = tree.Nodes[:3]
	re.state.setTree(key, tree)
	ev = next()
	assert.Equal(t, apiclient.EventModified, ev.Type)
	assert.Len(t, ev.Object.Nodes, 3)

	// Losing access ends the stream with a 403 error event.
	re.updateApp("guestbook", func(a *v1alpha1.Application) { a.Spec.Project = "proj-b" })
	select {
	case err := <-errs:
		ae := apiErr(t, err)
		assert.Equal(t, 403, ae.StatusCode)
	case <-time.After(20 * time.Second):
		t.Fatal("stream did not end after the project move")
	}

	// Deleting the Application ends a stream with DELETED.
	re.updateApp("guestbook", func(a *v1alpha1.Application) { a.Spec.Project = "proj-a" })
	s, err = re.rclient("alice").WatchApplicationResourceTree(ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook"}, nil)
	require.NoError(t, err)
	go func() {
		errs <- apiclient.Watch(s, func(ev apiclient.ApplicationTreeEvent) error { events <- ev; return nil })
	}()
	assert.Equal(t, apiclient.EventAdded, next().Type)
	require.NoError(t, re.kube.Delete(re.ctx, re.getApp(app.Namespace, app.Name)))
	assert.Equal(t, apiclient.EventDeleted, next().Type)
	select {
	case err := <-errs:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("stream did not end after DELETED")
	}

	// Streams are refused like reads (an error event before any tree).
	re.fixture("guestbook2", "proj-a")
	s, err = re.rclient("bob").WatchApplicationResourceTree(ctx, &apiclient.ApplicationTreeQuery{Name: "guestbook2"}, nil)
	require.NoError(t, err)
	err = apiclient.Watch(s, func(apiclient.ApplicationTreeEvent) error { return errors.New("no event expected") })
	assert.Equal(t, 403, apiErr(t, err).StatusCode)
}

func TestManagedResources(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	reader := re.rclient("reader")

	res, err := reader.GetApplicationManagedResources(re.ctx, &apiclient.ApplicationManagedResourcesQuery{Name: "guestbook"})
	require.NoError(t, err)
	require.Len(t, res.Items, 3, "the hook is excluded")
	var secret *apiclient.ResourceDiff
	for i := range res.Items {
		if res.Items[i].Kind == "Secret" {
			secret = &res.Items[i]
		}
	}
	require.NotNil(t, secret)
	for _, s := range []string{secret.TargetState, secret.LiveState, secret.NormalizedLiveState, secret.PredictedLiveState} {
		assert.NotContains(t, s, "aHVudGVyMg==", "Secret data must be masked")
		assert.NotContains(t, s, "bmV3")
		assert.NotContains(t, s, "t0p-secret", "sensitive annotations must be masked")
		assert.Contains(t, s, "++++")
	}
	assert.True(t, secret.Modified)

	res, err = reader.GetApplicationManagedResources(re.ctx, &apiclient.ApplicationManagedResourcesQuery{Name: "guestbook", Kind: "Deployment", Group: "apps"})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "web", res.Items[0].Name)
	res, err = reader.GetApplicationManagedResources(re.ctx, &apiclient.ApplicationManagedResourcesQuery{Name: "guestbook", Namespace: "elsewhere"})
	require.NoError(t, err)
	assert.Empty(t, res.Items)

	_, err = re.rclient("bob").GetApplicationManagedResources(re.ctx, &apiclient.ApplicationManagedResourcesQuery{Name: "guestbook"})
	requireProblem(t, err, 403, "permission denied")
}

func TestServerSideDiff(t *testing.T) {
	re := newResEnv(t, resEnvOptions{})
	re.fixture("guestbook", "proj-a")
	reader := re.rclient("reader")
	ns := re.appNS

	secretLive := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds","namespace":"` + ns + `"},"data":{"password":"aHVudGVyMg=="}}`
	secretTarget := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds","namespace":"` + ns + `"},"data":{"password":"bmV3"}}`
	cmTarget := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"new","namespace":"` + ns + `"},"data":{"a":"b"}}`
	res, err := reader.ApplicationServerSideDiff(re.ctx, &apiclient.ApplicationServerSideDiffRequest{
		Name: "guestbook",
		LiveResources: []apiclient.ResourceDiff{
			{Kind: "Secret", Namespace: ns, Name: "creds", LiveState: secretLive},
			{Kind: "ConfigMap", Namespace: ns, Name: "new", LiveState: "null"},
		},
		TargetManifests: []string{secretTarget, cmTarget},
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 2)
	assert.True(t, res.Modified)
	assert.NotContains(t, res.Items[0].TargetState, "bmV3")
	assert.NotContains(t, res.Items[0].LiveState, "aHVudGVyMg==")
	assert.Contains(t, res.Items[1].TargetState, `"a":"b"`, "non-Secret results are returned as is")

	// A live resource outside the managed resources is refused (controller validation).
	other := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"other","namespace":"` + ns + `"}}`
	_, err = reader.ApplicationServerSideDiff(re.ctx, &apiclient.ApplicationServerSideDiffRequest{
		Name: "guestbook", LiveResources: []apiclient.ResourceDiff{{Kind: "Secret", Namespace: ns, Name: "other", LiveState: other}}, TargetManifests: []string{other},
	})
	requireProblem(t, err, 403, "/Secret other not found as part of application guestbook")
	// Spoofed metadata: the live state must match its declared identity.
	_, err = reader.ApplicationServerSideDiff(re.ctx, &apiclient.ApplicationServerSideDiffRequest{
		Name: "guestbook", LiveResources: []apiclient.ResourceDiff{{Kind: "ConfigMap", Namespace: ns, Name: "cfg", LiveState: secretLive}}, TargetManifests: []string{secretTarget},
	})
	requireProblem(t, err, 400, "name mismatch: expected cfg, got creds for live resource /ConfigMap/cfg")
	_, err = reader.ApplicationServerSideDiff(re.ctx, &apiclient.ApplicationServerSideDiffRequest{
		Name: "guestbook", LiveResources: []apiclient.ResourceDiff{{Kind: "ConfigMap", Namespace: ns, Name: "cfg"}},
	})
	requireProblem(t, err, 400, "index-aligned")

	// A target paired with a managed live resource must be that resource: the controller
	// dry-run applies the target with its own credentials, so a foreign target (another
	// namespace, name, kind or group) is refused before the controller is called.
	cfgLive := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg","namespace":"` + ns + `"},"data":{"k":"v"}}`
	calls := re.state.ssdCalls.Load()
	for name, target := range map[string]string{
		"other namespace": `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg","namespace":"kube-system"},"data":{"k":"v"}}`,
		"other name":      `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"extension-apiserver-authentication","namespace":"` + ns + `"}}`,
		"other kind":      `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"cfg","namespace":"` + ns + `"}}`,
		"other group":     `{"apiVersion":"example.com/v1","kind":"ConfigMap","metadata":{"name":"cfg","namespace":"` + ns + `"}}`,
		"no namespace":    `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cfg"}}`,
	} {
		_, err = reader.ApplicationServerSideDiff(re.ctx, &apiclient.ApplicationServerSideDiffRequest{
			Name:            "guestbook",
			LiveResources:   []apiclient.ResourceDiff{{Kind: "ConfigMap", Namespace: ns, Name: "cfg", LiveState: cfgLive}},
			TargetManifests: []string{target},
		})
		requireProblem(t, err, 400, "target manifest 0 (")
		require.ErrorContains(t, err, "does not match live resource /ConfigMap "+ns+"/cfg", name)
	}
	_, err = reader.ApplicationServerSideDiff(re.ctx, &apiclient.ApplicationServerSideDiffRequest{
		Name:            "guestbook",
		LiveResources:   []apiclient.ResourceDiff{{Kind: "ConfigMap", Namespace: ns, Name: "cfg", LiveState: cfgLive}},
		TargetManifests: []string{"{not json"},
	})
	requireProblem(t, err, 400, "error unmarshaling target manifest 0")
	assert.Equal(t, calls, re.state.ssdCalls.Load(), "refused pairs never reach the controller's dry run")
	// The matching pair, and a target without a live state (no dry run), are diffed.
	res, err = reader.ApplicationServerSideDiff(re.ctx, &apiclient.ApplicationServerSideDiffRequest{
		Name: "guestbook",
		LiveResources: []apiclient.ResourceDiff{
			{Kind: "ConfigMap", Namespace: ns, Name: "cfg", LiveState: cfgLive},
			{Kind: "ConfigMap", Namespace: ns, Name: "new"},
		},
		TargetManifests: []string{strings.Replace(cfgLive, `"k":"v"`, `"k":"w"`, 1), cmTarget},
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 2)
	assert.True(t, res.Modified)
	assert.Equal(t, calls+1, re.state.ssdCalls.Load())
	_, err = re.rclient("bob").ApplicationServerSideDiff(re.ctx, &apiclient.ApplicationServerSideDiffRequest{Name: "guestbook"})
	requireProblem(t, err, 403, "permission denied")
}
