package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/health"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// DeletionKube creates delete hooks and deletes resources in a destination cluster;
// *sync.KubeClient implements it.
type DeletionKube interface {
	Create(ctx context.Context, obj *unstructured.Unstructured, opts syncpkg.CreateOptions) (string, error)
	Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string, opts syncpkg.DeleteOptions) error
}

// DeletionKubeFunc returns the DeletionKube of a destination cluster, impersonating user when
// it is not empty.
type DeletionKubeFunc func(ctx context.Context, cluster *v1alpha1.Cluster, user string) (DeletionKube, error)

func defaultDeletionKube(config core.ClusterConfigFunc, m *core.Metrics) DeletionKubeFunc {
	return func(ctx context.Context, cluster *v1alpha1.Cluster, user string) (DeletionKube, error) {
		cfg, err := config(ctx, cluster)
		if err != nil {
			return nil, fmt.Errorf("failed to get REST config of cluster %q: %w", cluster.Spec.Server, err)
		}
		cfg = m.WrapConfig(cfg, cluster.Spec.Server)
		if user != "" {
			if cfg, err = clusters.ImpersonatingConfig(cfg, user); err != nil {
				return nil, fmt.Errorf("cannot apply impersonation: %w", err)
			}
		}
		return syncpkg.NewKubeClient(cfg, syncpkg.KubeClientOptions{})
	}
}

// helmResourcePolicy is Helm's annotation; "keep" protects a resource from deletion.
const helmResourcePolicy = "helm.sh/resource-policy"

// propagationFinalizers are the cascade-deletion finalizers.
var propagationFinalizers = []string{v1alpha1.FinalizerResources, v1alpha1.FinalizerResourcesForeground, v1alpha1.FinalizerResourcesBackground}

// ownFinalizers are every finalizer this controller processes on Applications.
var ownFinalizers = append(slices.Clone(propagationFinalizers),
	v1alpha1.FinalizerPreDelete, v1alpha1.FinalizerPreDeleteCleanup, v1alpha1.FinalizerPostDelete, v1alpha1.FinalizerPostDeleteCleanup)

