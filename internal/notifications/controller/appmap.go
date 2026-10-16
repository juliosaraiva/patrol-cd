package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// OperationGetter returns an Operation (cd.patrol.example/v1alpha1) as an unstructured map. A
// missing Operation is reported with an error for which apierrors.IsNotFound is true. Callers
// must not modify the returned map.
type OperationGetter interface {
	GetOperation(ctx context.Context, namespace, name string) (map[string]any, error)
}

// errInvalidApplication reports an object that cannot be converted.
var errInvalidApplication = errors.New("invalid application object")

// aggregateConditionTypes are Patrol's aggregate condition types; Argo CD has no equivalent, so
// they are not shown to templates.
var aggregateConditionTypes = map[string]bool{
	v1alpha1.ApplicationConditionReconciled: true,
	v1alpha1.ApplicationConditionSynced:     true,
	v1alpha1.ApplicationConditionHealthy:    true,
}

// TemplateApplication returns the Argo CD compatible form of an Application (unstructured JSON)
// that triggers and templates evaluate as .app. The input is not modified. Differences from the
// stored object, all toward the Argo CD v3.5.5 JSON shape:
//
//   - status.conditions keeps only active conditions of the Argo CD types (status True, Patrol's
//     aggregate Reconciled/Synced/Healthy types dropped) as {type, message, lastTransitionTime};
//     the key is removed when none is active.
//   - status.operationState is completed from the Operation it names (when ops is not nil and the
//     Operation exists and belongs to the Application): fields missing from the summary (phase,
//     message, startedAt, finishedAt, retryCount) are copied, syncResult gets the Operation's
//     per-resource results and managedNamespaceMetadata (the summary's revision and source
//     fields win), and operation is set to the requested operation {sync, initiatedBy, info,
//     retry[, rollback]} (spec.sync.manifests omitted).
//   - while that Operation is not terminal, the top-level operation field is set like Argo CD's
//     Application.operation.
//   - a Cancelled operation phase is shown as Failed (message "Operation terminated" when the
//     summary has none): Argo CD has no Cancelled phase, a terminated operation ends Failed, so
//     on-sync-failed style triggers fire for it as upstream.
//
// A missing Operation (deleted by retention) leaves the summary as is. Other lookup errors are
// returned so the caller retries instead of rendering incomplete notifications.
func TemplateApplication(ctx context.Context, app map[string]any, ops OperationGetter) (map[string]any, error) {
	out, ok := copyJSON(app).(map[string]any)
	if !ok || out == nil {
		return nil, fmt.Errorf("%w: not an object", errInvalidApplication)
	}
	meta, ok := out["metadata"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: missing metadata", errInvalidApplication)
	}
	status, ok := out["status"].(map[string]any)
	if !ok {
		return out, nil
	}
	convertConditions(status)
	opState, ok := status["operationState"].(map[string]any)
	if !ok {
		return out, nil
	}
	defer argoPhase(opState)
	name, _ := opState["operationName"].(string)
	if name == "" || ops == nil {
		return out, nil
	}
	namespace, _ := meta["namespace"].(string)
	appName, _ := meta["name"].(string)
	op, err := ops.GetOperation(ctx, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return out, nil
		}
		return nil, fmt.Errorf("getting operation %s/%s: %w", namespace, name, err)
	}
	enrichOperationState(out, opState, op, appName)
	return out, nil
}

// terminatedMessage is Argo CD's message of a terminated operation.
const terminatedMessage = "Operation terminated"

// argoPhase maps Patrol-only operation phases to the Argo CD phase of the same outcome.
func argoPhase(opState map[string]any) {
	if phase, _ := opState["phase"].(string); phase != string(v1alpha1.OperationCancelled) {
		return
	}
	opState["phase"] = string(v1alpha1.OperationFailed)
	if msg, _ := opState["message"].(string); msg == "" {
		opState["message"] = terminatedMessage
	}
}

