package appset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Progressive sync status messages (upstream texts).
const (
	msgDefaultWaiting         = "No Application status found, defaulting status to Waiting"
	msgRevisionAndSpecChanged = "Application has pending changes (revision and spec differ), setting status to Waiting"
	msgRevisionChanged        = "Application has pending changes, setting status to Waiting"
	msgSpecChanged            = "Application has pending changes (spec differs), setting status to Waiting"
	msgSyncedHealthy          = "Application resource has synced, updating status to Healthy"
	msgSyncedProgressing      = "Application resource has synced, updating status to Progressing"
	msgSyncSucceeded          = "Application resource completed a sync successfully, updating status from Pending to Progressing"
	msgSyncCompleted          = "Application resource completed a sync, updating status from Pending to Progressing"
	msgBecameProgressing      = "Application resource became Progressing, updating status from Pending to Progressing"
	msgAppError               = "Application resource has error and cannot sync, updating status to Progressing"
	msgBecameHealthy          = "Application resource became Healthy, updating status from Progressing to Healthy"
	msgMovedToPending         = "Application moved to Pending status, watching for the Application resource to start Progressing"
	// SyncReason is the Reason info of Operations created by RollingSync.
	SyncReason = "ApplicationSet RollingSync triggered a sync of this Application resource"
	// defaultSyncRetryLimit is the retry limit of RollingSync Operations when the Application
	// configures none (the application controller's automated sync default).
	defaultSyncRetryLimit = 5
)

// buildSteps assigns Applications to RollingSync steps: steps[i] lists the Applications that
// step i+1 selects (an Application matching several steps is listed in each), and stepOf maps
// each selected Application to the first matching step (0-based).
func buildSteps(log *slog.Logger, appSet *v1alpha1.ApplicationSet, apps []v1alpha1.Application) ([][]string, map[string]int) {
	stepOf := map[string]int{}
	if !RollingSyncEnabled(appSet) {
		return [][]string{}, stepOf
	}
	defs := appSet.Spec.Strategy.RollingSync.Steps
	steps := make([][]string, len(defs))
	for i := range steps {
		steps[i] = []string{}
	}
	for _, app := range apps {
		for i, step := range defs {
			if !stepSelects(log, step, app.Labels) {
				continue
			}
			steps[i] = append(steps[i], app.Name)
			if first, ok := stepOf[app.Name]; ok {
				log.Warn("an ApplicationSet rolling sync step matchExpression selects an Application twice",
					appSetAttr(appSet), slog.String("application", app.Name), slog.Int("firstStep", first+1), slog.Int("step", i+1))
				continue
			}
			stepOf[app.Name] = i
		}
	}
	return steps, stepOf
}

// stepSelects evaluates a step's matchExpressions (AND) against labels: a present key must
// satisfy In/NotIn; an absent key fails In and is skipped otherwise.
func stepSelects(log *slog.Logger, step v1alpha1.ApplicationSetRolloutStep, labels map[string]string) bool {
	for _, expr := range step.MatchExpressions {
		val, ok := labels[expr.Key]
		if !ok {
			if expr.Operator == "In" {
				return false
			}
			continue
		}
		if !labelMatched(log, val, expr) {
			return false
		}
	}
	return true
}

func labelMatched(log *slog.Logger, val string, expr v1alpha1.ApplicationMatchExpression) bool {
	switch expr.Operator {
	case "In":
		return slices.Contains(expr.Values, val)
	case "NotIn":
		return !slices.Contains(expr.Values, val)
	}
	log.Error("skipping rolling sync step Application selection: invalid matchExpression operator", slog.String("operator", expr.Operator))
	return false
}

// stepNumber returns the 1-based step of an Application, or -1 when no step selects it.
func stepNumber(name string, stepOf map[string]int) int {
	if s, ok := stepOf[name]; ok {
		return s + 1
	}
	return -1
}

// disableAutomatedSync sets automated.enabled=false on a RollingSync-managed Application: the
// ApplicationSet controller triggers its syncs.
func disableAutomatedSync(app *v1alpha1.Application) {
	if app.Spec.SyncPolicy == nil || app.Spec.SyncPolicy.Automated == nil {
		return
	}
	app.Spec.SyncPolicy.Automated.Enabled = ptr.To(false)
}

