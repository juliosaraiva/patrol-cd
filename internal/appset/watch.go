package appset

import (
	"context"
	"log/slog"
	"maps"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// ShouldRequeueForApplication decides whether an update of an owned Application requeues its
// ApplicationSet: a change of spec, labels, annotations or finalizers (nil and empty are
// equal), and, with progressive syncs, a change of health, sync status, or operation phase or
// start time. Status-only changes such as reconciledAt do not requeue.
func ShouldRequeueForApplication(old, cur *v1alpha1.Application, progressiveSyncs bool) bool {
	if old == nil || cur == nil {
		return false
	}
	if !cmp.Equal(old.Spec, cur.Spec, cmpopts.EquateEmpty()) ||
		!cmp.Equal(old.GetAnnotations(), cur.GetAnnotations(), cmpopts.EquateEmpty()) ||
		!cmp.Equal(old.GetLabels(), cur.GetLabels(), cmpopts.EquateEmpty()) ||
		!cmp.Equal(old.GetFinalizers(), cur.GetFinalizers(), cmpopts.EquateEmpty()) {
		return true
	}
	if !progressiveSyncs {
		return false
	}
	if old.Status.Health.Status != cur.Status.Health.Status || old.Status.Sync.Status != cur.Status.Sync.Status {
		return true
	}
	o, n := old.Status.OperationState, cur.Status.OperationState
	if o != nil && n != nil {
		if o.Phase != n.Phase || !o.StartedAt.Equal(n.StartedAt) {
			return true
		}
	}
	return (o == nil) != (n == nil)
}

// ShouldRequeueForApplicationSet decides whether an ApplicationSet update requeues it: spec,
// labels, finalizers or deletionTimestamp changes; annotation changes except the sole removal of
// the refresh annotation; and, with progressive syncs, status.applicationStatus changes.
// Status bookkeeping by the controller does not requeue.
func ShouldRequeueForApplicationSet(old, cur *v1alpha1.ApplicationSet, progressiveSyncs bool) bool {
	if old == nil || cur == nil {
		return false
	}
	if progressiveSyncs && !cmp.Equal(old.Status.ApplicationStatus, cur.Status.ApplicationStatus, cmpopts.EquateEmpty()) {
		return true
	}
	if !cmp.Equal(old.Spec, cur.Spec, cmpopts.EquateEmpty()) ||
		!cmp.Equal(old.GetLabels(), cur.GetLabels(), cmpopts.EquateEmpty()) ||
		!cmp.Equal(old.GetFinalizers(), cur.GetFinalizers(), cmpopts.EquateEmpty()) ||
		!cmp.Equal(old.DeletionTimestamp, cur.DeletionTimestamp, cmpopts.EquateEmpty()) {
		return true
	}
	if !cmp.Equal(old.GetAnnotations(), cur.GetAnnotations(), cmpopts.EquateEmpty()) {
		_, hadRefresh := old.Annotations[v1alpha1.AnnotationKeyApplicationSetRefresh]
		_, hasRefresh := cur.Annotations[v1alpha1.AnnotationKeyApplicationSetRefresh]
		return !hadRefresh || hasRefresh
	}
	return false
}

func applicationSetPredicate(progressiveSyncs bool) predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, ok1 := e.ObjectOld.(*v1alpha1.ApplicationSet)
			cur, ok2 := e.ObjectNew.(*v1alpha1.ApplicationSet)
			return ok1 && ok2 && ShouldRequeueForApplicationSet(old, cur, progressiveSyncs)
		},
	}
}

// applicationPredicate filters owned Application events: creates are ignored (the controller
// created them), deletes and generic events always requeue.
func applicationPredicate(progressiveSyncs bool) predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, ok1 := e.ObjectOld.(*v1alpha1.Application)
			cur, ok2 := e.ObjectNew.(*v1alpha1.Application)
			return ok1 && ok2 && ShouldRequeueForApplication(old, cur, progressiveSyncs)
		},
	}
}

func namespacePredicate(m *NamespaceMatcher) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool { return m.Allowed(obj.GetNamespace()) })
}

// clusterEventHandler enqueues every ApplicationSet (in an allowed namespace) that uses a
// Clusters generator, at the top level or nested in Matrix/Merge, on any change of a Cluster
// of the control-plane namespace, except a status-only update: the controller writes that status
// on its cluster-info interval, and the generators do not read it.
type clusterEventHandler struct {
	client     client.Reader
	namespace  string
	namespaces *NamespaceMatcher
	log        *slog.Logger
}

var _ handler.TypedEventHandler[client.Object, reconcile.Request] = (*clusterEventHandler)(nil)

func (h *clusterEventHandler) Create(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueue(ctx, e.Object, q)
}

func (h *clusterEventHandler) Update(ctx context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if e.ObjectOld != nil && e.ObjectNew != nil && e.ObjectOld.GetUID() == e.ObjectNew.GetUID() && e.ObjectOld.GetGeneration() == e.ObjectNew.GetGeneration() &&
		maps.Equal(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()) && maps.Equal(e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()) {
		return
	}
	h.enqueue(ctx, e.ObjectNew, q)
}

func (h *clusterEventHandler) Delete(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueue(ctx, e.Object, q)
}

func (h *clusterEventHandler) Generic(ctx context.Context, e event.TypedGenericEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueue(ctx, e.Object, q)
}

func (h *clusterEventHandler) enqueue(ctx context.Context, obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if obj == nil || obj.GetNamespace() != h.namespace {
		return
	}
	var list v1alpha1.ApplicationSetList
	if err := h.client.List(ctx, &list); err != nil {
		h.log.ErrorContext(ctx, "unable to list ApplicationSets for a cluster event", slog.Any("error", err))
		return
	}
	for i := range list.Items {
		as := &list.Items[i]
		if !h.namespaces.Allowed(as.Namespace) || !usesClustersGenerator(h.log, as) {
			continue
		}
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: as.Namespace, Name: as.Name}})
	}
}

// usesClustersGenerator reports whether appSet has a Clusters generator at the top level or
// inside Matrix/Merge generators (one or two levels). Malformed nested JSON is logged and ignored.
func usesClustersGenerator(log *slog.Logger, appSet *v1alpha1.ApplicationSet) bool {
	for _, g := range appSet.Spec.Generators {
		if g.Clusters != nil {
			return true
		}
		var children []v1alpha1.ApplicationSetNestedGenerator
		if g.Matrix != nil {
			children = append(children, g.Matrix.Generators...)
		}
		if g.Merge != nil {
			children = append(children, g.Merge.Generators...)
		}
		if nestedUsesClusters(log, children) {
			return true
		}
	}
	return false
}

func nestedUsesClusters(log *slog.Logger, children []v1alpha1.ApplicationSetNestedGenerator) bool {
	for _, c := range children {
		if c.Clusters != nil {
			return true
		}
		if c.Matrix != nil {
			m, err := v1alpha1.ToNestedMatrixGenerator(c.Matrix)
			if err != nil {
				log.Warn("unable to decode a nested matrix generator", slog.Any("error", err))
			} else if terminalUsesClusters(m.Generators) {
				return true
			}
		}
		if c.Merge != nil {
			m, err := v1alpha1.ToNestedMergeGenerator(c.Merge)
			if err != nil {
				log.Warn("unable to decode a nested merge generator", slog.Any("error", err))
			} else if terminalUsesClusters(m.Generators) {
				return true
			}
		}
	}
	return false
}

func terminalUsesClusters(gens v1alpha1.ApplicationSetTerminalGenerators) bool {
	for _, g := range gens {
		if g.Clusters != nil {
			return true
		}
	}
	return false
}
