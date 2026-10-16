package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
)

// defaultAutomatedRetryLimit is the retry limit of automated syncs when the Application has
// no syncPolicy.retry (SYNC-024).
const defaultAutomatedRetryLimit = 5

// autoSyncInput is what the automated sync decision reads.
type autoSyncInput struct {
	app *v1alpha1.Application
	// sync is the sync status of the comparison just made.
	sync v1alpha1.SyncStatus
	// resources are the statuses of every managed resource (unbounded).
	resources []v1alpha1.ResourceStatus
	// revisionsMayHaveChanges is false when the new revision cannot change the manifests.
	revisionsMayHaveChanges bool
	// operationInProgress reports a non-terminal Operation of the Application.
	operationInProgress bool
	// selfHealAttempts is spec.sync.autoHealAttemptsCount of the last Operation.
	selfHealAttempts int64
	now              time.Time
	params           core.Params
}

// autoSyncDecision is the outcome of decideAutoSync.
type autoSyncDecision struct {
	// operation is the automated Operation to create (nil: none).
	operation *v1alpha1.Operation
	// condition is the SyncError condition to set (nil: SyncError is cleared).
	condition *compare.Condition
	// requeueAfter asks for a latest-level refresh after the self-heal backoff.
	requeueAfter time.Duration
	// outcome labels the decision (metrics, logs).
	outcome string
	// desired are the revisions the sync targets.
	desired []string
}

// decideAutoSync is upstream autoSync (SYNC-017..024) without side effects: it returns the
// Operation to create, the SyncError condition, or nothing.
func decideAutoSync(in autoSyncInput) autoSyncDecision {
	app := in.app
	if !app.Spec.IsAutomatedSyncEnabled() {
		return autoSyncDecision{outcome: "disabled"}
	}
	if in.operationInProgress {
		return autoSyncDecision{outcome: "operation-in-progress"}
	}
	if app.DeletionTimestamp != nil {
		return autoSyncDecision{outcome: "deleting"}
	}
	if in.sync.Status != v1alpha1.SyncStatusCodeOutOfSync {
		return autoSyncDecision{outcome: "not-out-of-sync"}
	}
	automated := app.Spec.SyncPolicy.Automated
	if !automated.GetPrune() {
		pruneOnly := true
		for _, r := range in.resources {
			if r.Status != v1alpha1.SyncStatusCodeSynced && !r.RequiresPruning {
				pruneOnly = false
				break
			}
		}
		if pruneOnly {
			return autoSyncDecision{outcome: "prune-only-without-auto-prune"}
		}
	}
	multi := app.Spec.HasMultipleSources()
	desired := []string{in.sync.Revision}
	if multi {
		desired = slices.Clone(in.sync.Revisions)
	}
	sync := &v1alpha1.SyncOperation{
		Prune: automated.GetPrune(),
	}
	if multi {
		sync.Revisions = slices.Clone(in.sync.Revisions)
	} else {
		sync.Revision = in.sync.Revision
	}
	if opts := app.Spec.SyncPolicy.SyncOptions; len(opts) > 0 {
		sync.SyncOptions = slices.Clone(opts)
	}

	attempted, lastRevisions, lastPhase := alreadyAttemptedSync(app, desired, in.revisionsMayHaveChanges)
	if attempted {
		if !lastPhase.IsSuccessful() {
			msg := ""
			if os := app.Status.OperationState; os != nil {
				msg = os.Message
			}
			return autoSyncDecision{
				outcome: "previous-attempt-failed", desired: desired,
				condition: &compare.Condition{Type: v1alpha1.ApplicationConditionSyncError, Reason: v1alpha1.ReasonAutoSyncFailed,
					Message: fmt.Sprintf("Failed last sync attempt to %s: %s", formatRevisions(lastRevisions), msg)},
			}
		}
		if !automated.GetSelfHeal() {
			return autoSyncDecision{outcome: "already-synced", desired: desired}
		}
		attempts := in.selfHealAttempts
		if remaining := selfHealRemainingBackoff(app, int(attempts), in.params, in.now); remaining > 0 {
			return autoSyncDecision{outcome: "self-heal-backoff", desired: desired, requeueAfter: remaining}
		}
		sync.SelfHealAttemptsCount = attempts + 1
		for _, r := range in.resources {
			if r.Status != v1alpha1.SyncStatusCodeSynced {
				sync.Resources = append(sync.Resources, v1alpha1.SyncOperationResource{Group: r.Group, Kind: r.Kind, Name: r.Name})
			}
		}
	}
	if automated.GetPrune() && !automated.GetAllowEmpty() {
		allPrune := true
		for _, r := range in.resources {
			if !r.RequiresPruning {
				allPrune = false
				break
			}
		}
		if allPrune {
			return autoSyncDecision{
				outcome: "would-wipe-out", desired: desired,
				condition: &compare.Condition{Type: v1alpha1.ApplicationConditionSyncError, Reason: v1alpha1.ReasonAutoSyncFailed,
					Message: fmt.Sprintf("Skipping sync attempt to %s: auto-sync will wipe out all resources", formatRevisions(desired))},
			}
		}
	}
	retry := &v1alpha1.RetryStrategy{Limit: defaultAutomatedRetryLimit}
	if r := app.Spec.SyncPolicy.Retry; r != nil {
		retry = r.DeepCopy()
	}
	op := &v1alpha1.Operation{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Operation"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            AutomatedOperationName(app, desired, sync.SelfHealAttemptsCount),
			Namespace:       app.Namespace,
			OwnerReferences: []metav1.OwnerReference{core.ApplicationOwnerReference(app)},
		},
		Spec: v1alpha1.OperationSpec{
			Application: app.Name,
			Sync:        sync,
			InitiatedBy: v1alpha1.OperationInitiator{Automated: true},
			Retry:       retry,
		},
	}
	if len(validation.IsValidLabelValue(app.Name)) == 0 {
		op.Labels = map[string]string{v1alpha1.LabelKeyOperationApplication: app.Name}
	}
	outcome := "sync"
	if sync.SelfHealAttemptsCount > 0 {
		outcome = "self-heal"
	}
	return autoSyncDecision{operation: op, outcome: outcome, desired: desired}
}

