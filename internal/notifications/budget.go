package notifications

import (
	"context"
	"fmt"
	"reflect"
	"time"
)

// TemplateLimits bounds the resources one render (every template and field rendered for
// one destination) may use. Templates are untrusted input: with self-service
// notifications they are written by application teams, and sprig functions such as
// repeat or until allocate their whole result before anything is written.
type TemplateLimits struct {
	// MaxBytes bounds the memory charged for values produced by template functions
	// (strings, lists, maps) and for rendered output, cumulatively per render. A single
	// value (including everything it references) may not exceed half of it.
	MaxBytes int64
	// MaxSteps bounds the number of function calls, range iterations and template
	// invocations per render.
	MaxSteps int64
	// Timeout bounds the wall-clock time of one render.
	Timeout time.Duration
}

// Template limit defaults.
const (
	DefaultTemplateMaxBytes int64 = 16 << 20
	DefaultTemplateMaxSteps int64 = 200_000
	DefaultTemplateTimeout        = 10 * time.Second
)

// Fixed sandbox bounds (not configurable: they protect the process, not the workload).
const (
	// maxValueDepth bounds the nesting depth of values produced or modified by template
	// functions. Deeply nested values make fmt, encoding/json and deep-copy recurse once
	// per level, and a cyclic value makes them recurse forever (an unrecoverable stack
	// overflow), so values that are too deep (or cyclic) are refused.
	maxValueDepth = 100
	// maxSharedMutations bounds set/merge calls that modify a map which may be referenced
	// from other values (application data or a nested map); each one re-validates every
	// value of the render.
	maxSharedMutations = 256
	// maxWalkNodes bounds the work spent measuring values per render.
	maxWalkNodes = 4 << 20
	// maxExpensiveCalls bounds key generation and password hashing calls per render.
	maxExpensiveCalls = 4
	// maxQuadraticOps bounds element comparisons of uniq/without.
	maxQuadraticOps = 1 << 24
	// maxRegexScanMatches bounds the matches inspected to size a regex replacement.
	maxRegexScanMatches = 1 << 16
)

func (l TemplateLimits) withDefaults() TemplateLimits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultTemplateMaxBytes
	}
	if l.MaxSteps <= 0 {
		l.MaxSteps = DefaultTemplateMaxSteps
	}
	if l.Timeout <= 0 {
		l.Timeout = DefaultTemplateTimeout
	}
	return l
}

func limitErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTemplateLimitExceeded, fmt.Sprintf(format, args...))
}

// renderBudget is the per-render sandbox state shared by every template function call.
// It is used by one goroutine (text/template executes synchronously).
type renderBudget struct {
	ctx      context.Context
	limits   TemplateLimits
	now      func() time.Time
	deadline time.Time

	bytes     int64
	steps     int64
	walked    int64
	expensive int
	mutations int

	// memo caches measurements of maps and slices by identity. Entries keep a reference
	// to the value so that its address cannot be reused while the render runs. The memo
	// is exact: it is updated or cleared whenever a template function modifies a map.
	memo map[containerKey]memoEntry
	// roots are maps created by a template function during this render and never stored
	// in another value: modifying them cannot change the size of any other value.
	roots map[containerKey]struct{}
	// tracked are the values that can reach every map or slice visible to the template:
	// the template data and every map or slice returned by a template function.
	tracked map[containerKey]reflect.Value
	data    reflect.Value

	funcs map[string]any
}

type containerKey struct {
	ptr   uintptr
	n     int
	isMap bool
}

// valueInfo measures a value: nesting depth (0 for scalars), logical size (an upper bound
// of its printed or JSON size before escaping, counting shared references every time they
// are reachable) and number of maps/slices.
type valueInfo struct {
	depth   int
	logical int64
	nodes   int64
}

type memoEntry struct {
	info valueInfo
	ref  any
}

