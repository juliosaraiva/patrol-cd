package appset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// OwnerIndex is the field index of Applications by the name of their controlling
// ApplicationSet. SetupWithManager registers it; clients used with NewReconciler must provide it.
const OwnerIndex = ".metadata.controller"

// OwnerIndexFunc is the OwnerIndex function.
func OwnerIndexFunc(obj client.Object) []string {
	owner := metav1.GetControllerOf(obj)
	if owner == nil {
		return nil
	}
	if owner.APIVersion != v1alpha1.GroupVersion.String() || owner.Kind != "ApplicationSet" {
		return nil
	}
	return []string{owner.Name}
}

// defaultPreservedAnnotations are preserved on generated Applications regardless of
// configuration: notification state and the refresh/hydrate requests.
var defaultPreservedAnnotations = []string{
	v1alpha1.AnnotationKeyNotified,
	v1alpha1.AnnotationKeyRefresh,
	v1alpha1.AnnotationKeyHydrate,
}

// preservedFinalizerPrefixes are live finalizers kept on update so the application controller's
// delete hooks are not disrupted (prefix match: "/<stage>" variants included).
var preservedFinalizerPrefixes = []string{v1alpha1.FinalizerPreDelete, v1alpha1.FinalizerPostDelete}

// resourcesFinalizers are the finalizers that cascade an Application's deletion to its resources.
var resourcesFinalizers = []string{
	v1alpha1.FinalizerResources,
	v1alpha1.FinalizerResourcesForeground,
	v1alpha1.FinalizerResourcesBackground,
}

// writeAction is the outcome of writing one Application.
type writeAction string

const (
	actionNone    writeAction = "unchanged"
	actionCreated writeAction = "created"
	actionUpdated writeAction = "updated"
)

// actionVerbs are the event actions of write actions.
var actionVerbs = map[writeAction]string{actionCreated: "Create", actionUpdated: "Update"}

// writeResult records the Applications written in one reconcile, so the status reflects them
// even before the informer cache has observed the writes.
type writeResult struct {
	mu      sync.Mutex
	written map[string]*v1alpha1.Application
	deleted map[string]bool
}

func newWriteResult() *writeResult {
	return &writeResult{written: map[string]*v1alpha1.Application{}, deleted: map[string]bool{}}
}

func (w *writeResult) put(app *v1alpha1.Application) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written[app.Name] = app.DeepCopy()
}

func (w *writeResult) markDeleted(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deleted[name] = true
	delete(w.written, name)
}

