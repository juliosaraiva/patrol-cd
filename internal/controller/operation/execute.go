package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// requeueSoonAfter is the delay of "continue immediately" requeues (the status write that
// precedes them normally triggers the next reconcile first).
const requeueSoonAfter = 50 * time.Millisecond

func requeueSoon() reconcile.Result { return reconcile.Result{RequeueAfter: requeueSoonAfter} }

// run advances a Running Operation by one step.
func (s *step) run(ctx context.Context) (reconcile.Result, error) {
	op := s.op
	if op.Spec.Cancel {
		if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
			st.Phase = v1alpha1.OperationTerminating
			st.Message = msgTerminating
		}); err != nil {
			return reconcile.Result{}, err
		}
		return s.terminate(ctx, "")
	}
	if t := s.r.cfg.Params.SyncTimeout; t > 0 && op.Status.StartedAt != nil && s.r.cfg.Clock.Since(op.Status.StartedAt.Time) > t {
		s.log.Info("terminating operation due to timeout", "startedAt", op.Status.StartedAt.Time, "timeout", t)
		if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
			st.Phase = v1alpha1.OperationTerminating
			st.Message = msgTerminatingTimeout
		}); err != nil {
			return reconcile.Result{}, err
		}
		return s.terminate(ctx, causeSyncTimeout)
	}
	if op.Status.FinishedAt != nil {
		// A failed attempt waits for its retry.
		retryAt, err := NextRetryAt(EffectiveRetry(op, s.app), op.Status.FinishedAt.Time, op.Status.RetryCount)
		if err != nil {
			return s.complete(ctx, v1alpha1.OperationError, err.Error(), v1alpha1.OperationReasonInfrastructureError)
		}
		if wait := retryAt.Sub(s.r.cfg.Clock.Now()); wait > 0 {
			s.log.Debug("waiting for the retry backoff", "retryAt", retryAt.Format(time.RFC3339))
			return reconcile.Result{RequeueAfter: wait}, nil
		}
		if err := s.startRetry(ctx); err != nil {
			return reconcile.Result{}, err
		}
	}
	st, err := s.r.cfg.CurrentSettings()
	if err != nil {
		return reconcile.Result{}, err
	}
	proj, err := s.r.cfg.ResolveProject(ctx, s.app, st)
	if err != nil {
		return s.attemptFailed(ctx, v1alpha1.OperationError, fmt.Sprintf("Failed to load application project: %v", err), v1alpha1.OperationReasonPermissionDenied)
	}
	var start *time.Time
	if op.Status.StartedAt != nil {
		start = &op.Status.StartedAt.Time
	}
	if allowed, werr := project.CanSync(proj, s.app, !s.isAutomated(), start, s.r.cfg.Clock.Now()); werr != nil || !allowed {
		msg := msgSyncWindowBlocked
		if werr != nil {
			msg += ": " + werr.Error()
		}
		if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) { st.Message = msg }); err != nil {
			return reconcile.Result{}, err
		}
		if err := s.updateApplication(ctx, nil); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: s.windowRecheck(proj)}, nil
	}

	x, res, err := s.prepare(ctx, st, proj)
	if err != nil {
		return reconcile.Result{}, err
	}
	if res != nil {
		return *res, nil
	}
	if op.Status.Plan == nil || (len(op.Status.Plan.ResolvedRevisions) == 0 && len(x.cmp.Revisions) > 0) {
		return s.persistPlan(ctx, x)
	}
	return s.syncStep(ctx, x)
}

