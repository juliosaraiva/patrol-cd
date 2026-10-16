package generators

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Matrix and Merge errors (upstream texts).
var (
	ErrMoreThanTwoGenerators        = errors.New("found more than two generators, Matrix support only two")
	ErrLessThanTwoGenerators        = errors.New("found less than two generators, Matrix support only two")
	ErrMoreThenOneInnerGenerators   = errors.New("found more than one generator in matrix.Generators")
	ErrLessThanTwoGeneratorsInMerge = errors.New("found less than two generators, Merge requires two or more")
	ErrNoMergeKeys                  = errors.New("no merge keys were specified, Merge requires at least one")
	ErrNonUniqueParamSets           = errors.New("the parameters from a generator were not unique by the given mergeKeys, Merge requires all param sets to be unique")
)

const maxDuration time.Duration = 1<<63 - 1

// MatrixGenerator combines the parameter sets of exactly two child generators. The second child
// is interpolated with each parameter set of the first.
type MatrixGenerator struct {
	children Set
	// maxParams bounds the product (zero: DefaultMaxParameterSets).
	maxParams int
}

// Template implements Generator. Child templates are never applied.
func (g *MatrixGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.Matrix.Template
}

// RequeueAfter implements Generator: the smallest non-zero child interval.
func (g *MatrixGenerator) RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration {
	return minChildRequeue(g.children, gen.Matrix.Generators)
}

// GenerateParams implements Generator. With goTemplate, each combination is the deep merge of
// the second child's parameters overridden by the first child's; with fasttemplate the string
// maps are unioned and conflicting keys fail.
func (g *MatrixGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.Matrix == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	children := gen.Matrix.Generators
	if len(children) < 2 {
		return nil, ErrLessThanTwoGenerators
	}
	if len(children) > 2 {
		return nil, ErrMoreThanTwoGenerators
	}
	first, err := childParams(ctx, g.children, children[0], appSet, nil)
	if err != nil {
		return nil, fmt.Errorf("error failed to get params for first generator in matrix generator: %w", err)
	}
	max := g.maxParams
	if max == 0 {
		max = DefaultMaxParameterSets
	}
	res := []map[string]any{}
	for _, a := range first {
		second, err := childParams(ctx, g.children, children[1], appSet, a)
		if err != nil {
			return nil, fmt.Errorf("failed to get params for second generator in the matrix generator: %w", err)
		}
		// Checked before combining: the product is the ApplicationSet's to choose.
		if len(second) > max-len(res) {
			return nil, tooManyParameterSets(max)
		}
		for _, b := range second {
			if appSet.Spec.GoTemplate {
				combined := map[string]any{}
				mergeOverride(combined, b)
				mergeOverride(combined, a)
				res = append(res, combined)
				continue
			}
			combined, err := combineStringMaps(a, b)
			if err != nil {
				return nil, fmt.Errorf("failed to combine string maps with merging params for the matrix generator: %w", err)
			}
			res = append(res, combined)
		}
	}
	return res, nil
}

// MergeGenerator merges the parameter sets of two or more children on mergeKeys.
type MergeGenerator struct {
	children Set
}

// Template implements Generator. Child templates are never applied.
func (g *MergeGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.Merge.Template
}

// RequeueAfter implements Generator: the smallest non-zero child interval.
func (g *MergeGenerator) RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration {
	return minChildRequeue(g.children, gen.Merge.Generators)
}

// GenerateParams implements Generator. The first child is the base; each later child, in order,
// overrides the base parameter sets with the same merge key values (fasttemplate: shallow;
// goTemplate: deep merge). Unmatched base sets are kept and unmatched later sets dropped. The
// output follows the order of the base child.
func (g *MergeGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.Merge == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	children := gen.Merge.Generators
	if len(children) < 2 {
		return nil, ErrLessThanTwoGeneratorsInMerge
	}
	all := make([][]map[string]any, 0, len(children))
	for i, child := range children {
		params, err := childParams(ctx, g.children, child, appSet, nil)
		if err != nil {
			return nil, fmt.Errorf("error getting param sets from generators: error getting params from generator %d of %d: %w", i+1, len(children), err)
		}
		all = append(all, params)
	}
	baseKeys, base, err := paramSetsByMergeKey(gen.Merge.MergeKeys, all[0])
	if err != nil {
		return nil, fmt.Errorf("error getting param sets by merge key: %w", err)
	}
	for _, sets := range all[1:] {
		_, overrides, err := paramSetsByMergeKey(gen.Merge.MergeKeys, sets)
		if err != nil {
			return nil, fmt.Errorf("error getting param sets by merge key: %w", err)
		}
		for key, baseSet := range base {
			override, ok := overrides[key]
			if !ok {
				continue
			}
			if appSet.Spec.GoTemplate {
				mergeOverride(baseSet, override)
			} else {
				for k, v := range override {
					baseSet[k] = v
				}
			}
		}
	}
	res := make([]map[string]any, 0, len(baseKeys))
	for _, k := range baseKeys {
		res = append(res, base[k])
	}
	return res, nil
}

