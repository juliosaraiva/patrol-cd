package appset

import (
	"context"
	"fmt"
	"slices"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Condition messages (upstream texts).
const (
	msgParametersGenerated = "Successfully generated parameters for all Applications"
	msgUpToDate            = "All applications have been generated successfully"
	msgRolloutComplete     = "ApplicationSet Rollout has completed"
	msgRolloutStepPrefix   = "ApplicationSet is performing rollout of step "
	msgNoConditions        = "No status conditions found for ApplicationSet"
	msgWaitingForHealth    = "Waiting for health status to be determined"
)

// condition is a condition to set (LastTransitionTime is managed by setConditions).
type condition struct {
	Type    string
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// IsRollingSyncStrategy reports whether appSet selects the RollingSync strategy.
func IsRollingSyncStrategy(appSet *v1alpha1.ApplicationSet) bool {
	s := appSet.Spec.Strategy
	return s != nil && s.Type == v1alpha1.ApplicationSetStrategyRollingSync && s.RollingSync != nil
}

// RollingSyncEnabled reports whether appSet selects RollingSync with at least one step.
func RollingSyncEnabled(appSet *v1alpha1.ApplicationSet) bool {
	return IsRollingSyncStrategy(appSet) && len(appSet.Spec.Strategy.RollingSync.Steps) > 0
}

// IsDeletionOrderReversed reports whether appSet's Applications are deleted in reverse step
// order when it is deleted.
func IsDeletionOrderReversed(appSet *v1alpha1.ApplicationSet) bool {
	return RollingSyncEnabled(appSet) && appSet.Spec.Strategy.DeletionOrder == v1alpha1.ApplicationSetDeletionOrderReverse
}

// conditionsFor computes the conditions written for c and the condition types it evaluates,
// with the upstream dependencies between them: ParametersGenerated is always evaluated,
// ErrorOccurred=True forces ResourcesUpToDate=False, ResourcesUpToDate=True forces
// ErrorOccurred=False, and RolloutProgressing is removed from non-RollingSync ApplicationSets.
func conditionsFor(appSet *v1alpha1.ApplicationSet, c condition, parametersGenerated bool) ([]condition, map[string]bool) {
	evaluated := map[string]bool{
		v1alpha1.ApplicationSetConditionParametersGenerated: true,
		v1alpha1.ApplicationSetConditionErrorOccurred:       false,
		v1alpha1.ApplicationSetConditionResourcesUpToDate:   false,
		v1alpha1.ApplicationSetConditionRolloutProgressing:  false,
	}
	evaluated[c.Type] = true
	out := []condition{c}
	if !IsRollingSyncStrategy(appSet) {
		evaluated[v1alpha1.ApplicationSetConditionRolloutProgressing] = true
	}
	if c.Type != v1alpha1.ApplicationSetConditionParametersGenerated {
		if parametersGenerated {
			out = append(out, condition{
				Type: v1alpha1.ApplicationSetConditionParametersGenerated, Status: metav1.ConditionTrue,
				Reason: v1alpha1.ApplicationSetReasonParametersGenerated, Message: msgParametersGenerated,
			})
		} else {
			out = append(out, condition{
				Type: v1alpha1.ApplicationSetConditionParametersGenerated, Status: metav1.ConditionFalse,
				Reason: v1alpha1.ApplicationSetReasonErrorOccurred, Message: c.Message,
			})
		}
	}
	switch c.Type {
	case v1alpha1.ApplicationSetConditionResourcesUpToDate:
		if c.Status == metav1.ConditionTrue {
			evaluated[v1alpha1.ApplicationSetConditionErrorOccurred] = true
			out = append(out, condition{
				Type: v1alpha1.ApplicationSetConditionErrorOccurred, Status: metav1.ConditionFalse,
				Reason: c.Reason, Message: c.Message,
			})
		}
	case v1alpha1.ApplicationSetConditionErrorOccurred:
		if c.Status == metav1.ConditionTrue {
			evaluated[v1alpha1.ApplicationSetConditionResourcesUpToDate] = true
			out = append(out, condition{
				Type: v1alpha1.ApplicationSetConditionResourcesUpToDate, Status: metav1.ConditionFalse,
				Reason: v1alpha1.ApplicationSetReasonErrorOccurred, Message: c.Message,
			})
		}
	case v1alpha1.ApplicationSetConditionRolloutProgressing:
		if !IsRollingSyncStrategy(appSet) {
			evaluated[c.Type] = false
		}
	}
	return out, evaluated
}

// applyConditions merges conditions into status: conditions of non-evaluated types are kept,
// evaluated types not in conditions are removed, and an unchanged condition (same status,
// reason and message) keeps its lastTransitionTime. The list is sorted by type and the health
// is recomputed.
func applyConditions(status *v1alpha1.ApplicationSetStatus, conditions []condition, evaluated map[string]bool, generation int64, now metav1.Time) {
	out := make([]metav1.Condition, 0, len(status.Conditions)+len(conditions))
	for _, cur := range status.Conditions {
		if !evaluated[cur.Type] {
			out = append(out, cur)
		}
	}
	for _, c := range conditions {
		if !evaluated[c.Type] {
			continue
		}
		next := metav1.Condition{
			Type: c.Type, Status: c.Status, Reason: c.Reason,
			Message: truncateMessage(c.Message), LastTransitionTime: now, ObservedGeneration: generation,
		}
		for _, cur := range status.Conditions {
			if cur.Type == c.Type && cur.Status == next.Status && cur.Reason == next.Reason && cur.Message == next.Message {
				next.LastTransitionTime = cur.LastTransitionTime
			}
		}
		out = append(out, next)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	status.Conditions = out
	status.Health = CalculateHealth(status)
}

func truncateMessage(m string) string {
	if len(m) <= maxConditionMessage {
		return m
	}
	const suffix = "... (truncated)"
	return m[:maxConditionMessage-len(suffix)] + suffix
}

// CalculateHealth derives the ApplicationSet health from its conditions: ErrorOccurred=True is
// Degraded, RolloutProgressing=True Progressing, ResourcesUpToDate=True Healthy, otherwise
// Unknown.
func CalculateHealth(status *v1alpha1.ApplicationSetStatus) v1alpha1.HealthStatus {
	if len(status.Conditions) == 0 {
		return v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusUnknown, Message: msgNoConditions}
	}
	var progressing, healthy *metav1.Condition
	for i := range status.Conditions {
		c := &status.Conditions[i]
		if c.Status != metav1.ConditionTrue {
			continue
		}
		switch c.Type {
		case v1alpha1.ApplicationSetConditionErrorOccurred:
			return v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusDegraded, Message: c.Message}
		case v1alpha1.ApplicationSetConditionRolloutProgressing:
			progressing = c
		case v1alpha1.ApplicationSetConditionResourcesUpToDate:
			healthy = c
		}
	}
	switch {
	case progressing != nil:
		return v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusProgressing, Message: progressing.Message}
	case healthy != nil:
		return v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy, Message: healthy.Message}
	}
	return v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusUnknown, Message: msgWaitingForHealth}
}