// finalize processes the Application's deletion finalizers in upstream order, one stage per
// pass (APP-057..066): PreDelete hooks, cascade deletion (highest sync wave first), PostDelete
// hooks, then the cleanup of each hook type. Errors become the DeletionError condition.
func (a *appRun) finalize(ctx context.Context) (reconcile.Result, error) {
	app := a.app
	if !slices.ContainsFunc(app.Finalizers, func(f string) bool { return slices.Contains(ownFinalizers, f) }) {
		return reconcile.Result{}, nil
	}
	// An in-flight operation finishes first (upstream processes deletion only without a
	// pending operation); Pending ones are rejected by the Operation reconciler.
	ops, err := core.ListOperations(ctx, a.r.c, app.Namespace, app.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	for i := range ops {
		if core.IsActive(&ops[i]) {
			a.log.Info("deletion waits for the running operation", "operation", ops[i].Name)
			return reconcile.Result{RequeueAfter: a.r.cfg.DeletionPoll}, nil
		}
	}
	done, err := a.finalizePass(ctx)
	if err != nil {
		if errors.Is(err, core.ErrGone) || ctx.Err() != nil {
			return reconcile.Result{}, err
		}
		a.countDeletion("error")
		a.log.Warn("application deletion failed", "error", err)
		a.r.cfg.Recorder.Eventf(app, nil, corev1.EventTypeWarning, core.EventReasonStatusRefreshed, core.ActionDelete, "Unable to delete application resources: %v", err)
		if perr := a.setDeletionError(ctx, err.Error()); perr != nil {
			return reconcile.Result{}, perr
		}
		return reconcile.Result{RequeueAfter: a.r.cfg.DeletionPoll * 5}, nil
	}
	if done {
		// A completed stage resolves an earlier DeletionError; a pass that is still waiting
		// (for example re-running a PreDelete hook that failed) keeps it visible.
		if perr := a.setDeletionError(ctx, ""); perr != nil {
			return reconcile.Result{}, perr
		}
		a.countDeletion("stage-completed")
		return reconcile.Result{RequeueAfter: requeueAfterStage}, nil
	}
	a.countDeletion("waiting")
	return reconcile.Result{RequeueAfter: a.r.cfg.DeletionPoll}, nil
}

// requeueAfterStage continues with the next stage after a finalizer was removed.
const requeueAfterStage = 50 * time.Millisecond

func (a *appRun) countDeletion(outcome string) {
	if a.r.cfg.Metrics != nil {
		a.r.cfg.Metrics.DeletionTotal.WithLabelValues(outcome).Inc()
	}
}

// finalizePass runs the first pending stage; done reports that a finalizer was removed.
func (a *appRun) finalizePass(ctx context.Context) (bool, error) {
	app := a.app
	proj, err := a.r.cfg.ResolveProject(ctx, app, a.st)
	if err != nil {
		return false, fmt.Errorf("error getting project: %w", err)
	}
	if a.destErr != nil {
		// APP-063: the destination cannot be resolved; drop every deletion finalizer without
		// touching resources or running hooks.
		a.log.Warn("unable to get destination cluster; resource entries removed from undefined cluster", "error", a.destErr)
		return true, a.removeFinalizers(ctx, ownFinalizers...)
	}
	imp := a.st.Impersonation()
	sa, err := project.SelectServiceAccount(proj, app, a.cluster.Spec.Server, project.ImpersonationSettings{Enabled: imp.Enabled, Enforced: imp.Enforced})
	if err != nil {
		return false, fmt.Errorf("cannot apply impersonation: %w", err)
	}
	kube, err := a.r.cfg.DeletionKube(ctx, a.cluster, sa.UserName)
	if err != nil {
		return false, err
	}
	cc, err := a.r.cfg.LiveState.ClusterCache(ctx, a.cluster)
	if err != nil {
		return false, fmt.Errorf("failed to get cluster info for %q: %w", a.cluster.Spec.Server, err)
	}
	d := &deletion{a: a, proj: proj, kube: kube, cc: cc}

	switch {
	case slices.Contains(app.Finalizers, v1alpha1.FinalizerPreDelete):
		done, err := d.runDeleteHooks(ctx, v1alpha1.HookTypePreDelete)
		if err != nil || !done {
			return false, err
		}
		return true, a.removeFinalizers(ctx, v1alpha1.FinalizerPreDelete)
	case slices.ContainsFunc(app.Finalizers, func(f string) bool { return slices.Contains(propagationFinalizers, f) }):
		done, err := d.cascade(ctx)
		if err != nil || !done {
			return false, err
		}
		return true, a.removeFinalizers(ctx, propagationFinalizers...)
	case slices.Contains(app.Finalizers, v1alpha1.FinalizerPostDelete):
		done, err := d.runDeleteHooks(ctx, v1alpha1.HookTypePostDelete)
		if err != nil || !done {
			return false, err
		}
		return true, a.removeFinalizers(ctx, v1alpha1.FinalizerPostDelete)
	case slices.Contains(app.Finalizers, v1alpha1.FinalizerPreDeleteCleanup):
		done, err := d.cleanupHooks(ctx, v1alpha1.HookTypePreDelete)
		if err != nil || !done {
			return false, err
		}
		return true, a.removeFinalizers(ctx, v1alpha1.FinalizerPreDeleteCleanup)
	case slices.Contains(app.Finalizers, v1alpha1.FinalizerPostDeleteCleanup):
		done, err := d.cleanupHooks(ctx, v1alpha1.HookTypePostDelete)
		if err != nil || !done {
			return false, err
		}
		return true, a.removeFinalizers(ctx, v1alpha1.FinalizerPostDeleteCleanup)
	}
	return false, nil
}

func (a *appRun) removeFinalizers(ctx context.Context, names ...string) error {
	_, err := core.PatchObject(ctx, a.r.c, a.r.cfg.APIReader, a.app, func(app *v1alpha1.Application) error {
		app.Finalizers = slices.DeleteFunc(app.Finalizers, func(f string) bool { return slices.Contains(names, f) })
		return nil
	})
	if err == nil {
		a.log.Info("removed application finalizers", "finalizers", names)
	}
	return err
}

// setDeletionError sets (message != "") or clears the DeletionError condition.
func (a *appRun) setDeletionError(ctx context.Context, message string) error {
	now := metav1.NewTime(a.now())
	_, err := core.PatchStatus(ctx, a.r.c, a.r.cfg.APIReader, a.app, func(app *v1alpha1.Application) error {
		var found []compare.Condition
		if message != "" {
			found = append(found, compare.Condition{Type: v1alpha1.ApplicationConditionDeletionError, Reason: v1alpha1.ReasonDeletionFailed, Message: message})
		}
		app.Status.Conditions = compare.MergeConditions(app.Status.Conditions, found, []string{v1alpha1.ApplicationConditionDeletionError}, app.Generation, now)
		return nil
	})
	return err
}

// deletion is the state of one deletion pass.
type deletion struct {
	a    *appRun
	proj *v1alpha1.AppProject
	kube DeletionKube
	cc   cache.ClusterCache
}

// liveObjects returns the managed live objects of the Application that the project permits
// (upstream getPermittedAppLiveObjects).
func (d *deletion) liveObjects(ctx context.Context) (map[cache.ResourceKey]*unstructured.Unstructured, error) {
	t := tracking.New(tracking.Config{InstanceLabelKey: d.a.st.InstanceLabelKey(), ControlPlaneNamespace: d.a.r.cfg.Namespace})
	instance := t.InstanceName(tracking.AppOf(d.a.app))
	objs, err := d.cc.GetManagedLiveObjs(ctx, nil, func(r *cache.Resource) bool { return compare.InfoOf(r).AppName == instance })
	if err != nil {
		return nil, err
	}
	lookup := d.a.r.cfg.ClusterLookup(d.a.st)
	for k, obj := range objs {
		ok, err := project.IsLiveResourcePermitted(ctx, d.proj, obj, d.a.cluster, lookup)
		if err != nil {
			return nil, err
		}
		if !ok {
			delete(objs, k)
		}
	}
	return objs, nil
}

// shouldBeDeleted excludes CRDs, the Application itself, Delete=false resources and Helm
// "keep" resources from cascade deletion (APP-061).
func shouldBeDeleted(app *v1alpha1.Application, opts syncpkg.Options, obj *unstructured.Unstructured) bool {
	if tracking.IsCRD(obj) || opts.DeleteDisabled(obj) {
		return false
	}
	gvk := obj.GroupVersionKind()
	if gvk.Group == v1alpha1.GroupName && gvk.Kind == "Application" && obj.GetName() == app.Name && obj.GetNamespace() == app.Namespace {
		return false
	}
	for _, p := range strings.Split(obj.GetAnnotations()[helmResourcePolicy], ",") {
		if strings.TrimSpace(p) == "keep" {
			return false
		}
	}
	return true
}

// highestWave keeps the objects of the highest sync wave (upstream FilterObjectsForDeletion,
// SYNC-070), sorted by key for deterministic deletes.
func highestWave(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	if len(objs) == 0 {
		return nil
	}
	top := syncpkg.Wave(objs[0])
	for _, o := range objs[1:] {
		top = max(top, syncpkg.Wave(o))
	}
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if syncpkg.Wave(o) == top {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return cache.KeyOf(out[i]).String() < cache.KeyOf(out[j]).String() })
	return out
}