// startRetry begins the next attempt of a failed Operation (SYNC-026/028): results, progress
// and finishedAt are reset; with retry.refresh the persisted revisions are dropped so the
// attempt resolves the latest ones.
func (s *step) startRetry(ctx context.Context) error {
	refresh := false
	if r := EffectiveRetry(s.op, s.app); r != nil && r.Refresh {
		refresh = true
	}
	extra := ""
	if refresh {
		extra = " with latest revisions"
	}
	msg := fmt.Sprintf("Retrying operation%s. Attempt #%d", extra, s.op.Status.RetryCount)
	s.log.Info(msg)
	return s.patch(ctx, func(st *v1alpha1.OperationStatus) {
		st.FinishedAt = nil
		st.Message = msg
		st.Progress = nil
		if st.SyncResult != nil {
			st.SyncResult.Resources = nil
			st.SyncResult.ManagedNamespaceMetadata = nil
		}
		if refresh {
			st.Plan = nil
			st.SyncResult = nil
		}
		s.setCondition(st, v1alpha1.OperationConditionRetrying, metav1.ConditionFalse, v1alpha1.OperationReasonRetryScheduled, msg)
	})
}

// execution is what one step needs: the comparison at the operation's revisions and the
// sync engine input built from it.
type execution struct {
	st      *settings.Settings
	proj    *v1alpha1.AppProject
	cluster *v1alpha1.Cluster
	cmp     *compare.Result
	sources v1alpha1.ApplicationSources
	multi   bool
	engine  *syncpkg.Engine
	input   *syncpkg.Input
}

// prepare compares the Application at the operation's revisions (the persisted resolved
// ones after the first step) and builds the engine input. A non-nil Result means the step
// ended (failure persisted).
func (s *step) prepare(ctx context.Context, st *settings.Settings, proj *v1alpha1.AppProject) (*execution, *reconcile.Result, error) {
	op, app := s.op, s.app
	fail := func(phase v1alpha1.OperationPhase, msg, reason string) (*execution, *reconcile.Result, error) {
		res, err := s.attemptFailed(ctx, phase, msg, reason)
		return nil, &res, err
	}
	lookup := s.r.cfg.ClusterLookup(st)
	if issues := project.ValidateApplication(ctx, app, proj, lookup, project.ValidateOptions{ControlPlaneNamespace: s.r.cfg.Namespace, SkipRefSources: true, SkipSyncOptions: true}); len(issues) > 0 {
		var msgs []string
		for _, i := range issues {
			msgs = append(msgs, i.Type+": "+i.Message)
		}
		return fail(v1alpha1.OperationError, strings.Join(msgs, ";"), v1alpha1.OperationReasonInvalidSpec)
	}
	cluster, err := s.r.cfg.DestinationCluster(ctx, app.Spec.Destination, st)
	if err != nil {
		return fail(v1alpha1.OperationError, fmt.Sprintf("Failed to get destination cluster: %v", err), v1alpha1.OperationReasonInvalidSpec)
	}

	sources, multi, revisions := s.operationSources()
	if s.overridesSources() {
		if msgs := validateOperationSources(ctx, app, proj, sources, multi, lookup); len(msgs) > 0 {
			return fail(v1alpha1.OperationError, strings.Join(msgs, ";"), v1alpha1.OperationReasonInvalidSpec)
		}
	}
	in := compare.Input{
		App:                    app,
		Project:                proj,
		Sources:                sources,
		Revisions:              revisions,
		HasMultipleSources:     multi,
		RollbackOrSyncOverride: true,
		SkipTree:               true,
	}
	planned := op.Status.Plan != nil && len(op.Status.Plan.ResolvedRevisions) == len(sources) && len(sources) > 0
	if planned {
		in.Revisions = slices.Clone(op.Status.Plan.ResolvedRevisions)
	} else {
		in.NoRevisionCache = true
	}
	if op.Spec.Sync != nil && len(op.Spec.Sync.Manifests) > 0 {
		in.LocalManifests = op.Spec.Sync.Manifests
	}
	res, err := s.r.cfg.Comparer.Compare(ctx, in)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, err
		}
		return fail(v1alpha1.OperationError, fmt.Sprintf("Failed to compare desired state: %v", err), v1alpha1.OperationReasonManifestGenerationFailed)
	}
	if msg := formatErrorConditions(res.Conditions); msg != "" {
		reason := v1alpha1.OperationReasonManifestGenerationFailed
		if !planned {
			reason = v1alpha1.OperationReasonRevisionResolutionFailed
		}
		return fail(v1alpha1.OperationError, msg, reason)
	}

	user := ""
	imp := st.Impersonation()
	sa, err := project.SelectServiceAccount(proj, app, cluster.Spec.Server, project.ImpersonationSettings{Enabled: imp.Enabled, Enforced: imp.Enforced})
	if err != nil {
		return fail(v1alpha1.OperationError, fmt.Sprintf("failed to find a matching service account to impersonate: %v", err), v1alpha1.OperationReasonPermissionDenied)
	}
	user = sa.UserName
	engine, err := s.r.cfg.Engines.Engine(ctx, cluster, user, st)
	if err != nil {
		return fail(v1alpha1.OperationError, err.Error(), v1alpha1.OperationReasonInfrastructureError)
	}
	cc, err := s.r.cfg.LiveState.ClusterCache(ctx, cluster)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, err
		}
		s.log.Warn("live-state cache unavailable; requeueing", "error", err)
		return nil, &reconcile.Result{RequeueAfter: s.r.cfg.PollInterval}, nil
	}

	x := &execution{st: st, proj: proj, cluster: cluster, cmp: res, sources: sources, multi: multi, engine: engine}
	x.input = s.engineInput(ctx, x, cc.IsNamespaced, lookup)
	return x, nil, nil
}

