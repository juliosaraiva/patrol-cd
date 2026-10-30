package applications_test

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// gatedTree passes tree reads to the real tree API client or, while blocked, holds them
// until their context ends (a controller that does not answer). It counts Tree calls.
type gatedTree struct {
	applications.TreeClient
	block atomic.Bool
	calls atomic.Int64
}

func (g *gatedTree) Tree(ctx context.Context, app *v1alpha1.Application) (*compare.ApplicationTree, error) {
	g.calls.Add(1)
	if g.block.Load() {
		<-ctx.Done()
		return nil, fmt.Errorf("treeapi client: %w", ctx.Err())
	}
	return g.TreeClient.Tree(ctx, app)
}

// nextFor returns the next watch event of Application name, skipping the others.
func nextFor(t *testing.T, ch <-chan watchResult, name string) apiclient.ApplicationWatchEvent {
	t.Helper()
	for {
		if ev := next(t, ch); ev.Object.Name == name {
			return ev
		}
	}
}

// With controller.resource.health.persist=false (resourceHealthSource appTree) the stored
// Application has no resource health; Get (with and without refresh), Watch, Sync, Rollback
// and Terminate responses fill it from the controller's resource tree (upstream
// inferResourcesStatusHealth), List and DELETED watch events do not, inline health is
// returned as stored, and a tree that cannot be read leaves the health empty without failing
// the request, triggering a refresh or, on a watch stream whose controller did not answer,
// being asked again for resourceHealthPause.
func TestApplicationResourceHealthFromTree(t *testing.T) {
	gate := &gatedTree{}
	var skew atomic.Int64 // the service's clock runs this far ahead
	re := newResEnv(t, resEnvOptions{
		resources: func(rd *applications.ResourceDeps) {
			gate.TreeClient = rd.Tree
			rd.Tree = gate
			rd.ResourceHealthTimeout = time.Second
		},
		now: func() time.Time { return time.Now().Add(time.Duration(skew.Load())) },
	})
	ns := re.appNS
	alice := re.rclient("alice")

	app := re.newApp("health", "proj-a")
	app.Spec.Source.TargetRevision = sha // a commit: syncs resolve no revision (no repositories here)
	app.Spec.Destination.Namespace = ns
	app = re.createApp(app)
	app.Status.ResourceHealthSource = v1alpha1.ResourceHealthLocationAppTree
	app.Status.Resources = []v1alpha1.ResourceStatus{
		{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: ns, Name: "web", Status: v1alpha1.SyncStatusCodeSynced},
		{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "cfg", Status: v1alpha1.SyncStatusCodeSynced},
		{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "pending", Status: v1alpha1.SyncStatusCodeOutOfSync},
		// Only an orphaned node of the tree has this key: orphans do not count.
		{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "stray", Status: v1alpha1.SyncStatusCodeSynced},
		{Version: "v1", Kind: "Secret", Namespace: ns, Name: "creds", Status: v1alpha1.SyncStatusCodeSynced},
	}
	app.Status.History = v1alpha1.RevisionHistories{{ID: 0, Revision: sha, DeployedAt: metav1.Now(), Source: app.Spec.Source.DeepCopy()}}
	require.NoError(t, re.kube.Status().Update(re.ctx, app))

	key := treeapi.AppKey{Namespace: app.Namespace, Name: app.Name}
	created := metav1.Now()
	setTree := func(cfg v1alpha1.HealthStatus) {
		re.state.setTree(key, &compare.ApplicationTree{
			Nodes: []compare.ResourceNode{
				{ResourceRef: compare.ResourceRef{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: ns, Name: "web", UID: "u-web"},
					Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}, CreatedAt: &created},
				{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "cfg", UID: "u-cfg"}, ResourceVersion: "7", Health: &cfg},
				// Never created: no health, UID, resourceVersion or creation time.
				{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "pending"}},
				// A live object without health.
				{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "Secret", Namespace: ns, Name: "creds", UID: "u-creds"}},
			},
			OrphanedNodes: []compare.ResourceNode{
				{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "stray", UID: "u-stray"},
					Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}},
			},
		})
	}
	degraded := v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusDegraded, Message: "bad"}
	healthy := v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}
	setTree(degraded)

	healthOf := func(a *v1alpha1.Application) map[string]*v1alpha1.HealthStatus {
		out := map[string]*v1alpha1.HealthStatus{}
		for _, r := range a.Status.Resources {
			out[r.Kind+"/"+r.Name] = r.Health
		}
		return out
	}
	requireFilled := func(t *testing.T, a *v1alpha1.Application, cfg v1alpha1.HealthStatus) {
		t.Helper()
		require.NotNil(t, a)
		assert.Equal(t, map[string]*v1alpha1.HealthStatus{
			"Deployment/web":    {Status: v1alpha1.HealthStatusHealthy},
			"ConfigMap/cfg":     &cfg,
			"ConfigMap/pending": {Status: v1alpha1.HealthStatusMissing, Message: "Resource has not been created"},
			"ConfigMap/stray":   nil,
			"Secret/creds":      nil,
		}, healthOf(a))
	}
	requireEmpty := func(t *testing.T, a *v1alpha1.Application) {
		t.Helper()
		require.NotNil(t, a)
		require.Len(t, a.Status.Resources, 5)
		for _, r := range a.Status.Resources {
			assert.Nil(t, r.Health, "%s/%s", r.Kind, r.Name)
		}
	}

	t.Run("get", func(t *testing.T) {
		got, err := alice.GetApplication(re.ctx, &apiclient.ApplicationGetRequest{Name: "health"})
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.ResourceHealthLocationAppTree, got.Status.ResourceHealthSource)
		requireFilled(t, got, degraded)
		// Nothing is written: the stored Application keeps no resource health.
		requireEmpty(t, re.getApp(re.ns, "health"))
	})

	t.Run("refresh", func(t *testing.T) {
		// Stand-in controller: clears the refresh annotation.
		ctx, cancel := context.WithCancel(re.ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for ctx.Err() == nil {
				cur := &v1alpha1.Application{}
				if re.kube.Get(ctx, client.ObjectKeyFromObject(app), cur) == nil {
					if _, ok := cur.Annotations[v1alpha1.AnnotationKeyRefresh]; ok {
						delete(cur.Annotations, v1alpha1.AnnotationKeyRefresh)
						_ = re.kube.Update(ctx, cur)
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
		got, err := alice.GetApplication(re.ctx, &apiclient.ApplicationGetRequest{Name: "health", Refresh: "normal"})
		cancel()
		<-done
		require.NoError(t, err)
		requireFilled(t, got, degraded)
	})

	t.Run("list", func(t *testing.T) {
		// Upstream List does not fill resource health.
		list, err := alice.ListApplications(re.ctx, &apiclient.ApplicationQuery{Name: "health"})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		requireEmpty(t, &list.Items[0])
	})

	t.Run("watch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(re.ctx)
		defer cancel()
		named, err := alice.WatchApplications(ctx, &apiclient.ApplicationQuery{Name: "health"}, nil)
		require.NoError(t, err)
		namedCh := watchEvents(t, named)
		all, err := alice.WatchApplications(ctx, &apiclient.ApplicationQuery{}, nil)
		require.NoError(t, err)
		allCh := watchEvents(t, all)

		ev := next(t, namedCh)
		assert.Equal(t, apiclient.EventAdded, ev.Type)
		requireFilled(t, &ev.Object, degraded)
		ev = nextFor(t, allCh, "health")
		assert.Equal(t, apiclient.EventAdded, ev.Type)
		requireFilled(t, &ev.Object, degraded)

		// The controller's tree changes, then the Application: both streams fill the event
		// (from one Application object shared by the subscribers: -race catches a fill that
		// writes into it).
		setTree(healthy)
		re.updateApp("health", func(a *v1alpha1.Application) { a.Annotations = map[string]string{"step": "watch"} })
		for _, ch := range []<-chan watchResult{namedCh, allCh} {
			ev = nextFor(t, ch, "health")
			assert.Equal(t, apiclient.EventModified, ev.Type)
			assert.Equal(t, "watch", ev.Object.Annotations["step"])
			requireFilled(t, &ev.Object, healthy)
		}
	})

	t.Run("sync", func(t *testing.T) {
		setTree(healthy)
		resp, err := alice.SyncApplication(re.ctx, &apiclient.ApplicationSyncRequest{Name: "health"})
		require.NoError(t, err)
		require.NotNil(t, resp.Operation)
		requireFilled(t, resp.Application, healthy)
		re.finishOperations(re.ns, "health")
	})

	t.Run("rollback", func(t *testing.T) {
		setTree(healthy)
		resp, err := alice.RollbackApplication(re.ctx, &apiclient.ApplicationRollbackRequest{Name: "health", ID: 0})
		require.NoError(t, err)
		require.NotNil(t, resp.Operation)
		requireFilled(t, resp.Application, healthy)
		re.finishOperations(re.ns, "health")
	})

	t.Run("terminate", func(t *testing.T) {
		setTree(healthy)
		_, err := alice.SyncApplication(re.ctx, &apiclient.ApplicationSyncRequest{Name: "health"})
		require.NoError(t, err)
		resp, err := alice.TerminateApplicationOperation(re.ctx, &apiclient.ApplicationRef{Name: "health"})
		require.NoError(t, err)
		require.NotNil(t, resp.Operation)
		requireFilled(t, resp.Application, healthy)
		re.finishOperations(re.ns, "health")
	})

	t.Run("inline", func(t *testing.T) {
		// controller.resource.health.persist=true: health is stored inline and returned as
		// stored, without a tree read.
		inline := re.createApp(re.newApp("inline", "proj-a"))
		inline.Status.Resources = []v1alpha1.ResourceStatus{{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: ns, Name: "web",
			Status: v1alpha1.SyncStatusCodeSynced, Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusProgressing, Message: "rolling"}}}
		require.NoError(t, re.kube.Status().Update(re.ctx, inline))
		before := gate.calls.Load()
		got, err := alice.GetApplication(re.ctx, &apiclient.ApplicationGetRequest{Name: "inline"})
		require.NoError(t, err)
		require.Len(t, got.Status.Resources, 1)
		assert.Equal(t, &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusProgressing, Message: "rolling"}, got.Status.Resources[0].Health)
		assert.Equal(t, before, gate.calls.Load(), "no tree read")
	})

	t.Run("unavailable", func(t *testing.T) {
		// The controller has no state for the Application: the health stays empty, the
		// request succeeds and no refresh is requested.
		re.state.setMissing(key, true)
		defer re.state.setMissing(key, false)
		before := re.state.calls.Load()
		got, err := alice.GetApplication(re.ctx, &apiclient.ApplicationGetRequest{Name: "health"})
		require.NoError(t, err)
		requireEmpty(t, got)
		assert.Equal(t, before+1, re.state.calls.Load(), "one tree read")
		assert.NotContains(t, re.getApp(re.ns, "health").Annotations, v1alpha1.AnnotationKeyRefresh)
	})

	t.Run("slow controller", func(t *testing.T) {
		// One watch stream: reads with an answer from the controller do not pause it, a
		// controller that does not answer pauses it for resourceHealthPause, and it reads
		// again once the pause has passed.
		defer gate.block.Store(false)
		defer re.state.setMissing(key, false)
		// The controller answers that it has no state: every event asks it again.
		re.state.setMissing(key, true)
		before := gate.calls.Load()
		ctx, cancel := context.WithCancel(re.ctx)
		defer cancel()
		s, err := alice.WatchApplications(ctx, &apiclient.ApplicationQuery{Name: "health"}, nil)
		require.NoError(t, err)
		ch := watchEvents(t, s)
		// until reads events up to the one annotated slow=v; it returns that event and the
		// number of events read.
		until := func(v string, check func(*v1alpha1.Application)) (apiclient.ApplicationWatchEvent, int64) {
			t.Helper()
			for n := int64(1); ; n++ {
				ev := next(t, ch)
				require.NotEqual(t, apiclient.EventDeleted, ev.Type)
				check(&ev.Object)
				if ev.Object.Annotations["slow"] == v {
					return ev, n
				}
			}
		}
		empty := func(a *v1alpha1.Application) { requireEmpty(t, a) }

		// The stream's first event (its List) comes before any write.
		ev := next(t, ch)
		require.Equal(t, apiclient.EventAdded, ev.Type)
		requireEmpty(t, &ev.Object)
		re.updateApp("health", func(a *v1alpha1.Application) { a.Annotations = map[string]string{"slow": "answer"} })
		_, events := until("answer", empty)
		assert.Equal(t, 1+events, gate.calls.Load()-before, "one tree read per event")
		re.state.setMissing(key, false)

		// The controller does not answer: the first read waits for ResourceHealthTimeout, then
		// the stream stops asking.
		gate.block.Store(true)
		before = gate.calls.Load()
		const updates = 5
		for i := 1; i <= updates; i++ {
			re.updateApp("health", func(a *v1alpha1.Application) { a.Annotations = map[string]string{"slow": strconv.Itoa(i)} })
		}
		until(strconv.Itoa(updates), empty)
		assert.Equal(t, int64(1), gate.calls.Load()-before, "tree reads during the pause")

		// Past resourceHealthPause (30 s) the stream reads again.
		gate.block.Store(false)
		setTree(healthy)
		skew.Add(int64(31 * time.Second))
		before = gate.calls.Load()
		re.updateApp("health", func(a *v1alpha1.Application) { a.Annotations = map[string]string{"slow": "after"} })
		ev, _ = until("after", func(*v1alpha1.Application) {})
		requireFilled(t, &ev.Object, healthy)
		assert.Equal(t, int64(1), gate.calls.Load()-before, "tree reads after the pause")
	})

	t.Run("watch deleted", func(t *testing.T) {
		// The controller drops the state of a deleted Application: its DELETED event is sent
		// as stored, without a tree read, although the tree here still has the Application.
		gone := re.createApp(re.newApp("gone", "proj-a"))
		gone.Status.ResourceHealthSource = v1alpha1.ResourceHealthLocationAppTree
		gone.Status.Resources = []v1alpha1.ResourceStatus{{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "cfg", Status: v1alpha1.SyncStatusCodeSynced}}
		require.NoError(t, re.kube.Status().Update(re.ctx, gone))
		re.state.setTree(treeapi.AppKey{Namespace: gone.Namespace, Name: gone.Name}, &compare.ApplicationTree{
			Nodes: []compare.ResourceNode{{ResourceRef: compare.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: ns, Name: "cfg", UID: "u-gone-cfg"}, Health: &healthy}},
		})

		ctx, cancel := context.WithCancel(re.ctx)
		defer cancel()
		s, err := alice.WatchApplications(ctx, &apiclient.ApplicationQuery{Name: "gone"}, nil)
		require.NoError(t, err)
		ch := watchEvents(t, s)
		// The stored state with its resources (the first event may predate the status update)
		// is filled from the tree.
		ev := next(t, ch)
		for len(ev.Object.Status.Resources) == 0 {
			ev = next(t, ch)
		}
		assert.NotEqual(t, apiclient.EventDeleted, ev.Type)
		require.Len(t, ev.Object.Status.Resources, 1)
		assert.Equal(t, &healthy, ev.Object.Status.Resources[0].Health)

		before := gate.calls.Load()
		require.NoError(t, re.kube.Delete(re.ctx, re.getApp(re.ns, "gone")))
		ev = next(t, ch)
		assert.Equal(t, apiclient.EventDeleted, ev.Type)
		require.Len(t, ev.Object.Status.Resources, 1)
		assert.Nil(t, ev.Object.Status.Resources[0].Health)
		assert.Equal(t, before, gate.calls.Load(), "no tree read for the DELETED event")
	})
}
