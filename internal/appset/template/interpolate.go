package template

import (
	"fmt"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// generatorsWithValues are the JSON keys of generator types that have a `values` map
// (map[string]string). Their values are pre-resolved specially during interpolation.
var generatorsWithValues = map[string]bool{
	"clusters":                true,
	"git":                     true,
	"scmProvider":             true,
	"clusterDecisionResource": true,
	"pullRequest":             true,
	"plugin":                  true,
}

// InterpolateGenerator renders every string field and map key of gen with params: the
// interpolation of a matrix generator's second child with each parameter set of the first.
//
// Generator `values` maps are resolved with a forced missingkey=error (Go templates); a value that
// fails only because a key is missing is kept verbatim so it can be resolved later against the
// child's own parameters. With empty params gen is returned unchanged (a deep copy).
func InterpolateGenerator(gen *v1alpha1.ApplicationSetGenerator, params map[string]any, o Options) (*v1alpha1.ApplicationSetGenerator, error) {
	if gen == nil {
		return nil, fmt.Errorf("generator is empty")
	}
	if len(params) == 0 {
		return gen.DeepCopy(), nil
	}
	doc, err := toGeneric(gen)
	if err != nil {
		return nil, fmt.Errorf("failed to replace parameters in generator: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return gen.DeepCopy(), nil
	}
	gw := generatorWalker{walker: walker{params: params, opts: o, ev: newEvaluation()}, valueOpts: valuesOptions(o)}
	rendered, err := gw.entry(root)
	if err != nil {
		return nil, fmt.Errorf("failed to replace parameters in generator: %w", err)
	}
	out := &v1alpha1.ApplicationSetGenerator{}
	if err := fromGeneric(rendered, out); err != nil {
		return nil, fmt.Errorf("failed to replace parameters in generator: %w", err)
	}
	return out, nil
}

func valuesOptions(o Options) Options {
	if !o.GoTemplate {
		return o
	}
	opts := make([]string, 0, len(o.GoTemplateOptions)+1)
	for _, opt := range o.GoTemplateOptions {
		if !strings.HasPrefix(opt, "missingkey=") {
			opts = append(opts, opt)
		}
	}
	opts = append(opts, "missingkey=error")
	return Options{GoTemplate: true, GoTemplateOptions: opts}
}

type generatorWalker struct {
	walker
	valueOpts Options
}

// entry walks a generator entry (top-level or a matrix/merge child).
func (g generatorWalker) entry(m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		v := m[k]
		if v == nil {
			continue
		}
		body, isObject := v.(map[string]any)
		var (
			r   any
			err error
		)
		switch {
		case isObject && generatorsWithValues[k]:
			r, err = g.terminal(body)
		case isObject && (k == "matrix" || k == "merge"):
			r, err = g.combination(body)
		default:
			r, err = g.value(v)
		}
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

// terminal walks the body of a generator that has a values map.
func (g generatorWalker) terminal(m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		v := m[k]
		if v == nil {
			continue
		}
		if values, ok := v.(map[string]any); ok && k == "values" {
			resolved, err := g.values(values)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
			continue
		}
		r, err := g.value(v)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

// combination walks the body of a matrix or merge generator.
func (g generatorWalker) combination(m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		v := m[k]
		if v == nil {
			continue
		}
		if children, ok := v.([]any); ok && k == "generators" {
			rendered := make([]any, len(children))
			for i, c := range children {
				child, isObject := c.(map[string]any)
				if !isObject {
					r, err := g.value(c)
					if err != nil {
						return nil, err
					}
					rendered[i] = r
					continue
				}
				r, err := g.entry(child)
				if err != nil {
					return nil, err
				}
				rendered[i] = r
			}
			out[k] = rendered
			continue
		}
		r, err := g.value(v)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

// values pre-resolves a generator values map. Keys are not templated.
func (g generatorWalker) values(m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		s, ok := m[k].(string)
		if !ok {
			out[k] = m[k]
			continue
		}
		resolved, err := g.ev.replace(s, g.params, g.valueOpts)
		if err != nil {
			if IsMissingKeyError(err) {
				out[k] = s
				continue
			}
			return nil, fmt.Errorf("failed to pre-resolve Values key %q: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}