// conditionsChanged reports whether two condition lists differ in type, status, reason or
// message (lastTransitionTime and observedGeneration alone do not cause a write).
func conditionsChanged(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Status != b[i].Status || a[i].Reason != b[i].Reason || a[i].Message != b[i].Message || a[i].ObservedGeneration != b[i].ObservedGeneration {
			return true
		}
	}
	return false
}

// setCondition sets c (and its dependent conditions) on appSet and persists the status when
// the conditions changed. A missing ApplicationSet is not an error.
func (r *Reconciler) setCondition(ctx context.Context, appSet *v1alpha1.ApplicationSet, c condition, parametersGenerated bool) error {
	conds, evaluated := conditionsFor(appSet, c, parametersGenerated)
	orig := appSet.DeepCopy()
	applyConditions(&appSet.Status, conds, evaluated, appSet.Generation, metav1.NewTime(r.cfg.now()))
	appSet.Status.ObservedGeneration = appSet.Generation
	if !conditionsChanged(orig.Status.Conditions, appSet.Status.Conditions) && orig.Status.Health == appSet.Status.Health &&
		orig.Status.ObservedGeneration == appSet.Status.ObservedGeneration {
		return nil
	}
	if err := r.patchStatus(ctx, orig, appSet); err != nil {
		return fmt.Errorf("unable to set application set condition: %w", err)
	}
	return nil
}