// overridesSources reports whether the Operation syncs other sources than the Application's:
// a sync with spec.sync.source(s) or a rollback to a history entry.
func (s *step) overridesSources() bool {
	if s.op.Spec.Rollback != nil {
		return true
	}
	sync := s.op.Spec.Sync
	return sync != nil && (sync.Source != nil || len(sync.Sources) > 0)
}

// validateOperationSources checks the sources an Operation overrides against the project
// exactly like the Application's own sources (source fields, spec.sourceRepos, destination;
// upstream ValidatePermissions). Operations are created directly by Kubernetes clients, so
// the API server's checks may not have run; the controller enforces the project itself.
func validateOperationSources(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, sources v1alpha1.ApplicationSources, multi bool, lookup project.ClusterLookup) []string {
	spec := app.Spec.DeepCopy()
	spec.SourceHydrator = nil
	spec.Source, spec.Sources = nil, nil
	switch {
	case multi:
		spec.Sources = slices.Clone(sources)
	case len(sources) > 0:
		src := sources[0]
		spec.Source = &src
	}
	issues, err := project.ValidatePermissions(ctx, spec, proj, lookup)
	if err != nil {
		return []string{v1alpha1.ApplicationConditionUnknownError + ": " + err.Error()}
	}
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Type+": "+i.Message)
	}
	return out
}

// operationSources returns the sources, multi-source flag and requested revisions of the
// Operation: a rollback's history entry, a sync's source override, or the Application's
// sources. A retry with retry.refresh requests the latest revisions.
func (s *step) operationSources() (v1alpha1.ApplicationSources, bool, []string) {
	op, app := s.op, s.app
	if op.Spec.Rollback != nil {
		if h, ok := core.FindHistory(app.Status.History, op.Spec.Rollback.ID); ok {
			if len(h.Sources) > 0 {
				return slices.Clone(h.Sources), true, slices.Clone(h.Revisions)
			}
			if h.Source != nil {
				return v1alpha1.ApplicationSources{*h.Source}, false, []string{h.Revision}
			}
		}
	}
	sources := app.Spec.GetSources()
	multi := app.Spec.HasMultipleSources()
	var revisions []string
	if sync := op.Spec.Sync; sync != nil {
		switch {
		case len(sync.Sources) > 0:
			sources, multi = slices.Clone(sync.Sources), true
		case sync.Source != nil:
			sources, multi = v1alpha1.ApplicationSources{*sync.Source}, false
		}
		if multi {
			revisions = slices.Clone(sync.Revisions)
		} else if sync.Revision != "" {
			revisions = []string{sync.Revision}
		}
	}
	if r := EffectiveRetry(op, app); r != nil && r.Refresh && op.Status.RetryCount > 0 {
		revisions = nil
	}
	if len(revisions) != len(sources) {
		revisions = nil
	}
	return sources, multi, revisions
}