// alreadyAttemptedSync reports whether the last Operation synced the desired revisions (only
// sources are compared when the new revisions cannot contain changes) and the same sources
// as the spec, with the last synced revisions and phase (upstream alreadyAttemptedSync,
// SYNC-021). A completed Operation without sync result counts as attempted, so errors do not
// loop.
func alreadyAttemptedSync(app *v1alpha1.Application, desired []string, newRevisionHasChanges bool) (bool, []string, v1alpha1.OperationPhase) {
	os := app.Status.OperationState
	if os == nil {
		return false, nil, ""
	}
	if os.SyncResult == nil {
		return os.Phase.IsTerminal(), nil, os.Phase
	}
	multi := app.Spec.HasMultipleSources()
	last := []string{os.SyncResult.Revision}
	if multi {
		last = os.SyncResult.Revisions
	}
	if newRevisionHasChanges {
		if multi {
			if !slices.Equal(os.SyncResult.Revisions, desired) {
				return false, last, os.Phase
			}
		} else if len(desired) != 1 || os.SyncResult.Revision != desired[0] {
			return false, last, os.Phase
		}
	}
	if multi {
		return equality.Semantic.DeepEqual(nilIfEmpty(app.Spec.Sources), nilIfEmpty(os.SyncResult.Sources)), last, os.Phase
	}
	sources := app.Spec.GetSources()
	if len(sources) == 0 || os.SyncResult.Source == nil {
		return len(sources) == 0 && os.SyncResult.Source == nil, last, os.Phase
	}
	return equality.Semantic.DeepEqual(sources[0], *os.SyncResult.Source), last, os.Phase
}

