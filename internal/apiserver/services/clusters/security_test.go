package clusters_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// localConnector returns a connector whose in-cluster configuration is the fake cluster
// *local (patrol-server's own credentials in production).
func localConnector(local **fakeCluster) func(*testEnv) clusters.Connector {
	return func(*testEnv) clusters.Connector {
		return clusters.NewConnector(clusters.ConnectorOptions{Timeout: 5 * time.Second, Options: clusterlib.Options{
			InClusterConfig: func() (*rest.Config, error) {
				l := *local
				return &rest.Config{Host: l.srv.URL, BearerToken: goodToken, TLSClientConfig: rest.TLSClientConfig{CAData: l.caPEM}}, nil
			},
		}})
	}
}

// TestClusterInClusterRegistration: registering the in-cluster server replaces the implicit
// in-cluster cluster for every project, so it needs update on its unscoped object; the
// create permission of a project (or of every object) is not enough (review finding:
// project-scoped takeover of in-cluster with patrol-server's own credentials).
func TestClusterInClusterRegistration(t *testing.T) {
	var local *fakeCluster
	e := newEnv(t, envOptions{connector: localConnector(&local)})
	local = newFakeCluster(t, e.cfg, e.clientset, "")
	local.addToken(goodToken)
	e.createProject("team-a")
	ctx := context.Background()
	admin, bob, henry := e.client("admin"), e.client("bob"), e.client("henry")

	takeover := &apiclient.ClusterInput{Server: inCluster, Name: "bobs", Project: "team-a", Namespaces: []string{"team-a-ns"}}
	for _, upsert := range []bool{false, true} {
		// bob (clusters, *, team-a/*) cannot see the implicit cluster: it exists, nothing more.
		_, err := bob.CreateCluster(ctx, takeover, upsert)
		ae := apiErr(t, err)
		assert.Equal(t, http.StatusConflict, ae.StatusCode)
		assert.Equal(t, `cluster "https://kubernetes.default.svc" already exists`, ae.Problem.Detail)
		// henry may create any cluster object and get in-cluster, but not update it.
		_, err = henry.CreateCluster(ctx, takeover, upsert)
		ae = apiErr(t, err)
		assert.Equal(t, http.StatusForbidden, ae.StatusCode)
		_, err = henry.CreateCluster(ctx, &apiclient.ClusterInput{Server: inCluster, Name: "local"}, upsert)
		assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	}
	assert.Nil(t, e.clusterObject(inCluster), "no object registers the in-cluster server")
	l, err := admin.ListClusters(ctx, &apiclient.ListClustersRequest{Server: inCluster})
	require.NoError(t, err)
	require.Len(t, l.Items, 1)
	assert.True(t, l.Items[0].InCluster)
	assert.Equal(t, "in-cluster", l.Items[0].Spec.Name)
	assert.Empty(t, l.Items[0].Spec.Project)
	assert.Empty(t, l.Items[0].Spec.Namespaces)

	// A request identical to the implicit cluster returns it without creating an object.
	same, err := henry.CreateCluster(ctx, &apiclient.ClusterInput{Server: inCluster, Name: "in-cluster"}, false)
	require.NoError(t, err)
	assert.True(t, same.InCluster)
	assert.Nil(t, e.clusterObject(inCluster))

	// The admin (update on the unscoped object) registers it, upsert or not (upstream
	// creates its Secret for a plain create).
	got, err := admin.CreateCluster(ctx, &apiclient.ClusterInput{Server: inCluster, Name: "local", Namespaces: []string{"apps"}}, false)
	require.NoError(t, err)
	assert.False(t, got.InCluster)
	assert.Equal(t, "local", got.Spec.Name)
	obj := e.clusterObject(inCluster)
	require.NotNil(t, obj)
	assert.Equal(t, []string{"apps"}, obj.Spec.Namespaces)
	// An object now exists: bob still only learns that.
	_, err = bob.CreateCluster(ctx, takeover, true)
	assert.Equal(t, http.StatusConflict, apiErr(t, err).StatusCode)
}