// overlay returns current with the written objects substituted, written objects that are
// missing added, and deleted objects removed.
func (w *writeResult) overlay(current []v1alpha1.Application) []v1alpha1.Application {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := map[string]bool{}
	out := make([]v1alpha1.Application, 0, len(current)+len(w.written))
	for i := range current {
		name := current[i].Name
		if w.deleted[name] && w.written[name] == nil {
			continue
		}
		seen[name] = true
		if wa := w.written[name]; wa != nil {
			out = append(out, *wa.DeepCopy())
			continue
		}
		out = append(out, current[i])
	}
	for name, wa := range w.written {
		if !seen[name] {
			out = append(out, *wa.DeepCopy())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// currentApplications lists the Applications controlled by appSet in its namespace.
func (r *Reconciler) currentApplications(ctx context.Context, appSet *v1alpha1.ApplicationSet) ([]v1alpha1.Application, error) {
	var list v1alpha1.ApplicationList
	if err := r.client.List(ctx, &list, client.InNamespace(appSet.Namespace), client.MatchingFields{OwnerIndex: appSet.Name}); err != nil {
		return nil, fmt.Errorf("error retrieving applications: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

// createOnly keeps the desired Applications that are not already owned by appSet (create-only
// and create-delete policies): existing Applications are never updated.
func createOnly(current, desired []v1alpha1.Application) []v1alpha1.Application {
	owned := make(map[string]bool, len(current))
	for i := range current {
		owned[current[i].Name] = true
	}
	var out []v1alpha1.Application
	for i := range desired {
		if !owned[desired[i].Name] {
			out = append(out, desired[i])
		}
	}
	return out
}

// createOrUpdate creates or updates every desired Application (sorted by name) with bounded
// parallelism. Per-Application errors do not stop the others; the error of the
// lexicographically first failing Application is returned. Context cancellation aborts.
func (r *Reconciler) createOrUpdate(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, desired []v1alpha1.Application, differ *ignoreDiffer, res *writeResult) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.cfg.ConcurrentApplicationUpdates)
	var mu sync.Mutex
	appErrors := map[string]error{}
	for i := range desired {
		app := desired[i].DeepCopy()
		app.Spec = *NormalizeApplicationSpec(&app.Spec)
		g.Go(func() error {
			alog := log.With(slog.String("application", app.Name))
			action, written, err := r.createOrUpdateOne(gctx, alog, appSet, app, differ)
			if err != nil {
				alog.ErrorContext(gctx, "failed to write Application", slog.String("action", string(action)), slog.Any("error", err))
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				mu.Lock()
				appErrors[app.Name] = err
				mu.Unlock()
				return nil
			}
			if written != nil {
				res.put(written)
			}
			if action != actionNone {
				r.event(appSet, corev1.EventTypeNormal, string(action), actionVerbs[action], "%s Application %q", action, app.Name)
				alog.InfoContext(gctx, string(action)+" Application")
			} else {
				alog.DebugContext(gctx, "unchanged Application")
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return firstAppError(appErrors)
}

// createOrUpdateOne creates the Application or patches the live one toward desired. It
// returns the written (or unchanged live) object.
func (r *Reconciler) createOrUpdateOne(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, desired *v1alpha1.Application, differ *ignoreDiffer) (writeAction, *v1alpha1.Application, error) {
	found := &v1alpha1.Application{}
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.client.Get(ctx, key, found); err != nil {
		if !apierrors.IsNotFound(err) {
			return actionNone, nil, err
		}
		created := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
		if err := r.mutate(appSet, created, desired); err != nil {
			return actionNone, nil, err
		}
		err := r.client.Create(ctx, created)
		if err == nil {
			return actionCreated, created, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return actionNone, nil, err
		}
		// The cache has not observed the Application yet (e.g. created by the previous
		// reconcile): read it from the API server and update it instead.
		found = &v1alpha1.Application{}
		if err := r.apiReader.Get(ctx, key, found); err != nil {
			return actionNone, nil, err
		}
	}
	normalizedLive := found.DeepCopy()
	if err := r.mutate(appSet, found, desired); err != nil {
		return actionNone, nil, err
	}
	normalizedLive.Spec = *NormalizeApplicationSpec(&normalizedLive.Spec)
	if err := differ.apply(ctx, normalizedLive, found); err != nil {
		return actionNone, nil, err
	}
	equal, err := jsonEqual(normalizedLive, found)
	if err != nil {
		return actionNone, nil, err
	}
	if equal {
		return actionNone, found, nil
	}
	patch := client.MergeFrom(normalizedLive)
	if log.Enabled(ctx, slog.LevelDebug) {
		if data, err := patch.Data(found); err == nil {
			log.DebugContext(ctx, "patching application", slog.String("patch", string(data)))
		}
	}
	if err := r.client.Patch(ctx, found, patch); err != nil {
		return actionNone, nil, err
	}
	return actionUpdated, found, nil
}

// mutate sets the fields the ApplicationSet controls on found: spec, labels, annotations and
// finalizers (preserved keys and delete-hook finalizers keep their live values), and the
// controller owner reference. found's name and namespace are never changed.
func (r *Reconciler) mutate(appSet *v1alpha1.ApplicationSet, found, desired *v1alpha1.Application) error {
	name, namespace := found.Name, found.Namespace
	found.Spec = *desired.Spec.DeepCopy()

	annotations := copyMap(desired.Annotations)
	labels := copyMap(desired.Labels)
	var preservedAnnotations, preservedLabels []string
	if pf := appSet.Spec.PreservedFields; pf != nil {
		preservedAnnotations = append(preservedAnnotations, pf.Annotations...)
		preservedLabels = append(preservedLabels, pf.Labels...)
	}
	preservedAnnotations = append(preservedAnnotations, r.cfg.GlobalPreservedAnnotations...)
	preservedLabels = append(preservedLabels, r.cfg.GlobalPreservedLabels...)
	preservedAnnotations = append(preservedAnnotations, defaultPreservedAnnotations...)
	for _, k := range preservedAnnotations {
		if v, ok := found.Annotations[k]; ok {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[k] = v
		}
	}
	for _, k := range preservedLabels {
		if v, ok := found.Labels[k]; ok {
			if labels == nil {
				labels = map[string]string{}
			}
			labels[k] = v
		}
	}
	finalizers := slices.Clone(desired.Finalizers)
	for _, f := range found.Finalizers {
		for _, prefix := range preservedFinalizerPrefixes {
			if strings.HasPrefix(f, prefix) && !slices.Contains(finalizers, f) {
				finalizers = append(finalizers, f)
			}
		}
	}
	found.Annotations = annotations
	found.Labels = labels
	found.Finalizers = finalizers
	if err := controllerutil.SetControllerReference(appSet, found, r.scheme); err != nil {
		return err
	}
	if found.Name != name || found.Namespace != namespace {
		return errors.New("MutateFn cannot mutate object name and/or object namespace")
	}
	return nil
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// firstAppError returns the error of the lexicographically smallest Application name, so the
// result does not depend on scheduling.
func firstAppError(appErrors map[string]error) error {
	if len(appErrors) == 0 {
		return nil
	}
	names := make([]string, 0, len(appErrors))
	for n := range appErrors {
		names = append(names, n)
	}
	sort.Strings(names)
	return appErrors[names[0]]
}

// deleteStale deletes the Applications owned by appSet that are not in generated (the full
// generated list, invalid Applications included, so they are not deleted). Before deleting,
// the resources finalizer is removed when the destination is invalid, and every resources
// finalizer is removed when preserveResourcesOnDeletion is set.
func (r *Reconciler) deleteStale(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, current, generated []v1alpha1.Application, res *writeResult) error {
	keep := make(map[string]bool, len(generated))
	for i := range generated {
		keep[generated[i].Name] = true
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.cfg.ConcurrentApplicationUpdates)
	var mu sync.Mutex
	appErrors := map[string]error{}
	for i := range current {
		if keep[current[i].Name] {
			continue
		}
		app := current[i].DeepCopy()
		g.Go(func() error {
			alog := log.With(slog.String("application", app.Name))
			err := r.deleteApplication(gctx, alog, appSet, app)
			if err != nil {
				alog.ErrorContext(gctx, "failed to delete Application", slog.Any("error", err))
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				mu.Lock()
				appErrors[app.Name] = err
				mu.Unlock()
				return nil
			}
			res.markDeleted(app.Name)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return firstAppError(appErrors)
}

// deleteApplication releases finalizers as configured and deletes app.
func (r *Reconciler) deleteApplication(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, app *v1alpha1.Application) error {
	if err := r.releaseFinalizers(ctx, log, appSet, app); err != nil {
		return err
	}
	if err := r.client.Delete(ctx, app); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	r.event(appSet, corev1.EventTypeNormal, "Deleted", "Delete", "Deleted Application %q", app.Name)
	log.InfoContext(ctx, "deleted Application")
	return nil
}

// releaseFinalizers removes, before deletion, the resources finalizer of an Application whose
// destination is invalid (its cluster is gone; otherwise it would never finish deleting), and
// every resources finalizer when the ApplicationSet preserves resources on deletion.
func (r *Reconciler) releaseFinalizers(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, app *v1alpha1.Application) error {
	if len(app.Finalizers) == 0 {
		return nil
	}
	preserve := appSet.Spec.SyncPolicy != nil && appSet.Spec.SyncPolicy.PreserveResourcesOnDeletion
	var remove []string
	reason := ""
	switch {
	case preserve:
		remove = resourcesFinalizers
		reason = "because the ApplicationSet preserves resources on deletion"
	default:
		valid, err := r.validDestination(ctx, log, app)
		if err != nil {
			return err
		}
		if !valid {
			remove = []string{v1alpha1.FinalizerResources}
			reason = "because application has an invalid destination"
		}
	}
	if len(remove) == 0 {
		return nil
	}
	kept := make([]string, 0, len(app.Finalizers))
	for _, f := range app.Finalizers {
		if !slices.Contains(remove, f) {
			kept = append(kept, f)
		}
	}
	if len(kept) == len(app.Finalizers) {
		return nil
	}
	updated := app.DeepCopy()
	updated.Finalizers = kept
	if err := r.client.Patch(ctx, updated, client.MergeFromWithOptions(app, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("error updating finalizers: %w", err)
	}
	updated.DeepCopyInto(app)
	r.event(appSet, corev1.EventTypeNormal, "Updated", "Update", "Updated Application %q finalizer before deletion, %s", app.Name, reason)
	log.InfoContext(ctx, "removed Application finalizer before deletion", slog.String("reason", reason))
	return nil
}

// validDestination reports whether app's destination resolves to a registered cluster (by
// server, or by name; the in-cluster destination unless disabled).
func (r *Reconciler) validDestination(ctx context.Context, log *slog.Logger, app *v1alpha1.Application) (bool, error) {
	idx, err := r.clusterIndex(ctx)
	if err != nil {
		return false, err
	}
	if _, err := project.ResolveDestination(ctx, app.Spec.Destination, idx); err != nil {
		log.WarnContext(ctx, "the destination cluster of the Application could not be found", slog.Any("error", err))
		return false, nil
	}
	return true, nil
}

// clusterIndex lists the registered clusters of the control-plane namespace.
func (r *Reconciler) clusterIndex(ctx context.Context) (*project.ClusterIndex, error) {
	var list v1alpha1.ClusterList
	if err := r.client.List(ctx, &list, client.InNamespace(r.cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("error listing clusters: %w", err)
	}
	return project.NewClusterIndex(list.Items, !r.cfg.DisableInCluster), nil
}

// removeOwnerReferences releases every Application owned by appSet from its ownership, so
// garbage collection does not delete them (non-delete policies on ApplicationSet deletion).
// Owner references to other objects are kept.
func (r *Reconciler) removeOwnerReferences(ctx context.Context, appSet *v1alpha1.ApplicationSet, current []v1alpha1.Application) error {
	for i := range current {
		app := &current[i]
		refs := make([]metav1.OwnerReference, 0, len(app.OwnerReferences))
		for _, ref := range app.OwnerReferences {
			if ref.APIVersion == v1alpha1.GroupVersion.String() && ref.Kind == "ApplicationSet" && ref.Name == appSet.Name {
				continue
			}
			refs = append(refs, ref)
		}
		if len(refs) == len(app.OwnerReferences) {
			continue
		}
		updated := app.DeepCopy()
		updated.OwnerReferences = nil
		if len(refs) > 0 {
			updated.OwnerReferences = refs
		}
		if err := r.client.Patch(ctx, updated, client.MergeFromWithOptions(app, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("error updating application: %w", err)
		}
	}
	return nil
}

// stripResourcesFinalizers removes the resources finalizers of the Applications owned by appSet
// (preserveResourcesOnDeletion during ApplicationSet deletion), so their deletion by the
// garbage collector leaves the deployed resources in place.
func (r *Reconciler) stripResourcesFinalizers(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, current []v1alpha1.Application) error {
	for i := range current {
		app := current[i].DeepCopy()
		if err := r.releaseFinalizers(ctx, log.With(slog.String("application", app.Name)), appSet, app); err != nil {
			return err
		}
	}
	return nil
}