// isApplicationWithError reports whether the Application has a condition that prevents it
// from syncing (InvalidSpecError or UnknownError, active).
func isApplicationWithError(app *v1alpha1.Application) bool {
	for _, c := range app.Status.Conditions {
		if c.Status == metav1.ConditionFalse {
			continue
		}
		if c.Type == v1alpha1.ApplicationConditionInvalidSpecError || c.Type == v1alpha1.ApplicationConditionUnknownError {
			return true
		}
	}
	return false
}

// progressiveSync updates status.applicationStatus and the RolloutProgressing condition of a
// RollingSync ApplicationSet and returns the Applications whose step may sync.
func (r *Reconciler) progressiveSync(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, current, desired []v1alpha1.Application, differ *ignoreDiffer) (map[string]bool, error) {
	steps, stepOf := buildSteps(log, appSet, desired)
	if err := r.updateApplicationStatus(ctx, log, appSet, current, desired, stepOf, differ); err != nil {
		return nil, fmt.Errorf("failed to update applicationset app status: %w", err)
	}
	for i, names := range steps {
		log.DebugContext(ctx, "rolling sync step", slog.Int("step", i+1), slog.Any("applications", names))
	}
	toSync := appsToSync(appSet, steps, current)
	if err := r.updateApplicationStatusProgress(ctx, log, appSet, toSync, stepOf); err != nil {
		return nil, fmt.Errorf("failed to update applicationset application status progress: %w", err)
	}
	if err := r.updateRolloutCondition(ctx, appSet); err != nil {
		return nil, err
	}
	return toSync, nil
}

// updateApplicationStatus advances each owned Application's progressive sync status from its
// sync/health/operation state, and resets it to Waiting when its revisions or desired spec
// changed.
func (r *Reconciler) updateApplicationStatus(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, current, desired []v1alpha1.Application, stepOf map[string]int, differ *ignoreDiffer) error {
	now := metav1.NewTime(r.cfg.now())
	desiredByName := make(map[string]*v1alpha1.Application, len(desired))
	for i := range desired {
		desiredByName[desired[i].Name] = &desired[i]
	}
	statuses := make([]v1alpha1.ApplicationSetApplicationStatus, 0, len(current))
	for i := range current {
		app := &current[i]
		var cur v1alpha1.ApplicationSetApplicationStatus
		if idx := findApplicationStatus(appSet.Status.ApplicationStatus, app.Name); idx >= 0 {
			cur = appSet.Status.ApplicationStatus[idx]
		} else {
			cur = v1alpha1.ApplicationSetApplicationStatus{
				Application:        app.Name,
				TargetRevisions:    app.Status.GetRevisions(),
				LastTransitionTime: &now,
				Message:            msgDefaultWaiting,
				Status:             v1alpha1.ProgressiveSyncWaiting,
				Step:               strconv.Itoa(stepNumber(app.Name, stepOf)),
			}
		}
		next := *cur.DeepCopy()
		next.Step = strconv.Itoa(stepNumber(app.Name, stepOf))
		transitioned := false
		transition := func(status v1alpha1.ProgressiveSyncStatusCode, msg string) {
			next.Status, next.Message, next.LastTransitionTime = status, msg, &now
			transitioned = true
		}

		revisionsChanged := !slices.Equal(cur.TargetRevisions, app.Status.GetRevisions())
		specChanged := false
		if d, ok := desiredByName[app.Name]; ok {
			want := d.DeepCopy()
			disableAutomatedSync(want)
			equivalent, err := specsEquivalent(ctx, differ, app, want)
			if err != nil {
				log.WarnContext(ctx, "could not compare desired and live specs; leaving progressive sync status unchanged",
					slog.String("application", app.Name), slog.Any("error", err))
			} else {
				specChanged = !equivalent
			}
		}
		if revisionsChanged || specChanged {
			next.TargetRevisions = app.Status.GetRevisions()
			switch {
			case revisionsChanged && specChanged:
				transition(v1alpha1.ProgressiveSyncWaiting, msgRevisionAndSpecChanged)
			case revisionsChanged:
				transition(v1alpha1.ProgressiveSyncWaiting, msgRevisionChanged)
			default:
				transition(v1alpha1.ProgressiveSyncWaiting, msgSpecChanged)
			}
		}

		syncStatus, health := app.Status.Sync.Status, app.Status.Health.Status
		if next.Status == v1alpha1.ProgressiveSyncWaiting {
			// A Waiting Application that is already Synced does not need a sync.
			if syncStatus == v1alpha1.SyncStatusCodeSynced {
				if health == v1alpha1.HealthStatusHealthy {
					transition(v1alpha1.ProgressiveSyncHealthy, msgSyncedHealthy)
				} else {
					transition(v1alpha1.ProgressiveSyncProgressing, msgSyncedProgressing)
				}
			}
		} else {
			if cur.Status == v1alpha1.ProgressiveSyncPending {
				pendingSince := time.Time{}
				if cur.LastTransitionTime != nil {
					pendingSince = cur.LastTransitionTime.Time
				}
				op := app.Status.OperationState
				switch started, ok := rollingSyncStarted(appSet, app, pendingSince); {
				case ok && started:
					switch {
					case op.Phase.IsSuccessful():
						transition(v1alpha1.ProgressiveSyncProgressing, msgSyncSucceeded)
					case op.Phase.IsTerminal():
						transition(v1alpha1.ProgressiveSyncProgressing, msgSyncCompleted)
					default:
						transition(v1alpha1.ProgressiveSyncProgressing, msgBecameProgressing)
					}
				case ok:
					// Our Operation was rejected; triggerSyncs recreates it. Stay Pending.
				case reconciledSince(app, next.LastTransitionTime):
					switch {
					case op != nil && op.StartedAt != nil && op.StartedAt.After(pendingSince):
						switch {
						case op.Phase.IsSuccessful():
							transition(v1alpha1.ProgressiveSyncProgressing, msgSyncSucceeded)
						case op.Phase.IsTerminal():
							transition(v1alpha1.ProgressiveSyncProgressing, msgSyncCompleted)
						default:
							transition(v1alpha1.ProgressiveSyncProgressing, msgBecameProgressing)
						}
					case isApplicationWithError(app):
						transition(v1alpha1.ProgressiveSyncProgressing, msgAppError)
					}
				}
			}
			if cur.Status == v1alpha1.ProgressiveSyncProgressing && health == v1alpha1.HealthStatusHealthy && syncStatus == v1alpha1.SyncStatusCodeSynced {
				transition(v1alpha1.ProgressiveSyncHealthy, msgBecameHealthy)
			}
		}
		if transitioned {
			log.InfoContext(ctx, "progressive sync application changed status",
				slog.String("application", app.Name), slog.String("status", string(next.Status)),
				slog.String("step", next.Step), slog.String("message", next.Message))
		}
		statuses = append(statuses, next)
	}
	return r.setApplicationStatus(ctx, appSet, statuses)
}