// engineInput builds the sync engine input from the comparison.
func (s *step) engineInput(ctx context.Context, x *execution, isNamespaced func(gk schema.GroupKind) (bool, error), lookup project.ClusterLookup) *syncpkg.Input {
	op, app, st := s.op, s.app, x.st
	t := tracking.New(tracking.Config{InstanceLabelKey: st.InstanceLabelKey(), ControlPlaneNamespace: s.r.cfg.Namespace})
	method, installationID := st.TrackingMethod(), st.InstallationID()
	appID := tracking.AppOf(app)
	instance := t.InstanceName(appID)
	destNS := app.Spec.Destination.Namespace
	in := &syncpkg.Input{
		OperationUID: string(op.UID),
		Attempt:      op.Status.RetryCount,
		App: syncpkg.AppRef{
			Name:        app.Name,
			Namespace:   app.Namespace,
			Annotations: app.Annotations,
			Destination: v1alpha1.ApplicationDestination{Server: x.cluster.Spec.Server, Name: app.Spec.Destination.Name, Namespace: destNS},
		},
		Revision:     strings.Join(x.cmp.Revisions, ","),
		Targets:      x.cmp.Targets,
		Live:         x.cmp.Live,
		DiffResults:  x.cmp.DiffResults,
		IsNamespaced: isNamespaced,
		SetTracking: func(obj *unstructured.Unstructured) error {
			return t.SetAppInstance(obj, instance, destNS, method, installationID)
		},
		SharedWith: func(live *unstructured.Unstructured) (string, bool) {
			other, shared := t.DetectSharedResource(live, appID, method, installationID)
			return tracking.DisplayName(other), shared
		},
		IsSelfReferenced: func(live, target *unstructured.Unstructured) bool {
			return t.IsSelfReferencedObject(live, target, method, installationID)
		},
		Permission: func(obj *unstructured.Unstructured, namespaced bool) error {
			return project.ValidateSyncPermissions(ctx, x.proj, obj, project.ResourceScope{Known: true, Namespaced: namespaced}, x.cluster, lookup)
		},
		Normalizer: x.cmp.Normalizer,
	}
	var appOptions v1alpha1.SyncOptions
	if app.Spec.SyncPolicy != nil {
		appOptions = app.Spec.SyncPolicy.SyncOptions
	}
	in.SyncOptions = MergeSyncOptions(appOptions, nil)
	if sync := op.Spec.Sync; sync != nil {
		in.SyncOptions = MergeSyncOptions(appOptions, sync.SyncOptions)
		in.Prune = sync.Prune
		in.DryRun = sync.DryRun
		in.Strategy = sync.SyncStrategy
		in.Resources = sync.Resources
	}
	if op.Spec.Rollback != nil {
		// Rollbacks skip hooks (upstream uses the apply strategy).
		force := in.Strategy != nil && ((in.Strategy.Apply != nil && in.Strategy.Apply.Force) || (in.Strategy.Hook != nil && in.Strategy.Hook.Force))
		in.Strategy = &v1alpha1.SyncStrategy{Apply: &v1alpha1.SyncStrategyApply{Force: force}}
	}
	if app.Spec.SyncPolicy != nil && app.Spec.SyncPolicy.ManagedNamespaceMetadata != nil {
		in.ManagedNamespaceMetadata = app.Spec.SyncPolicy.ManagedNamespaceMetadata.DeepCopy()
		in.TrackManagedNamespace = true
	}
	if op.Status.StartedAt != nil {
		in.StartedAt = op.Status.StartedAt.Time
	}
	if op.Status.SyncResult != nil {
		in.Previous = op.Status.SyncResult.Resources
	}
	in.Progress = op.Status.Progress
	return in
}

