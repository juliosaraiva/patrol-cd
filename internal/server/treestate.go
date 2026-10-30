package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	krest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/app"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// liveGetTimeout bounds one live object read from a destination cluster.
const liveGetTimeout = time.Minute

// stateSource is the comparison state served by treeState (*app.Reconciler).
type stateSource interface {
	State(key types.NamespacedName) (app.AppState, bool)
}

// treeState implements treeapi.StateProvider over the Application reconciler's in-memory
// comparison state (the tree and the masked managed resources of the last comparison), the
// live-state caches and the destination clusters. Applications whose shard this replica
// does not own are answered with a *treeapi.NotOwnedError so the client re-routes.
type treeState struct {
	reader        client.Reader
	app           stateSource
	own           *sharding.Ownership
	matcher       *core.NamespaceMatcher
	settings      *settings.Manager
	registry      *clusters.Registry
	live          *compare.LiveStateCache
	ssd           *compare.KubeServerSideDiff
	clusterConfig func(ctx context.Context, c *v1alpha1.Cluster) (*krest.Config, error)
	namespace     string
	jqTimeout     time.Duration
	log           *slog.Logger
}

var _ treeapi.StateProvider = (*treeState)(nil)

// application reads the Application and checks this replica serves it.
func (t *treeState) application(ctx context.Context, key treeapi.AppKey) (*v1alpha1.Application, error) {
	if !t.matcher.Allowed(key.Namespace) {
		return nil, fmt.Errorf("%w: application %s", treeapi.ErrNotFound, key)
	}
	var a v1alpha1.Application
	if err := t.reader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: application %s", treeapi.ErrNotFound, key)
		}
		return nil, fmt.Errorf("%w: read application %s: %v", treeapi.ErrUnavailable, key, err)
	}
	if t.own != nil {
		shard, managed := t.own.ShardForApplication(&a)
		if !managed {
			return nil, &treeapi.NotOwnedError{Shard: -1}
		}
		if !t.own.OwnsApplication(&a) {
			return nil, &treeapi.NotOwnedError{Shard: shard}
		}
	}
	return &a, nil
}

func (t *treeState) state(ctx context.Context, key treeapi.AppKey) (*v1alpha1.Application, app.AppState, error) {
	a, err := t.application(ctx, key)
	if err != nil {
		return nil, app.AppState{}, err
	}
	st, ok := t.app.State(types.NamespacedName{Namespace: key.Namespace, Name: key.Name})
	if !ok {
		return nil, app.AppState{}, fmt.Errorf("%w: application %s has not been compared yet", treeapi.ErrUnavailable, key)
	}
	return a, st, nil
}

// Tree implements treeapi.StateProvider.
func (t *treeState) Tree(ctx context.Context, key treeapi.AppKey) (*compare.ApplicationTree, error) {
	_, st, err := t.state(ctx, key)
	if err != nil {
		return nil, err
	}
	if st.Tree == nil {
		return nil, fmt.Errorf("%w: the resource tree of application %s is not available", treeapi.ErrUnavailable, key)
	}
	return st.Tree, nil
}

// ManagedResources implements treeapi.StateProvider. An Application with spec errors has no
// managed resources to report, which is not the same as none: as for one never compared, the
// answer is unavailable, so the API reports its error conditions (upstream deletes the cache
// entry; the tree stays readable, empty).
func (t *treeState) ManagedResources(ctx context.Context, key treeapi.AppKey) ([]compare.ManagedResource, error) {
	_, st, err := t.state(ctx, key)
	if err != nil {
		return nil, err
	}
	if st.SpecInvalid {
		return nil, fmt.Errorf("%w: application %s has an invalid spec", treeapi.ErrUnavailable, key)
	}
	return st.Managed, nil
}

// SensitiveAnnotations implements treeapi.StateProvider.
func (t *treeState) SensitiveAnnotations() []string {
	if st := t.settings.Current(); st != nil {
		return st.SensitiveMaskAnnotations()
	}
	return nil
}

// destination resolves the Application's destination cluster.
func (t *treeState) destination(a *v1alpha1.Application) (*v1alpha1.Cluster, error) {
	c, err := t.registry.DestinationCluster(a.Spec.Destination)
	if err != nil {
		return nil, fmt.Errorf("%w: destination of application %s/%s: %v", treeapi.ErrUnavailable, a.Namespace, a.Name, err)
	}
	return c, nil
}