// convertConditions rewrites status.conditions to the Argo CD ApplicationCondition shape.
func convertConditions(status map[string]any) {
	list, ok := status["conditions"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := c["type"].(string)
		if typ == "" || aggregateConditionTypes[typ] {
			continue
		}
		if s, ok := c["status"].(string); ok && s != "True" {
			continue
		}
		ac := map[string]any{"type": typ}
		if m, ok := c["message"]; ok {
			ac["message"] = m
		}
		if t, ok := c["lastTransitionTime"]; ok {
			ac["lastTransitionTime"] = t
		}
		out = append(out, ac)
	}
	if len(out) == 0 {
		delete(status, "conditions")
		return
	}
	status["conditions"] = out
}

// enrichOperationState merges the Operation into the summary (see TemplateApplication). out is
// the converted application and opState its status.operationState; both are private copies.
func enrichOperationState(out, opState, op map[string]any, appName string) {
	spec, _ := op["spec"].(map[string]any)
	opStatus, _ := op["status"].(map[string]any)
	if spec == nil {
		return
	}
	if owner, _ := spec["application"].(string); owner != appName {
		// Not this Application's operation: never expose another Application's results.
		return
	}
	if opStatus == nil {
		opStatus = map[string]any{}
	}
	// A summary and an Operation that disagree on startedAt describe different executions (an
	// Operation re-created under the same name); keep the summary only.
	if s, ok := opState["startedAt"].(string); ok && s != "" {
		if o, ok := opStatus["startedAt"].(string); ok && o != "" && o != s {
			return
		}
	}
	for _, k := range []string{"phase", "message", "startedAt", "finishedAt", "retryCount"} {
		if _, present := opState[k]; present {
			continue
		}
		if v, ok := opStatus[k]; ok {
			opState[k] = copyJSON(v)
		}
	}
	if sr, ok := opStatus["syncResult"].(map[string]any); ok {
		merged, _ := copyJSON(sr).(map[string]any)
		if summary, ok := opState["syncResult"].(map[string]any); ok {
			for k, v := range summary {
				merged[k] = v
			}
		}
		opState["syncResult"] = merged
	}
	operation := requestedOperation(spec, opState)
	if len(operation) == 0 {
		return
	}
	if _, present := opState["operation"]; !present {
		opState["operation"] = operation
	}
	phase, _ := opStatus["phase"].(string)
	if !terminalPhase(phase) {
		if _, present := out["operation"]; !present {
			out["operation"] = copyJSON(operation)
		}
	}
}

// requestedOperation builds Argo CD's Operation shape {sync, initiatedBy, info, retry} from an
// Operation spec (plus Patrol's rollback).
func requestedOperation(spec, opState map[string]any) map[string]any {
	operation := map[string]any{}
	if sync, ok := spec["sync"].(map[string]any); ok {
		s, _ := copyJSON(sync).(map[string]any)
		delete(s, "manifests")
		operation["sync"] = s
	}
	if rb, ok := spec["rollback"].(map[string]any); ok {
		operation["rollback"] = copyJSON(rb)
	}
	if ib, ok := spec["initiatedBy"].(map[string]any); ok && len(ib) > 0 {
		operation["initiatedBy"] = copyJSON(ib)
	} else if ib, ok := opState["initiatedBy"].(map[string]any); ok && len(ib) > 0 {
		operation["initiatedBy"] = copyJSON(ib)
	}
	for _, k := range []string{"info", "retry"} {
		if v, ok := spec[k]; ok && v != nil {
			operation[k] = copyJSON(v)
		}
	}
	return operation
}

// terminalPhase reports whether an Operation phase is final. An empty phase is Pending.
func terminalPhase(phase string) bool {
	switch v1alpha1.OperationPhase(phase) {
	case v1alpha1.OperationSucceeded, v1alpha1.OperationFailed, v1alpha1.OperationError,
		v1alpha1.OperationCancelled, v1alpha1.OperationRejected:
		return true
	}
	return false
}

// copyJSON deep-copies a JSON-like value (maps, slices, scalars). Unknown types are returned as
// is (they are immutable scalars in decoded JSON).
func copyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = copyJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = copyJSON(val)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, val := range t {
			out[k] = val
		}
		return out
	case []string:
		return append([]string(nil), t...)
	default:
		return v
	}
}