// selfHealRemainingBackoff is the time left before the next self-heal attempt (SYNC-023):
// with a backoff, the attempts-th step of it (0 for the first attempt) minus the time since
// the last operation finished; without, the fixed self-heal timeout minus that time.
func selfHealRemainingBackoff(app *v1alpha1.Application, attempts int, p core.Params, now time.Time) time.Duration {
	os := app.Status.OperationState
	if os == nil {
		return 0
	}
	var since *time.Duration
	if os.FinishedAt != nil {
		d := now.Sub(os.FinishedAt.Time)
		since = &d
	}
	var delay time.Duration
	if p.SelfHealBackoff == nil {
		delay = p.SelfHealTimeout
	} else {
		b := *p.SelfHealBackoff
		b.Steps = attempts
		for range attempts {
			delay = b.Step()
		}
	}
	if since == nil {
		return delay
	}
	return delay - *since
}

func formatRevisions(revs []string) string { return "[" + strings.Join(revs, " ") + "]" }

// AutomatedOperationName is the deterministic name of the automated Operation syncing app to
// revisions at a self-heal attempt: "<app>-auto-<10 hex>" with the hash over the Application
// UID, the revisions, the attempt, the spec sources and the Operation summarized in
// status.operationState (name and start). Concurrent evaluations of the same state create one
// Operation (the second create gets AlreadyExists); once any other Operation ran, the summary
// differs and the next decision gets a new name, so an earlier automated Operation of the
// same revisions and attempt never blocks it.
func AutomatedOperationName(app *v1alpha1.Application, revisions []string, attempt int64) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00", app.UID, strings.Join(revisions, "\x01"), attempt)
	if b, err := json.Marshal(app.Spec.GetSources()); err == nil {
		h.Write(b)
	}
	if os := app.Status.OperationState; os != nil {
		started := ""
		if os.StartedAt != nil {
			started = os.StartedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(h, "\x00%s\x00%s", os.OperationName, started)
	}
	suffix := "-auto-" + hex.EncodeToString(h.Sum(nil))[:10]
	prefix := app.Name
	// Room is kept for the "-<n>" suffix of createAutomatedOperation.
	if limit := validation.DNS1123SubdomainMaxLength - len(suffix) - 2; len(prefix) > limit {
		prefix = strings.TrimRight(prefix[:limit], "-.")
	}
	return prefix + suffix
}

