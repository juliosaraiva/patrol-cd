package app

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
)

func TestIndexKeys(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Server: "https://k8s", Namespace: "web"}}}
	assert.Equal(t, []string{"web"}, destinationNamespaceKeys(app))
	assert.Nil(t, destinationNamespaceKeys(&v1alpha1.Application{}), "no destination namespace")
	assert.Nil(t, destinationNamespaceKeys(&v1alpha1.AppProject{}), "not an Application")

	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	assert.Nil(t, orphanMonitoringKeys(proj), "monitoring off")
	proj.Spec.OrphanedResources = &v1alpha1.OrphanedResourcesMonitorSettings{}
	assert.Equal(t, []string{orphanMonitoringKey}, orphanMonitoringKeys(proj), "an empty orphanedResources enables monitoring")
	assert.Nil(t, orphanMonitoringKeys(app), "not an AppProject")
}

// An instance name counts as an existing Application unless the read says it does not exist.
func TestApplicationExists(t *testing.T) {
	t.Parallel()
	inControlPlane := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: "patrol", Name: "a"}}
	inTeam := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "b"}}
	exists := ApplicationExists(liveEventsReconciler(t, nil, inControlPlane, inTeam).c, "patrol")
	assert.True(t, exists("a"), "control-plane namespace")
	assert.True(t, exists("team_b"), "another namespace")
	assert.False(t, exists("gone"))
	assert.False(t, exists("team_a"), "the name in another namespace")
	assert.True(t, ApplicationExists(failingReader{}, "patrol")("a"), "a failed read is not a missing Application")
}

// failingReader fails every read.
type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("cache not ready")
}

// An Application this replica stops processing loses its orphaned-resources series: its
// namespace is no longer allowed, or another shard owns its cluster. One with skip-reconcile
// keeps it, as it keeps the rest of its state.
func TestReconcileForgetsOrphanedSeries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		namespace   string
		server      string
		annotations map[string]string
		ownsCluster func(*v1alpha1.Cluster) bool
		want        int
	}{
		{name: "namespace not allowed", namespace: "team", server: testServer},
		{name: "registered cluster of another shard", namespace: "patrol", server: testServer, ownsCluster: func(c *v1alpha1.Cluster) bool { return c == nil }},
		// An unresolvable destination belongs to shard 0, here another shard's.
		{name: "unresolvable destination of another shard", namespace: "patrol", server: "https://unknown", ownsCluster: func(c *v1alpha1.Cluster) bool { return c != nil }},
		{name: "skip-reconcile", namespace: "patrol", server: testServer, annotations: map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"},
			ownsCluster: func(*v1alpha1.Cluster) bool { return false }, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := &v1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{Namespace: tc.namespace, Name: "a", Annotations: tc.annotations},
				Spec:       v1alpha1.ApplicationSpec{Project: "watch", Destination: v1alpha1.ApplicationDestination{Server: tc.server, Namespace: "web"}},
			}
			cluster := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "k8s", Namespace: "patrol"}, Spec: v1alpha1.ClusterSpec{Server: testServer, Name: "k8s"}}
			r := liveEventsReconciler(t, nil, app, cluster)
			reg := prometheus.NewRegistry()
			m, err := core.NewMetrics(reg)
			require.NoError(t, err)
			r.cfg.Metrics = m
			r.cfg.OwnsCluster = tc.ownsCluster
			m.SetOrphanedResources(tc.namespace, "a", "watch", true, 3)

			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(app)})
			require.NoError(t, err)
			n, err := testutil.GatherAndCount(reg, "patrol_app_orphaned_resources_count")
			require.NoError(t, err)
			assert.Equal(t, tc.want, n)
		})
	}
}