// persistPlan records the resolution step: resolved revisions (before any mutation), the
// rendered-input digest, the resolved destination, planning warnings and task totals.
func (s *step) persistPlan(ctx context.Context, x *execution) (reconcile.Result, error) {
	plan := &v1alpha1.OperationPlan{
		ResolvedRevisions:   slices.Clone(x.cmp.Revisions),
		RenderedInputDigest: renderedDigest(x.cmp, x.input),
		Destination:         x.input.App.Destination,
	}
	var total int64
	if p, err := x.engine.Plan(ctx, x.input); err == nil {
		plan.Warnings = p.Warnings
		total = int64(p.TotalTasks)
	} else if ctx.Err() != nil {
		return reconcile.Result{}, err
	} else {
		s.log.Debug("planning preview failed; the first step will report it", "error", err)
	}
	result := s.syncResultSummary(x)
	if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
		st.Plan = plan
		if st.SyncResult == nil {
			st.SyncResult = &v1alpha1.SyncOperationResult{}
		}
		st.SyncResult.Revision, st.SyncResult.Revisions = result.Revision, result.Revisions
		st.SyncResult.Source, st.SyncResult.Sources = result.Source, result.Sources
		if st.Progress == nil {
			st.Progress = &v1alpha1.OperationProgress{}
		}
		st.Progress.TotalTasks = total
		s.setCondition(st, v1alpha1.OperationConditionPlanned, metav1.ConditionTrue, v1alpha1.OperationReasonRevisionsResolved,
			"resolved revisions "+strings.Join(plan.ResolvedRevisions, ", "))
	}); err != nil {
		return reconcile.Result{}, err
	}
	s.log.Info("operation planned", "revisions", plan.ResolvedRevisions, "tasks", total)
	if err := s.updateApplication(ctx, nil); err != nil {
		return reconcile.Result{}, err
	}
	return requeueSoon(), nil
}

// syncResultSummary returns the synced revisions and sources (SYNC-075, SYNC-089).
func (s *step) syncResultSummary(x *execution) v1alpha1.SyncOperationResult {
	var out v1alpha1.SyncOperationResult
	if x.multi {
		out.Revisions = slices.Clone(x.cmp.Revisions)
		out.Sources = slices.Clone(x.sources)
		return out
	}
	if len(x.cmp.Revisions) > 0 {
		out.Revision = x.cmp.Revisions[0]
	}
	if len(x.sources) > 0 {
		src := x.sources[0]
		out.Source = &src
	}
	return out
}

// syncStep runs one engine step and persists its outcome.
func (s *step) syncStep(ctx context.Context, x *execution) (reconcile.Result, error) {
	if d := renderedDigest(x.cmp, x.input); s.op.Status.Plan.RenderedInputDigest != "" && d != s.op.Status.Plan.RenderedInputDigest {
		const w = "rendered manifests changed since the operation was planned"
		if !slices.Contains(s.op.Status.Plan.Warnings, w) {
			s.log.Warn(w)
			if err := s.patch(ctx, func(st *v1alpha1.OperationStatus) {
				st.Plan.Warnings = append(st.Plan.Warnings, w)
			}); err != nil {
				return reconcile.Result{}, err
			}
		}
	}
	res, err := x.engine.Step(ctx, x.input)
	if err != nil {
		if ctx.Err() != nil {
			return reconcile.Result{}, err
		}
		if syncpkg.IsTransient(err) {
			s.log.Info("transient error during sync step; retrying", "error", err)
			if res != nil {
				if perr := s.persistStep(ctx, res); perr != nil {
					return reconcile.Result{}, perr
				}
			}
			return reconcile.Result{RequeueAfter: s.r.cfg.PollInterval}, nil
		}
		return s.attemptFailed(ctx, v1alpha1.OperationError, err.Error(), v1alpha1.OperationReasonInfrastructureError)
	}
	if err := s.persistStep(ctx, res); err != nil {
		return reconcile.Result{}, err
	}
	switch res.Phase {
	case v1alpha1.OperationRunning:
		if err := s.updateApplication(ctx, nil); err != nil {
			return reconcile.Result{}, err
		}
		if res.RequeueAfter <= 0 {
			return requeueSoon(), nil
		}
		return reconcile.Result{RequeueAfter: res.RequeueAfter}, nil
	case v1alpha1.OperationSucceeded:
		return s.complete(ctx, v1alpha1.OperationSucceeded, res.Message, v1alpha1.OperationReasonSucceeded)
	case v1alpha1.OperationFailed:
		return s.attemptFailed(ctx, v1alpha1.OperationFailed, res.Message, v1alpha1.OperationReasonSyncFailed)
	default:
		return s.attemptFailed(ctx, v1alpha1.OperationError, res.Message, v1alpha1.OperationReasonInfrastructureError)
	}
}

