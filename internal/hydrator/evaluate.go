package hydrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// Reasons logged by the hydration need evaluation (upstream wording).
const (
	reasonNotConfigured = "source hydrator not configured"
	reasonNoOperation   = "no previous hydrate operation"
	reasonInProgress    = "hydration operation already in progress"
	reasonHardRequested = "hard hydrate requested"
	reasonSpecDiffers   = "spec.sourceHydrator differs"
	reasonRetryFailed   = "retrying previous failed hydration"
	reasonFailedLongAgo = "previous hydrate operation failed more than 2 minutes ago"
	reasonFailedWait    = "previous hydrate operation failed"
	reasonNoComparedRev = "no last compared dry revision"
	reasonUnknown       = "cannot determine if hydration is needed"
	reasonNewRevision   = "new revision may have changes"
	reasonNotNeeded     = "hydration not needed"
)

// decision is the outcome of a hydration need evaluation.
type decision struct {
	needed bool
	// hard is true for a hard hydrate request (the commit-server is called even when the
	// dry SHA is already hydrated).
	hard   bool
	reason string
	// resolved is the dry revision resolved by the evaluation ("" when it was skipped).
	resolved string
	// timeBased is true when the decision depends on elapsed time (failure cooldown).
	timeBased bool
	err       error
}

// hydrateRequest reads the hydrate annotation: requested when present; hard when its value
// is "hard" (every other value is a normal request, as upstream).
func hydrateRequest(app *v1alpha1.Application) (value string, requested, hard bool) {
	value, requested = app.Annotations[v1alpha1.AnnotationKeyHydrate]
	return value, requested, requested && v1alpha1.HydrateType(value) == v1alpha1.HydrateTypeHard
}

// ReconcileApp is the app-hydration reconciler: it decides whether the Application needs
// hydration (APP-114), records the compared dry revision, consumes the hydrate annotation
// (APP-115), enqueues the Application's hydration key, and requeues itself to poll the dry
// branch and to recover stale Hydrating operations (APP-131).
func (h *Hydrator) ReconcileApp(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	app := &v1alpha1.Application{}
	if err := h.d.Client.Get(ctx, req.NamespacedName, app); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get application %s: %w", req.NamespacedName, err)
	}
	if !h.accept(app) || app.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}
	log := h.log.With(slog.String("application", app.QualifiedName()))
	if h.d.Settings.Current() == nil {
		h.metrics.observeEvaluation(OutcomeError)
		return reconcile.Result{}, errSettingsNotLoaded
	}

	value, requested, _ := hydrateRequest(app)
	evaluatedRV := app.ResourceVersion
	d := h.evaluate(ctx, app)
	evaluatedSpec := ""
	if d.needed {
		// Taken before the writes below, which may return a newer version.
		evaluatedSpec = specDigest(app)
	}
	switch {
	case d.err != nil:
		h.metrics.observeEvaluation(OutcomeError)
		log.Warn("hydrator: failed to check for new revision changes", slog.String("reason", d.reason), slog.Any("error", d.err))
	case d.needed:
		h.metrics.observeEvaluation(OutcomeHydrate)
	default:
		h.metrics.observeEvaluation(OutcomeSkip)
	}

	// The resolved revision is recorded when no hydration follows (a hydration records the
	// dry SHA it used): an unchanged revision, or a commit outside manifest-generate-paths.
	if !d.needed && d.resolved != "" && d.resolved != app.Status.SourceHydrator.LastComparedDryRevision {
		resolved := d.resolved
		err := h.patchSourceHydrator(ctx, app, func(a *v1alpha1.Application) {
			if cur := a.Status.SourceHydrator.CurrentOperation; cur != nil && cur.Phase == v1alpha1.HydrateOperationPhaseHydrating {
				return
			}
			a.Status.SourceHydrator.LastComparedDryRevision = resolved
		})
		if errors.Is(err, errAppGone) {
			return reconcile.Result{}, nil
		}
		if err != nil {
			return reconcile.Result{}, err
		}
	}
	if requested {
		if err := h.consumeHydrateRequest(ctx, app, value); err != nil {
			return reconcile.Result{}, err
		}
	}

	stale := h.isStaleHydrating(app)
	if d.needed || stale {
		k, _ := KeyOf(app)
		if d.hard {
			h.forced.Store(k, struct{}{})
		}
		reason := d.reason
		if stale && !d.needed {
			reason = "hydration in progress for longer than the refresh timeout"
		}
		log.Info("hydrator: hydrating app", slog.String("reason", reason))
		h.request(k, req.NamespacedName, keyRequest{
			rv:        evaluatedRV,
			spec:      evaluatedSpec,
			drySHA:    d.resolved,
			coverable: d.needed && !d.hard && !d.timeBased,
		})
	} else {
		log.Debug("hydrator: skipping hydration", slog.String("reason", d.reason))
	}
	return reconcile.Result{RequeueAfter: h.requeueAfter(app)}, nil
}