// propagationPolicy returns the propagation of the first propagation finalizer in list
// order: Background for the /background variant, Foreground otherwise (APP-057..059).
func propagationPolicy(finalizers []string) metav1.DeletionPropagation {
	for _, f := range finalizers {
		switch f {
		case v1alpha1.FinalizerResourcesBackground:
			return metav1.DeletePropagationBackground
		case v1alpha1.FinalizerResources, v1alpha1.FinalizerResourcesForeground:
			return metav1.DeletePropagationForeground
		}
	}
	return metav1.DeletePropagationForeground
}

// cascade deletes the Application's resources wave by wave, highest first; done reports
// that nothing deletable remains.
func (d *deletion) cascade(ctx context.Context) (bool, error) {
	app := d.a.app
	objs, err := d.liveObjects(ctx)
	if err != nil {
		return false, err
	}
	opts := syncpkg.ParseOptions(syncOptions(app))
	approved := syncpkg.DeletionApproved(app.Annotations, app.DeletionTimestamp.Time)
	var candidates []*unstructured.Unstructured
	for _, k := range sortedKeys(objs) {
		obj := objs[k]
		if obj.GetDeletionTimestamp() != nil {
			d.a.log.Info("objects remaining for deletion", "count", len(objs))
			return false, nil
		}
		if !shouldBeDeleted(app, opts, obj) {
			continue
		}
		if opts.RequiresDeletionConfirmation(obj) && !approved {
			d.a.log.Info("resource requires manual confirmation to delete", "resource", k.String())
			return false, nil
		}
		candidates = append(candidates, obj)
	}
	if len(candidates) == 0 {
		d.a.log.Info("deleted application resources")
		return true, nil
	}
	policy := propagationPolicy(app.Finalizers)
	wave := highestWave(candidates)
	d.a.log.Info("deleting application resources", "count", len(wave), "wave", syncpkg.Wave(wave[0]), "propagation", policy, "remaining", len(candidates))
	var errs []error
	for _, obj := range wave {
		err := d.kube.Delete(ctx, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName(), syncpkg.DeleteOptions{PropagationPolicy: policy})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s: %w", cache.KeyOf(obj), err))
		}
	}
	return false, errors.Join(errs...)
}

