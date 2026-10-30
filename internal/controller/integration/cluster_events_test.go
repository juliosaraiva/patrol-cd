package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestClusterEventsRefreshApplications: the cluster-info updater writes Cluster.status on its own
// schedule, and a write must not cost every Application of the cluster a comparison. A change of the
// Cluster's labels, which generators and templates can read, still compares them.
func TestClusterEventsRefreshApplications(t *testing.T) {
	e := newEnv(t)
	e.startController()
	e.repo.commit(e.repoURL, "ce", "main", configMap("ce-cm", map[string]string{"k": "v"}, nil))
	a := e.newApp("ce")
	e.create(a)
	e.waitApp(a, "compared", outOfSync)
	// reconciledAt has whole seconds: a later comparison must be able to move it.
	time.Sleep(1500 * time.Millisecond)
	before := e.getApp(a)
	require.NotNil(t, before.Status.ReconciledAt)

	key := client.ObjectKey{Namespace: e.ns, Name: "dest"}
	c := &v1alpha1.Cluster{}
	require.NoError(t, e.c.Get(e.ctx, key, c))
	now := metav1.Now()
	c.Status.ConnectionState = &v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusSuccessful, AttemptedAt: &now}
	require.NoError(t, e.c.Status().Update(e.ctx, c))
	time.Sleep(3 * time.Second)
	assert.True(t, e.getApp(a).Status.ReconciledAt.Equal(before.Status.ReconciledAt), "a status write compares nothing")

	require.NoError(t, e.c.Get(e.ctx, key, c))
	c.Labels = map[string]string{"team": "payments"}
	require.NoError(t, e.c.Update(e.ctx, c))
	e.waitApp(a, "compared after a label change", func(a *v1alpha1.Application) bool {
		return a.Status.ReconciledAt.After(before.Status.ReconciledAt.Time)
	})
}