// patchStatus writes the status of modified with a merge patch computed from orig. The
// controller is the only writer of the status, so no optimistic lock is used; arrays (conditions,
// applicationStatus, resources) are replaced as a whole.
func (r *Reconciler) patchStatus(ctx context.Context, orig, modified *v1alpha1.ApplicationSet) error {
	if err := r.client.Status().Patch(ctx, modified, client.MergeFrom(orig)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// updateResourcesStatus rebuilds status.resources from the owned Applications (sorted by name,
// truncated to MaxResourcesStatusCount; resourcesCount keeps the total).
func (r *Reconciler) updateResourcesStatus(ctx context.Context, appSet *v1alpha1.ApplicationSet, apps []v1alpha1.Application) error {
	statuses := make([]v1alpha1.ResourceStatus, 0, len(apps))
	for i := range apps {
		app := &apps[i]
		statuses = append(statuses, v1alpha1.ResourceStatus{
			Group:     v1alpha1.GroupVersion.Group,
			Version:   v1alpha1.GroupVersion.Version,
			Kind:      "Application",
			Name:      app.Name,
			Namespace: app.Namespace,
			Status:    app.Status.Sync.Status,
			Health:    &v1alpha1.HealthStatus{Status: app.Status.Health.Status},
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	count := int64(len(statuses))
	if limit := r.cfg.MaxResourcesStatusCount; limit > 0 && len(statuses) > limit {
		r.cfg.log.WarnContext(ctx, "truncating ApplicationSet resource status",
			appSetAttr(appSet), "total", len(statuses), "max", limit)
		statuses = statuses[:limit]
	}
	orig := appSet.DeepCopy()
	if len(statuses) == 0 {
		statuses = nil
	}
	appSet.Status.Resources = statuses
	appSet.Status.ResourcesCount = count
	changed, err := jsonChanged(orig.Status.Resources, appSet.Status.Resources)
	if err != nil {
		return err
	}
	if !changed && orig.Status.ResourcesCount == count {
		return nil
	}
	if err := r.patchStatus(ctx, orig, appSet); err != nil {
		return fmt.Errorf("unable to set application set status: %w", err)
	}
	return nil
}

func jsonChanged(a, b any) (bool, error) {
	eq, err := jsonEqual(a, b)
	return !eq, err
}

// setApplicationStatus persists status.applicationStatus (sorted by Application name) when an
// entry's status, step, message or target revisions changed, or entries were added or removed.
// (Upstream ignores target revision changes, so a revision change that ends in the same status
// and message is never recorded and is re-detected on every reconcile.)
func (r *Reconciler) setApplicationStatus(ctx context.Context, appSet *v1alpha1.ApplicationSet, statuses []v1alpha1.ApplicationSetApplicationStatus) error {
	changed := len(statuses) != len(appSet.Status.ApplicationStatus)
	if !changed {
		for _, s := range statuses {
			idx := findApplicationStatus(appSet.Status.ApplicationStatus, s.Application)
			if idx < 0 {
				changed = true
				break
			}
			cur := appSet.Status.ApplicationStatus[idx]
			if cur.Status != s.Status || cur.Step != s.Step || cur.Message != s.Message || !slices.Equal(cur.TargetRevisions, s.TargetRevisions) {
				changed = true
				break
			}
		}
	}
	if !changed {
		return nil
	}
	orig := appSet.DeepCopy()
	sorted := make([]v1alpha1.ApplicationSetApplicationStatus, len(statuses))
	copy(sorted, statuses)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Application < sorted[j].Application })
	if len(sorted) == 0 {
		sorted = nil
	}
	appSet.Status.ApplicationStatus = sorted
	if err := r.patchStatus(ctx, orig, appSet); err != nil {
		return fmt.Errorf("unable to set application set status: %w", err)
	}
	return nil
}

func findApplicationStatus(statuses []v1alpha1.ApplicationSetApplicationStatus, name string) int {
	for i := range statuses {
		if statuses[i].Application == name {
			return i
		}
	}
	return -1
}
