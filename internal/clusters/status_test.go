package clusters

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestComputeClusterInfo covers REPO-094's status mapping.
func TestComputeClusterInfo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	synced := now.Add(-time.Minute)
	tests := []struct {
		name    string
		state   *CacheState
		apps    int64
		status  v1alpha1.ConnectionStatus
		message string
		version string
		cache   bool
	}{
		{name: "not monitored, no apps", apps: 0, status: v1alpha1.ConnectionStatusUnknown, message: NoApplicationsMessage},
		{name: "not monitored, with apps", apps: 2, status: v1alpha1.ConnectionStatusUnknown},
		{name: "never synced", state: &CacheState{ServerVersion: "v1.36.0"}, apps: 1, status: v1alpha1.ConnectionStatusUnknown, version: "v1.36.0"},
		{name: "synced", state: &CacheState{ServerVersion: "v1.36.0", LastCacheSyncTime: &synced, APIsCount: 40, ResourcesCount: 900, APIVersions: []string{"v1", "apps/v1"}},
			apps: 3, status: v1alpha1.ConnectionStatusSuccessful, version: "v1.36.0", cache: true},
		{name: "sync error", state: &CacheState{ServerVersion: "v1.36.0", LastCacheSyncTime: &synced, SyncError: errors.New("dial tcp: i/o timeout")},
			apps: 1, status: v1alpha1.ConnectionStatusFailed, message: "dial tcp: i/o timeout", version: "v1.36.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := ComputeClusterInfo(tc.state, tc.apps, now)
			assert.Equal(t, tc.status, u.ConnectionState.Status)
			assert.Equal(t, tc.message, u.ConnectionState.Message)
			assert.True(t, u.ConnectionState.AttemptedAt.Time.Equal(now))
			assert.Equal(t, tc.version, u.ServerVersion)
			assert.Equal(t, tc.apps, u.Info.ApplicationsCount)
			if tc.cache {
				require.NotNil(t, u.Info.CacheInfo.LastCacheSyncTime)
				assert.True(t, u.Info.CacheInfo.LastCacheSyncTime.Time.Equal(synced))
				assert.Equal(t, int64(40), u.Info.CacheInfo.APIsCount)
				assert.Equal(t, int64(900), u.Info.CacheInfo.ResourcesCount)
				assert.Equal(t, []string{"v1", "apps/v1"}, u.Info.APIVersions)
			} else {
				assert.Nil(t, u.Info.CacheInfo.LastCacheSyncTime)
			}
		})
	}
}

func TestProbeErrors(t *testing.T) {
	t.Parallel()
	_, err := Probe(context.Background(), nil)
	require.Error(t, err)
	srv := newPlainAPIServer(t)
	srv.mu.Lock()
	srv.version = "not-a-version"
	srv.mu.Unlock()
	_, err = probeWith(t, cluster("v", srv.URL), nil, Options{})
	require.ErrorContains(t, err, "failed to parse server version")
	_, err = probeWith(t, cluster("v", "http://127.0.0.1:1"), nil, Options{})
	require.ErrorContains(t, err, "failed to get server version")
}

func TestApplyConnectionConditions(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := cluster("c", "https://c")
	c.Generation = 4
	state, ver := ProbeUpdate("v1.36.0", nil, now)
	applyConnection(c, state, ver, true)
	assert.Equal(t, "v1.36.0", c.Status.ServerVersion)
	assert.Equal(t, int64(4), c.Status.ObservedGeneration)
	ready := meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, v1alpha1.ReasonConnectionSuccessful, ready.Reason)
	assert.Equal(t, int64(4), ready.ObservedGeneration)
	failed := meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionConnectionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, metav1.ConditionFalse, failed.Status)

	// Same state later keeps attemptedAt so periodic writes are no-ops.
	first := c.Status.ConnectionState.AttemptedAt
	state, ver = ProbeUpdate("v1.36.0", nil, now.Add(time.Minute))
	applyConnection(c, state, ver, true)
	assert.Equal(t, first, c.Status.ConnectionState.AttemptedAt)

	state, ver = ProbeUpdate("", errors.New("boom"), now)
	applyConnection(c, state, ver, true)
	assert.Equal(t, "v1.36.0", c.Status.ServerVersion, "a failure keeps the last version")
	assert.Equal(t, metav1.ConditionFalse, meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionReady).Status)
	assert.Equal(t, "boom", meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionReady).Message)
	assert.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionConnectionFailed).Status)

	applyConnection(c, v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusUnknown, Message: NoApplicationsMessage}, "", false)
	assert.Equal(t, metav1.ConditionUnknown, meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionReady).Status)
	assert.Nil(t, meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionConnectionFailed))
}