// rollingSyncStarted identifies the Operation this controller created for the Pending
// transition at pendingSince by its deterministic name (operationName) in the Application's
// operation state. ok reports whether the operation state names that Operation; started is false
// when it was Rejected (triggerSyncs recreates it). Matching by identity instead of by start time
// does not depend on timestamps, which the API server stores with second precision: an Operation
// that starts in the same second as the Pending transition is detected, and no
// status.reconciledAt is required since the application controller wrote the operation state
// after the Operation was created.
func rollingSyncStarted(appSet *v1alpha1.ApplicationSet, app *v1alpha1.Application, pendingSince time.Time) (started, ok bool) {
	op := app.Status.OperationState
	if op == nil || op.OperationName == "" || op.OperationName != operationName(appSet.UID, app.Name, pendingSince) {
		return false, false
	}
	return op.Phase != v1alpha1.OperationRejected, true
}

// reconciledSince reports whether the application controller reconciled app after the given
// transition (upstream semantics: strictly after, or never reconciled).
func reconciledSince(app *v1alpha1.Application, transition *metav1.Time) bool {
	return app.Status.ReconciledAt == nil || (transition != nil && app.Status.ReconciledAt.After(transition.Time))
}

// appsToSync returns the Applications whose step may sync: every Application of step N+1 once
// every Application listed in steps 1..N exists and is Healthy. Steps without Applications do
// not block.
func appsToSync(appSet *v1alpha1.ApplicationSet, steps [][]string, current []v1alpha1.Application) map[string]bool {
	out := map[string]bool{}
	exists := make(map[string]bool, len(current))
	for i := range current {
		exists[current[i].Name] = true
	}
	for _, names := range steps {
		for _, n := range names {
			out[n] = true
		}
		complete := true
		for _, n := range names {
			idx := findApplicationStatus(appSet.Status.ApplicationStatus, n)
			if !exists[n] || idx < 0 || appSet.Status.ApplicationStatus[idx].Status != v1alpha1.ProgressiveSyncHealthy {
				complete = false
				break
			}
		}
		if !complete {
			break
		}
	}
	return out
}