// autoSync evaluates automated sync after a comparison and creates the Operation.
func (a *appRun) autoSync(ctx context.Context, res *compare.Result, lastOp *v1alpha1.Operation) autoSyncDecision {
	app := a.app
	if !app.Spec.IsAutomatedSyncEnabled() {
		return autoSyncDecision{outcome: "disabled"}
	}
	ops, err := core.ListOperations(ctx, a.r.c, app.Namespace, app.Name)
	if err != nil {
		a.log.Warn("cannot list operations for automated sync", "error", err)
		return autoSyncDecision{outcome: "error"}
	}
	inProgress := core.HasNonTerminal(ops)
	if inProgress && res.SyncStatus.Status == v1alpha1.SyncStatusCodeOutOfSync {
		// The Operation cache can lag behind the Application cache (an Operation that just
		// completed still looks Running), and its completion would not trigger another
		// evaluation: confirm with the API server.
		live, err := core.ListOperationsLive(ctx, a.r.cfg.APIReader, app.Namespace, app.Name)
		if err != nil {
			a.log.Warn("cannot list operations for automated sync", "error", err)
			return autoSyncDecision{outcome: "error"}
		}
		inProgress = core.HasNonTerminal(live)
	}
	in := autoSyncInput{
		app:                     app,
		sync:                    res.SyncStatus,
		resources:               managedStatuses(res),
		revisionsMayHaveChanges: res.RevisionsMayHaveChanges,
		operationInProgress:     inProgress,
		now:                     a.now(),
		params:                  a.r.cfg.Params,
	}
	if lastOp != nil && lastOp.Spec.Sync != nil {
		in.selfHealAttempts = lastOp.Spec.Sync.SelfHealAttemptsCount
	}
	dec := decideAutoSync(in)
	if a.r.cfg.Metrics != nil {
		a.r.cfg.Metrics.AutoSyncTotal.WithLabelValues(dec.outcome).Inc()
	}
	switch {
	case dec.condition != nil:
		a.log.Warn("skipping auto-sync", "reason", dec.outcome, "message", dec.condition.Message)
	case dec.requeueAfter > 0:
		a.log.Info("skipping auto-sync: self-heal backoff", "revisions", dec.desired, "retryIn", dec.requeueAfter)
	case dec.operation == nil:
		a.log.Debug("skipping auto-sync", "reason", dec.outcome)
	}
	if dec.operation == nil {
		return dec
	}
	created, err := a.createAutomatedOperation(ctx, dec.operation)
	if err != nil {
		a.log.Error("failed to initiate auto-sync", "revisions", dec.desired, "error", err)
		return autoSyncDecision{outcome: "error", condition: &compare.Condition{Type: v1alpha1.ApplicationConditionSyncError, Reason: v1alpha1.ReasonAutoSyncFailed, Message: err.Error()}}
	}
	if !created {
		a.log.Info("automated operation already exists", "operation", dec.operation.Name)
		return autoSyncDecision{outcome: "already-exists"}
	}
	msg := fmt.Sprintf("Initiated automated sync to '%s'", strings.Join(dec.desired, ", "))
	a.log.Info(msg, "operation", dec.operation.Name, "selfHeal", dec.operation.Spec.Sync.SelfHealAttemptsCount > 0)
	a.r.cfg.Recorder.Eventf(app, dec.operation, corev1.EventTypeNormal, core.EventReasonOperationStarted, core.ActionSync, "%s", msg)
	return dec
}

// maxNameAttempts bounds the suffixed names tried when the deterministic name of an
// automated Operation belongs to an earlier Operation that never ran.
const maxNameAttempts = 5

// createAutomatedOperation creates op under its deterministic name. When that name is taken
// by an Operation that is still in progress or that ran (a concurrent evaluation of the same
// state, or one whose completion this evaluation has not observed yet), nothing is created.
// When it is taken by one that never ran (Rejected because another Operation held the slot,
// or cancelled while Pending), "-<n>" suffixes are tried and then a generated name, so the
// attempt is never lost.
func (a *appRun) createAutomatedOperation(ctx context.Context, op *v1alpha1.Operation) (bool, error) {
	base := op.Name
	for i := range maxNameAttempts {
		if i > 0 {
			op.Name = fmt.Sprintf("%s-%d", base, i)
		}
		err := a.r.c.Create(ctx, op)
		if err == nil {
			return true, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		existing := &v1alpha1.Operation{}
		if gerr := a.r.cfg.APIReader.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: op.Name}, existing); gerr != nil {
			if apierrors.IsNotFound(gerr) {
				continue
			}
			return false, gerr
		}
		if !existing.Status.Phase.IsTerminal() || existing.Status.StartedAt != nil {
			return false, nil
		}
	}
	op.Name, op.GenerateName = "", base+"-"
	if err := a.r.c.Create(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

// managedStatuses returns the sync status of every managed resource (status.resources is
// bounded; the auto-sync rules need all of them).
func managedStatuses(res *compare.Result) []v1alpha1.ResourceStatus {
	out := make([]v1alpha1.ResourceStatus, 0, len(res.Managed))
	for _, m := range res.Managed {
		out = append(out, v1alpha1.ResourceStatus{
			Group: m.Key.Group, Version: m.Version, Kind: m.Key.Kind, Namespace: m.Key.Namespace, Name: m.Key.Name,
			Status: m.Status, Hook: m.Hook, RequiresPruning: m.RequiresPruning,
		})
	}
	return out
}
