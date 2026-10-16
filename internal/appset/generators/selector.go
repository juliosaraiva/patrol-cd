package generators

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Selector matches flattened parameter sets.
type Selector interface {
	Matches(labels.Labels) bool
}

// LabelSelectorAsSelector converts a LabelSelector into a parameter selector. Unlike
// metav1.LabelSelectorAsSelector, values are not validated as label values, so URLs and other
// parameter values can be matched. Keys must still be qualified names. A nil selector matches
// nothing; an empty one matches everything.
func LabelSelectorAsSelector(ps *metav1.LabelSelector) (Selector, error) {
	if ps == nil {
		return nothingSelector{}, nil
	}
	if len(ps.MatchLabels)+len(ps.MatchExpressions) == 0 {
		return requirements(nil), nil
	}
	reqs := make(requirements, 0, len(ps.MatchLabels)+len(ps.MatchExpressions))
	for k, v := range ps.MatchLabels {
		r, err := newRequirement(k, selection.Equals, []string{v})
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, r)
	}
	for _, expr := range ps.MatchExpressions {
		var op selection.Operator
		switch expr.Operator {
		case metav1.LabelSelectorOpIn:
			op = selection.In
		case metav1.LabelSelectorOpNotIn:
			op = selection.NotIn
		case metav1.LabelSelectorOpExists:
			op = selection.Exists
		case metav1.LabelSelectorOpDoesNotExist:
			op = selection.DoesNotExist
		default:
			return nil, fmt.Errorf("%q is not a valid pod selector operator", expr.Operator)
		}
		r, err := newRequirement(expr.Key, op, slices.Clone(expr.Values))
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, r)
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].key < reqs[j].key })
	return reqs, nil
}

type nothingSelector struct{}

func (nothingSelector) Matches(labels.Labels) bool { return false }

type requirement struct {
	key    string
	op     selection.Operator
	values []string
}

type requirements []requirement

func (rs requirements) Matches(ls labels.Labels) bool {
	for _, r := range rs {
		if !r.matches(ls) {
			return false
		}
	}
	return true
}

func newRequirement(key string, op selection.Operator, vals []string) (requirement, error) {
	var problems []string
	if errs := validation.IsQualifiedName(key); len(errs) != 0 {
		problems = append(problems, fmt.Sprintf("key: Invalid value: %q: %s", key, strings.Join(errs, "; ")))
	}
	switch op {
	case selection.In, selection.NotIn:
		if len(vals) == 0 {
			problems = append(problems, "values: Invalid value: for 'in', 'notin' operators, values set can't be empty")
		}
	case selection.Equals:
		if len(vals) != 1 {
			problems = append(problems, "values: Invalid value: exact-match compatibility requires one single value")
		}
	case selection.Exists, selection.DoesNotExist:
		if len(vals) != 0 {
			problems = append(problems, "values: Invalid value: values set must be empty for exists and does not exist")
		}
	}
	if len(problems) > 0 {
		return requirement{}, fmt.Errorf("%s", strings.Join(problems, ", "))
	}
	return requirement{key: key, op: op, values: vals}, nil
}

func (r requirement) matches(ls labels.Labels) bool {
	switch r.op {
	case selection.In, selection.Equals:
		return ls.Has(r.key) && slices.Contains(r.values, ls.Get(r.key))
	case selection.NotIn:
		return !ls.Has(r.key) || !slices.Contains(r.values, ls.Get(r.key))
	case selection.Exists:
		return ls.Has(r.key)
	case selection.DoesNotExist:
		return !ls.Has(r.key)
	default:
		return false
	}
}
