package integration

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/server"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
	"github.com/juliosaraiva/patrol-cd/test/integration/stack"
)

// fastClusterInfo shortens the time a test waits for the cluster-info updater.
func fastClusterInfo(c *server.ControllerConfig) { c.ClusterInfoInterval = time.Second }

// waitCluster waits until the API reports a cluster that cond accepts.
func waitCluster(t *testing.T, s *stack.Stack, d *stack.Cluster, what string, cond func(*apiclient.RegisteredCluster) bool) *apiclient.RegisteredCluster {
	t.Helper()
	var last *apiclient.RegisteredCluster
	stack.Eventually(t, time.Minute, what, func() (bool, string) {
		c, err := s.API(t).GetCluster(t.Context(), apiclient.ClusterByServer(d.Server))
		if err != nil {
			return false, err.Error()
		}
		last = c
		return cond(c), fmt.Sprintf("labels=%v status=%+v connection=%+v", c.Metadata.Labels, c.Status, c.Status.ConnectionState)
	})
	return last
}

func connectionIs(c *apiclient.RegisteredCluster, status v1alpha1.ConnectionStatus) bool {
	return c.Status.ConnectionState != nil && c.Status.ConnectionState.Status == status
}

// TestClusterInfoWrittenByController: RunController keeps the status of the clusters of its shards,
// as the API and the CLI show it.
func TestClusterInfoWrittenByController(t *testing.T) {
	s := stack.Start(t, stack.Options{DedicatedDestinations: 2, Controller: fastClusterInfo})
	used, idle := s.Destination(2), s.Destination(3)
	ns := s.NewNamespace(t, used, "clusterinfo")
	repo := s.Git().NewRepo(t, "clusterinfo")
	repo.CommitFiles(t, "v1", map[string]string{"app/cm.yaml": configMap("cm", map[string]string{"v": "1"}, nil)})
	s.CreateApp(t, s.NewApp("by-server", repo, "app", used, ns))
	byName := s.NewApp("by-name", repo, "app", used, ns)
	byName.Spec.Destination.Server, byName.Spec.Destination.Name = "", used.Name
	s.CreateApp(t, byName)

	c := waitCluster(t, s, used, "the status of a cluster with Applications", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusSuccessful) && c.Status.Info.ApplicationsCount == 2
	})
	assert.NotEmpty(t, c.Status.ServerVersion)
	assert.Contains(t, c.Status.Info.APIVersions, "v1/ConfigMap")
	assert.Contains(t, c.Status.Info.APIVersions, "apps/v1/Deployment")
	assert.Positive(t, c.Status.Info.CacheInfo.APIsCount)
	assert.Positive(t, c.Status.Info.CacheInfo.ResourcesCount)
	assert.NotNil(t, c.Status.Info.CacheInfo.LastCacheSyncTime)
	assert.Empty(t, c.Metadata.Labels[v1alpha1.LabelKeyClusterKubernetesVersion], "the version label is opt-in")

	unused := waitCluster(t, s, idle, "the status of a cluster without Applications", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusUnknown)
	})
	assert.Equal(t, clusters.NoApplicationsMessage, unused.Status.ConnectionState.Message)
	assert.Empty(t, unused.Status.ServerVersion)

	_, err := s.API(t).UpdateCluster(t.Context(), apiclient.ClusterByServer(used.Server), &apiclient.ClusterInput{
		Server: used.Server,
		Labels: map[string]string{v1alpha1.LabelKeyAutoLabelClusterInfo: "true"},
	}, "labels")
	require.NoError(t, err)
	waitCluster(t, s, used, "the version label of a cluster that opted in", func(l *apiclient.RegisteredCluster) bool {
		return l.Metadata.Labels[v1alpha1.LabelKeyClusterKubernetesVersion] == c.Status.ServerVersion
	})

	_, err = s.API(t).UpdateCluster(t.Context(), apiclient.ClusterByServer(used.Server), &apiclient.ClusterInput{
		Server: used.Server,
		Labels: map[string]string{v1alpha1.LabelKeyAutoLabelClusterInfo: "true", v1alpha1.LabelKeyClusterKubernetesVersion: "v0.0.0"},
	}, "labels")
	require.NoError(t, err)
	waitCluster(t, s, used, "a stale version label replaced", func(l *apiclient.RegisteredCluster) bool {
		return l.Metadata.Labels[v1alpha1.LabelKeyClusterKubernetesVersion] == c.Status.ServerVersion
	})

	row := func(d *stack.Cluster) string {
		for line := range strings.Lines(s.CLI(t, "cluster", "list").Stdout) {
			if strings.HasPrefix(line, d.Server+" ") {
				return line
			}
		}
		return ""
	}
	assert.Contains(t, row(used), c.Status.ServerVersion)
	assert.Contains(t, row(used), string(v1alpha1.ConnectionStatusSuccessful))
	assert.Contains(t, row(idle), string(v1alpha1.ConnectionStatusUnknown))
}

// TestManifestsMatchControllerCapabilities: GET .../manifests renders with the API versions of the
// Cluster status, so a chart gated on an API of the destination gives what the controller applied.
func TestManifestsMatchControllerCapabilities(t *testing.T) {
	s := stack.Start(t, stack.Options{DedicatedDestinations: 1, Controller: fastClusterInfo})
	d := s.Destination(2)
	ns := s.NewNamespace(t, d, "capabilities")
	d.Apply(t, "", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.patrol-test.example.com
spec:
  group: patrol-test.example.com
  scope: Namespaced
  names: {kind: Widget, plural: widgets, singular: widget}
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
`)
	const widgets = "patrol-test.example.com/v1/Widget"
	repo := s.Git().NewRepo(t, "capabilities")
	rev := repo.CommitFiles(t, "v1", map[string]string{
		"chart/Chart.yaml": "apiVersion: v2\nname: capabilities\nversion: 0.1.0\n",
		"chart/templates/gated.yaml": `{{- if .Capabilities.APIVersions.Has "` + widgets + `" }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: gated
{{- end }}
`,
		"chart/templates/always.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: always\n",
	})
	s.CreateApp(t, s.NewApp("capabilities", repo, "chart", d, ns))
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "capabilities"}))
	s.WaitSyncedHealthy(t, "capabilities", rev)
	require.True(t, d.Exists(t, "v1", "ConfigMap", ns, "gated"), "the controller rendered with the destination's APIs")

	waitCluster(t, s, d, "the status lists the destination's API", func(c *apiclient.RegisteredCluster) bool {
		return slices.Contains(c.Status.Info.APIVersions, widgets)
	})
	got, err := s.API(t).GetApplicationManifests(t.Context(), &apiclient.ApplicationManifestQuery{Name: "capabilities"})
	require.NoError(t, err)
	assert.Contains(t, strings.Join(got.Manifests, "\n"), `"gated"`)
}
