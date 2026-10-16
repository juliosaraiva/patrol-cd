package clusters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	versioninfo "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// NoApplicationsMessage is upstream's connection message for an unmonitored cluster with no
// applications.
const NoApplicationsMessage = "Cluster has no applications and is not being monitored."

// Probe checks connectivity to the cluster behind cfg with GET /version and returns the server
// version normalized to "v<major>.<minor>.<patch>" (vendor suffixes such as "-eks-1" or "+k3s1"
// removed). The request is bounded by ctx.
func Probe(ctx context.Context, cfg *rest.Config) (string, error) {
	if cfg == nil {
		return "", errors.New("probe: nil config")
	}
	dc, err := discovery.NewDiscoveryClientForConfig(rest.CopyConfig(cfg))
	if err != nil {
		return "", fmt.Errorf("failed to create discovery client: %w", err)
	}
	body, err := dc.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("failed to get server version: %w", err)
	}
	var info versioninfo.Info
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("failed to get server version: decode /version: %w", err)
	}
	v, err := version.ParseGeneric(info.GitVersion)
	if err != nil {
		return "", fmt.Errorf("failed to parse server version: %w", err)
	}
	return "v" + v.String(), nil
}

// CacheState is the live-state cache information of a cluster, as reported by the
// controller's cache (internal/cache ClusterInfo plus the served API versions).
type CacheState struct {
	ServerVersion     string
	APIVersions       []string
	APIsCount         int64
	ResourcesCount    int64
	LastCacheSyncTime *time.Time
	SyncError         error
}

// StatusUpdate is the connection state, server version and info derived for a cluster.
type StatusUpdate struct {
	ConnectionState v1alpha1.ConnectionState
	ServerVersion   string
	Info            v1alpha1.ClusterInfo
}

// ComputeClusterInfo maps cache information to the status upstream's cluster-info updater
// produces: without cache information the state is Unknown (with NoApplicationsMessage when no
// application targets the cluster); without a completed sync it is Unknown; a sync error makes
// it Failed with the error; otherwise it is Successful with the cache statistics.
func ComputeClusterInfo(state *CacheState, applications int64, now time.Time) StatusUpdate {
	t := metav1.NewTime(now)
	u := StatusUpdate{
		ConnectionState: v1alpha1.ConnectionState{AttemptedAt: &t},
		Info:            v1alpha1.ClusterInfo{ApplicationsCount: applications},
	}
	if state == nil {
		u.ConnectionState.Status = v1alpha1.ConnectionStatusUnknown
		if applications == 0 {
			u.ConnectionState.Message = NoApplicationsMessage
		}
		return u
	}
	u.ServerVersion = state.ServerVersion
	u.Info.APIVersions = append([]string(nil), state.APIVersions...)
	switch {
	case state.LastCacheSyncTime == nil:
		u.ConnectionState.Status = v1alpha1.ConnectionStatusUnknown
	case state.SyncError == nil:
		u.ConnectionState.Status = v1alpha1.ConnectionStatusSuccessful
		synced := metav1.NewTime(*state.LastCacheSyncTime)
		u.Info.CacheInfo = v1alpha1.ClusterCacheInfo{
			LastCacheSyncTime: &synced,
			APIsCount:         state.APIsCount,
			ResourcesCount:    state.ResourcesCount,
		}
	default:
		u.ConnectionState.Status = v1alpha1.ConnectionStatusFailed
		u.ConnectionState.Message = state.SyncError.Error()
	}
	return u
}

// ProbeUpdate returns the status of a connection check: Successful with the version, or Failed
// with the error message (the version is left unchanged).
func ProbeUpdate(version string, probeErr error, now time.Time) (v1alpha1.ConnectionState, string) {
	t := metav1.NewTime(now)
	if probeErr != nil {
		return v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusFailed, Message: probeErr.Error(), AttemptedAt: &t}, ""
	}
	return v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusSuccessful, AttemptedAt: &t}, version
}

