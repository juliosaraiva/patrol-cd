package diff

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// serverSideDiff asks the API server (through the DryRunner) what the object would look like
// after applying config, then compares that with live. config and live are owned by the
// caller (already copied and pre-normalized without the user normalizer).
func serverSideDiff(ctx context.Context, config, live *unstructured.Unstructured, o Options) (*Result, error) {
	if o.DryRunner == nil {
		return nil, ErrNoDryRunner
	}
	predicted, err := o.DryRunner.ServerSideApplyDryRun(ctx, config.DeepCopy(), o.Manager)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDryRun, err)
	}
	if predicted == nil || predicted.Object == nil {
		return nil, fmt.Errorf("%w: empty result", ErrDryRun)
	}
	if predicted, err = roundTrip(predicted); err != nil {
		return nil, err
	}
	if o.IgnoreMutationWebhook {
		if predicted, err = removeWebhookMutation(predicted, live, o.GVKParser, o.Manager); err != nil {
			return nil, fmt.Errorf("remove mutation webhook changes: %w", err)
		}
	}
	// Give the predicted state the same treatment the live state received.
	if predicted, err = remarshal(predicted, o.Log); err != nil {
		return nil, err
	}
	for _, obj := range []*unstructured.Unstructured{predicted, live} {
		normalizeObject(ctx, obj, o, false)
		unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
		unstructured.RemoveNestedField(obj.Object, "metadata", "resourceVersion")
		unstructured.RemoveNestedField(obj.Object, "metadata", "annotations", AnnotationLastAppliedConfig)
	}
	predictedBytes, err := marshalCanonical(predicted)
	if err != nil {
		return nil, fmt.Errorf("predicted state: %w", err)
	}
	liveBytes, err := marshalCanonical(live)
	if err != nil {
		return nil, fmt.Errorf("live state: %w", err)
	}
	return newResult(predictedBytes, liveBytes), nil
}

func marshalCanonical(obj *unstructured.Unstructured) ([]byte, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return canonicalJSON(data)
}

// removeWebhookMutation reverts, in the dry-run result, every field that is not owned by
// manager back to its live value: fields set by mutating webhooks (or defaulted by the API
// server) are therefore not reported as differences, while removals performed by the apply
// (fields the manager stopped declaring) are kept.
func removeWebhookMutation(predicted, live *unstructured.Unstructured, parser *managedfields.GvkParser, manager string) (*unstructured.Unstructured, error) {
	entries := predicted.GetManagedFields()
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoManagedFields, describe(predicted))
	}
	if parser == nil {
		return nil, ErrNoGVKParser
	}
	gvk := predicted.GroupVersionKind()
	pt := parser.Type(gvk)
	if pt == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSchema, gvk)
	}
	tvPredicted, err := pt.FromUnstructured(predicted.Object)
	if err != nil {
		return nil, fmt.Errorf("type predicted state as %s: %w", gvk, err)
	}
	tvLive, err := pt.FromUnstructured(live.Object)
	if err != nil {
		return nil, fmt.Errorf("type live state as %s: %w", gvk, err)
	}

	owned := fieldpath.NewSet()
	for _, e := range entries {
		set, err := fieldSetOf(e)
		if err != nil {
			return nil, fmt.Errorf("managedFields of %s: %w", e.Manager, err)
		}
		if e.Manager == manager {
			owned = owned.Union(set)
		}
	}
	if owned.Empty() {
		return nil, fmt.Errorf("%w: %s", ErrManagerNotFound, manager)
	}

	predictedFields, err := tvPredicted.ToFieldSet()
	if err != nil {
		return nil, fmt.Errorf("field set of predicted state: %w", err)
	}
	notOwned := withoutOwnedAncestors(predictedFields.Difference(owned), owned)

	cmp, err := tvLive.Compare(tvPredicted)
	if err != nil {
		return nil, fmt.Errorf("compare live and predicted state: %w", err)
	}
	removed := fieldpath.NewSet()
	if cmp.Removed != nil {
		// Removals of fields the manager does not own are not ours to report.
		removed = cmp.Removed.Difference(notOwned)
	}

	// Keep fields that identify associative list items; removing them would make items
	// unidentifiable when merging back onto live.
	notOwned = withoutCompositeKeyFields(notOwned)
	tvPredicted = tvPredicted.RemoveItems(notOwned)
	merged, err := tvLive.Merge(tvPredicted)
	if err != nil {
		return nil, fmt.Errorf("merge predicted state onto live state: %w", err)
	}
	// Merging restores fields that the apply removed; remove them again.
	merged = merged.RemoveItems(removed)
	m, ok := merged.AsValue().Unstructured().(map[string]any)
	if !ok {
		return nil, fmt.Errorf("predicted state is not an object: %T", merged.AsValue().Unstructured())
	}
	return &unstructured.Unstructured{Object: deepCopyMap(m)}, nil
}

// withoutOwnedAncestors drops from candidates every path that has an owned strict descendant.
// Under x-kubernetes-preserve-unknown-fields the API server records only the owned leaves,
// so the containers above them appear unowned; removing them would drop the owned leaves.
func withoutOwnedAncestors(candidates, owned *fieldpath.Set) *fieldpath.Set {
	out := fieldpath.NewSet()
	for p := range candidates.All() {
		sub := owned
		for _, pe := range p {
			if sub = sub.WithPrefix(pe); sub.Empty() {
				break
			}
		}
		if sub.Empty() {
			out.Insert(p)
		}
	}
	return out
}

// withoutCompositeKeyFields drops paths whose final field is one of the key fields of an
// enclosing associative-list item identified by a composite key
// (e.g. .ports[containerPort=80,protocol="TCP"].protocol).
func withoutCompositeKeyFields(paths *fieldpath.Set) *fieldpath.Set {
	out := fieldpath.NewSet()
	for p := range paths.All() {
		if !isCompositeKeyField(p) {
			out.Insert(p)
		}
	}
	return out
}

func isCompositeKeyField(p fieldpath.Path) bool {
	if len(p) == 0 || p[len(p)-1].FieldName == nil {
		return false
	}
	name := *p[len(p)-1].FieldName
	for i := len(p) - 2; i >= 0; i-- {
		key := p[i].Key
		if key == nil || len(*key) < 2 {
			continue
		}
		for _, f := range *key {
			if f.Name == name {
				return true
			}
		}
	}
	return false
}
