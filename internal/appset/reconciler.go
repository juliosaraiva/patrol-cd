package appset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// Reconciler reconciles ApplicationSets.
type Reconciler struct {
	cfg        *completed
	client     client.Client
	apiReader  client.Reader
	scheme     *runtime.Scheme
	recorder   events.EventRecorder
	generators generators.Set
	projects   *project.KubeLookup
	metrics    *metrics
}

// NewReconciler returns a Reconciler. c reads through the manager cache and must carry the
// OwnerIndex field index for Applications; apiReader reads the API server directly (Reverse
// deletion verification and Operation lookups). recorder may be nil (no events). With
// cfg.DryRun every write of c is a server-side dry run.
// conflictRequeueDelay is how long a pass that lost an optimistic-concurrency race waits before
// regenerating from the current object.
const conflictRequeueDelay = time.Second

func NewReconciler(cfg Config, c client.Client, apiReader client.Reader, recorder events.EventRecorder) (*Reconciler, error) {
	if c == nil {
		return nil, errors.New("appset: a client is required")
	}
	if apiReader == nil {
		apiReader = c
	}
	if cfg.Generators.Client == nil {
		cfg.Generators.Client = c
	}
	comp, err := cfg.complete()
	if err != nil {
		return nil, err
	}
	m, err := newMetrics(comp.Registry, c, comp.namespaces, comp.MetricsApplicationSetLabels)
	if err != nil {
		return nil, err
	}
	writer := c
	if comp.DryRun {
		writer = client.NewDryRunClient(c)
	}
	return &Reconciler{
		cfg:        comp,
		client:     writer,
		apiReader:  apiReader,
		scheme:     c.Scheme(),
		recorder:   recorder,
		generators: generators.New(comp.Generators),
		projects:   project.NewKubeLookup(c, comp.Namespace, !comp.DisableInCluster),
		metrics:    m,
	}, nil
}

func appSetAttr(appSet *v1alpha1.ApplicationSet) slog.Attr {
	return slog.String("applicationset", appSet.Namespace+"/"+appSet.Name)
}