// TestClusterAmbientAuthGate: awsAuthConfig and execProviderConfig make Patrol components
// authenticate with their own identity or run a command, toward a server URL chosen by the
// caller; project-scoped grants cannot store them (review finding).
func TestClusterAmbientAuthGate(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.createProject("team-a")
	ctx := context.Background()
	admin, bob := e.client("admin"), e.client("bob")

	execCfg := apiclient.ClusterConfigInput{ExecProviderConfig: &v1alpha1.ExecProviderConfig{
		Command: "/bin/sh", Args: []string{"-c", "id"}, APIVersion: "client.authentication.k8s.io/v1beta1",
	}}
	awsCfg := apiclient.ClusterConfigInput{AWSAuthConfig: &v1alpha1.AWSAuthConfig{ClusterName: "victim-prod-eks"}}
	gcpCfg := apiclient.ClusterConfigInput{ExecProviderConfig: &v1alpha1.ExecProviderConfig{
		Command: "argocd-k8s-auth", Args: []string{"gcp"}, APIVersion: "client.authentication.k8s.io/v1beta1",
	}}
	for name, cfg := range map[string]apiclient.ClusterConfigInput{"exec": execCfg, "aws": awsCfg, "gcp": gcpCfg} {
		server := "https://" + name + ".attacker.example.com"
		for _, upsert := range []bool{false, true} {
			_, err := bob.CreateCluster(ctx, &apiclient.ClusterInput{Server: server, Project: "team-a", Config: cfg}, upsert)
			ae := apiErr(t, err)
			assert.Equal(t, http.StatusForbidden, ae.StatusCode, name)
			assert.True(t, strings.HasPrefix(ae.Problem.Detail, "awsAuthConfig and execProviderConfig need clusters, create on the server outside any project: permission denied: clusters, create, "+server+", sub: bob"), ae.Problem.Detail)
		}
		assert.Nil(t, e.clusterObject(server), name)
	}

	// A global grant may store them; the API server does not probe them (no ambient
	// identity is used for a caller-chosen URL), the controller does.
	eks := "https://eks.example.com"
	created, err := admin.CreateCluster(ctx, &apiclient.ClusterInput{Server: eks, Name: "eks", Project: "team-a", Config: awsCfg}, false)
	require.NoError(t, err)
	require.NotNil(t, created.Spec.Config.AWSAuthConfig)
	obj := e.clusterObject(eks)
	require.NotNil(t, obj)
	assert.Nil(t, obj.Status.ConnectionState, "not probed by the API server")

	// bob (update on team-a clusters) may change other fields of that cluster...
	upd, err := bob.UpdateCluster(ctx, apiclient.ClusterByServer(eks), &apiclient.ClusterInput{Namespaces: []string{"apps"}}, "namespaces")
	require.NoError(t, err)
	assert.Equal(t, []string{"apps"}, upd.Spec.Namespaces)
	require.NotNil(t, upd.Spec.Config.AWSAuthConfig, "config untouched")
	// ...but not write such a configuration, partially or fully.
	_, err = bob.UpdateCluster(ctx, apiclient.ClusterByServer(eks), &apiclient.ClusterInput{Config: execCfg}, "config")
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusForbidden, ae.StatusCode)
	assert.True(t, strings.HasPrefix(ae.Problem.Detail, "awsAuthConfig and execProviderConfig need clusters, update on the server outside any project: permission denied: clusters, update, "+eks), ae.Problem.Detail)
	_, err = bob.UpdateCluster(ctx, apiclient.ClusterByServer(eks), &apiclient.ClusterInput{Name: "eks", Project: "team-a", Config: awsCfg})
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	_, err = bob.CreateCluster(ctx, &apiclient.ClusterInput{Server: eks, Name: "eks2", Project: "team-a", Config: awsCfg}, true)
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	stored := e.clusterObject(eks)
	assert.Equal(t, "eks", stored.Spec.Name)
	assert.Equal(t, "victim-prod-eks", stored.Spec.Config.AWSAuthConfig.ClusterName)
	assert.Nil(t, stored.Spec.Config.ExecProviderConfig)
}

// hookConnector runs the hook installed in onProbe (once) before delegating.
type hookConnector struct {
	inner   clusters.Connector
	onProbe atomic.Pointer[func()]
}

func (h *hookConnector) Probe(ctx context.Context, c *v1alpha1.Cluster, d map[string][]byte) (string, error) {
	if f := h.onProbe.Swap(nil); f != nil {
		(*f)()
	}
	return h.inner.Probe(ctx, c, d)
}