func newRenderBudget(ctx context.Context, limits TemplateLimits, data map[string]any) *renderBudget {
	if ctx == nil {
		ctx = context.Background()
	}
	limits = limits.withDefaults()
	b := &renderBudget{
		ctx:     ctx,
		limits:  limits,
		now:     time.Now,
		memo:    map[containerKey]memoEntry{},
		roots:   map[containerKey]struct{}{},
		tracked: map[containerKey]reflect.Value{},
		data:    reflect.ValueOf(data),
	}
	b.deadline = b.now().Add(limits.Timeout)
	return b
}

// maxValueBytes is the logical size limit of a single value.
func (b *renderBudget) maxValueBytes() int64 { return b.limits.MaxBytes / 2 }

// step accounts one unit of template work and enforces the step, deadline and
// cancellation limits.
func (b *renderBudget) step() error {
	b.steps++
	if b.steps > b.limits.MaxSteps {
		return limitErr("template execution exceeded %d steps", b.limits.MaxSteps)
	}
	if err := b.ctx.Err(); err != nil {
		return fmt.Errorf("template rendering: %w", err)
	}
	if b.now().After(b.deadline) {
		return limitErr("template rendering exceeded %s", b.limits.Timeout)
	}
	return nil
}

// tick is called at the start of every range iteration and template invocation.
func (b *renderBudget) tick() (string, error) { return "", b.step() }

// reserve fails when n more bytes would exceed the memory budget.
func (b *renderBudget) reserve(n int64) error {
	if n < 0 || n > b.limits.MaxBytes-b.bytes {
		return limitErr("template values exceed %d bytes", b.limits.MaxBytes)
	}
	return nil
}

// charge accounts n allocated bytes.
func (b *renderBudget) charge(n int64) error {
	if err := b.reserve(n); err != nil {
		return err
	}
	b.bytes += n
	return nil
}

// ---------------------------------------------------------------------------------------
// Value measurement

// container returns the map or slice (not []byte) held by v, unwrapping interfaces.
func container(v reflect.Value) (reflect.Value, bool) {
	for v.IsValid() && v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if !v.IsValid() {
		return v, false
	}
	switch v.Kind() {
	case reflect.Map:
		return v, true
	case reflect.Slice, reflect.Array:
		return v, v.Type().Elem().Kind() != reflect.Uint8
	}
	return v, false
}

func keyOf(v reflect.Value) (containerKey, bool) {
	switch v.Kind() {
	case reflect.Map:
		if v.IsNil() {
			return containerKey{}, false
		}
		return containerKey{ptr: v.Pointer(), isMap: true}, true
	case reflect.Slice:
		if v.IsNil() {
			return containerKey{}, false
		}
		return containerKey{ptr: v.Pointer(), n: v.Len()}, true
	}
	return containerKey{}, false
}

func satAdd(a, b int64) int64 {
	if a > 0 && b > 0 && a > (1<<62)-b {
		return 1 << 62
	}
	return a + b
}

func satMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > (1<<62)/b {
		return 1 << 62
	}
	return a * b
}

// scalarLogical is the logical size of a non-container value.
func scalarLogical(v reflect.Value) int64 {
	for v.IsValid() && v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if !v.IsValid() {
		return 5
	}
	switch v.Kind() {
	case reflect.String:
		return int64(v.Len()) + 2
	case reflect.Slice, reflect.Array:
		// []byte prints as a list of numbers.
		return satMul(int64(v.Len()), 4) + 2
	}
	return 24
}

// info measures v, using and filling the memo.
func (b *renderBudget) info(v reflect.Value) (valueInfo, error) { return b.measure(v, 0) }