// persistStep writes a step's results, progress and message (the phase is decided by the
// caller).
func (s *step) persistStep(ctx context.Context, res *syncpkg.StepResult) error {
	return s.patch(ctx, func(st *v1alpha1.OperationStatus) {
		if st.SyncResult == nil {
			st.SyncResult = &v1alpha1.SyncOperationResult{}
		}
		st.SyncResult.Resources = res.Results
		if res.ManagedNamespaceMetadata != nil {
			st.SyncResult.ManagedNamespaceMetadata = res.ManagedNamespaceMetadata
		}
		if res.Progress != nil {
			st.Progress = res.Progress
		}
		if res.Phase == v1alpha1.OperationRunning {
			st.Message = res.Message
		}
	})
}

// terminate cancels a Terminating Operation (spec.cancel or sync timeout): running hooks are
// cleaned up and the Operation ends Cancelled (or Error when cleanup failed). Applied
// resources are not rolled back and no retry follows.
func (s *step) terminate(ctx context.Context, cause string) (reconcile.Result, error) {
	if cause == "" && s.op.Status.Message == msgTerminatingTimeout {
		cause = causeSyncTimeout
	}
	finish := func(phase v1alpha1.OperationPhase, msg string) (reconcile.Result, error) {
		if cause != "" {
			msg = fmt.Sprintf("%s, triggered by %s", msg, cause)
		}
		if n := s.op.Status.RetryCount; n > 0 {
			msg = fmt.Sprintf("%s (retried %d times).", msg, n)
		}
		reason := v1alpha1.OperationReasonCancelled
		if phase != v1alpha1.OperationCancelled {
			reason = v1alpha1.OperationReasonInfrastructureError
		}
		return s.complete(ctx, phase, msg, reason)
	}
	st, err := s.r.cfg.CurrentSettings()
	if err != nil {
		return reconcile.Result{}, err
	}
	proj, err := s.r.cfg.ResolveProject(ctx, s.app, st)
	if err != nil || s.op.Status.Plan == nil {
		// Nothing was applied yet (the plan is persisted before any mutation), or nothing can
		// be cleaned up without the project.
		return finish(v1alpha1.OperationCancelled, msgTerminated)
	}
	x, failure, err := s.prepareForTermination(ctx, st, proj)
	if err != nil {
		return reconcile.Result{}, err
	}
	if failure != "" {
		// Nothing is cleaned up: the hooks stay for the user (upstream SyncAppState returns
		// before Terminate on these errors).
		s.log.Warn("operation termination failed", "error", failure)
		return finish(v1alpha1.OperationError, "Operation termination had errors: "+failure)
	}
	out, err := x.engine.Terminate(ctx, x.input)
	if err != nil {
		if ctx.Err() != nil {
			return reconcile.Result{}, err
		}
		if syncpkg.IsTransient(err) {
			return reconcile.Result{RequeueAfter: s.r.cfg.PollInterval}, nil
		}
		return finish(v1alpha1.OperationError, "Operation termination had errors: "+err.Error())
	}
	if err := s.persistStep(ctx, out); err != nil {
		return reconcile.Result{}, err
	}
	return finish(out.Phase, out.Message)
}