// maxUpdateValue returns the number of Applications of a step that may be Pending or
// Progressing at once (unlimited when maxUpdate is unset). Percentages apply to the step's
// status entries rounded down, but any percentage other than "0%" allows at least one; an
// invalid value allows one.
func maxUpdateValue(log *slog.Logger, maxUpdate *intstr.IntOrString, total int) (int, bool) {
	if maxUpdate == nil {
		return 0, false
	}
	v, err := intstr.GetScaledValueFromIntOrPercent(maxUpdate, total, false)
	if err != nil {
		log.Warn("invalid rolling sync maxUpdate value, ignoring it for this step", slog.String("maxUpdate", maxUpdate.String()), slog.Any("error", err))
	}
	if maxUpdate.Type == intstr.String && maxUpdate.StrVal != "0%" && v < 1 {
		v = 1
	}
	return v, true
}

// updateApplicationStatusProgress moves Waiting Applications of the steps that may sync to
// Pending, within each step's maxUpdate.
func (r *Reconciler) updateApplicationStatusProgress(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, toSync map[string]bool, stepOf map[string]int) error {
	if !RollingSyncEnabled(appSet) {
		return r.setApplicationStatus(ctx, appSet, nil)
	}
	now := metav1.NewTime(r.cfg.now())
	defs := appSet.Spec.Strategy.RollingSync.Steps
	updating := make([]int, len(defs))
	total := make([]int, len(defs))
	// Applications without a step count against the first step, as upstream.
	for _, s := range appSet.Status.ApplicationStatus {
		step := stepOf[s.Application]
		total[step]++
		if s.Status == v1alpha1.ProgressiveSyncPending || s.Status == v1alpha1.ProgressiveSyncProgressing {
			updating[step]++
		}
	}
	statuses := make([]v1alpha1.ApplicationSetApplicationStatus, 0, len(appSet.Status.ApplicationStatus))
	for _, s := range appSet.Status.ApplicationStatus {
		s = *s.DeepCopy()
		step := stepOf[s.Application]
		allowed := true
		if limit, ok := maxUpdateValue(log, defs[step].MaxUpdate, total[step]); ok && updating[step] >= limit {
			allowed = false
			log.DebugContext(ctx, "Application is not allowed to update yet",
				slog.String("application", s.Application), slog.Int("updating", updating[step]), slog.Int("maxUpdate", limit),
				slog.Int("step", stepNumber(s.Application, stepOf)))
		}
		if s.Status == v1alpha1.ProgressiveSyncWaiting && toSync[s.Application] && allowed {
			s.Status, s.Message, s.LastTransitionTime = v1alpha1.ProgressiveSyncPending, msgMovedToPending, &now
			updating[step]++
			log.InfoContext(ctx, "progressive sync application changed status",
				slog.String("application", s.Application), slog.String("status", string(s.Status)), slog.String("step", s.Step))
		}
		statuses = append(statuses, s)
	}
	return r.setApplicationStatus(ctx, appSet, statuses)
}

// updateRolloutCondition sets RolloutProgressing: True with the first step whose Applications
// are not all Healthy, False when every step completed (steps without Applications count as
// complete).
func (r *Reconciler) updateRolloutCondition(ctx context.Context, appSet *v1alpha1.ApplicationSet) error {
	if !IsRollingSyncStrategy(appSet) {
		return nil
	}
	complete := map[string]bool{}
	for _, s := range appSet.Status.ApplicationStatus {
		healthy := s.Status == v1alpha1.ProgressiveSyncHealthy
		if v, ok := complete[s.Step]; ok {
			complete[s.Step] = v && healthy
		} else {
			complete[s.Step] = healthy
		}
	}
	progressingStep := ""
	for i := range appSet.Spec.Strategy.RollingSync.Steps {
		step := strconv.Itoa(i + 1)
		done, ok := complete[step]
		if ok && !done {
			progressingStep = step
			break
		}
	}
	c := condition{
		Type: v1alpha1.ApplicationSetConditionRolloutProgressing, Status: metav1.ConditionFalse,
		Reason: v1alpha1.ApplicationSetReasonApplicationSetRolloutComplete, Message: msgRolloutComplete,
	}
	if progressingStep != "" {
		c = condition{
			Type: v1alpha1.ApplicationSetConditionRolloutProgressing, Status: metav1.ConditionTrue,
			Reason: v1alpha1.ApplicationSetReasonApplicationSetModified, Message: msgRolloutStepPrefix + progressingStep,
		}
	}
	return r.setCondition(ctx, appSet, c, true)
}