// StatusWriter persists Cluster status through the status subresource. Writes re-read the
// object and retry on conflicts and transient API errors with Backoff. The caller decides when
// to write (after a probe, on the cluster-info interval, ...).
type StatusWriter struct {
	client  dynamic.Interface
	backoff wait.Backoff
	now     func() time.Time
}

// DefaultStatusBackoff retries five times starting at 100ms, doubling, with jitter.
var DefaultStatusBackoff = wait.Backoff{Steps: 5, Duration: 100 * time.Millisecond, Factor: 2, Jitter: 0.1, Cap: 5 * time.Second}

// StatusWriterOption configures a StatusWriter.
type StatusWriterOption func(*StatusWriter)

// WithStatusBackoff replaces DefaultStatusBackoff.
func WithStatusBackoff(b wait.Backoff) StatusWriterOption {
	return func(w *StatusWriter) { w.backoff = b }
}

// WithStatusClock replaces time.Now.
func WithStatusClock(now func() time.Time) StatusWriterOption {
	return func(w *StatusWriter) { w.now = now }
}

// NewStatusWriter returns a writer using dyn.
func NewStatusWriter(dyn dynamic.Interface, opts ...StatusWriterOption) *StatusWriter {
	w := &StatusWriter{client: dyn, backoff: DefaultStatusBackoff, now: time.Now}
	for _, o := range opts {
		o(w)
	}
	return w
}

// errSynthetic is returned for writes to the synthetic in-cluster cluster.
var errSynthetic = errors.New("the synthetic in-cluster cluster has no object to update")

// Update applies mutate to the latest version of the Cluster namespace/name and writes its
// status when it changed. mutate may be called several times (after conflicts). It returns the
// stored object.
func (w *StatusWriter) Update(ctx context.Context, namespace, name string, mutate func(*v1alpha1.Cluster) error) (*v1alpha1.Cluster, error) {
	if name == "" {
		return nil, errSynthetic
	}
	res := w.client.Resource(ClusterGVR).Namespace(namespace)
	var out *v1alpha1.Cluster
	attempt := func(ctx context.Context) error {
		u, err := res.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		c := &v1alpha1.Cluster{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, c); err != nil {
			return fmt.Errorf("decode cluster: %w", err)
		}
		before := c.Status.DeepCopy()
		if err := mutate(c); err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(before, &c.Status) {
			out = c
			return nil
		}
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(c)
		if err != nil {
			return fmt.Errorf("encode cluster: %w", err)
		}
		nu := &unstructured.Unstructured{Object: obj}
		nu.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Cluster"))
		stored, err := res.UpdateStatus(ctx, nu, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		sc := &v1alpha1.Cluster{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, sc); err != nil {
			return fmt.Errorf("decode cluster: %w", err)
		}
		out = sc
		return nil
	}
	if err := w.retry(ctx, attempt); err != nil {
		return nil, fmt.Errorf("update status of cluster %s/%s: %w", namespace, name, err)
	}
	return out, nil
}

// retry runs fn until it succeeds, fails permanently, or the backoff is exhausted.
func (w *StatusWriter) retry(ctx context.Context, fn func(context.Context) error) error {
	b := w.backoff
	attempts := max(b.Steps, 1)
	var last error
	for i := 0; ; i++ {
		last = fn(ctx)
		if last == nil || !isRetryableAPIError(last) || i == attempts-1 {
			return last
		}
		t := time.NewTimer(b.Step())
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (last error: %w)", ctx.Err(), last)
		case <-t.C:
		}
	}
}

func isRetryableAPIError(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) ||
		utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err)
}

// RecordProbe stores the result of Probe: connection state, server version (on success),
// the Ready and ConnectionFailed conditions and observedGeneration.
func (w *StatusWriter) RecordProbe(ctx context.Context, c *v1alpha1.Cluster, version string, probeErr error) (*v1alpha1.Cluster, error) {
	if IsSynthetic(c) {
		return nil, errSynthetic
	}
	return w.Update(ctx, c.Namespace, c.Name, func(cur *v1alpha1.Cluster) error {
		state, ver := ProbeUpdate(version, probeErr, w.now())
		applyConnection(cur, state, ver, probeErr == nil)
		return nil
	})
}

