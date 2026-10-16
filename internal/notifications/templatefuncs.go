package notifications

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	texttemplate "text/template"
	"text/template/parse"
	"unicode"
	"unicode/utf8"

	"github.com/Masterminds/sprig/v3"
)

// templateFuncs is sprig's text function map without functions that read the controller
// environment (env, expandenv) or perform network lookups (getHostByName), and with
// until/untilStep/seq replaced by versions that cannot loop forever on integer overflow.
// At render time every function is wrapped by the render budget (see renderBudget.wrap).
var templateFuncs = func() texttemplate.FuncMap {
	f := sprig.TxtFuncMap()
	delete(f, "env")
	delete(f, "expandenv")
	delete(f, "getHostByName")
	f["until"] = until
	f["untilStep"] = untilStep
	f["seq"] = seq
	return f
}()

// builtinOverrides are text/template builtins whose output can be much larger than their
// input; they are replaced by budgeted equivalents (same semantics).
var builtinOverrides = map[string]any{
	"print":    fmt.Sprint,
	"printf":   fmt.Sprintf,
	"println":  fmt.Sprintln,
	"html":     texttemplate.HTMLEscaper,
	"js":       texttemplate.JSEscaper,
	"urlquery": texttemplate.URLQueryEscaper,
}

// sandboxedFuncs are the functions instrumented at render time.
var sandboxedFuncs = func() map[string]reflect.Value {
	out := make(map[string]reflect.Value, len(templateFuncs)+len(builtinOverrides))
	for k, v := range templateFuncs {
		out[k] = reflect.ValueOf(v)
	}
	for k, v := range builtinOverrides {
		out[k] = reflect.ValueOf(v)
	}
	return out
}()

// TemplateFuncNames returns the names of the functions available to templates (besides the
// text/template builtins).
func TemplateFuncNames() []string {
	out := make([]string, 0, len(templateFuncs))
	for k := range templateFuncs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mutatingFuncs modify maps in place. Renders of templates using them get a private copy
// of the application data, so that one render cannot affect another.
var mutatingFuncs = map[string]bool{
	"set": true, "unset": true, "merge": true, "mergeOverwrite": true, "mustMerge": true, "mustMergeOverwrite": true,
}

// ---------------------------------------------------------------------------------------
// Compilation: parse trees are instrumented so that every range iteration and template
// invocation calls the budget's tick function.

const tickFunc = "_patrolTemplateTick"

var tickNode = func() parse.Node {
	t := texttemplate.Must(texttemplate.New("limits").Funcs(texttemplate.FuncMap{tickFunc: func() string { return "" }}).Parse("{{" + tickFunc + "}}"))
	return t.Root.Nodes[0]
}()

// compiledText is one template string ready for budgeted execution.
type compiledText struct {
	tmpl    *texttemplate.Template
	funcs   []string
	mutates bool
}

func compileText(name, src string) (*compiledText, error) {
	full, err := texttemplate.New(name).Funcs(templateFuncs).Parse(src)
	if err != nil {
		return nil, err
	}
	used := map[string]struct{}{}
	forEachTree(full, func(root *parse.ListNode) { collectIdentifiers(root, used) })
	// Re-parse with only the functions in use so that per-render clones stay cheap.
	minimal := texttemplate.FuncMap{}
	ct := &compiledText{}
	for fn := range used {
		if f, ok := templateFuncs[fn]; ok {
			minimal[fn] = f
		}
		if _, ok := sandboxedFuncs[fn]; ok {
			ct.funcs = append(ct.funcs, fn)
		}
		if mutatingFuncs[fn] {
			ct.mutates = true
		}
	}
	sort.Strings(ct.funcs)
	t, err := texttemplate.New(name).Funcs(minimal).Parse(src)
	if err != nil {
		return nil, err
	}
	forEachTree(t, func(root *parse.ListNode) {
		insertTicks(root)
		root.Nodes = append([]parse.Node{tickNode}, root.Nodes...)
	})
	ct.tmpl = t
	return ct, nil
}

func forEachTree(t *texttemplate.Template, fn func(root *parse.ListNode)) {
	seen := map[*parse.Tree]bool{}
	for _, tt := range t.Templates() {
		if tt.Tree == nil || tt.Root == nil || seen[tt.Tree] {
			continue
		}
		seen[tt.Tree] = true
		fn(tt.Root)
	}
}

func collectIdentifiers(n parse.Node, used map[string]struct{}) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			collectIdentifiers(c, used)
		}
	case *parse.ActionNode:
		collectIdentifiers(n.Pipe, used)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			collectIdentifiers(c, used)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			collectIdentifiers(a, used)
		}
	case *parse.ChainNode:
		collectIdentifiers(n.Node, used)
	case *parse.IdentifierNode:
		used[n.Ident] = struct{}{}
	case *parse.IfNode:
		collectBranch(&n.BranchNode, used)
	case *parse.WithNode:
		collectBranch(&n.BranchNode, used)
	case *parse.RangeNode:
		collectBranch(&n.BranchNode, used)
	case *parse.TemplateNode:
		collectIdentifiers(n.Pipe, used)
	}
}