// syncRequest is a RollingSync sync to trigger for one Application.
type syncRequest struct {
	app         string
	prune       bool
	hasPolicy   bool
	syncOptions v1alpha1.SyncOptions
	retry       *v1alpha1.RetryStrategy
	pendingAt   time.Time
}

// prepareRollingSync disables automated sync on every desired Application, records the
// progressive status annotation, and returns the syncs to trigger: the Applications of steps
// that may sync whose status is Pending.
func prepareRollingSync(log *slog.Logger, appSet *v1alpha1.ApplicationSet, toSync map[string]bool, desired []v1alpha1.Application) ([]v1alpha1.Application, []syncRequest) {
	out := make([]v1alpha1.Application, 0, len(desired))
	var requests []syncRequest
	for i := range desired {
		app := desired[i].DeepCopy()
		prune := false
		if app.Spec.SyncPolicy.IsAutomatedSyncEnabled() {
			prune = app.Spec.SyncPolicy.Automated.GetPrune()
		}
		disableAutomatedSync(app)
		idx := findApplicationStatus(appSet.Status.ApplicationStatus, app.Name)
		if idx >= 0 {
			if app.Annotations == nil {
				app.Annotations = map[string]string{}
			}
			app.Annotations[v1alpha1.AnnotationKeyApplicationSetProgressiveStatus] = string(appSet.Status.ApplicationStatus[idx].Status)
		}
		if idx >= 0 && appSet.Status.ApplicationStatus[idx].Status == v1alpha1.ProgressiveSyncPending && toSync[app.Name] {
			st := appSet.Status.ApplicationStatus[idx]
			req := syncRequest{app: app.Name, prune: prune}
			if st.LastTransitionTime != nil {
				req.pendingAt = st.LastTransitionTime.Time
			}
			if p := app.Spec.SyncPolicy; p != nil {
				req.hasPolicy = true
				req.syncOptions = append(v1alpha1.SyncOptions(nil), p.SyncOptions...)
				if p.Retry != nil {
					req.retry = p.Retry.DeepCopy()
				}
			}
			log.Info("triggering rolling sync for application", slog.String("application", app.Name), slog.Bool("prune", prune))
			requests = append(requests, req)
		}
		out = append(out, *app)
	}
	return out, requests
}

// operationName is the deterministic name of the Operation that syncs app for the Pending
// transition at pendingAt, so repeated reconciles create it once.
func operationName(appSetUID types.UID, app string, pendingAt time.Time) string {
	sum := sha256.Sum256([]byte(string(appSetUID) + "/" + app + "/" + pendingAt.UTC().Format(time.RFC3339Nano)))
	suffix := "-rollingsync-" + hex.EncodeToString(sum[:])[:10]
	prefix := app
	if max := 253 - len(suffix); len(prefix) > max {
		prefix = strings.TrimRight(prefix[:max], "-.")
	}
	return prefix + suffix
}

// buildOperation returns the Operation a RollingSync sync request creates.
func buildOperation(appSet *v1alpha1.ApplicationSet, app *v1alpha1.Application, req syncRequest) *v1alpha1.Operation {
	op := &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      operationName(appSet.UID, req.app, req.pendingAt),
			Namespace: app.Namespace,
			Labels:    map[string]string{v1alpha1.LabelKeyOperationApplication: app.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1alpha1.GroupVersion.String(),
				Kind:               "Application",
				Name:               app.Name,
				UID:                app.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: v1alpha1.OperationSpec{
			Application: app.Name,
			Sync:        &v1alpha1.SyncOperation{},
			InitiatedBy: v1alpha1.OperationInitiator{Username: SyncInitiator, Automated: true},
			Info:        []v1alpha1.Info{{Name: "Reason", Value: SyncReason}},
			Retry:       &v1alpha1.RetryStrategy{Limit: defaultSyncRetryLimit},
		},
	}
	if req.hasPolicy {
		if req.retry != nil {
			op.Spec.Retry = req.retry
		}
		if len(req.syncOptions) > 0 {
			op.Spec.Sync.SyncOptions = req.syncOptions
		}
		op.Spec.Sync.Prune = req.prune
	}
	return op
}