func (b *renderBudget) measure(v reflect.Value, level int) (valueInfo, error) {
	c, ok := container(v)
	if !ok {
		return valueInfo{logical: scalarLogical(v)}, nil
	}
	if level >= maxValueDepth {
		return valueInfo{}, limitErr("value nesting is deeper than %d levels (or cyclic)", maxValueDepth)
	}
	key, keyed := keyOf(c)
	if keyed {
		if e, ok := b.memo[key]; ok {
			return e.info, nil
		}
	}
	out := valueInfo{logical: 16}
	add := func(ci valueInfo) error {
		if err := b.walk(); err != nil {
			return err
		}
		out.logical = satAdd(out.logical, ci.logical+2)
		out.nodes = satAdd(out.nodes, ci.nodes)
		if ci.depth > out.depth {
			out.depth = ci.depth
		}
		if out.logical > b.maxValueBytes() {
			return limitErr("a template value exceeds %d bytes", b.maxValueBytes())
		}
		return nil
	}
	switch c.Kind() {
	case reflect.Map:
		iter := c.MapRange()
		for iter.Next() {
			ci, err := b.measure(iter.Value(), level+1)
			if err != nil {
				return valueInfo{}, err
			}
			ci.logical = satAdd(ci.logical, scalarLogical(iter.Key()))
			if err := add(ci); err != nil {
				return valueInfo{}, err
			}
		}
	default:
		for i := 0; i < c.Len(); i++ {
			ci, err := b.measure(c.Index(i), level+1)
			if err != nil {
				return valueInfo{}, err
			}
			if err := add(ci); err != nil {
				return valueInfo{}, err
			}
		}
	}
	out.depth++
	out.nodes++
	if out.depth > maxValueDepth {
		return valueInfo{}, limitErr("value nesting is deeper than %d levels (or cyclic)", maxValueDepth)
	}
	if keyed {
		b.memo[key] = memoEntry{info: out, ref: c.Interface()}
	}
	return out, nil
}

// reachable calls visit for every map and slice reachable from v (v included), without
// using the memo. Shared values are visited once.
func (b *renderBudget) reachable(v reflect.Value, seen map[containerKey]struct{}, level int, visit func(containerKey) bool) error {
	c, ok := container(v)
	if !ok {
		return nil
	}
	if level >= maxValueDepth {
		return limitErr("value nesting is deeper than %d levels (or cyclic)", maxValueDepth)
	}
	if key, keyed := keyOf(c); keyed {
		if _, dup := seen[key]; dup {
			return nil
		}
		seen[key] = struct{}{}
		if !visit(key) {
			return nil
		}
	}
	if err := b.walk(); err != nil {
		return err
	}
	switch c.Kind() {
	case reflect.Map:
		iter := c.MapRange()
		for iter.Next() {
			if err := b.reachable(iter.Value(), seen, level+1, visit); err != nil {
				return err
			}
		}
	default:
		for i := 0; i < c.Len(); i++ {
			if err := b.reachable(c.Index(i), seen, level+1, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// walk accounts one visited map entry or list element of a value measurement.
func (b *renderBudget) walk() error {
	b.walked++
	if b.walked > maxWalkNodes {
		return limitErr("template values are too complex to validate")
	}
	if b.walked%4096 == 0 {
		if err := b.ctx.Err(); err != nil {
			return fmt.Errorf("template rendering: %w", err)
		}
		if b.now().After(b.deadline) {
			return limitErr("template rendering exceeded %s", b.limits.Timeout)
		}
	}
	return nil
}

// recheck re-measures every tracked value from scratch after a map that may be shared was
// modified.
func (b *renderBudget) recheck() error {
	clear(b.memo)
	if b.data.IsValid() {
		if _, err := b.info(b.data); err != nil {
			return err
		}
	}
	for _, v := range b.tracked {
		if _, err := b.info(v); err != nil {
			return err
		}
	}
	return nil
}

func (b *renderBudget) dropRoot(v reflect.Value) {
	if c, ok := container(v); ok {
		if key, keyed := keyOf(c); keyed {
			delete(b.roots, key)
		}
	}
}

// fmtEstimate bounds the formatted size of v (fmt %v or JSON, before escaping).
func (b *renderBudget) fmtEstimate(v reflect.Value) (int64, error) {
	if _, ok := container(v); !ok {
		return scalarLogical(v), nil
	}
	in, err := b.info(v)
	if err != nil {
		return 0, err
	}
	return satAdd(in.logical, satMul(in.nodes, 8)), nil
}