func collectBranch(b *parse.BranchNode, used map[string]struct{}) {
	collectIdentifiers(b.Pipe, used)
	collectIdentifiers(b.List, used)
	collectIdentifiers(b.ElseList, used)
}

// insertTicks prepends the tick action to the body of every range.
func insertTicks(n parse.Node) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			insertTicks(c)
		}
	case *parse.IfNode:
		insertTicks(n.List)
		insertTicks(n.ElseList)
	case *parse.WithNode:
		insertTicks(n.List)
		insertTicks(n.ElseList)
	case *parse.RangeNode:
		insertTicks(n.List)
		insertTicks(n.ElseList)
		if n.List != nil {
			n.List.Nodes = append([]parse.Node{tickNode}, n.List.Nodes...)
		}
	}
}

// execute renders ct with the budget's data.
func (b *renderBudget) execute(ct *compiledText) (string, error) {
	t, err := ct.tmpl.Clone()
	if err != nil {
		return "", err
	}
	t.Funcs(b.funcMap(ct.funcs))
	var buf limitedBuffer
	if err := t.Execute(&buf, b.data.Interface()); err != nil {
		return "", err
	}
	if err := b.charge(int64(buf.Len())); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// funcMap returns the instrumented functions for names (built once per render).
func (b *renderBudget) funcMap(names []string) texttemplate.FuncMap {
	if b.funcs == nil {
		b.funcs = map[string]any{tickFunc: b.tick}
	}
	for _, name := range names {
		if _, ok := b.funcs[name]; ok {
			continue
		}
		if fn, ok := sandboxedFuncs[name]; ok {
			b.funcs[name] = b.wrap(name, fn)
		}
	}
	return b.funcs
}

// ---------------------------------------------------------------------------------------
// Function instrumentation

var errorType = reflect.TypeFor[error]()

// funcSpec describes how a function is budgeted besides the common accounting (one step
// per call, charging the size of the result, validating maps and slices it returns).
type funcSpec struct {
	// pre validates the arguments before the call; the returned function (optional) runs
	// after a successful call.
	pre func(b *renderBudget, args []reflect.Value) (func() error, error)
	// expensive marks key generation and password hashing.
	expensive bool
	// stores marks functions whose result holds their arguments (so argument maps are no
	// longer roots).
	stores bool
	// freshMap marks functions returning a newly allocated map.
	freshMap bool
	// child marks functions returning one of their arguments or a part of one (a map
	// entry, a list element, a sub-list): nothing new is allocated or made reachable.
	child bool
}

// wrap returns fn instrumented with the budget. The wrapper always has an error result, so
// a limit violation is reported as a template execution error.
func (b *renderBudget) wrap(name string, fn reflect.Value) any {
	ft := fn.Type()
	in := make([]reflect.Type, ft.NumIn())
	for i := range in {
		in[i] = ft.In(i)
	}
	res := ft.Out(0)
	wt := reflect.FuncOf(in, []reflect.Type{res, errorType}, ft.IsVariadic())
	spec := funcSpecs[name]
	return reflect.MakeFunc(wt, func(args []reflect.Value) []reflect.Value {
		out, err := b.call(spec, fn, args)
		if err != nil {
			return []reflect.Value{reflect.Zero(res), reflect.ValueOf(&err).Elem()}
		}
		return []reflect.Value{out, reflect.Zero(errorType)}
	}).Interface()
}

func (b *renderBudget) call(spec funcSpec, fn reflect.Value, args []reflect.Value) (reflect.Value, error) {
	if err := b.step(); err != nil {
		return reflect.Value{}, err
	}
	if spec.expensive {
		b.expensive++
		if b.expensive > maxExpensiveCalls {
			return reflect.Value{}, limitErr("more than %d key generation or password hashing calls", maxExpensiveCalls)
		}
	}
	var after func() error
	if spec.pre != nil {
		var err error
		if after, err = spec.pre(b, args); err != nil {
			return reflect.Value{}, err
		}
	}
	var out []reflect.Value
	if fn.Type().IsVariadic() {
		out = fn.CallSlice(args)
	} else {
		out = fn.Call(args)
	}
	if len(out) == 2 && !out[1].IsNil() {
		return reflect.Value{}, out[1].Interface().(error)
	}
	if after != nil {
		if err := after(); err != nil {
			return reflect.Value{}, err
		}
	}
	if err := b.account(spec, fn.Type().IsVariadic(), args, out[0]); err != nil {
		return reflect.Value{}, err
	}
	return out[0], nil
}

// flatArgs returns the arguments with the variadic slice expanded.
func flatArgs(variadic bool, args []reflect.Value) []reflect.Value {
	if !variadic || len(args) == 0 {
		return args
	}
	last := args[len(args)-1]
	out := append([]reflect.Value(nil), args[:len(args)-1]...)
	for i := 0; i < last.Len(); i++ {
		out = append(out, last.Index(i))
	}
	return out
}

// expandVariadic expands a trailing []interface{} argument (the variadic parameter of the
// formatting functions; their other parameters are strings or interface{}).
func expandVariadic(args []reflect.Value) []reflect.Value {
	if len(args) == 0 {
		return args
	}
	last := args[len(args)-1]
	if last.Kind() != reflect.Slice || last.Type().Elem().Kind() != reflect.Interface {
		return args
	}
	return flatArgs(true, args)
}

// containerOverhead is charged for every map or slice a function returns, covering the
// sandbox's own bookkeeping for it.
const containerOverhead = 256

// account charges the result and validates maps and slices returned by a function.
func (b *renderBudget) account(spec funcSpec, variadic bool, args []reflect.Value, res reflect.Value) error {
	if spec.child {
		return nil
	}
	c, ok := container(res)
	if !ok {
		for res.IsValid() && res.Kind() == reflect.Interface {
			res = res.Elem()
		}
		if res.IsValid() && (res.Kind() == reflect.String || res.Kind() == reflect.Slice) {
			return b.charge(int64(res.Len()))
		}
		return nil
	}
	key, keyed := keyOf(c)
	flat := flatArgs(variadic, args)
	if keyed {
		for _, a := range flat {
			if ac, ok := container(a); ok {
				if ak, ok := keyOf(ac); ok && ak == key {
					return nil // returns one of its arguments: nothing new
				}
			}
		}
	}
	size := int64(c.Len())
	if c.Kind() == reflect.Map {
		size = satMul(size, 48)
	} else {
		size = satMul(size, int64(c.Type().Elem().Size()))
	}
	if err := b.charge(satAdd(size, containerOverhead)); err != nil {
		return err
	}
	if spec.stores {
		for _, a := range flat {
			b.dropRoot(a)
		}
	}
	if _, err := b.info(c); err != nil {
		return err
	}
	if keyed {
		b.tracked[key] = c
		if spec.freshMap && c.Kind() == reflect.Map {
			b.roots[key] = struct{}{}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------
// Per-function rules

var funcSpecs = func() map[string]funcSpec {
	m := map[string]funcSpec{
		"repeat":  {pre: preRepeat},
		"indent":  {pre: preIndent(0)},
		"nindent": {pre: preIndent(1)},
		"replace": {pre: preReplace},
		"wrap": {pre: func(b *renderBudget, a []reflect.Value) (func() error, error) {
			s := int64(a[1].Len())
			return nil, b.reserve(satAdd(satMul(s, 2), 1))
		}},
		"wrapWith": {pre: func(b *renderBudget, a []reflect.Value) (func() error, error) {
			l := max(a[0].Int(), 1)
			s := int64(a[2].Len())
			return nil, b.reserve(satAdd(s, satMul(s/l+1, int64(a[1].Len()))))
		}},
		"randBytes": {pre: func(b *renderBudget, a []reflect.Value) (func() error, error) {
			return nil, b.reserve(satMul(max(a[0].Int(), 0), 3))
		}},
		"splitList": {pre: preSplit(0, 16)},
		"split":     {pre: preSplit(0, 64)},
		"splitn":    {pre: preSplitN},
		"until": {pre: func(b *renderBudget, a []reflect.Value) (func() error, error) {
			n := a[0].Int()
			step := int64(1)
			if n < 0 {
				step = -1
			}
			return nil, reserveSteps(b, 0, n, step, 8)
		}},
		"untilStep": {pre: func(b *renderBudget, a []reflect.Value) (func() error, error) {
			return nil, reserveSteps(b, a[0].Int(), a[1].Int(), a[2].Int(), 8)
		}},
		"seq":      {pre: preSeq},
		"printf":   {pre: prePrintf},
		"set":      {pre: preSet},
		"uniq":     {pre: preQuadratic},
		"mustUniq": {pre: preQuadratic},
		"without":  {pre: preWithout},
		"fromJson": {pre: preFromJSON, freshMap: true},
		"deepCopy": {pre: preDeepCopy, freshMap: true},
		"dict":     {stores: true, freshMap: true},
		"pick":     {freshMap: true},
		"omit":     {freshMap: true},
		"urlParse": {freshMap: true},
	}
	m["mustWithout"] = m["without"]
	m["mustFromJson"] = m["fromJson"]
	m["mustDeepCopy"] = m["deepCopy"]
	for _, name := range []string{"randAlphaNum", "randAlpha", "randAscii", "randNumeric"} {
		m[name] = funcSpec{pre: func(b *renderBudget, a []reflect.Value) (func() error, error) {
			return nil, b.reserve(max(a[0].Int(), 0))
		}}
	}
	for _, name := range []string{"regexReplaceAll", "mustRegexReplaceAll"} {
		m[name] = funcSpec{pre: preRegexReplace(false)}
	}
	for _, name := range []string{"regexReplaceAllLiteral", "mustRegexReplaceAllLiteral"} {
		m[name] = funcSpec{pre: preRegexReplace(true)}
	}
	for _, name := range []string{"regexFindAll", "mustRegexFindAll", "regexSplit", "mustRegexSplit"} {
		m[name] = funcSpec{pre: func(b *renderBudget, a []reflect.Value) (func() error, error) {
			n := int64(a[1].Len()) + 1
			if lim := a[2].Int(); lim >= 0 && lim < n {
				n = lim
			}
			return nil, b.reserve(satMul(n, 64))
		}}
	}
	for _, name := range []string{"merge", "mergeOverwrite", "mustMerge", "mustMergeOverwrite"} {
		m[name] = funcSpec{pre: preMerge}
	}
	for _, name := range []string{"list", "tuple", "append", "push", "mustAppend", "mustPush", "prepend", "mustPrepend"} {
		m[name] = funcSpec{stores: true}
	}
	for _, name := range []string{
		"get", "dig", "first", "mustFirst", "last", "mustLast", "default", "coalesce", "ternary",
		"slice", "mustSlice", "rest", "mustRest", "initial", "mustInitial",
	} {
		m[name] = funcSpec{child: true}
	}
	// Functions formatting arbitrary values: bounded by the measured size of the
	// arguments times the worst-case escaping factor.
	for name, factor := range map[string]int64{
		"print": 1, "println": 1, "toString": 1, "cat": 1, "toStrings": 2,
		"quote": 6, "squote": 6, "html": 6, "js": 6, "urlquery": 3,
		"toJson": 6, "toRawJson": 6, "mustToJson": 6, "mustToRawJson": 6,
	} {
		m[name] = funcSpec{pre: preFormat(factor, false)}
	}
	m["join"] = funcSpec{pre: preJoin}
	for _, name := range []string{"toPrettyJson", "mustToPrettyJson"} {
		m[name] = funcSpec{pre: preFormat(6, true)}
	}
	for _, name := range []string{
		"bcrypt", "htpasswd", "genPrivateKey", "derivePassword", "genCA", "genCAWithKey",
		"genSelfSignedCert", "genSelfSignedCertWithKey", "genSignedCert", "genSignedCertWithKey",
	} {
		m[name] = funcSpec{expensive: true}
	}
	return m
}()

func preRepeat(b *renderBudget, a []reflect.Value) (func() error, error) {
	n := a[0].Int()
	if n < 0 {
		return nil, fmt.Errorf("repeat: negative count %d", n)
	}
	return nil, b.reserve(satMul(n, int64(a[1].Len())))
}

func preIndent(extra int64) func(b *renderBudget, a []reflect.Value) (func() error, error) {
	return func(b *renderBudget, a []reflect.Value) (func() error, error) {
		n := a[0].Int()
		if n < 0 {
			return nil, fmt.Errorf("indent: negative width %d", n)
		}
		s := a[1].String()
		lines := int64(strings.Count(s, "\n")) + 1
		return nil, b.reserve(satAdd(int64(len(s))+extra, satMul(n, lines)))
	}
}

func preReplace(b *renderBudget, a []reflect.Value) (func() error, error) {
	old, repl, src := a[0].String(), a[1].String(), a[2].String()
	var n int64
	if old == "" {
		n = int64(utf8.RuneCountInString(src)) + 1
	} else {
		n = int64(strings.Count(src, old))
	}
	return nil, b.reserve(satAdd(int64(len(src)), satMul(n, int64(len(repl)-len(old)))))
}

func splitCount(sep, s string) int64 {
	if sep == "" {
		return int64(utf8.RuneCountInString(s))
	}
	return int64(strings.Count(s, sep)) + 1
}

func preSplit(sepIdx int, per int64) func(b *renderBudget, a []reflect.Value) (func() error, error) {
	return func(b *renderBudget, a []reflect.Value) (func() error, error) {
		return nil, b.reserve(satMul(splitCount(a[sepIdx].String(), a[len(a)-1].String()), per))
	}
}

func preSplitN(b *renderBudget, a []reflect.Value) (func() error, error) {
	n := splitCount(a[0].String(), a[2].String())
	if lim := a[1].Int(); lim >= 0 && lim < n {
		n = lim
	}
	return nil, b.reserve(satMul(n, 64))
}

// stepCount is the number of values untilStep(start, stop, step) yields.
func stepCount(start, stop, step int64) uint64 {
	ceilDiv := func(a, b uint64) uint64 {
		q := a / b
		if a%b != 0 {
			q++
		}
		return q
	}
	if stop < start {
		if step >= 0 {
			return 0
		}
		return ceilDiv(uint64(start)-uint64(stop), uint64(-(step+1))+1)
	}
	if step <= 0 {
		return 0
	}
	return ceilDiv(uint64(stop)-uint64(start), uint64(step))
}

func reserveSteps(b *renderBudget, start, stop, step, per int64) error {
	n := stepCount(start, stop, step)
	if n > uint64(b.limits.MaxBytes) {
		return limitErr("sequence of %d values exceeds the template limits", n)
	}
	return b.reserve(satMul(int64(n), per))
}

// seqBounds resolves sprig's seq parameters to untilStep arguments.
func seqBounds(params []int) (start, stop, step int, ok bool) {
	switch len(params) {
	case 1:
		start, end := 1, params[0]
		step = 1
		if end < start {
			step = -1
		}
		return start, end + step, step, true
	case 2:
		start, end := params[0], params[1]
		step = 1
		if end < start {
			step = -1
		}
		return start, end + step, step, true
	case 3:
		start, step, end := params[0], params[1], params[2]
		increment := 1
		if end < start {
			increment = -1
			if step > 0 {
				return 0, 0, 0, false
			}
		}
		return start, end + increment, step, true
	}
	return 0, 0, 0, false
}

func preSeq(b *renderBudget, a []reflect.Value) (func() error, error) {
	params := make([]int, a[0].Len())
	for i := range params {
		params[i] = int(a[0].Index(i).Int())
	}
	start, stop, step, ok := seqBounds(params)
	if !ok {
		return nil, nil
	}
	return nil, reserveSteps(b, int64(start), int64(stop), int64(step), 21)
}

// until, untilStep and seq mirror sprig's functions. sprig's untilStep never terminates
// when start+step overflows; these stop at the last representable value.
func until(count int) []int {
	step := 1
	if count < 0 {
		step = -1
	}
	return untilStep(0, count, step)
}

func untilStep(start, stop, step int) []int {
	n := stepCount(int64(start), int64(stop), int64(step))
	v := make([]int, 0, n)
	for i, x := uint64(0), start; i < n; i, x = i+1, x+step {
		v = append(v, x)
	}
	return v
}

func seq(params ...int) string {
	start, stop, step, ok := seqBounds(params)
	if !ok {
		return ""
	}
	vals := untilStep(start, stop, step)
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, " ")
}

func preQuadratic(b *renderBudget, a []reflect.Value) (func() error, error) {
	n := int64(listLen(a[0]))
	if satMul(n, n)/2 > maxQuadraticOps {
		return nil, limitErr("list of %d elements is too long for this function", n)
	}
	return nil, nil
}

func preWithout(b *renderBudget, a []reflect.Value) (func() error, error) {
	if satMul(int64(listLen(a[0])), int64(a[1].Len())) > maxQuadraticOps {
		return nil, limitErr("list is too long for this function")
	}
	return nil, nil
}

func listLen(v reflect.Value) int {
	if c, ok := container(v); ok && c.Kind() != reflect.Map {
		return c.Len()
	}
	return 0
}

func preFromJSON(b *renderBudget, a []reflect.Value) (func() error, error) {
	return nil, b.reserve(satMul(int64(a[0].Len()), 8))
}

func preDeepCopy(b *renderBudget, a []reflect.Value) (func() error, error) {
	in, err := b.info(a[0])
	if err != nil {
		return nil, err
	}
	return nil, b.reserve(satAdd(satMul(in.nodes, 64), in.logical))
}

func preFormat(factor int64, pretty bool) func(b *renderBudget, a []reflect.Value) (func() error, error) {
	return func(b *renderBudget, a []reflect.Value) (func() error, error) {
		var total int64
		for _, v := range expandVariadic(a) {
			n, err := b.fmtEstimate(v)
			if err != nil {
				return nil, err
			}
			total = satAdd(total, n)
			if pretty {
				if _, ok := container(v); ok {
					in, err := b.info(v)
					if err != nil {
						return nil, err
					}
					total = satAdd(total, satMul(in.nodes, int64(2*in.depth+2)*8))
				}
			}
		}
		return nil, b.reserve(satMul(total, factor))
	}
}

// preJoin bounds join(sep, list): the elements plus one separator between each.
func preJoin(b *renderBudget, a []reflect.Value) (func() error, error) {
	n, err := b.fmtEstimate(a[1])
	if err != nil {
		return nil, err
	}
	sep := satMul(int64(a[0].Len()), int64(listLen(a[1])))
	return nil, b.reserve(satAdd(n, sep))
}

// prePrintf bounds fmt.Sprintf: every verb's argument (an argument may be referenced
// repeatedly with %[n]), widths and precisions, and the arguments themselves (unused
// arguments are printed as %!(EXTRA ...)). Escaping verbs (%q, %x, % x) expand an
// argument up to six times.
func prePrintf(b *renderBudget, a []reflect.Value) (func() error, error) {
	format := a[0].String()
	n := a[1].Len()
	args := make([]any, n)
	cost := make([]int64, n)
	total := int64(len(format))
	for i := 0; i < n; i++ {
		v := a[1].Index(i)
		est, err := b.fmtEstimate(v)
		if err != nil {
			return nil, err
		}
		cost[i] = satMul(est, 6)
		total = satAdd(total, cost[i])
		args[i] = v.Interface()
	}
	return nil, b.reserve(satAdd(total, printfCost(format, args, cost)))
}

// fmtMaxPad is fmt's own limit: larger widths and precisions are reported as BADWIDTH or
// BADPREC and not applied.
const fmtMaxPad = 1_000_000

// printfPadding bounds the padding requested by format's widths and precisions.
func printfPadding(format string, args []any) int64 { return printfCost(format, args, nil) }

// printfCost returns the padding requested by format's widths and precisions plus, when
// cost is set, cost[i] for every verb formatting argument i.
func printfCost(format string, args []any, cost []int64) int64 {
	var total int64
	argNum := 0
	n := len(format)
	index := func(i int) int {
		if i < n && format[i] == '[' {
			if j := strings.IndexByte(format[i:], ']'); j > 0 {
				if v, err := strconv.Atoi(format[i+1 : i+j]); err == nil && v > 0 {
					argNum = v - 1
				}
				return i + j + 1
			}
		}
		return i
	}
	number := func(i int) (int64, int) {
		if i < n && format[i] == '*' {
			var w int64
			if argNum >= 0 && argNum < len(args) {
				if rv := reflect.ValueOf(args[argNum]); rv.IsValid() && rv.CanInt() {
					w = rv.Int()
				}
			}
			argNum++
			if w < 0 {
				w = -w
			}
			if w > fmtMaxPad {
				w = 0
			}
			return w, i + 1
		}
		var w int64
		start := i
		for i < n && format[i] >= '0' && format[i] <= '9' {
			if w <= fmtMaxPad {
				w = w*10 + int64(format[i]-'0')
			}
			i++
		}
		if i == start || w > fmtMaxPad {
			w = 0
		}
		return w, i
	}
	for i := 0; i < n; i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if i < n && format[i] == '%' {
			continue
		}
		for i < n && strings.IndexByte("+-# 0", format[i]) >= 0 {
			i++
		}
		i = index(i)
		w, j := number(i)
		total = satAdd(total, w)
		i = j
		if i < n && format[i] == '.' {
			i = index(i + 1)
			p, j := number(i)
			total = satAdd(total, p)
			i = j
		}
		i = index(i)
		if argNum >= 0 && argNum < len(cost) {
			total = satAdd(total, cost[argNum])
		}
		argNum++
	}
	return total
}

// preRegexReplace bounds regexReplaceAll[Literal]: the input plus every replacement, a
// $name reference expanding to at most the whole match.
func preRegexReplace(literal bool) func(b *renderBudget, a []reflect.Value) (func() error, error) {
	return func(b *renderBudget, a []reflect.Value) (func() error, error) {
		pattern, s, repl := a[0].String(), a[1].String(), a[2].String()
		fixed, refs := int64(len(repl)), int64(0)
		if !literal {
			fixed, refs = expandShape(repl)
		}
		sl := int64(len(s))
		// Cheap bound: a match at every position, every reference expanding to all of s.
		if b.reserve(satAdd(sl, satMul(sl+1, satAdd(fixed, satMul(refs, sl))))) == nil {
			return nil, nil
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, nil // reported by the function itself
		}
		matches := re.FindAllStringIndex(s, maxRegexScanMatches)
		if len(matches) == maxRegexScanMatches {
			return nil, limitErr("regular expression replacement over more than %d matches", maxRegexScanMatches)
		}
		total := sl
		for _, m := range matches {
			total = satAdd(total, satAdd(fixed, satMul(refs, int64(m[1]-m[0]))))
		}
		return nil, b.reserve(total)
	}
}

// expandShape returns the literal byte count and the number of group references of a
// regexp replacement template (regexp.Expand syntax).
func expandShape(tmpl string) (fixed, refs int64) {
	for {
		before, after, ok := strings.Cut(tmpl, "$")
		fixed += int64(len(before))
		if !ok {
			return fixed, refs
		}
		tmpl = after
		if tmpl != "" && tmpl[0] == '$' {
			fixed++
			tmpl = tmpl[1:]
			continue
		}
		rest, ok := extractRef(tmpl)
		if !ok {
			fixed++
			continue
		}
		refs++
		tmpl = rest
	}
}

// extractRef parses a $name or ${name} reference (after the '$').
func extractRef(s string) (string, bool) {
	brace := false
	if s != "" && s[0] == '{' {
		brace = true
		s = s[1:]
	}
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			break
		}
		i += size
	}
	if i == 0 {
		return "", false
	}
	if brace {
		if i >= len(s) || s[i] != '}' {
			return "", false
		}
		i++
	}
	return s[i:], true
}