// triggerSyncs ensures one Operation exists for every sync request. Operations rejected because
// another Operation was in progress are deleted so a later reconcile retries; the returned
// duration asks for that requeue.
func (r *Reconciler) triggerSyncs(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, requests []syncRequest, apps map[string]*v1alpha1.Application) (time.Duration, error) {
	var requeue time.Duration
	var errs []error
	for _, req := range requests {
		app := apps[req.app]
		if app == nil || app.UID == "" {
			// Not written yet (e.g. invalid); the next reconcile retries.
			requeue = ReverseDeletionRequeue
			continue
		}
		op := buildOperation(appSet, app, req)
		existing := &v1alpha1.Operation{}
		err := r.apiReader.Get(ctx, client.ObjectKeyFromObject(op), existing)
		switch {
		case apierrors.IsNotFound(err):
			if err := r.client.Create(ctx, op); err != nil && !apierrors.IsAlreadyExists(err) {
				errs = append(errs, fmt.Errorf("error creating sync operation for application %s: %w", app.Name, err))
				continue
			}
			log.InfoContext(ctx, "created rolling sync operation", slog.String("application", app.Name), slog.String("operation", op.Name))
		case err != nil:
			errs = append(errs, fmt.Errorf("error getting sync operation for application %s: %w", app.Name, err))
		case existing.Status.Phase == v1alpha1.OperationRejected:
			if err := r.client.Delete(ctx, existing, client.Preconditions{UID: &existing.UID}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("error deleting rejected sync operation for application %s: %w", app.Name, err))
				continue
			}
			log.InfoContext(ctx, "rolling sync operation was rejected; it will be retried",
				slog.String("application", app.Name), slog.String("operation", existing.Name), slog.String("message", existing.Status.Message))
			requeue = ReverseDeletionRequeue
		}
	}
	return requeue, errors.Join(errs...)
}

// reverseDeletion deletes the Applications assigned to RollingSync steps one at a time, last
// step first, while appSet is being deleted. It returns a requeue interval while deletions are
// in flight and 0 once every such Application is gone.
func (r *Reconciler) reverseDeletion(ctx context.Context, log *slog.Logger, appSet *v1alpha1.ApplicationSet, current []v1alpha1.Application) (time.Duration, error) {
	steps := len(appSet.Spec.Strategy.RollingSync.Steps)
	_, stepOf := buildSteps(log, appSet, current)
	type entry struct {
		name  string
		order int
	}
	order := make([]entry, 0, len(stepOf))
	for name, step := range stepOf {
		order = append(order, entry{name: name, order: steps - step - 1})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].order != order[j].order {
			return order[i].order < order[j].order
		}
		return order[i].name < order[j].name
	})
	for _, e := range order {
		app := &v1alpha1.Application{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: appSet.Namespace, Name: e.name}, app); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return 0, fmt.Errorf("error retrieving application %s: %w", e.name, err)
		}
		alog := log.With(slog.String("application", e.name))
		if app.DeletionTimestamp != nil {
			if r.cfg.now().Sub(app.DeletionTimestamp.Time) <= StaleTerminatingThreshold {
				return ReverseDeletionRequeue, nil
			}
			live := &v1alpha1.Application{}
			err := r.apiReader.Get(ctx, types.NamespacedName{Namespace: appSet.Namespace, Name: e.name}, live)
			switch {
			case apierrors.IsNotFound(err):
				alog.InfoContext(ctx, "application is absent from the API server but still cached; treating it as deleted")
				continue
			case err != nil:
				return 0, fmt.Errorf("application %s has not been deleted in over %s and could not be verified against the API server: %w", e.name, StaleTerminatingThreshold, err)
			case live.UID != app.UID:
				return 0, fmt.Errorf("application %s was recreated while being deleted", e.name)
			default:
				return 0, errors.New("application has not been deleted in over 2 minutes")
			}
		}
		if err := r.deleteApplication(ctx, alog, appSet, app); err != nil {
			return 0, err
		}
		return ReverseDeletionRequeue, nil
	}
	log.InfoContext(ctx, "completed reverse deletion")
	return 0, nil
}