func syncOptions(app *v1alpha1.Application) v1alpha1.SyncOptions {
	if app.Spec.SyncPolicy == nil {
		return nil
	}
	return app.Spec.SyncPolicy.SyncOptions
}

func sortedKeys(m map[cache.ResourceKey]*unstructured.Unstructured) []cache.ResourceKey {
	keys := make([]cache.ResourceKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

func isDeleteHookOfType(obj *unstructured.Unstructured, t v1alpha1.HookType) bool {
	if t == v1alpha1.HookTypePreDelete {
		return syncpkg.IsPreDeleteHook(obj)
	}
	return syncpkg.IsPostDeleteHook(obj)
}

// runDeleteHooks creates the missing PreDelete/PostDelete hooks rendered from the
// Application's target revisions and waits for them (APP-064/065): created hooks end the pass;
// Progressing hooks wait; Degraded hooks are deleted (so a fixed definition can be retried)
// and fail the pass; all Healthy (or without health) completes it.
func (d *deletion) runDeleteHooks(ctx context.Context, hookType v1alpha1.HookType) (bool, error) {
	app := d.a.app
	targets, err := d.renderDeleteHooks(ctx, hookType)
	if err != nil {
		return false, err
	}
	live, err := d.liveObjects(ctx)
	if err != nil {
		return false, err
	}
	running := map[cache.ResourceKey]*unstructured.Unstructured{}
	for k, obj := range live {
		if isDeleteHookOfType(obj, hookType) {
			running[k] = obj
		}
	}
	st := d.a.st
	t := tracking.New(tracking.Config{InstanceLabelKey: st.InstanceLabelKey(), ControlPlaneNamespace: d.a.r.cfg.Namespace})
	instance := t.InstanceName(tracking.AppOf(app))
	lookup := d.a.r.cfg.ClusterLookup(st)
	created := 0
	for _, obj := range targets {
		gk := obj.GroupVersionKind().GroupKind()
		namespaced, err := d.cc.IsNamespaced(gk)
		if err != nil {
			return false, fmt.Errorf("failed to determine scope of %s hook %s: %w", hookType, cache.KeyOf(obj), err)
		}
		if namespaced && obj.GetNamespace() == "" {
			obj.SetNamespace(app.Spec.Destination.Namespace)
		}
		if !namespaced {
			obj.SetNamespace("")
		}
		key := cache.KeyOf(obj)
		if _, ok := running[key]; ok {
			continue
		}
		if err := project.ValidateSyncPermissions(ctx, d.proj, obj, project.ResourceScope{Known: true, Namespaced: namespaced}, d.a.cluster, lookup); err != nil {
			return false, fmt.Errorf("%s hook %s is not permitted: %w", hookType, key, err)
		}
		if err := t.SetAppInstance(obj, instance, app.Spec.Destination.Namespace, st.TrackingMethod(), st.InstallationID()); err != nil {
			return false, fmt.Errorf("failed to set app instance tracking on %s hook %s: %w", hookType, key, err)
		}
		d.a.log.Info("creating delete hook", "type", hookType, "resource", key.String())
		_, err = d.kube.Create(ctx, obj, syncpkg.CreateOptions{Validate: true})
		switch {
		case err == nil:
			// Wait until the live-state cache observed it and its health can be assessed.
			created++
			continue
		case !apierrors.IsAlreadyExists(err):
			return false, fmt.Errorf("create %s hook %s: %w", hookType, key, err)
		}
		existing, gerr := d.kube.Get(ctx, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())
		switch {
		case apierrors.IsNotFound(gerr):
			// Deleted in between: created again by the next pass.
			created++
		case gerr != nil:
			return false, fmt.Errorf("get %s hook %s: %w", hookType, key, gerr)
		case t.GetAppName(existing, st.TrackingMethod(), st.InstallationID()) == instance:
			// Created by an earlier pass but not (yet) in the live-state cache, e.g. a kind the
			// cache excludes: its health is assessed from the object just read.
			running[key] = existing
		default:
			// The name belongs to an object this Application does not track: the hook is
			// skipped, as upstream does, and deletion proceeds without it.
			d.a.log.Warn("delete hook skipped: an object with its name exists and is not tracked by the application", "type", hookType, "resource", key.String())
			d.a.r.cfg.Recorder.Eventf(app, nil, corev1.EventTypeWarning, core.EventReasonDeleteHookSkipped, core.ActionDelete,
				"%s hook %s skipped: an object with that name exists and is not tracked by the application", hookType, key)
		}
	}
	if created > 0 {
		return false, nil
	}
	rt := lua.New(st.ResourceOverrides(), lua.WithLogger(d.a.log))
	progressing := 0
	var failed []string
	var failedObjs []*unstructured.Unstructured
	for _, k := range sortedKeys(running) {
		obj := running[k]
		hs, err := health.Assess(obj, rt)
		if err != nil {
			return false, err
		}
		status := health.StatusHealthy
		if hs != nil {
			status = hs.Status
		}
		switch status {
		case health.StatusProgressing:
			progressing++
		case health.StatusDegraded:
			failed = append(failed, obj.GetNamespace()+"/"+obj.GetName())
			failedObjs = append(failedObjs, obj)
		}
	}
	if len(failed) > 0 {
		for _, obj := range failedObjs {
			if err := d.kube.Delete(ctx, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName(), syncpkg.DeleteOptions{PropagationPolicy: metav1.DeletePropagationBackground}); err != nil && !apierrors.IsNotFound(err) {
				d.a.log.Warn("failed to delete a failed delete hook", "resource", cache.KeyOf(obj).String(), "error", err)
			}
		}
		return false, fmt.Errorf("%s hook(s) failed: %s", hookType, strings.Join(failed, ", "))
	}
	if progressing > 0 {
		d.a.log.Info("waiting for delete hooks", "type", hookType, "count", progressing)
		return false, nil
	}
	return true, nil
}

// renderDeleteHooks renders the Application at its target revisions and returns its delete
// hooks of hookType.
func (d *deletion) renderDeleteHooks(ctx context.Context, hookType v1alpha1.HookType) ([]*unstructured.Unstructured, error) {
	res, err := d.a.r.cfg.Comparer.Compare(ctx, compare.Input{App: d.a.app, Project: d.proj, SkipTree: true, RollbackOrSyncOverride: true})
	if err != nil {
		return nil, fmt.Errorf("failed to render %s hooks: %w", hookType, err)
	}
	if res.FailedToLoad {
		return nil, fmt.Errorf("failed to render %s hooks: %s", hookType, strings.Join(res.ConditionMessages(v1alpha1.ApplicationConditionComparisonError), "; "))
	}
	var out []*unstructured.Unstructured
	for _, r := range res.ManifestResponses {
		if r == nil {
			continue
		}
		for _, m := range r.Manifests {
			obj := &unstructured.Unstructured{}
			if err := obj.UnmarshalJSON([]byte(m)); err != nil {
				return nil, fmt.Errorf("failed to decode a rendered manifest: %w", err)
			}
			if isDeleteHookOfType(obj, hookType) {
				out = append(out, obj)
			}
		}
	}
	return out, nil
}

// cleanupHooks deletes the live hooks of hookType whose delete policy matches their
// aggregated health (no policy: always) and waits until they are gone (APP-066).
func (d *deletion) cleanupHooks(ctx context.Context, hookType v1alpha1.HookType) (bool, error) {
	live, err := d.liveObjects(ctx)
	if err != nil {
		return false, err
	}
	rt := lua.New(d.a.st.ResourceOverrides(), lua.WithLogger(d.a.log))
	aggregated := health.StatusHealthy
	var hooks []*unstructured.Unstructured
	for _, k := range sortedKeys(live) {
		obj := live[k]
		if !isDeleteHookOfType(obj, hookType) {
			continue
		}
		hs, err := health.Assess(obj, rt)
		if err != nil {
			return false, err
		}
		status := health.StatusHealthy
		if hs != nil {
			status = hs.Status
		}
		if health.IsWorse(aggregated, status) {
			aggregated = status
		}
		hooks = append(hooks, obj)
	}
	pending := 0
	for _, obj := range hooks {
		policies := explicitDeletePolicies(obj)
		del := len(policies) == 0
		for _, p := range policies {
			if (p == v1alpha1.HookDeletePolicyHookFailed && aggregated == health.StatusDegraded) ||
				(p == v1alpha1.HookDeletePolicyHookSucceeded && aggregated == health.StatusHealthy) {
				del = true
			}
		}
		if !del {
			continue
		}
		pending++
		if obj.GetDeletionTimestamp() != nil {
			continue
		}
		if err := d.kube.Delete(ctx, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName(), syncpkg.DeleteOptions{PropagationPolicy: metav1.DeletePropagationBackground}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return pending == 0, nil
}

// explicitDeletePolicies returns the hook delete policies declared on obj, nil when it
// declares none (the cleanup stage deletes such hooks; sync.DeletePolicies would report the
// BeforeHookCreation default instead).
func explicitDeletePolicies(obj *unstructured.Unstructured) []v1alpha1.HookDeletePolicy {
	if len(syncpkg.AnnotationOptions(obj, v1alpha1.AnnotationKeyHookDeletePolicy)) == 0 &&
		len(syncpkg.AnnotationOptions(obj, syncpkg.AnnotationKeyHelmHookDeletePolicy)) == 0 {
		return nil
	}
	return syncpkg.DeletePolicies(obj)
}