func (h *hookConnector) Kubernetes(ctx context.Context, c *v1alpha1.Cluster, d map[string][]byte) (kubernetes.Interface, error) {
	return h.inner.Kubernetes(ctx, c, d)
}

// TestClusterPartialUpdateConcurrency: a partial update changes only the listed fields of
// the latest stored object, so changes written meanwhile (during the connection check) are
// kept; a cluster moved to another project meanwhile needs update there too (review finding:
// lost update).
func TestClusterPartialUpdateConcurrency(t *testing.T) {
	hc := &hookConnector{inner: clusters.NewConnector(clusters.ConnectorOptions{Timeout: 5 * time.Second})}
	e := newEnv(t, envOptions{connector: func(*testEnv) clusters.Connector { return hc }})
	e.createProject("team-a")
	e.createProject("team-b")
	f := newFakeCluster(t, e.cfg, e.clientset, "")
	f.addToken(goodToken)
	ctx := context.Background()
	admin, bob := e.client("admin"), e.client("bob")
	_, err := bob.CreateCluster(ctx, input(f, "prod", "team-a", goodToken), false)
	require.NoError(t, err)

	concurrently := func(mutate func(c *v1alpha1.Cluster)) {
		var once sync.Once
		fn := func() {
			once.Do(func() {
				obj := e.clusterObject(f.srv.URL)
				if !assert.NotNil(t, obj) {
					return
				}
				mutate(obj)
				assert.NoError(t, e.kube.Update(ctx, obj)) // server goroutine: no FailNow
			})
		}
		hc.onProbe.Store(&fn)
	}
	concurrently(func(c *v1alpha1.Cluster) {
		c.Spec.Name = "renamed-concurrently"
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[v1alpha1.AnnotationKeyRefresh] = "2026-10-16T00:00:00Z"
		c.Labels["owner"] = "ops"
	})
	got, err := bob.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Namespaces: []string{"apps"}}, "namespaces")
	require.NoError(t, err)
	assert.Equal(t, "renamed-concurrently", got.Spec.Name)
	obj := e.clusterObject(f.srv.URL)
	assert.Equal(t, "renamed-concurrently", obj.Spec.Name, "concurrent rename kept")
	assert.Equal(t, []string{"apps"}, obj.Spec.Namespaces)
	assert.Equal(t, "2026-10-16T00:00:00Z", obj.Annotations[v1alpha1.AnnotationKeyRefresh], "concurrent refresh request kept")
	assert.Equal(t, map[string]string{"env": "test", "owner": "ops"}, obj.Labels)
	require.NotNil(t, obj.Status.ConnectionState, "the check matches the stored connection settings")

	// Updating labels replaces them, still keeping the concurrent rename of the name.
	concurrently(func(c *v1alpha1.Cluster) { c.Spec.Name = "second-rename" })
	_, err = bob.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Labels: map[string]string{"tier": "1"}}, "labels")
	require.NoError(t, err)
	obj = e.clusterObject(f.srv.URL)
	assert.Equal(t, "second-rename", obj.Spec.Name)
	assert.Equal(t, map[string]string{"tier": "1"}, obj.Labels)

	// Moved to team-b during the check: bob's update is refused and changes nothing.
	concurrently(func(c *v1alpha1.Cluster) { c.Spec.Project = "team-b" })
	_, err = bob.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Namespaces: []string{"other"}}, "namespaces")
	assert.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)
	obj = e.clusterObject(f.srv.URL)
	assert.Equal(t, "team-b", obj.Spec.Project)
	assert.Equal(t, []string{"apps"}, obj.Spec.Namespaces)

	// A client-supplied resourceVersion that went stale during the check: 409.
	cur, err := admin.GetCluster(ctx, apiclient.ClusterByServer(f.srv.URL))
	require.NoError(t, err)
	concurrently(func(c *v1alpha1.Cluster) { c.Spec.Name = "third-rename" })
	_, err = admin.UpdateCluster(ctx, apiclient.ClusterByServer(f.srv.URL), &apiclient.ClusterInput{Namespaces: []string{"x"}, ResourceVersion: cur.Metadata.ResourceVersion}, "namespaces")
	assert.Equal(t, http.StatusConflict, apiErr(t, err).StatusCode)
	assert.Equal(t, "third-rename", e.clusterObject(f.srv.URL).Spec.Name)
}