// prepareForTermination is prepare without failing the Operation: a comparison failure
// still lets the engine clean up the hooks recorded in the results. failure is set when the
// termination cannot run at all (destination, impersonation or engine errors); the engine
// then is not built, so hooks are never deleted with the controller's own identity when the
// project requires impersonation.
func (s *step) prepareForTermination(ctx context.Context, st *settings.Settings, proj *v1alpha1.AppProject) (x *execution, failure string, err error) {
	cluster, err := s.r.cfg.DestinationCluster(ctx, s.app.Spec.Destination, st)
	if err != nil {
		return nil, fmt.Sprintf("failed to get destination cluster: %v", err), nil
	}
	imp := st.Impersonation()
	sa, err := project.SelectServiceAccount(proj, s.app, cluster.Spec.Server, project.ImpersonationSettings{Enabled: imp.Enabled, Enforced: imp.Enforced})
	if err != nil {
		return nil, fmt.Sprintf("failed to find a matching service account to impersonate: %v", err), nil
	}
	engine, err := s.r.cfg.Engines.Engine(ctx, cluster, sa.UserName, st)
	if err != nil {
		return nil, err.Error(), nil
	}
	lookup := s.r.cfg.ClusterLookup(st)
	sources, multi, revisions := s.operationSources()
	res := &compare.Result{}
	// Sources the project does not permit are not rendered; the engine still cleans up the
	// hooks recorded in the results.
	if !s.overridesSources() || len(validateOperationSources(ctx, s.app, proj, sources, multi, lookup)) == 0 {
		in := compare.Input{App: s.app, Project: proj, Sources: sources, Revisions: revisions, HasMultipleSources: multi, RollbackOrSyncOverride: true, SkipTree: true}
		if p := s.op.Status.Plan; p != nil && len(p.ResolvedRevisions) == len(sources) {
			in.Revisions = slices.Clone(p.ResolvedRevisions)
		}
		if s.op.Spec.Sync != nil && len(s.op.Spec.Sync.Manifests) > 0 {
			in.LocalManifests = s.op.Spec.Sync.Manifests
		}
		cmp, cerr := s.r.cfg.Comparer.Compare(ctx, in)
		switch {
		case cerr == nil:
			res = cmp
		case ctx.Err() != nil:
			return nil, "", cerr
		}
	}
	isNamespaced := func(gk schema.GroupKind) (bool, error) { return false, errors.New("unknown scope") }
	if cc, err := s.r.cfg.LiveState.ClusterCache(ctx, cluster); err == nil {
		isNamespaced = cc.IsNamespaced
	}
	x = &execution{st: st, proj: proj, cluster: cluster, cmp: res, sources: sources, multi: multi, engine: engine}
	x.input = s.engineInput(ctx, x, isNamespaced, lookup)
	return x, "", nil
}

// formatErrorConditions formats the error conditions of a comparison as the operation
// message (upstream FormatAppConditions over the *Error condition types).
func formatErrorConditions(conds []compare.Condition) string {
	var out []string
	for _, c := range conds {
		if strings.HasSuffix(c.Type, "Error") {
			out = append(out, c.Type+": "+c.Message)
		}
	}
	return strings.Join(out, ";")
}

// renderedDigest is the sha256 over the sorted rendered manifests (repo-server responses or
// local manifests): status.plan.renderedInputDigest.
func renderedDigest(res *compare.Result, in *syncpkg.Input) string {
	var docs []string
	for _, r := range res.ManifestResponses {
		if r != nil {
			docs = append(docs, r.Manifests...)
		}
	}
	if len(docs) == 0 {
		return syncpkg.Digest(in.Targets)
	}
	sort.Strings(docs)
	h := sha256.New()
	for _, d := range docs {
		h.Write([]byte(d))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MergeSyncOptions returns the operation's sync options merged over the Application's
// (OperationSpec.sync.syncOptions): every operation option, then the Application options
// whose key the operation does not set. Keys are unique in the result, so first-match and
// last-match readers agree.
func MergeSyncOptions(app, op v1alpha1.SyncOptions) v1alpha1.SyncOptions {
	if len(app) == 0 && len(op) == 0 {
		return nil
	}
	out := make(v1alpha1.SyncOptions, 0, len(app)+len(op))
	keys := map[string]bool{}
	for _, list := range []v1alpha1.SyncOptions{op, app} {
		for _, o := range list {
			k, _, ok := v1alpha1.ParseSyncOption(o)
			if !ok || keys[k] {
				continue
			}
			keys[k] = true
			out = append(out, o)
		}
	}
	return out
}