// event records a Kubernetes event on appSet when a recorder is configured.
func (r *Reconciler) event(appSet *v1alpha1.ApplicationSet, eventType, reason, action, format string, args ...any) {
	if r.recorder == nil {
		return
	}
	r.recorder.Eventf(appSet, nil, eventType, reason, action, format, args...)
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log := r.cfg.log.With(slog.String("applicationset", req.String()))
	defer func() {
		if rec := recover(); rec != nil {
			log.ErrorContext(ctx, "recovered from panic", slog.Any("panic", rec), slog.String("stack", string(debug.Stack())))
			result, err = ctrl.Result{}, fmt.Errorf("panic while reconciling ApplicationSet %s: %v", req, rec)
		}
	}()
	if !r.cfg.namespaces.Allowed(req.Namespace) {
		return ctrl.Result{}, nil
	}
	appSet := &v1alpha1.ApplicationSet{}
	if err := r.client.Get(ctx, req.NamespacedName, appSet); err != nil {
		if apierrors.IsNotFound(err) {
			r.metrics.forget(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		log.InfoContext(ctx, "unable to get ApplicationSet", slog.Any("error", err))
		return ctrl.Result{}, err
	}
	start := r.cfg.now()
	defer func() { r.metrics.observeReconcile(appSet, r.cfg.now().Sub(start)) }()

	if appSet.DeletionTimestamp != nil {
		return r.reconcileDeletion(ctx, log, appSet)
	}
	return r.reconcile(ctx, log, appSet)
}

// reconcileDeletion processes a terminating ApplicationSet: Applications are released from its
// ownership when the policy forbids deleting them, resources finalizers are removed when
// resources are preserved, Reverse deletion runs step by step, and the finalizers are removed.
func (r *Reconciler) reconcileDeletion(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet) (ctrl.Result, error) {
	if !hasAppSetFinalizer(appSet) {
		return ctrl.Result{}, nil
	}
	current, err := r.currentApplications(ctx, appSet)
	if err != nil {
		return ctrl.Result{}, err
	}
	policy := EffectivePolicy(appSet, r.cfg.Policy, r.cfg.EnablePolicyOverride)
	if !policy.AllowDelete() {
		log.DebugContext(ctx, "the ApplicationSet policy does not allow deleting Applications; releasing them")
		if err := r.removeOwnerReferences(ctx, appSet, current); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if appSet.Spec.SyncPolicy != nil && appSet.Spec.SyncPolicy.PreserveResourcesOnDeletion {
			if err := r.stripResourcesFinalizers(ctx, log, appSet, current); err != nil {
				return ctrl.Result{}, err
			}
		}
		if IsDeletionOrderReversed(appSet) {
			requeue, err := r.reverseDeletion(ctx, log, appSet, current)
			if err != nil {
				return ctrl.Result{}, err
			}
			if requeue > 0 {
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
		}
	}
	if err := r.patchFinalizers(ctx, appSet, slices.DeleteFunc(slices.Clone(appSet.Finalizers), isAppSetFinalizer)); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// isAppSetFinalizer matches the ApplicationSet finalizer and the resources finalizer Argo CD
// uses on ApplicationSets (migrated manifests).
func isAppSetFinalizer(f string) bool {
	return f == v1alpha1.FinalizerApplicationSet || f == v1alpha1.FinalizerResources
}

func hasAppSetFinalizer(appSet *v1alpha1.ApplicationSet) bool {
	return slices.ContainsFunc(appSet.Finalizers, isAppSetFinalizer)
}

// needsFinalizer reports whether the controller adds its finalizer: ordered deletion
// (progressive syncs enabled) or preserved resources need to act before the garbage collector
// deletes the Applications.
func (r *Reconciler) needsFinalizer(appSet *v1alpha1.ApplicationSet) bool {
	if r.cfg.EnableProgressiveSyncs && IsDeletionOrderReversed(appSet) {
		return true
	}
	return appSet.Spec.SyncPolicy != nil && appSet.Spec.SyncPolicy.PreserveResourcesOnDeletion
}

// patchFinalizers writes the finalizer list of appSet (optimistic lock).
func (r *Reconciler) patchFinalizers(ctx context.Context, appSet *v1alpha1.ApplicationSet, finalizers []string) error {
	orig := appSet.DeepCopy()
	appSet.Finalizers = finalizers
	if len(finalizers) == 0 {
		appSet.Finalizers = nil
	}
	return r.client.Patch(ctx, appSet, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// reconcile generates, validates and applies the Applications of a live ApplicationSet.
func (r *Reconciler) reconcile(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet) (ctrl.Result, error) {
	if r.needsFinalizer(appSet) && !slices.Contains(appSet.Finalizers, v1alpha1.FinalizerApplicationSet) {
		if err := r.patchFinalizers(ctx, appSet, append(slices.Clone(appSet.Finalizers), v1alpha1.FinalizerApplicationSet)); err != nil {
			return ctrl.Result{}, fmt.Errorf("error adding the ApplicationSet finalizer: %w", err)
		}
	}
	if err := CheckInvalidGenerators(appSet); err != nil {
		log.WarnContext(ctx, err.Error())
	}

	generated, reason, err := GenerateApplications(ctx, log, appSet, r.generators)
	if err != nil {
		log.ErrorContext(ctx, "unable to generate applications", slog.Any("error", err))
		if ctx.Err() != nil {
			return ctrl.Result{}, ctx.Err()
		}
		_ = r.setCondition(ctx, appSet, errorCondition(reason, err), false)
		return ctrl.Result{RequeueAfter: RequeueOnValidationError}, nil
	}

	validationErrors, validIdx, err := r.validate(ctx, appSet, generated)
	if err != nil {
		log.ErrorContext(ctx, "error occurred during application validation", slog.Any("error", err))
		if ctx.Err() != nil {
			return ctrl.Result{}, ctx.Err()
		}
		_ = r.setCondition(ctx, appSet, errorCondition(v1alpha1.ApplicationSetReasonApplicationValidationError, err), true)
		return ctrl.Result{RequeueAfter: RequeueOnValidationError}, nil
	}

	current, err := r.currentApplications(ctx, appSet)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get current applications for application set: %w", err)
	}

	differ, err := newIgnoreDiffer(appSet.Spec.IgnoreApplicationDifferences, log)
	if err != nil {
		_ = r.setCondition(ctx, appSet, errorCondition(v1alpha1.ApplicationSetReasonUpdateApplicationError, err), true)
		return ctrl.Result{}, err
	}

	toSync := map[string]bool{}
	if r.cfg.EnableProgressiveSyncs {
		switch {
		case !IsRollingSyncStrategy(appSet):
			if len(appSet.Status.ApplicationStatus) > 0 {
				log.InfoContext(ctx, "removing progressive sync application statuses", slog.Int("count", len(appSet.Status.ApplicationStatus)))
				if err := r.setApplicationStatus(ctx, appSet, nil); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to clear previous AppSet application statuses: %w", err)
				}
			}
		default:
			toSync, err = r.progressiveSync(ctx, log, appSet, current, generated, differ)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to perform progressive sync reconciliation for application set: %w", err)
			}
		}
	} else if len(appSet.Status.ApplicationStatus) > 0 {
		log.InfoContext(ctx, "progressive syncs are disabled; removing application statuses", slog.Int("count", len(appSet.Status.ApplicationStatus)))
		if err := r.setApplicationStatus(ctx, appSet, nil); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to clear AppSet application statuses when progressive syncs are disabled: %w", err)
		}
	}

	if len(validationErrors) > 0 {
		_ = r.setCondition(ctx, appSet, errorCondition(v1alpha1.ApplicationSetReasonApplicationValidationError, validationSummary(log, validationErrors)), true)
	}

	valid := make([]v1alpha1.Application, 0, len(generated))
	for i := range generated {
		if validIdx[i] {
			valid = append(valid, generated[i])
		}
	}
	var syncs []syncRequest
	if r.cfg.EnableProgressiveSyncs && RollingSyncEnabled(appSet) {
		valid, syncs = prepareRollingSync(log, appSet, toSync, valid)
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Name < valid[j].Name })

	policy := EffectivePolicy(appSet, r.cfg.Policy, r.cfg.EnablePolicyOverride)
	written := newWriteResult()
	if policy.AllowUpdate() {
		if err := r.createOrUpdate(ctx, log, appSet, valid, differ, written); err != nil {
			_ = r.setCondition(ctx, appSet, errorCondition(v1alpha1.ApplicationSetReasonUpdateApplicationError, err), true)
			return ctrl.Result{}, err
		}
	} else {
		if err := r.createOrUpdate(ctx, log, appSet, createOnly(current, valid), differ, written); err != nil {
			_ = r.setCondition(ctx, appSet, errorCondition(v1alpha1.ApplicationSetReasonCreateApplicationError, err), true)
			return ctrl.Result{}, err
		}
	}

	var requeue time.Duration
	if len(syncs) > 0 && policy.AllowUpdate() {
		live := map[string]*v1alpha1.Application{}
		for _, app := range written.overlay(current) {
			live[app.Name] = &app
		}
		requeue, err = r.triggerSyncs(ctx, log, appSet, syncs, live)
		if err != nil {
			_ = r.setCondition(ctx, appSet, errorCondition(v1alpha1.ApplicationSetReasonSyncApplicationError, err), true)
			return ctrl.Result{}, err
		}
	}

	if policy.AllowDelete() {
		if err := r.deleteStale(ctx, log, appSet, current, generated, written); err != nil {
			_ = r.setCondition(ctx, appSet, errorCondition(v1alpha1.ApplicationSetReasonDeleteApplicationError, err), true)
			return ctrl.Result{}, err
		}
	}

	current, err = r.currentApplications(ctx, appSet)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get current applications for application set: %w", err)
	}
	if err := r.updateResourcesStatus(ctx, appSet, written.overlay(current)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update resources status for application set: %w", err)
	}

	if generators.RefreshRequired(appSet) {
		if err := r.removeRefreshAnnotation(ctx, appSet); err != nil {
			if apierrors.IsConflict(err) {
				// The ApplicationSet changed since this pass read it (a newer refresh request,
				// or the cache lagged behind this controller's own writes): the requeued
				// pass regenerates from the current object, so this is not an error.
				log.DebugContext(ctx, "the ApplicationSet changed while removing the refresh annotation; requeueing", slog.Any("error", err))
				return ctrl.Result{RequeueAfter: conflictRequeueDelay}, nil
			}
			log.WarnContext(ctx, "error occurred while removing the refresh annotation", slog.Any("error", err))
			_ = r.setCondition(ctx, appSet, errorCondition(v1alpha1.ApplicationSetReasonRefreshApplicationError, err), true)
			return ctrl.Result{}, err
		}
	}

	requeueAfter := r.generators.MinRequeueAfter(appSet.Spec.Generators)
	if len(validationErrors) == 0 {
		if err := r.setCondition(ctx, appSet, condition{
			Type: v1alpha1.ApplicationSetConditionResourcesUpToDate, Status: metav1.ConditionTrue,
			Reason: v1alpha1.ApplicationSetReasonApplicationSetUpToDate, Message: msgUpToDate,
		}, true); err != nil {
			return ctrl.Result{}, err
		}
	} else if requeueAfter == 0 {
		requeueAfter = RequeueOnValidationError
	}
	if requeue > 0 && (requeueAfter == 0 || requeue < requeueAfter) {
		requeueAfter = requeue
	}
	log.InfoContext(ctx, "end reconcile", slog.Duration("requeueAfter", requeueAfter), slog.Int("applications", len(generated)))
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func errorCondition(reason string, err error) condition {
	if reason == "" {
		reason = v1alpha1.ApplicationSetReasonErrorOccurred
	}
	return condition{Type: v1alpha1.ApplicationSetConditionErrorOccurred, Status: metav1.ConditionTrue, Reason: reason, Message: err.Error()}
}

// validationSummary returns the error message of the last invalid Application (by qualified
// name), with " (and N more)" when several are invalid.
func validationSummary(log *slog.Logger, errs map[string]error) error {
	names := make([]string, 0, len(errs))
	for n := range errs {
		names = append(names, n)
	}
	sort.Strings(names)
	msg := ""
	for _, n := range names {
		msg = errs[n].Error()
		log.Error("validation error found during application validation", slog.String("application", n), slog.String("error", msg))
	}
	if len(errs) > 1 {
		msg = fmt.Sprintf("%s (and %d more)", msg, len(errs)-1)
	}
	return errors.New(msg)
}

// validate checks every generated Application: unique name within the ApplicationSet (the
// first occurrence of a name stays valid); the project exists and permits the ApplicationSet's
// namespace (spec.sourceNamespaces, for ApplicationSets outside the control-plane namespace, so
// a templated project cannot escalate); the destination resolves to a registered cluster and
// is permitted by the project. It returns the errors by qualified name and the validity of each
// Application by index. Lookup failures are returned as an error.
func (r *Reconciler) validate(ctx context.Context, appSet *v1alpha1.ApplicationSet, apps []v1alpha1.Application) (map[string]error, []bool, error) {
	invalid := map[string]error{}
	valid := make([]bool, len(apps))
	if len(apps) == 0 {
		return invalid, valid, nil
	}
	idx, err := r.clusterIndex(ctx)
	if err != nil {
		return nil, nil, err
	}
	lookups := project.Lookups{Projects: r.projects, Repositories: r.projects, Clusters: idx, GlobalProjects: r.cfg.GlobalProjects}
	names := map[string]bool{}
	for i := range apps {
		app := &apps[i]
		key := app.Namespace + "/" + app.Name
		if names[app.Name] {
			invalid[key] = fmt.Errorf("ApplicationSet %s contains applications with duplicate name: %s", appSet.Name, app.Name)
			continue
		}
		names[app.Name] = true
		projName := app.Spec.GetProject()
		proj, _, err := project.ResolveAppProject(ctx, app, r.cfg.Namespace, lookups)
		if err != nil {
			var notPermitted *project.NotPermittedError
			switch {
			case project.IsNotFound(err):
				invalid[key] = fmt.Errorf("application references project %s which does not exist", projName)
				continue
			case errors.As(err, &notPermitted):
				invalid[key] = notPermitted
				continue
			}
			return nil, nil, err
		}
		cluster, err := project.ResolveDestination(ctx, app.Spec.Destination, idx)
		if err != nil {
			invalid[key] = fmt.Errorf("application destination spec is invalid: %s", err.Error())
			continue
		}
		permitted, err := project.IsDestinationPermitted(ctx, proj, project.DestinationOf(cluster, app.Spec.Destination.Namespace), idx)
		if err != nil {
			return nil, nil, err
		}
		if !permitted {
			server := cluster.Spec.Server
			if app.Spec.Destination.Name != "" {
				server = cluster.Spec.Name
			}
			invalid[key] = fmt.Errorf("application destination server '%s' and namespace '%s' do not match any of the allowed destinations in project '%s'",
				server, app.Spec.Destination.Namespace, proj.Name)
			continue
		}
		valid[i] = true
	}
	return invalid, valid, nil
}

// removeRefreshAnnotation removes the refresh annotation (optimistic lock: a refresh requested
// meanwhile makes the patch fail, and the retry regenerates).
func (r *Reconciler) removeRefreshAnnotation(ctx context.Context, appSet *v1alpha1.ApplicationSet) error {
	orig := appSet.DeepCopy()
	delete(appSet.Annotations, v1alpha1.AnnotationKeyApplicationSetRefresh)
	if len(appSet.Annotations) == 0 {
		appSet.Annotations = nil
	}
	return r.client.Patch(ctx, appSet, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}
