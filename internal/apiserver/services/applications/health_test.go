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
// Application has no resource health; Get (with and without refresh), Watch, Sync and
// Rollback responses fill it from the controller's resource tree (upstream
// inferResourcesStatusHealth), List does not, inline health is returned as stored, and a
// tree that cannot be read leaves the health empty without failing the request, triggering
// a refresh or, on a watch stream, being asked again for resourceHealthPause.
func TestApplicationResourceHealthFromTree(t *testing.T) {
	gate := &gatedTree{}
	re := newResEnv(t, resEnvOptions{resources: func(rd *applications.ResourceDeps) {
		gate.TreeClient = rd.Tree
		rd.Tree = gate
		rd.ResourceHealthTimeout = time.Second
	}})
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
		resp, err := alice.SyncApplication(re.ctx, &apiclient.ApplicationSyncRequest{Name: "health"})
		require.NoError(t, err)
		require.NotNil(t, resp.Operation)
		requireFilled(t, resp.Application, healthy)
		re.finishOperations(re.ns, "health")
	})

	t.Run("rollback", func(t *testing.T) {
		resp, err := alice.RollbackApplication(re.ctx, &apiclient.ApplicationRollbackRequest{Name: "health", ID: 0})
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
		// A controller that does not answer: the first read of a watch stream waits for
		// ResourceHealthTimeout, then the stream stops asking (resourceHealthPause).
		gate.block.Store(true)
		defer gate.block.Store(false)
		before := gate.calls.Load()
		ctx, cancel := context.WithCancel(re.ctx)
		defer cancel()
		s, err := alice.WatchApplications(ctx, &apiclient.ApplicationQuery{}, nil)
		require.NoError(t, err)
		ch := watchEvents(t, s)
		ev := nextFor(t, ch, "health")
		assert.Equal(t, apiclient.EventAdded, ev.Type)
		requireEmpty(t, &ev.Object)

		const updates = 5
		for i := 1; i <= updates; i++ {
			re.updateApp("health", func(a *v1alpha1.Application) { a.Annotations = map[string]string{"slow": strconv.Itoa(i)} })
		}
		for {
			ev = nextFor(t, ch, "health")
			assert.Equal(t, apiclient.EventModified, ev.Type)
			requireEmpty(t, &ev.Object)
			if ev.Object.Annotations["slow"] == strconv.Itoa(updates) {
				break
			}
		}
		assert.Equal(t, int64(1), gate.calls.Load()-before, "tree reads of the stream")
	})
}