// preSet validates set(dict, key, value): the value must not contain dict (a cyclic value
// crashes fmt and deep copies), and the resulting value must stay within the limits.
func preSet(b *renderBudget, a []reflect.Value) (func() error, error) {
	d, key, val := a[0], a[1].String(), a[2]
	dk, ok := keyOf(d)
	if !ok {
		return nil, nil // nil map: reported by the function itself
	}
	if _, isC := container(val); isC {
		cyclic := false
		err := b.reachable(val, map[containerKey]struct{}{}, 0, func(k containerKey) bool {
			if k == dk {
				cyclic = true
			}
			return !cyclic
		})
		if err != nil {
			return nil, err
		}
		if cyclic {
			return nil, limitErr("set: the value contains the dictionary itself (cyclic value)")
		}
	}
	if _, root := b.roots[dk]; !root {
		b.mutations++
		if b.mutations > maxSharedMutations {
			return nil, limitErr("more than %d modifications of shared dictionaries", maxSharedMutations)
		}
		return func() error {
			b.dropRoot(val)
			return b.recheck()
		}, nil
	}
	// A root is referenced by no other value: update its measurement incrementally.
	di, err := b.info(d)
	if err != nil {
		return nil, err
	}
	vi, err := b.measure(val, 1)
	if err != nil {
		return nil, err
	}
	keyLogical := scalarLogical(reflect.ValueOf(key)) + 2
	logical := satAdd(di.logical, satAdd(vi.logical, keyLogical))
	if old := d.MapIndex(reflect.ValueOf(key)); old.IsValid() {
		oi, err := b.measure(old, 1)
		if err != nil {
			return nil, err
		}
		logical -= oi.logical + keyLogical
	}
	ni := valueInfo{depth: max(di.depth, vi.depth+1), logical: logical, nodes: satAdd(di.nodes, vi.nodes)}
	if ni.depth > maxValueDepth {
		return nil, limitErr("value nesting is deeper than %d levels (or cyclic)", maxValueDepth)
	}
	if ni.logical > b.maxValueBytes() {
		return nil, limitErr("a template value exceeds %d bytes", b.maxValueBytes())
	}
	return func() error {
		b.dropRoot(val)
		b.memo[dk] = memoEntry{info: ni, ref: d.Interface()}
		return nil
	}, nil
}

// preMerge validates merge(dst, srcs...): sources must not reference maps reachable from
// dst (that could create a cyclic value), and every value is re-validated afterwards.
func preMerge(b *renderBudget, a []reflect.Value) (func() error, error) {
	dst := map[containerKey]struct{}{}
	err := b.reachable(a[0], map[containerKey]struct{}{}, 0, func(k containerKey) bool {
		if k.isMap {
			dst[k] = struct{}{}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	shared := false
	seen := map[containerKey]struct{}{}
	for i := 0; i < a[1].Len() && !shared; i++ {
		err := b.reachable(a[1].Index(i), seen, 0, func(k containerKey) bool {
			if _, ok := dst[k]; ok {
				shared = true
			}
			return !shared
		})
		if err != nil {
			return nil, err
		}
	}
	if shared {
		return nil, limitErr("merge: a source references a dictionary of the destination")
	}
	b.mutations++
	if b.mutations > maxSharedMutations {
		return nil, limitErr("more than %d modifications of shared dictionaries", maxSharedMutations)
	}
	return b.recheck, nil
}
