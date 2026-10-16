package diff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gobwas/glob"
	"github.com/itchyny/gojq"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DefaultJQTimeout bounds the evaluation of one jq path expression against one object
// (ignore.normalizer.jq.timeout).
const DefaultJQTimeout = time.Second

// IgnoreRule selects resources and the parts of them to ignore when diffing. It is the
// neutral form of both Application spec.ignoreDifferences entries and system-level
// resource.customizations.ignoreDifferences overrides (for an override keyed "group/kind",
// set Group and Kind; for "all" or "*/*" use "*" for both).
type IgnoreRule struct {
	// Group and Kind are glob patterns (github.com/gobwas/glob syntax, "*" matches anything).
	// The core API group is the empty string.
	Group string
	Kind  string
	// Name and Namespace restrict the rule to one object when non-empty (exact match).
	Name      string
	Namespace string
	// JSONPointers are RFC 6901 pointers removed from both sides (e.g. /spec/replicas).
	JSONPointers []string
	// JQPathExpressions are jq path expressions; every path they produce is deleted
	// (evaluated as del(<expression>)).
	JQPathExpressions []string
	// ManagedFieldsManagers lists trusted managers whose owned fields are not drift.
	ManagedFieldsManagers []string
}

// IgnoreNormalizerOptions configures NewIgnoreNormalizer.
type IgnoreNormalizerOptions struct {
	// JQTimeout bounds each jq expression evaluation. Zero means DefaultJQTimeout.
	JQTimeout time.Duration
	// Log receives diagnostics for patches that fail to apply. Nil discards them.
	Log *slog.Logger
}

// IgnoreNormalizer removes ignored fields from objects. It implements Normalizer and
// TrustedManagersProvider and is safe for concurrent use.
type IgnoreNormalizer struct {
	rules []compiledRule
	log   *slog.Logger
}

type compiledRule struct {
	group, kind     matcher
	name, namespace string
	patches         []ignorePatch
	managers        []string
}

type ignorePatch interface {
	apply(ctx context.Context, obj map[string]any) (map[string]any, error)
	// expected reports whether err is an expected outcome (e.g. the path does not exist) that
	// should not be logged.
	expected(err error) bool
}

// matcher is a compiled glob; an invalid pattern never matches (baseline behavior).
type matcher struct {
	g *glob.Pattern
}

func compileMatcher(pattern string) (matcher, error) {
	g, err := glob.Compile(pattern)
	if err != nil {
		return matcher{}, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	return matcher{g: g}, nil
}

func (m matcher) match(s string) bool {
	return m.g != nil && m.g.Match(s)
}

// NewIgnoreNormalizer compiles rules. An invalid jq expression is an error wrapping
// ErrInvalidIgnoreRule. Invalid group/kind glob patterns are logged and never match, and JSON
// pointers are validated when applied, both as in the baseline.
func NewIgnoreNormalizer(rules []IgnoreRule, opts IgnoreNormalizerOptions) (*IgnoreNormalizer, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	timeout := opts.JQTimeout
	if timeout <= 0 {
		timeout = DefaultJQTimeout
	}
	n := &IgnoreNormalizer{log: log, rules: make([]compiledRule, 0, len(rules))}
	for i, r := range rules {
		cr := compiledRule{name: r.Name, namespace: r.Namespace, managers: slices.Clone(r.ManagedFieldsManagers)}
		var err error
		if cr.group, err = compileMatcher(r.Group); err != nil {
			log.Warn("ignore rule group pattern does not compile; rule never matches", "rule", i, "error", err)
		}
		if cr.kind, err = compileMatcher(r.Kind); err != nil {
			log.Warn("ignore rule kind pattern does not compile; rule never matches", "rule", i, "error", err)
		}
		for _, p := range r.JSONPointers {
			cr.patches = append(cr.patches, pointerPatch{pointer: p})
		}
		for _, expr := range r.JQPathExpressions {
			jp, err := compileJQPatch(expr, timeout)
			if err != nil {
				return nil, fmt.Errorf("%w: rule %d: jq path expression %q: %w", ErrInvalidIgnoreRule, i, expr, err)
			}
			cr.patches = append(cr.patches, jp)
		}
		n.rules = append(n.rules, cr)
	}
	return n, nil
}

func (r *compiledRule) matches(gk schema.GroupKind, name, namespace string) bool {
	return r.group.match(gk.Group) && r.kind.match(gk.Kind) &&
		(r.name == "" || r.name == name) &&
		(r.namespace == "" || r.namespace == namespace)
}

// Normalize removes the fields selected by every matching rule. Patches that fail to apply
// are skipped (and logged unless the selected path simply does not exist), as in the
// baseline.
//
// Unlike a patch failure, a done ctx is an error: when ctx is done on entry, or a patch
// fails while ctx is done (a jq expression interrupted by cancellation or by the caller's
// deadline), Normalize returns an error wrapping ctx.Err() and un may be partially
// normalized. Skipping the rule instead would report ignored fields as drift. The
// per-expression jq timeout (IgnoreNormalizerOptions.JQTimeout) is not a context error: that
// patch is skipped.
func (n *IgnoreNormalizer) Normalize(ctx context.Context, un *unstructured.Unstructured) error {
	if un == nil || un.Object == nil {
		return errors.New("diff: cannot normalize a nil object")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("diff: ignore rules for %s not applied: %w", describe(un), err)
	}
	gvk := un.GroupVersionKind()
	name, namespace := un.GetName(), un.GetNamespace()
	obj := un.Object
	for i := range n.rules {
		r := &n.rules[i]
		if !r.matches(gvk.GroupKind(), name, namespace) {
			continue
		}
		for _, p := range r.patches {
			patched, err := p.apply(ctx, obj)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					un.Object = obj
					return fmt.Errorf("diff: ignore rules for %s interrupted: %w", describe(un), ctxErr)
				}
				if !p.expected(err) {
					n.log.Debug("failed to apply normalization patch",
						"group", gvk.Group, "kind", gvk.Kind, "namespace", namespace, "name", name,
						"error", sanitizeError(un, err))
				}
				continue
			}
			obj = patched
		}
	}
	un.Object = obj
	return nil
}