// ServerSideDiff implements treeapi.StateProvider: dry-run apply diffs with the
// Application's normalization (ignoreDifferences, system overrides, known types), tracking
// keys and mutation-webhook option, as the comparison of the controller uses.
func (t *treeState) ServerSideDiff(ctx context.Context, key treeapi.AppKey, live, target []*unstructured.Unstructured) ([]diff.Result, error) {
	a, err := t.application(ctx, key)
	if err != nil {
		return nil, err
	}
	st := t.settings.Current()
	if st == nil {
		return nil, fmt.Errorf("%w: settings are not loaded", treeapi.ErrUnavailable)
	}
	cluster, err := t.destination(a)
	if err != nil {
		return nil, err
	}
	n, err := compare.NewNormalizer(st, a, t.jqTimeout, t.log)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", treeapi.ErrInvalidArgument, err)
	}
	o := diff.DefaultOptions()
	o.Normalizer = n
	o.IgnoreAggregatedRoles = st.CompareOptions().IgnoreAggregatedRoles
	ann := diff.ParseCompareAnnotation(a.Annotations, v1alpha1.AnnotationKeyCompareOptions)
	o.ServerSide = true
	o.IgnoreMutationWebhook = !ann.IncludeMutationWebhook
	o.StructuredMerge = a.Spec.SyncPolicy != nil && a.Spec.SyncPolicy.SyncOptions.HasOption(v1alpha1.SyncOptionKeyServerSideApply+"="+v1alpha1.SyncOptionValueTrue)
	o.SyncOptionsAnnotation = v1alpha1.AnnotationKeySyncOptions
	o.Manager = diff.DefaultManager
	tracker := tracking.New(tracking.Config{InstanceLabelKey: st.InstanceLabelKey(), ControlPlaneNamespace: t.namespace})
	labelKey, annotationKey := tracker.DiffTrackingKeys(st.TrackingMethod())
	o.Tracking = diff.TrackingOptions{LabelKey: labelKey, AnnotationKey: annotationKey}
	o.Log = t.log
	if o.GVKParser, err = t.ssd.GVKParser(ctx, cluster); err != nil {
		return nil, fmt.Errorf("%w: %v", treeapi.ErrUnavailable, err)
	}
	if o.DryRunner, err = t.ssd.DryRunner(ctx, cluster); err != nil {
		return nil, fmt.Errorf("%w: %v", treeapi.ErrUnavailable, err)
	}
	res, err := diff.DiffArray(ctx, target, live, o)
	if err != nil {
		return nil, err
	}
	return res.Diffs, nil
}

// LiveResource implements treeapi.StateProvider: a GET through the destination cluster's
// configuration (the controller's identity), with the resource of the kind taken from the
// cluster's discovery data.
func (t *treeState) LiveResource(ctx context.Context, key treeapi.AppKey, ref compare.ResourceRef) (*unstructured.Unstructured, error) {
	a, err := t.application(ctx, key)
	if err != nil {
		return nil, err
	}
	cluster, err := t.destination(a)
	if err != nil {
		return nil, err
	}
	cc, err := t.live.ClusterCache(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", treeapi.ErrUnavailable, err)
	}
	var gvr schema.GroupVersionResource
	namespaced := ref.Namespace != ""
	found := false
	for _, r := range cc.GetAPIResources() {
		if r.GroupKind.Group == ref.Group && r.GroupKind.Kind == ref.Kind {
			gvr, namespaced, found = r.GroupVersionResource, r.Meta.Namespaced, true
			if ref.Version == "" || r.GroupVersionResource.Version == ref.Version {
				break
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: the cluster serves no resource of kind %s in group %q", treeapi.ErrNotFound, ref.Kind, ref.Group)
	}
	if ref.Version != "" {
		gvr.Version = ref.Version
	}
	cfg, err := t.clusterConfig(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", treeapi.ErrUnavailable, err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", treeapi.ErrUnavailable, err)
	}
	ctx, cancel := context.WithTimeout(ctx, liveGetTimeout)
	defer cancel()
	var ri dynamic.ResourceInterface = dyn.Resource(gvr)
	if namespaced {
		ri = dyn.Resource(gvr).Namespace(ref.Namespace)
	}
	obj, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		return obj, nil
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("%w: %s %s", treeapi.ErrNotFound, ref.Kind, ref.Name)
	case errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("%w: %v", treeapi.ErrUnavailable, err)
	}
	return nil, err
}