// paramSetsByMergeKey indexes parameter sets by the JSON of their merge key values (top-level
// keys only; duplicate merge keys are de-duplicated). It returns the keys in input order.
func paramSetsByMergeKey(mergeKeys []string, sets []map[string]any) ([]string, map[string]map[string]any, error) {
	if len(mergeKeys) < 1 {
		return nil, nil, ErrNoMergeKeys
	}
	unique := map[string]bool{}
	for _, k := range mergeKeys {
		unique[k] = true
	}
	order := make([]string, 0, len(sets))
	out := make(map[string]map[string]any, len(sets))
	for _, set := range sets {
		key := map[string]any{}
		for k := range unique {
			key[k] = set[k]
		}
		b, err := json.Marshal(key)
		if err != nil {
			return nil, nil, fmt.Errorf("error marshalling param set key json: %w", err)
		}
		ks := string(b)
		if _, dup := out[ks]; dup {
			return nil, nil, fmt.Errorf("%w. Duplicate key was %s", ErrNonUniqueParamSets, ks)
		}
		out[ks] = copyParams(set)
		order = append(order, ks)
	}
	return order, out, nil
}

// childParams generates the parameters of one Matrix/Merge child. Nested matrix/merge
// generators are decoded from their raw JSON; their own children may only be terminal.
func childParams(ctx context.Context, set Set, child v1alpha1.ApplicationSetNestedGenerator, appSet *v1alpha1.ApplicationSet, params map[string]any) ([]map[string]any, error) {
	entry, err := nestedToGenerator(child)
	if err != nil {
		return nil, err
	}
	results, err := set.Transform(ctx, entry, v1alpha1.ApplicationSetTemplate{}, appSet, params)
	if err != nil {
		return nil, fmt.Errorf("child generator returned an error on parameter generation: %w", err)
	}
	if len(results) == 0 {
		return nil, errors.New("child generator generated no parameters")
	}
	if len(results) > 1 {
		return nil, ErrMoreThenOneInnerGenerators
	}
	return results[0].Params, nil
}

// nestedToGenerator converts a Matrix/Merge child into a generator entry.
func nestedToGenerator(n v1alpha1.ApplicationSetNestedGenerator) (v1alpha1.ApplicationSetGenerator, error) {
	out := v1alpha1.ApplicationSetGenerator{
		List:                    n.List,
		Clusters:                n.Clusters,
		Git:                     n.Git,
		SCMProvider:             n.SCMProvider,
		ClusterDecisionResource: n.ClusterDecisionResource,
		PullRequest:             n.PullRequest,
		Plugin:                  n.Plugin,
		Selector:                n.Selector,
	}
	if n.Matrix != nil {
		m, err := v1alpha1.ToNestedMatrixGenerator(n.Matrix)
		if err != nil {
			return out, err
		}
		out.Matrix = &v1alpha1.MatrixGenerator{Generators: terminalToNested(m.Generators)}
	}
	if n.Merge != nil {
		m, err := v1alpha1.ToNestedMergeGenerator(n.Merge)
		if err != nil {
			return out, fmt.Errorf("error converting to nested merge generator: %w", err)
		}
		out.Merge = &v1alpha1.MergeGenerator{Generators: terminalToNested(m.Generators), MergeKeys: m.MergeKeys}
	}
	return out, nil
}

func terminalToNested(in v1alpha1.ApplicationSetTerminalGenerators) []v1alpha1.ApplicationSetNestedGenerator {
	out := make([]v1alpha1.ApplicationSetNestedGenerator, 0, len(in))
	for _, t := range in {
		out = append(out, v1alpha1.ApplicationSetNestedGenerator{
			List:                    t.List,
			Clusters:                t.Clusters,
			Git:                     t.Git,
			SCMProvider:             t.SCMProvider,
			ClusterDecisionResource: t.ClusterDecisionResource,
			PullRequest:             t.PullRequest,
			Plugin:                  t.Plugin,
			Selector:                t.Selector,
		})
	}
	return out
}

func minChildRequeue(set Set, children []v1alpha1.ApplicationSetNestedGenerator) time.Duration {
	res := maxDuration
	found := false
	for _, child := range children {
		entry, err := nestedToGenerator(child)
		if err != nil {
			continue
		}
		entry.Selector = nil
		for _, kind := range Relevant(&entry) {
			g, ok := set[kind]
			if !ok || g == nil {
				continue
			}
			d := g.RequeueAfter(&entry)
			if d != NoRequeueAfter && d < res {
				res, found = d, true
			}
		}
	}
	if !found {
		return NoRequeueAfter
	}
	return res
}
