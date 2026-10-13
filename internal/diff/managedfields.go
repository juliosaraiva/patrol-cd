package diff

import (
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"sigs.k8s.io/structured-merge-diff/v6/typed"
)

// normalizeTrustedManagers runs NormalizeManagedFields when o.Normalizer declares trusted
// managers for the config's group/kind/name/namespace.
func normalizeTrustedManagers(config, live *unstructured.Unstructured, o Options) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	p, ok := o.Normalizer.(TrustedManagersProvider)
	if !ok {
		return config, live, nil
	}
	gvk := config.GroupVersionKind()
	managers := p.TrustedManagers(gvk.GroupKind(), config.GetName(), config.GetNamespace())
	if len(managers) == 0 {
		return config, live, nil
	}
	normLive, normConfig, err := NormalizeManagedFields(live, config, managers, o.GVKParser)
	if err != nil {
		return nil, nil, fmt.Errorf("managedFieldsManagers normalization of %s: %w", describe(config), err)
	}
	return normConfig, normLive, nil
}

// NormalizeManagedFields implements the managedFieldsManagers ignore rule: every field that
// config would change (same path, different value) and that is owned in live.managedFields by
// one of trustedManagers is removed from both objects, so the trusted manager's value wins
// and is not reported as drift. Inputs are not modified; the returned objects are copies.
//
// When the objects cannot be typed with the schema (e.g. they violate it) they are returned
// unchanged, as in the baseline. Without a parser the deduced schema is used, which treats
// lists atomically.
func NormalizeManagedFields(live, config *unstructured.Unstructured, trustedManagers []string, parser *managedfields.GvkParser) (normLive, normConfig *unstructured.Unstructured, err error) {
	if live == nil || config == nil {
		return live, config, nil
	}
	liveCopy, configCopy := live.DeepCopy(), config.DeepCopy()
	if len(trustedManagers) == 0 {
		return liveCopy, configCopy, nil
	}
	ty := resolveTyper(config.GroupVersionKind(), parser)
	if ty == nil {
		ty = parseableTyper{&typed.DeducedParseableType}
	}
	tvLive, err := ty.fromUnstructured(liveCopy.Object)
	if err != nil {
		return liveCopy, configCopy, nil //nolint:nilerr // untypable objects are compared as-is (baseline behavior)
	}
	tvConfig, err := ty.fromUnstructured(configCopy.Object)
	if err != nil {
		return liveCopy, configCopy, nil //nolint:nilerr // see above
	}
	cmp, err := tvLive.Compare(tvConfig)
	if err != nil {
		return liveCopy, configCopy, nil //nolint:nilerr // see above
	}

	normalized := false
	for _, e := range live.GetManagedFields() {
		if !slices.Contains(trustedManagers, e.Manager) {
			continue
		}
		set, err := fieldSetOf(e)
		if err != nil {
			return nil, nil, fmt.Errorf("managedFields of %s: %w", e.Manager, err)
		}
		normalized = true
		overlap := set.Intersection(cmp.Modified)
		if overlap.Empty() {
			continue
		}
		tvLive = tvLive.RemoveItems(overlap)
		tvConfig = tvConfig.RemoveItems(overlap)
	}
	if !normalized {
		return liveCopy, configCopy, nil
	}
	l, ok := tvLive.AsValue().Unstructured().(map[string]any)
	if !ok {
		return nil, nil, errors.New("normalized live state is not an object")
	}
	c, ok := tvConfig.AsValue().Unstructured().(map[string]any)
	if !ok {
		return nil, nil, errors.New("normalized config is not an object")
	}
	return &unstructured.Unstructured{Object: deepCopyMap(l)}, &unstructured.Unstructured{Object: deepCopyMap(c)}, nil
}