// TestStatusWriterRetriesConflicts uses a fake dynamic client that rejects the first status
// update with a conflict.
func TestStatusWriterRetriesConflicts(t *testing.T) {
	t.Parallel()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ClusterGVR: "ClusterList"}, toUnstructured(t, cluster("a", "https://a")))
	conflicts := 0
	dyn.PrependReactor("update", "clusters", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" && conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(ClusterGVR.GroupResource(), "a", errors.New("stale"))
		}
		return false, nil, nil
	})
	w := NewStatusWriter(dyn, WithStatusBackoff(wait.Backoff{Steps: 3, Duration: time.Millisecond, Factor: 1}))
	mutations := 0
	out, err := w.Update(context.Background(), "patrol", "a", func(c *v1alpha1.Cluster) error {
		mutations++
		c.Status.ServerVersion = "v1.36.0"
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, mutations, "the object is re-read after a conflict")
	assert.Equal(t, "v1.36.0", out.Status.ServerVersion)

	_, err = w.Update(context.Background(), "patrol", "missing", func(*v1alpha1.Cluster) error { return nil })
	require.True(t, apierrors.IsNotFound(err), "%v", err)
	_, err = w.RecordProbe(context.Background(), &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: InClusterServer}}, "v1", nil)
	require.ErrorIs(t, err, errSynthetic)
}

// TestStatusWriterEnvtest writes probe results, cluster info and the version label to a real
// Cluster object (status subresource of the generated CRD).
func TestStatusWriterEnvtest(t *testing.T) {
	cfg := envConfig(t)
	ctx := context.Background()
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	ns := createNamespace(t, cfg, "status")
	c := cluster("target", "https://target.example", func(c *v1alpha1.Cluster) {
		c.Namespace = ns
		c.Labels = map[string]string{v1alpha1.LabelKeyAutoLabelClusterInfo: "true"}
	})
	u, err := dyn.Resource(ClusterGVR).Namespace(ns).Create(ctx, toUnstructured(t, c), metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, c))

	// Probe a real API server (envtest itself) and record the result.
	ver, err := Probe(ctx, cfg)
	require.NoError(t, err)
	assert.Regexp(t, `^v1\.\d+\.\d+$`, ver)
	w := NewStatusWriter(dyn)
	out, err := w.RecordProbe(ctx, c, ver, nil)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, out.Status.ConnectionState.Status)
	assert.Equal(t, ver, out.Status.ServerVersion)
	assert.Equal(t, out.Generation, out.Status.ObservedGeneration)
	assert.True(t, meta.IsStatusConditionTrue(out.Status.Conditions, v1alpha1.ConditionReady))

	out, err = w.RecordProbe(ctx, out, "", errors.New("connection refused"))
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusFailed, out.Status.ConnectionState.Status)
	assert.Equal(t, "connection refused", out.Status.ConnectionState.Message)
	assert.True(t, meta.IsStatusConditionTrue(out.Status.Conditions, v1alpha1.ConditionConnectionFailed))

	synced := time.Now().Add(-time.Second).Truncate(time.Second)
	out, err = w.RecordClusterInfo(ctx, out, ComputeClusterInfo(&CacheState{
		ServerVersion: ver, LastCacheSyncTime: &synced, APIsCount: 12, ResourcesCount: 34, APIVersions: []string{"v1"},
	}, 5, time.Now()))
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ConnectionStatusSuccessful, out.Status.ConnectionState.Status)
	assert.Equal(t, int64(5), out.Status.Info.ApplicationsCount)
	assert.Equal(t, int64(34), out.Status.Info.CacheInfo.ResourcesCount)
	require.NotNil(t, out.Status.Info.CacheInfo.LastCacheSyncTime)
	assert.True(t, out.Status.Info.CacheInfo.LastCacheSyncTime.Time.Equal(synced))

	// Unchanged status: no write (resourceVersion stays).
	again, err := w.RecordClusterInfo(ctx, out, ComputeClusterInfo(&CacheState{
		ServerVersion: ver, LastCacheSyncTime: &synced, APIsCount: 12, ResourcesCount: 34, APIVersions: []string{"v1"},
	}, 5, time.Now().Add(time.Minute)))
	require.NoError(t, err)
	assert.Equal(t, out.ResourceVersion, again.ResourceVersion)

	// REPO-095: the version label is maintained only for opted-in clusters.
	patched, err := w.RecordVersionLabel(ctx, out, ver)
	require.NoError(t, err)
	assert.True(t, patched)
	got, err := dyn.Resource(ClusterGVR).Namespace(ns).Get(ctx, c.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ver, got.GetLabels()[v1alpha1.LabelKeyClusterKubernetesVersion])
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(got.Object, out))
	patched, err = w.RecordVersionLabel(ctx, out, ver)
	require.NoError(t, err)
	assert.False(t, patched, "unchanged version")
}