// TrustedManagers returns the managedFieldsManagers of all rules matching the object.
func (n *IgnoreNormalizer) TrustedManagers(gk schema.GroupKind, name, namespace string) []string {
	var out []string
	for i := range n.rules {
		r := &n.rules[i]
		if len(r.managers) == 0 || !r.matches(gk, name, namespace) {
			continue
		}
		for _, m := range r.managers {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// errPointerMissing marks a JSON pointer that does not resolve; it is expected and silent.
var errPointerMissing = errors.New("path does not exist")

// pointerPatch removes the value at an RFC 6901 JSON pointer, with the semantics of an RFC
// 6902 "remove" operation (negative array indices count from the end, as in the baseline).
type pointerPatch struct{ pointer string }

func (p pointerPatch) expected(err error) bool { return errors.Is(err, errPointerMissing) }

func (p pointerPatch) apply(_ context.Context, obj map[string]any) (map[string]any, error) {
	// Like the JSON patch implementation used by the baseline (evanphx/json-patch v4 API),
	// the segment before the first "/" is ignored and a pointer without "/" addresses nothing.
	tokens := strings.Split(p.pointer, "/")
	if len(tokens) < 2 {
		return nil, fmt.Errorf("remove %q: %w", p.pointer, errPointerMissing)
	}
	tokens = tokens[1:]
	for i, t := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	var parent any = obj
	for _, t := range tokens[:len(tokens)-1] {
		child, err := pointerChild(parent, t)
		if err != nil {
			return nil, fmt.Errorf("remove %q: %w", p.pointer, err)
		}
		parent = child
	}
	last := tokens[len(tokens)-1]
	switch c := parent.(type) {
	case map[string]any:
		if _, ok := c[last]; !ok {
			return nil, fmt.Errorf("remove %q: %w", p.pointer, errPointerMissing)
		}
		delete(c, last)
	case []any:
		idx, err := arrayIndex(last, len(c))
		if err != nil {
			return nil, fmt.Errorf("remove %q: %w", p.pointer, err)
		}
		removed := make([]any, 0, len(c)-1)
		removed = append(removed, c[:idx]...)
		removed = append(removed, c[idx+1:]...)
		if err := replaceChild(obj, tokens[:len(tokens)-1], removed); err != nil {
			return nil, fmt.Errorf("remove %q: %w", p.pointer, err)
		}
	default:
		return nil, fmt.Errorf("remove %q: %w", p.pointer, errPointerMissing)
	}
	return obj, nil
}

func pointerChild(parent any, token string) (any, error) {
	switch c := parent.(type) {
	case map[string]any:
		v, ok := c[token]
		if !ok {
			return nil, errPointerMissing
		}
		return v, nil
	case []any:
		idx, err := arrayIndex(token, len(c))
		if err != nil {
			return nil, errPointerMissing
		}
		return c[idx], nil
	default:
		return nil, errPointerMissing
	}
}

// arrayIndex parses an array index token. Negative indices count from the end.
func arrayIndex(token string, length int) (int, error) {
	idx, err := strconv.Atoi(token)
	if err != nil {
		return 0, fmt.Errorf("invalid array index %q", token)
	}
	if idx < 0 {
		idx += length
	}
	if idx < 0 || idx >= length {
		return 0, fmt.Errorf("array index %q out of bounds", token)
	}
	return idx, nil
}

// replaceChild sets the value addressed by tokens (relative to root) to v.
func replaceChild(root map[string]any, tokens []string, v []any) error {
	if len(tokens) == 0 {
		return errPointerMissing // the root is always an object
	}
	parent, err := walk(root, tokens[:len(tokens)-1])
	if err != nil {
		return err
	}
	last := tokens[len(tokens)-1]
	switch c := parent.(type) {
	case map[string]any:
		c[last] = v
	case []any:
		idx, err := arrayIndex(last, len(c))
		if err != nil {
			return err
		}
		c[idx] = v
	default:
		return errPointerMissing
	}
	return nil
}

func walk(root map[string]any, tokens []string) (any, error) {
	var cur any = root
	for _, t := range tokens {
		next, err := pointerChild(cur, t)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// jqPatch deletes every path produced by a jq path expression.
type jqPatch struct {
	code    *gojq.Code
	timeout time.Duration
}

func compileJQPatch(expr string, timeout time.Duration) (jqPatch, error) {
	// The expression is embedded verbatim, exactly like the baseline, so that expressions
	// written for it behave the same.
	q, err := gojq.Parse("del(" + expr + ")")
	if err != nil {
		return jqPatch{}, fmt.Errorf("parse: %w", err)
	}
	// No environment loader: $ENV/env are empty, so expressions cannot read the
	// controller's environment.
	code, err := gojq.Compile(q, gojq.WithEnvironLoader(func() []string { return nil }))
	if err != nil {
		return jqPatch{}, fmt.Errorf("compile: %w", err)
	}
	return jqPatch{code: code, timeout: timeout}, nil
}

func (jqPatch) expected(error) bool { return false }

// apply evaluates the expression against obj. Every failure that happens while the parent ctx
// is done is reported as an error wrapping the parent's ctx.Err() (never as a jq failure or as
// the per-expression timeout), so callers can tell an interrupted evaluation from a patch that
// does not apply. The per-expression timeout error does not wrap context.DeadlineExceeded.
func (p jqPatch) apply(ctx context.Context, obj map[string]any) (_ map[string]any, err error) {
	defer func() {
		// Expressions are user input evaluated by a third-party interpreter: contain any panic.
		if r := recover(); r != nil {
			err = fmt.Errorf("jq patch evaluation failed: %v", r)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = fmt.Errorf("jq patch interrupted: %w", ctxErr)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err // rewritten by the deferred check
	}
	runCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	iter := p.code.RunWithContext(runCtx, toJQValue(obj))
	first, ok := iter.Next()
	if !ok {
		return nil, errors.New("jq patch did not return any data")
	}
	if err, isErr := first.(error); isErr {
		if errors.Is(err, context.DeadlineExceeded) {
			// Only reached when the parent is not done (see the deferred check): this is the
			// per-expression timeout. Deliberately not wrapped.
			return nil, fmt.Errorf("jq patch execution timed out (%s)", p.timeout)
		}
		return nil, fmt.Errorf("jq patch returned error: %w", err)
	}
	if _, more := iter.Next(); more {
		return nil, errors.New("jq patch returned multiple objects")
	}
	m, isMap := fromJQValue(first).(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("jq patch returned %T instead of an object", first)
	}
	return m, nil
}