// evaluate decides whether app needs hydration (upstream appNeedsHydration, in the same
// order).
func (h *Hydrator) evaluate(ctx context.Context, app *v1alpha1.Application) decision {
	_, requested, hard := hydrateRequest(app)
	st := app.Status.SourceHydrator
	cur := st.CurrentOperation
	switch {
	case app.Spec.SourceHydrator == nil:
		return decision{reason: reasonNotConfigured}
	case cur == nil:
		return decision{needed: true, hard: hard, reason: reasonNoOperation}
	case cur.Phase == v1alpha1.HydrateOperationPhaseHydrating:
		return decision{reason: reasonInProgress}
	case hard:
		return decision{needed: true, hard: true, reason: reasonHardRequested}
	case cur.SourceHydrator == nil || !apiequality.Semantic.DeepEqual(*app.Spec.SourceHydrator, *cur.SourceHydrator):
		return decision{needed: true, reason: reasonSpecDiffers}
	case cur.Phase == v1alpha1.HydrateOperationPhaseFailed:
		withinCooldown := cur.FinishedAt == nil || h.d.Now().Sub(cur.FinishedAt.Time) <= FailureCooldown
		switch {
		case requested:
			return decision{needed: true, reason: reasonRetryFailed}
		case !withinCooldown:
			return decision{needed: true, reason: reasonFailedLongAgo, timeBased: true}
		case st.LastComparedDryRevision == "":
			// No baseline: a revision check would always report changes (tight retry loop).
			return decision{reason: reasonFailedWait}
		}
		// Within the cooldown with a baseline: a new dry commit retries immediately.
	}
	if st.LastComparedDryRevision == "" {
		return decision{needed: true, reason: reasonNoComparedRev}
	}
	changed, resolved, err := h.dryRevisionChanged(ctx, app, requested)
	if err != nil {
		return decision{reason: reasonUnknown, resolved: resolved, err: err}
	}
	if changed {
		return decision{needed: true, reason: reasonNewRevision, resolved: resolved}
	}
	return decision{reason: reasonNotNeeded, resolved: resolved}
}

// dryRevisionChanged resolves the dry targetRevision and reports whether it may change the
// hydrated output compared to lastComparedDryRevision (upstream EvaluateAppRevisionsChanges
// for the dry source). With the manifest-generate-paths annotation and a full-depth
// repository, the repo-server decides from the files changed between the two commits
// (APP-133). noRevisionCache bypasses the repo-server's revision cache (explicit request).
func (h *Hydrator) dryRevisionChanged(ctx context.Context, app *v1alpha1.Application, noRevisionCache bool) (bool, string, error) {
	st := h.d.Settings.Current()
	if st == nil {
		return false, "", errSettingsNotLoaded
	}
	last := app.Status.SourceHydrator.LastComparedDryRevision
	src := app.Spec.SourceHydrator.GetDrySource()
	if src.TargetRevision == last && git.IsCommitSHA(last) {
		return false, last, nil
	}
	proj, err := h.d.Projects.ResolveAppProject(ctx, app)
	if err != nil {
		return false, "", fmt.Errorf("failed to get project for revision check: %w", err)
	}
	repo, err := h.d.Repositories.GetRepository(ctx, src.RepoURL, proj.Name)
	if err != nil {
		return false, "", fmt.Errorf("failed to get repo %q: %w", git.SanitizeURL(src.RepoURL), err)
	}
	if repo == nil {
		repo = &apiclient.Repository{URL: src.RepoURL}
	}
	instance := tracking.InstanceName(app.Name, app.Namespace, h.d.ControlPlaneNamespace)
	if mgp := app.Annotations[v1alpha1.AnnotationKeyManifestGeneratePaths]; mgp != "" && repo.Depth == 0 {
		resp, err := h.d.RepoServer.UpdateRevisionForPaths(ctx, &apiclient.UpdateRevisionForPathsRequest{
			Repo:           *repo,
			AppLabelKey:    st.InstanceLabelKey(),
			AppName:        instance,
			Namespace:      app.Spec.Destination.Namespace,
			Source:         src,
			TrackingMethod: string(st.TrackingMethod()),
			SyncedRevision: last,
			Revision:       src.TargetRevision,
			Paths:          refreshPaths(mgp, src.Path),
			InstallationID: st.InstallationID(),
		})
		if err != nil {
			return false, "", fmt.Errorf("failed to update revision for paths: %w", err)
		}
		if resp.Revision != "" {
			return resp.Changes, resp.Revision, nil
		}
		resolved, err := h.resolveRevision(ctx, repo, instance, src, noRevisionCache)
		if err != nil {
			return false, "", err
		}
		return resp.Changes, resolved, nil
	}
	resolved, err := h.resolveRevision(ctx, repo, instance, src, noRevisionCache)
	if err != nil {
		return false, "", err
	}
	return resolved != last, resolved, nil
}