// RecordClusterInfo stores a ComputeClusterInfo result. The cache-derived status replaces the
// connection state, server version and info, as upstream's cluster-info updater does.
func (w *StatusWriter) RecordClusterInfo(ctx context.Context, c *v1alpha1.Cluster, u StatusUpdate) (*v1alpha1.Cluster, error) {
	if IsSynthetic(c) {
		return nil, errSynthetic
	}
	return w.Update(ctx, c.Namespace, c.Name, func(cur *v1alpha1.Cluster) error {
		applyConnection(cur, u.ConnectionState, u.ServerVersion, true)
		cur.Status.ServerVersion = u.ServerVersion
		cur.Status.Info.ApplicationsCount = u.Info.ApplicationsCount
		cur.Status.Info.APIVersions = u.Info.APIVersions
		cur.Status.Info.CacheInfo = u.Info.CacheInfo
		return nil
	})
}

// applyConnection sets the connection state, version and conditions. AttemptedAt is kept when
// nothing else changed, so periodic writes of an unchanged state are no-ops.
func applyConnection(c *v1alpha1.Cluster, state v1alpha1.ConnectionState, version string, setVersion bool) {
	if prev := c.Status.ConnectionState; prev != nil && prev.Status == state.Status && prev.Message == state.Message {
		state.AttemptedAt = prev.AttemptedAt
	}
	c.Status.ConnectionState = &state
	if setVersion && version != "" {
		c.Status.ServerVersion = version
	}
	c.Status.ObservedGeneration = c.Generation
	ready := metav1.Condition{Type: v1alpha1.ConditionReady, ObservedGeneration: c.Generation}
	failed := metav1.Condition{Type: v1alpha1.ConditionConnectionFailed, ObservedGeneration: c.Generation}
	switch state.Status {
	case v1alpha1.ConnectionStatusSuccessful:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionTrue, v1alpha1.ReasonConnectionSuccessful, "Connection successful"
		failed.Status, failed.Reason, failed.Message = metav1.ConditionFalse, v1alpha1.ReasonConnectionSuccessful, "Connection successful"
	case v1alpha1.ConnectionStatusFailed:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, v1alpha1.ReasonConnectionFailed, state.Message
		failed.Status, failed.Reason, failed.Message = metav1.ConditionTrue, v1alpha1.ReasonConnectionFailed, state.Message
	default:
		msg := state.Message
		if msg == "" {
			msg = "Connection state unknown"
		}
		ready.Status, ready.Reason, ready.Message = metav1.ConditionUnknown, v1alpha1.ReasonCheckPending, msg
		meta.RemoveStatusCondition(&c.Status.Conditions, v1alpha1.ConditionConnectionFailed)
		meta.SetStatusCondition(&c.Status.Conditions, ready)
		return
	}
	meta.SetStatusCondition(&c.Status.Conditions, ready)
	meta.SetStatusCondition(&c.Status.Conditions, failed)
}

// RecordVersionLabel maintains cd.patrol.example/kubernetes-version on clusters labeled
// cd.patrol.example/auto-label-cluster-info=true (REPO-095) with a merge patch of the label. It
// reports whether a patch was sent.
func (w *StatusWriter) RecordVersionLabel(ctx context.Context, c *v1alpha1.Cluster, version string) (bool, error) {
	if IsSynthetic(c) {
		return false, nil
	}
	probe := c.DeepCopy()
	if probe.Labels == nil {
		return false, nil
	}
	if !AutoLabelKubernetesVersion(probe, version) {
		return false, nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{v1alpha1.LabelKeyClusterKubernetesVersion: version}}})
	if err != nil {
		return false, fmt.Errorf("encode label patch: %w", err)
	}
	err = w.retry(ctx, func(ctx context.Context) error {
		_, err := w.client.Resource(ClusterGVR).Namespace(c.Namespace).Patch(ctx, c.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("label cluster %s/%s with its Kubernetes version: %w", c.Namespace, c.Name, err)
	}
	return true, nil
}
