package clusters

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// The registry is the live implementation of internal/project's ClusterLookup.
var _ project.ClusterLookup = (*Registry)(nil)

// TestRegistryAsProjectLookup resolves destinations through internal/project with the registry
// and checks the upstream messages for a disabled in-cluster cluster (REPO-093).
func TestRegistryAsProjectLookup(t *testing.T) {
	t.Parallel()
	env := newFakeEnv(t, []*v1alpha1.Cluster{cluster("prod", "https://prod.example", func(c *v1alpha1.Cluster) { c.Spec.Project = "team" })},
		nil, nil, RegistryOptions{InClusterEnabled: ptr.To(false)})
	ctx := context.Background()
	c, err := project.ResolveDestination(ctx, v1alpha1.ApplicationDestination{Name: "prod"}, env.reg)
	require.NoError(t, err)
	assert.Equal(t, "https://prod.example", c.Spec.Server)
	_, err = project.ResolveDestination(ctx, v1alpha1.ApplicationDestination{Name: InClusterName}, env.reg)
	require.EqualError(t, err, "there are no clusters with this name: in-cluster")
	_, err = project.ResolveDestination(ctx, v1alpha1.ApplicationDestination{Server: InClusterServer}, env.reg)
	require.EqualError(t, err, `error getting cluster by server "https://kubernetes.default.svc": cluster "https://kubernetes.default.svc" is disabled`)
	scoped, err := env.reg.GetProjectClusters(ctx, "team")
	require.NoError(t, err)
	require.Len(t, scoped, 1)
}