func (h *Hydrator) resolveRevision(ctx context.Context, repo *apiclient.Repository, instance string, src v1alpha1.ApplicationSource, noRevisionCache bool) (string, error) {
	resp, err := h.d.RepoServer.ResolveRevision(ctx, &apiclient.ResolveRevisionRequest{
		Repo:         *repo,
		App:          instance,
		Revision:     src.TargetRevision,
		Source:       src,
		ForceRefresh: noRevisionCache,
	})
	if err != nil {
		return "", fmt.Errorf("failed to resolve dry revision %q: %w", src.TargetRevision, err)
	}
	if resp.Revision == "" {
		return "", errors.New("resolved revision is empty for dry source")
	}
	return resp.Revision, nil
}

// isStaleHydrating reports whether app has been Hydrating for longer than the refresh
// timeout (the worker crashed or fell behind; APP-131).
func (h *Hydrator) isStaleHydrating(app *v1alpha1.Application) bool {
	cur := app.Status.SourceHydrator.CurrentOperation
	if cur == nil || cur.Phase != v1alpha1.HydrateOperationPhaseHydrating {
		return false
	}
	if cur.StartedAt == nil {
		return true
	}
	return h.d.Now().Sub(cur.StartedAt.Time) > h.staleTimeout()
}

// requeueAfter is when app must be evaluated again: the poll interval, earlier when a failed
// operation leaves its cooldown or a Hydrating operation becomes stale.
func (h *Hydrator) requeueAfter(app *v1alpha1.Application) time.Duration {
	next := h.pollInterval()
	earlier := func(d time.Duration) {
		if d < time.Second {
			d = time.Second
		}
		if next == 0 || d < next {
			next = d
		}
	}
	now := h.d.Now()
	if cur := app.Status.SourceHydrator.CurrentOperation; cur != nil {
		switch cur.Phase {
		case v1alpha1.HydrateOperationPhaseFailed:
			if cur.FinishedAt != nil {
				earlier(cur.FinishedAt.Add(FailureCooldown).Sub(now) + time.Second)
			}
		case v1alpha1.HydrateOperationPhaseHydrating:
			if cur.StartedAt != nil {
				earlier(cur.StartedAt.Add(h.staleTimeout()).Sub(now) + time.Second)
			}
		}
	}
	return next
}

// appPredicate filters the app-hydration controller's events: Applications this hydrator
// handles, on creation (and the initial listing), spec changes, hydrate requests, and
// changes of status.sourceHydrator. Other status writes (sync, health) do not trigger an
// evaluation; the dry branch is polled with RequeueAfter instead.
func appPredicate(accept func(*v1alpha1.Application) bool) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			app, ok := e.Object.(*v1alpha1.Application)
			return ok && accept(app)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldApp, ok1 := e.ObjectOld.(*v1alpha1.Application)
			newApp, ok2 := e.ObjectNew.(*v1alpha1.Application)
			if !ok1 || !ok2 || !accept(newApp) {
				return false
			}
			if oldApp.Generation != newApp.Generation {
				return true
			}
			if _, ok := newApp.Annotations[v1alpha1.AnnotationKeyHydrate]; ok {
				return true
			}
			if oldApp.Annotations[v1alpha1.AnnotationKeyManifestGeneratePaths] != newApp.Annotations[v1alpha1.AnnotationKeyManifestGeneratePaths] {
				return true
			}
			return !apiequality.Semantic.DeepEqual(oldApp.Status.SourceHydrator, newApp.Status.SourceHydrator)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
