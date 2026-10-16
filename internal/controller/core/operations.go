package core

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// OperationApplicationIndex is the cache field index of Operations by spec.application.
// Decisions that must be correct (one non-terminal Operation per Application, retention)
// use spec.application, never the optional cd.patrol.example/application label.
const OperationApplicationIndex = "spec.application"

// IndexOperations registers OperationApplicationIndex.
func IndexOperations(ctx context.Context, indexer client.FieldIndexer) error {
	err := indexer.IndexField(ctx, &v1alpha1.Operation{}, OperationApplicationIndex, func(o client.Object) []string {
		op, ok := o.(*v1alpha1.Operation)
		if !ok || op.Spec.Application == "" {
			return nil
		}
		return []string{op.Spec.Application}
	})
	if err != nil {
		return fmt.Errorf("index operations by application: %w", err)
	}
	return nil
}

// ListOperations returns the Operations of an Application from a cache with
// OperationApplicationIndex, oldest first (see Older).
func ListOperations(ctx context.Context, reader client.Reader, namespace, app string) ([]v1alpha1.Operation, error) {
	var list v1alpha1.OperationList
	if err := reader.List(ctx, &list, client.InNamespace(namespace), client.MatchingFields{OperationApplicationIndex: app}); err != nil {
		return nil, fmt.Errorf("list operations of %s/%s: %w", namespace, app, err)
	}
	SortOperations(list.Items)
	return list.Items, nil
}

// ListOperationsLive lists the Operations of an Application from a reader without field
// indexes (the API server), oldest first. Admission and retention use it so a stale cache
// cannot admit two Operations.
//
// The API server filters by label: the Operations labelled cd.patrol.example/application=app
// (when app is a valid label value) plus the unlabelled ones, both filtered by
// spec.application. Every Operation the controller has processed carries the label (the
// Operation reconciler adds or corrects it before admission) and the Operation admission
// policy rejects a label that differs from spec.application, so no Operation of app is
// missed while the transfer stays proportional to app's own Operations.
func ListOperationsLive(ctx context.Context, reader client.Reader, namespace, app string) ([]v1alpha1.Operation, error) {
	var out []v1alpha1.Operation
	keep := func(items []v1alpha1.Operation) {
		for i := range items {
			if items[i].Spec.Application == app {
				out = append(out, items[i])
			}
		}
	}
	if len(validation.IsValidLabelValue(app)) == 0 {
		var labelled v1alpha1.OperationList
		if err := reader.List(ctx, &labelled, client.InNamespace(namespace), client.MatchingLabels{v1alpha1.LabelKeyOperationApplication: app}); err != nil {
			return nil, fmt.Errorf("list operations of %s/%s: %w", namespace, app, err)
		}
		keep(labelled.Items)
	}
	var unlabelled v1alpha1.OperationList
	if err := reader.List(ctx, &unlabelled, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: unlabelledSelector}); err != nil {
		return nil, fmt.Errorf("list unlabelled operations in %s: %w", namespace, err)
	}
	keep(unlabelled.Items)
	SortOperations(out)
	return out, nil
}

// unlabelledSelector selects Operations without the application label.
var unlabelledSelector = func() labels.Selector {
	req, err := labels.NewRequirement(v1alpha1.LabelKeyOperationApplication, selection.DoesNotExist, nil)
	if err != nil {
		// The key is a constant valid label key.
		return labels.Nothing()
	}
	return labels.NewSelector().Add(*req)
}()

// ApplicationLabelValue returns the cd.patrol.example/application label value of the
// Operations of app, or "" when app is not a valid label value (such Operations stay
// unlabelled).
func ApplicationLabelValue(app string) string {
	if len(validation.IsValidLabelValue(app)) == 0 {
		return app
	}
	return ""
}

// Older reports whether a was created before b: creationTimestamp first, then name (the
// admission order of sync-state-machine.md).
func Older(a, b *v1alpha1.Operation) bool {
	return compareAge(a, b) < 0
}

func compareAge(a, b *v1alpha1.Operation) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// SortOperations orders ops oldest first.
func SortOperations(ops []v1alpha1.Operation) {
	slices.SortFunc(ops, func(a, b v1alpha1.Operation) int { return compareAge(&a, &b) })
}

// IsActive reports whether an Operation holds its Application's operation slot: it was
// admitted (Running or Terminating).
func IsActive(op *v1alpha1.Operation) bool {
	return op.Status.Phase == v1alpha1.OperationRunning || op.Status.Phase == v1alpha1.OperationTerminating
}

// IsNonTerminal reports whether an Operation has not reached a terminal phase (it is
// Pending, Running or Terminating, or not processed yet).
func IsNonTerminal(op *v1alpha1.Operation) bool { return !op.Status.Phase.IsTerminal() }

// HasNonTerminal reports whether any of ops is non-terminal.
func HasNonTerminal(ops []v1alpha1.Operation) bool {
	for i := range ops {
		if IsNonTerminal(&ops[i]) {
			return true
		}
	}
	return false
}

// ApplicationOwnerReference is the controller owner reference of an Operation to its
// Application (garbage collection removes Operations with their Application).
func ApplicationOwnerReference(app *v1alpha1.Application) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "Application",
		Name:               app.Name,
		UID:                app.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// HasOwnerReference reports whether obj has an owner reference to app.
func HasOwnerReference(obj metav1.Object, app *v1alpha1.Application) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == app.UID {
			return true
		}
	}
	return false
}
