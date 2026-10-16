// Package retention bounds the Operations kept per Application (ADR-0011): the newest
// revisionHistoryLimit terminal Operations are kept, plus every non-terminal Operation and the
// Operation summarized in Application.status.operationState; older terminal ones are deleted.
package retention

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
)

// Select returns the Operations to delete from ops (one Application's Operations, any
// order): the terminal ones older than the newest limit terminal ones, except keep (the
// Operation named by status.operationState). Non-terminal Operations are never selected. The
// result is ordered oldest first; ops is not modified.
func Select(ops []v1alpha1.Operation, limit int, keep string) []v1alpha1.Operation {
	if limit < 0 {
		limit = 0
	}
	sorted := append([]v1alpha1.Operation(nil), ops...)
	core.SortOperations(sorted)
	var terminal []v1alpha1.Operation
	for _, op := range sorted {
		if op.Status.Phase.IsTerminal() {
			terminal = append(terminal, op)
		}
	}
	if len(terminal) <= limit {
		return nil
	}
	var out []v1alpha1.Operation
	for _, op := range terminal[:len(terminal)-limit] {
		if op.Name == keep {
			continue
		}
		out = append(out, op)
	}
	return out
}

// Needed reports whether ops (one Application's Operations) hold more than limit terminal
// Operations, i.e. whether Collect could delete anything.
func Needed(ops []v1alpha1.Operation, limit int) bool {
	limit = max(limit, 0)
	n := 0
	for i := range ops {
		if ops[i].Status.Phase.IsTerminal() {
			n++
		}
	}
	return n > limit
}

// CollectIfNeeded runs Collect only when the cached Operations of app (read through c, which
// must have core.OperationApplicationIndex) count more than the retention limit of terminal
// ones, so the API server is listed only when something may be deleted. current, when not
// nil, replaces its cached copy (an Operation the cache may still show as running). A cache
// that lags behind is caught up by the next terminal Operation reconcile.
func CollectIfNeeded(ctx context.Context, c client.Client, reader client.Reader, app *v1alpha1.Application, current *v1alpha1.Operation, log *slog.Logger) (int, error) {
	ops, err := core.ListOperations(ctx, c, app.Namespace, app.Name)
	if err != nil {
		return 0, err
	}
	if current != nil {
		found := false
		for i := range ops {
			if ops[i].UID == current.UID {
				ops[i] = *current
				found = true
			}
		}
		if !found {
			ops = append(ops, *current)
		}
	}
	if !Needed(ops, core.RevisionHistoryLimit(app)) {
		return 0, nil
	}
	return Collect(ctx, c, reader, app, log)
}

// Collect deletes the Operations of app that Select chooses, reading them from reader (the
// API server, so recently created Operations are counted). It returns the number deleted.
// Deletions carry a UID precondition; Operations already gone are ignored.
func Collect(ctx context.Context, c client.Client, reader client.Reader, app *v1alpha1.Application, log *slog.Logger) (int, error) {
	ops, err := core.ListOperationsLive(ctx, reader, app.Namespace, app.Name)
	if err != nil {
		return 0, err
	}
	keep := ""
	if app.Status.OperationState != nil {
		keep = app.Status.OperationState.OperationName
	}
	deleted := 0
	for _, op := range Select(ops, core.RevisionHistoryLimit(app), keep) {
		uid := op.UID
		err := c.Delete(ctx, &op, client.Preconditions{UID: &uid})
		switch {
		case err == nil:
			deleted++
			if log != nil {
				log.Debug("deleted operation beyond retention", "application", app.QualifiedName(), "operation", op.Name, "phase", op.Status.Phase)
			}
		case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		default:
			return deleted, fmt.Errorf("delete operation %s/%s: %w", op.Namespace, op.Name, err)
		}
	}
	return deleted, nil
}
