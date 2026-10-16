package template

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	gotemplate "text/template"
	"text/template/parse"
	"unicode/utf8"
)

// Evaluation limits. Templates are untrusted input: anyone who may create an ApplicationSet in
// an allowed namespace controls them, and the controller is shared by every tenant. The output
// cap alone does not bound memory: functions such as until, repeat or indent allocate their whole
// result before anything is written, and range loops and recursive template invocations run
// without output. Every evaluation (one generated Application, one generator interpolation, one
// standalone Replace) is therefore bounded in work, value size and output.
const (
	// MaxEvaluationSteps bounds the work of one evaluation. A step is roughly one template node
	// evaluated: every template invocation charges the node count of the invoked template,
	// every range iteration the node count of its body, every function call one step plus the
	// size of its arguments (strings per 64 bytes, collections per element) and the values it
	// measures; key generation and password hashing are charged in bulk.
	MaxEvaluationSteps int64 = 1_000_000
	// MaxEvaluationBytes bounds the total output of one evaluation (sum of its rendered
	// strings). A generated Application larger than that could not be stored anyway.
	MaxEvaluationBytes int64 = 4 << 20
	// MaxValueBytes bounds the estimated size of one value a template function builds,
	// serializes or formats (before it is built: the estimate counts shared references every
	// time they are reachable, escaping and indentation).
	MaxValueBytes int64 = 8 << 20
	// MaxSequenceLength bounds the elements of until, untilStep and seq and the pieces of
	// split, splitList, splitn, regexSplit and regexFindAll.
	MaxSequenceLength = 10_000
	// maxValueDepth bounds the nesting depth of values measured by the limits: fmt,
	// encoding/json, YAML encoding and deep copies recurse once per level.
	maxValueDepth = 200
	// bytesPerStep converts string bytes processed by a function into steps.
	bytesPerStep = 64
)

// ErrLimitExceeded is wrapped by every error caused by an evaluation limit.
var ErrLimitExceeded = errors.New("template exceeds an evaluation limit")

func limitErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLimitExceeded, fmt.Sprintf(format, args...))
}

// evaluation is the budget of one evaluation, shared by the strings it renders. It is used by
// one goroutine.
type evaluation struct {
	steps    int64
	rendered int64
}

func newEvaluation() *evaluation { return &evaluation{} }

// step charges n steps.
func (e *evaluation) step(n int64) error {
	if n < 0 || n > MaxEvaluationSteps-e.steps {
		e.steps = MaxEvaluationSteps
		return limitErr("evaluation exceeds %d steps", MaxEvaluationSteps)
	}
	e.steps += n
	return nil
}

// remainingOutput is the output the evaluation may still produce in one string.
func (e *evaluation) remainingOutput() int {
	return int(min(int64(MaxRenderedBytes), MaxEvaluationBytes-e.rendered))
}

// output charges n rendered bytes.
func (e *evaluation) output(n int) error {
	if int64(n) > MaxEvaluationBytes-e.rendered {
		return limitErr("rendered output exceeds %d bytes", MaxEvaluationBytes)
	}
	e.rendered += int64(n)
	return e.step(int64(n)/bytesPerStep + 1)
}

// ---------------------------------------------------------------------------------------
// Parse tree instrumentation: every template invocation and every range iteration calls the
// step function with the node count of the template or loop body.

const stepFunc = "_patrolEvaluationStep"

// stepAction returns the action {{_patrolEvaluationStep weight}}, which outputs nothing.
func stepAction(pos parse.Pos, weight int) *parse.ActionNode {
	num := &parse.NumberNode{
		NodeType: parse.NodeNumber, Pos: pos,
		IsInt: true, Int64: int64(weight), IsUint: true, Uint64: uint64(weight),
		IsFloat: true, Float64: float64(weight), Text: strconv.Itoa(weight),
	}
	cmd := &parse.CommandNode{NodeType: parse.NodeCommand, Pos: pos, Args: []parse.Node{parse.NewIdentifier(stepFunc).SetPos(pos), num}}
	pipe := &parse.PipeNode{NodeType: parse.NodePipe, Pos: pos, Cmds: []*parse.CommandNode{cmd}}
	return &parse.ActionNode{NodeType: parse.NodeAction, Pos: pos, Pipe: pipe}
}

// instrument inserts the step actions into every tree of t and returns the functions the trees
// call.
func instrument(t *gotemplate.Template) map[string]bool {
	used := map[string]bool{}
	seen := map[*parse.Tree]bool{}
	for _, tt := range t.Templates() {
		if tt.Tree == nil || tt.Root == nil || seen[tt.Tree] {
			continue
		}
		seen[tt.Tree] = true
		collectIdentifiers(tt.Root, used)
		instrumentNode(tt.Root)
		tt.Root.Nodes = append([]parse.Node{stepAction(tt.Root.Pos, countNodes(tt.Root)+1)}, tt.Root.Nodes...)
	}
	return used
}

func instrumentNode(n parse.Node) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			instrumentNode(c)
		}
	case *parse.IfNode:
		instrumentNode(n.List)
		instrumentNode(n.ElseList)
	case *parse.WithNode:
		instrumentNode(n.List)
		instrumentNode(n.ElseList)
	case *parse.RangeNode:
		instrumentNode(n.List)
		instrumentNode(n.ElseList)
		if n.List != nil {
			n.List.Nodes = append([]parse.Node{stepAction(n.Pos, countNodes(n.List)+1)}, n.List.Nodes...)
		}
	}
}

// countNodes counts the nodes of a subtree.
func countNodes(n parse.Node) int {
	switch n := n.(type) {
	case nil:
		return 0
	case *parse.ListNode:
		if n == nil {
			return 0
		}
		c := 1
		for _, x := range n.Nodes {
			c += countNodes(x)
		}
		return c
	case *parse.ActionNode:
		return 1 + countNodes(n.Pipe)
	case *parse.PipeNode:
		if n == nil {
			return 0
		}
		c := 1 + len(n.Decl)
		for _, cmd := range n.Cmds {
			c += countNodes(cmd)
		}
		return c
	case *parse.CommandNode:
		c := 1
		for _, a := range n.Args {
			c += countNodes(a)
		}
		return c
	case *parse.ChainNode:
		return 1 + len(n.Field) + countNodes(n.Node)
	case *parse.IfNode:
		return 1 + countNodes(n.Pipe) + countNodes(n.List) + countNodes(n.ElseList)
	case *parse.WithNode:
		return 1 + countNodes(n.Pipe) + countNodes(n.List) + countNodes(n.ElseList)
	case *parse.RangeNode:
		return 1 + countNodes(n.Pipe) + countNodes(n.List) + countNodes(n.ElseList)
	case *parse.TemplateNode:
		return 1 + countNodes(n.Pipe)
	default:
		return 1
	}
}

func collectIdentifiers(n parse.Node, used map[string]bool) {
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
		used[n.Ident] = true
	case *parse.IfNode:
		collectIdentifiers(n.Pipe, used)
		collectIdentifiers(n.List, used)
		collectIdentifiers(n.ElseList, used)
	case *parse.WithNode:
		collectIdentifiers(n.Pipe, used)
		collectIdentifiers(n.List, used)
		collectIdentifiers(n.ElseList, used)
	case *parse.RangeNode:
		collectIdentifiers(n.Pipe, used)
		collectIdentifiers(n.List, used)
		collectIdentifiers(n.ElseList, used)
	case *parse.TemplateNode:
		collectIdentifiers(n.Pipe, used)
	}
}

// ---------------------------------------------------------------------------------------
// Bounded functions

// builtinOverrides are text/template builtins that format their arguments; they are replaced
// by bounded equivalents. and/or keep their short-circuit builtins; the others do not allocate
// beyond their arguments.
var builtinOverrides = map[string]any{
	"print":    fmt.Sprint,
	"printf":   fmt.Sprintf,
	"println":  fmt.Sprintln,
	"html":     gotemplate.HTMLEscaper,
	"js":       gotemplate.JSEscaper,
	"urlquery": gotemplate.URLQueryEscaper,
}

// policy describes how a function is bounded. Every call is charged one step plus the
// shallow size of its arguments.
type policy struct {
	// estimate returns an upper bound of the bytes the call allocates; it must not exceed
	// MaxValueBytes.
	estimate func(e *evaluation, args []reflect.Value) (int64, error)
	// result measures the returned value, which must not exceed MaxValueBytes.
	result bool
	// mutates marks functions that modify their first argument (a map) in place with the
	// other arguments: a value that reaches that map would become cyclic.
	mutates bool
	// cost is charged in addition (CPU-heavy functions).
	cost int64
	// quadratic charges the square of the first list argument's length / 8 (uniq).
	quadratic bool
}

func fmtLike(factor int64) policy {
	return policy{estimate: func(e *evaluation, args []reflect.Value) (int64, error) {
		m, err := e.measureAll(args)
		return factor*m.bytes + 32*m.nodes, err
	}}
}

func jsonLike(pretty bool) policy {
	return policy{estimate: func(e *evaluation, args []reflect.Value) (int64, error) {
		m, err := e.measureAll(args)
		est := 6*m.bytes + 32*m.nodes
		if pretty {
			est += 4 * m.depthSum
		}
		return est, err
	}}
}

var (
	resultChecked = policy{result: true}
	mutating      = policy{result: true, mutates: true}
	deepCopying   = policy{result: true, estimate: func(e *evaluation, args []reflect.Value) (int64, error) {
		m, err := e.measureAll(args)
		return m.bytes + 16*m.nodes, err
	}}
)

// policies lists the functions that need more than the default accounting. Functions not
// listed produce at most their input (accessors, predicates, shrinking string functions).
var policies = map[string]policy{
	// Sequences and repetition.
	"until":                      {estimate: estimateUntil},
	"untilStep":                  {estimate: estimateUntilStep},
	"seq":                        {estimate: estimateSeq},
	"repeat":                     {estimate: estimateRepeat},
	"indent":                     {estimate: estimateIndent},
	"nindent":                    {estimate: estimateIndent},
	"replace":                    {estimate: estimateReplace},
	"wrap":                       {estimate: estimateWrap},
	"wrapWith":                   {estimate: estimateWrap},
	"randAlphaNum":               {estimate: estimateCount(1)},
	"randAlpha":                  {estimate: estimateCount(1)},
	"randAscii":                  {estimate: estimateCount(1)},
	"randNumeric":                {estimate: estimateCount(1)},
	"randBytes":                  {estimate: estimateCount(2)},
	"join":                       {estimate: estimateJoin},
	"printf":                     {estimate: estimatePrintf},
	"split":                      {estimate: estimatePieces(0, 1, -1, 64), result: true},
	"splitList":                  {estimate: estimatePieces(0, 1, -1, 16), result: true},
	"splitn":                     {estimate: estimatePieces(0, 2, 1, 64), result: true},
	"regexSplit":                 {estimate: estimateRegexPieces, result: true},
	"mustRegexSplit":             {estimate: estimateRegexPieces, result: true},
	"regexFindAll":               {estimate: estimateRegexPieces, result: true},
	"mustRegexFindAll":           {estimate: estimateRegexPieces, result: true},
	"regexReplaceAll":            {estimate: estimateRegexReplace(true)},
	"mustRegexReplaceAll":        {estimate: estimateRegexReplace(true)},
	"regexReplaceAllLiteral":     {estimate: estimateRegexReplace(false)},
	"mustRegexReplaceAllLiteral": {estimate: estimateRegexReplace(false)},

	// Formatting and serialization.
	"print":            fmtLike(4),
	"println":          fmtLike(4),
	"toString":         fmtLike(4),
	"toStrings":        fmtLike(4),
	"sortAlpha":        fmtLike(4),
	"cat":              fmtLike(4),
	"quote":            fmtLike(4),
	"squote":           fmtLike(4),
	"html":             fmtLike(6),
	"js":               fmtLike(6),
	"urlquery":         fmtLike(3),
	"urlJoin":          fmtLike(4),
	"upper":            fmtLike(2),
	"lower":            fmtLike(2),
	"title":            fmtLike(2),
	"untitle":          fmtLike(2),
	"swapcase":         fmtLike(2),
	"snakecase":        fmtLike(2),
	"camelcase":        fmtLike(2),
	"kebabcase":        fmtLike(2),
	"shuffle":          fmtLike(2),
	"b64enc":           fmtLike(2),
	"b32enc":           fmtLike(2),
	"encryptAES":       fmtLike(2),
	"toJson":           jsonLike(false),
	"mustToJson":       jsonLike(false),
	"toRawJson":        jsonLike(false),
	"mustToRawJson":    jsonLike(false),
	"toPrettyJson":     jsonLike(true),
	"mustToPrettyJson": jsonLike(true),
	"toYaml":           jsonLike(true),

	// Values built from their arguments.
	"list":               resultChecked,
	"tuple":              resultChecked,
	"dict":               resultChecked,
	"append":             resultChecked,
	"push":               resultChecked,
	"mustAppend":         resultChecked,
	"mustPush":           resultChecked,
	"prepend":            resultChecked,
	"mustPrepend":        resultChecked,
	"concat":             resultChecked,
	"chunk":              resultChecked,
	"mustChunk":          resultChecked,
	"pick":               resultChecked,
	"omit":               resultChecked,
	"fromJson":           resultChecked,
	"mustFromJson":       resultChecked,
	"fromYaml":           resultChecked,
	"fromYamlArray":      resultChecked,
	"deepCopy":           deepCopying,
	"mustDeepCopy":       deepCopying,
	"set":                mutating,
	"merge":              mutating,
	"mustMerge":          mutating,
	"mergeOverwrite":     mutating,
	"mustMergeOverwrite": mutating,
	"uniq":               {quadratic: true},
	"mustUniq":           {quadratic: true},

	// CPU-heavy functions.
	"genPrivateKey":            {cost: 400_000},
	"genCA":                    {cost: 200_000},
	"genCAWithKey":             {cost: 200_000},
	"genSelfSignedCert":        {cost: 200_000},
	"genSelfSignedCertWithKey": {cost: 200_000},
	"genSignedCert":            {cost: 200_000},
	"genSignedCertWithKey":     {cost: 200_000},
	"derivePassword":           {cost: 200_000},
	"bcrypt":                   {cost: 100_000},
	"htpasswd":                 {cost: 100_000},
}

// funcs returns the bounded implementations of the used functions plus the step function.
func (e *evaluation) funcs(used map[string]bool) gotemplate.FuncMap {
	out := gotemplate.FuncMap{stepFunc: func(weight int) (string, error) { return "", e.step(int64(weight)) }}
	for name := range used {
		fn, ok := funcMap[name]
		if !ok {
			if fn, ok = builtinOverrides[name]; !ok {
				continue
			}
		}
		out[name] = e.wrap(name, fn, policies[name])
	}
	return out
}

var errorType = reflect.TypeFor[error]()

// wrap returns fn bounded by p, with an additional error result.
func (e *evaluation) wrap(name string, fn any, p policy) any {
	fv := reflect.ValueOf(fn)
	ft := fv.Type()
	ins := make([]reflect.Type, ft.NumIn())
	for i := range ins {
		ins[i] = ft.In(i)
	}
	outT := ft.Out(0)
	hasErr := ft.NumOut() == 2
	wt := reflect.FuncOf(ins, []reflect.Type{outT, errorType}, ft.IsVariadic())
	return reflect.MakeFunc(wt, func(args []reflect.Value) []reflect.Value {
		fail := func(err error) []reflect.Value {
			ev := reflect.New(errorType).Elem()
			ev.Set(reflect.ValueOf(fmt.Errorf("%s: %w", name, err)))
			return []reflect.Value{reflect.Zero(outT), ev}
		}
		if err := e.before(p, args); err != nil {
			return fail(err)
		}
		var out []reflect.Value
		if ft.IsVariadic() {
			out = fv.CallSlice(args)
		} else {
			out = fv.Call(args)
		}
		if hasErr && !out[1].IsNil() {
			return out
		}
		if p.result {
			m := measure{}
			if err := e.measureValue(out[0], &m, 0); err != nil {
				return fail(err)
			}
			if est := m.bytes + 16*m.nodes; est > MaxValueBytes {
				return fail(limitErr("the result exceeds %d bytes", MaxValueBytes))
			}
		}
		return []reflect.Value{out[0], reflect.Zero(errorType)}
	}).Interface()
}

func (e *evaluation) before(p policy, args []reflect.Value) error {
	cost := int64(1) + p.cost
	for _, a := range args {
		cost += shallowSize(a)
	}
	if p.quadratic && len(args) > 0 {
		n := shallowLen(args[0])
		cost += n * n / 8
	}
	if err := e.step(cost); err != nil {
		return err
	}
	if p.mutates && len(args) > 1 {
		if err := e.checkAcyclic(args[0], args[1:]); err != nil {
			return err
		}
	}
	if p.estimate != nil {
		est, err := p.estimate(e, args)
		if err != nil {
			return err
		}
		if est > MaxValueBytes {
			return limitErr("the result would exceed %d bytes", MaxValueBytes)
		}
		if err := e.step(est / bytesPerStep); err != nil {
			return err
		}
	}
	return nil
}

// shallowSize is the default charge of an argument: string bytes per bytesPerStep, collection
// elements.
func shallowSize(v reflect.Value) int64 {
	v = indirect(v)
	switch v.Kind() {
	case reflect.String:
		return int64(v.Len()) / bytesPerStep
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return int64(v.Len()) / bytesPerStep
		}
		return int64(v.Len())
	case reflect.Map:
		return int64(v.Len())
	}
	return 0
}

func shallowLen(v reflect.Value) int64 {
	v = indirect(v)
	switch v.Kind() {
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map:
		return int64(v.Len())
	}
	return 0
}

func indirect(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// measure is the estimated printed size of a value.
type measure struct {
	bytes    int64 // content bytes (strings, scalars, delimiters)
	nodes    int64 // values (each map, slice, scalar)
	depthSum int64 // sum of the nesting depth of every value (indentation)
}

func (e *evaluation) measureAll(args []reflect.Value) (measure, error) {
	m := measure{}
	for _, a := range args {
		if err := e.measureValue(a, &m, 0); err != nil {
			return m, err
		}
	}
	return m, nil
}

// measureValue adds v to m. Every value visited is charged one step, so measuring is bounded
// by the evaluation budget; values larger than MaxValueBytes or deeper than maxValueDepth (a
// cyclic value is infinitely deep) fail.
func (e *evaluation) measureValue(v reflect.Value, m *measure, depth int) error {
	if depth > maxValueDepth {
		return limitErr("value nesting exceeds %d levels", maxValueDepth)
	}
	if m.bytes > MaxValueBytes {
		return limitErr("the value exceeds %d bytes", MaxValueBytes)
	}
	if err := e.step(1); err != nil {
		return err
	}
	m.nodes++
	m.depthSum += int64(depth)
	v = indirect(v)
	switch v.Kind() {
	case reflect.Invalid:
		m.bytes += 5
	case reflect.String:
		m.bytes += int64(v.Len()) + 2
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			m.bytes += int64(v.Len()) + 2
			return nil
		}
		m.bytes += 2
		for i := 0; i < v.Len(); i++ {
			m.bytes++
			if err := e.measureValue(v.Index(i), m, depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		m.bytes += 2
		iter := v.MapRange()
		for iter.Next() {
			m.bytes += 2
			if err := e.measureValue(iter.Key(), m, depth+1); err != nil {
				return err
			}
			if err := e.measureValue(iter.Value(), m, depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		m.bytes += 2
		for i := 0; i < v.NumField(); i++ {
			if err := e.measureValue(v.Field(i), m, depth+1); err != nil {
				return err
			}
		}
	default:
		m.bytes += 32
	}
	if m.bytes > MaxValueBytes {
		return limitErr("the value exceeds %d bytes", MaxValueBytes)
	}
	return nil
}

// checkAcyclic fails when dst (a map modified in place) is reachable from any of values: the
// modification would make it cyclic, and printing or serializing a cyclic value overflows the
// stack (an unrecoverable crash).
func (e *evaluation) checkAcyclic(dst reflect.Value, values []reflect.Value) error {
	dst = indirect(dst)
	if dst.Kind() != reflect.Map || dst.IsNil() {
		return nil
	}
	target := dst.UnsafePointer()
	var reach func(v reflect.Value, depth int) (bool, error)
	reach = func(v reflect.Value, depth int) (bool, error) {
		if depth > maxValueDepth {
			return false, limitErr("value nesting exceeds %d levels", maxValueDepth)
		}
		if err := e.step(1); err != nil {
			return false, err
		}
		v = indirect(v)
		switch v.Kind() {
		case reflect.Map:
			if v.UnsafePointer() == target {
				return true, nil
			}
			iter := v.MapRange()
			for iter.Next() {
				if ok, err := reach(iter.Value(), depth+1); ok || err != nil {
					return ok, err
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				return false, nil
			}
			for i := 0; i < v.Len(); i++ {
				if ok, err := reach(v.Index(i), depth+1); ok || err != nil {
					return ok, err
				}
			}
		}
		return false, nil
	}
	for _, v := range values {
		ok, err := reach(v, 0)
		if err != nil {
			return err
		}
		if ok {
			return limitErr("the operation would make a value contain itself")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------
// Estimators. Arguments have the declared types of the wrapped function (text/template
// converts them before the call).

func intArg(args []reflect.Value, i int) int64 {
	if i >= len(args) {
		return 0
	}
	v := indirect(args[i])
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if u := v.Uint(); u <= uint64(1<<62) {
			return int64(u)
		}
		return 1 << 62
	}
	return 0
}

func strArg(args []reflect.Value, i int) string {
	if i >= len(args) {
		return ""
	}
	v := indirect(args[i])
	if v.Kind() == reflect.String {
		return v.String()
	}
	return ""
}

func absDiff(a, b int64) uint64 {
	if a > b {
		return uint64(a) - uint64(b)
	}
	return uint64(b) - uint64(a)
}

// sequenceLength bounds the elements of untilStep(start, stop, step) (sprig semantics: step
// must point from start towards stop).
func sequenceLength(start, stop, step int64) uint64 {
	switch {
	case step == 0, stop > start && step < 0, stop < start && step > 0, stop == start:
		return 0
	}
	s := uint64(step)
	if step < 0 {
		s = uint64(-(step + 1)) + 1
	}
	d := absDiff(start, stop)
	n := d / s
	if d%s != 0 {
		n++
	}
	return n
}

// maxSequenceBound bounds the start, stop and step of sequences: sprig's loops add the step
// after the last element, which overflows (and never terminates) near the int64 limits.
const maxSequenceBound = 1 << 53

func checkSequenceBounds(values ...int64) error {
	for _, v := range values {
		if v > maxSequenceBound || v < -maxSequenceBound {
			return limitErr("sequence bound %d exceeds ±%d", v, int64(maxSequenceBound))
		}
	}
	return nil
}

func checkSequence(n uint64) (int64, error) {
	if n > MaxSequenceLength {
		return 0, limitErr("sequence of %d elements exceeds %d", n, MaxSequenceLength)
	}
	return int64(n) * 24, nil
}

func estimateUntil(_ *evaluation, args []reflect.Value) (int64, error) {
	n := intArg(args, 0)
	if err := checkSequenceBounds(n); err != nil {
		return 0, err
	}
	step := int64(1)
	if n < 0 {
		step = -1
	}
	return checkSequence(sequenceLength(0, n, step))
}

func estimateUntilStep(_ *evaluation, args []reflect.Value) (int64, error) {
	if err := checkSequenceBounds(intArg(args, 0), intArg(args, 1), intArg(args, 2)); err != nil {
		return 0, err
	}
	return checkSequence(sequenceLength(intArg(args, 0), intArg(args, 1), intArg(args, 2)))
}

// estimateSeq bounds seq's elements: at most |end-start|+1 whatever the step.
func estimateSeq(_ *evaluation, args []reflect.Value) (int64, error) {
	if len(args) == 0 {
		return 0, nil
	}
	params := indirect(args[len(args)-1])
	if params.Kind() != reflect.Slice {
		return 0, nil
	}
	ints := make([]int64, params.Len())
	for i := range ints {
		ints[i] = intArg([]reflect.Value{params.Index(i)}, 0)
	}
	if err := checkSequenceBounds(ints...); err != nil {
		return 0, err
	}
	var d uint64
	switch len(ints) {
	case 0:
		return 0, nil
	case 1:
		d = absDiff(1, ints[0])
	case 2:
		d = absDiff(ints[0], ints[1])
	default:
		d = absDiff(ints[0], ints[2])
		if s := ints[1]; s != 0 && s != -1<<63 {
			if s < 0 {
				s = -s
			}
			d /= uint64(s)
		}
	}
	return checkSequence(d + 1)
}

// mulLimit returns a*b, or MaxValueBytes+1 when it would exceed MaxValueBytes.
func mulLimit(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > (MaxValueBytes+1)/b+1 {
		return MaxValueBytes + 1
	}
	return min(a*b, MaxValueBytes+1)
}

func estimateRepeat(_ *evaluation, args []reflect.Value) (int64, error) {
	count := intArg(args, 0)
	if count < 0 {
		return 0, fmt.Errorf("negative repeat count %d", count)
	}
	return mulLimit(count, int64(len(strArg(args, 1)))), nil
}

func estimateIndent(_ *evaluation, args []reflect.Value) (int64, error) {
	spaces := intArg(args, 0)
	if spaces < 0 {
		return 0, fmt.Errorf("negative indentation %d", spaces)
	}
	s := strArg(args, 1)
	lines := int64(strings.Count(s, "\n")) + 1
	return int64(len(s)) + 1 + mulLimit(spaces, lines), nil
}

// estimateReplace: replace old new src (strings.Replace with every occurrence).
func estimateReplace(_ *evaluation, args []reflect.Value) (int64, error) {
	old, repl, src := strArg(args, 0), strArg(args, 1), strArg(args, 2)
	var n int64
	if old == "" {
		n = int64(utf8.RuneCountInString(src)) + 1
	} else {
		n = int64(strings.Count(src, old))
	}
	return int64(len(src)) + mulLimit(n, int64(len(repl))), nil
}

// estimateWrap: wrap n str / wrapWith n sep str insert the separator at most once per byte.
func estimateWrap(_ *evaluation, args []reflect.Value) (int64, error) {
	s, sep := strArg(args, 1), "\n"
	if len(args) == 3 {
		sep, s = strArg(args, 1), strArg(args, 2)
	}
	return int64(len(s)) + mulLimit(int64(len(s))+1, int64(len(sep))), nil
}

func estimateCount(factor int64) func(*evaluation, []reflect.Value) (int64, error) {
	return func(_ *evaluation, args []reflect.Value) (int64, error) {
		return mulLimit(intArg(args, 0), factor), nil
	}
}

func estimateJoin(e *evaluation, args []reflect.Value) (int64, error) {
	sep := strArg(args, 0)
	m, err := e.measureAll(args[1:])
	if err != nil {
		return 0, err
	}
	return 4*m.bytes + 32*m.nodes + mulLimit(m.nodes, int64(len(sep))), nil
}

// estimatePieces bounds the pieces of split-like functions: sep and the string are given by
// argument index; limitArg (or -1) is splitn's maximum number of pieces.
func estimatePieces(sepArg, strIdx, limitArg int, perPiece int64) func(*evaluation, []reflect.Value) (int64, error) {
	return func(_ *evaluation, args []reflect.Value) (int64, error) {
		sep, s := strArg(args, sepArg), strArg(args, strIdx)
		var n int64
		if sep == "" {
			n = int64(utf8.RuneCountInString(s))
		} else {
			n = int64(strings.Count(s, sep)) + 1
		}
		if limitArg >= 0 {
			if l := intArg(args, limitArg); l >= 0 && l < n {
				n = l
			}
		}
		if n > MaxSequenceLength {
			return 0, limitErr("%d pieces exceed %d", n, MaxSequenceLength)
		}
		return int64(len(s)) + n*perPiece, nil
	}
}

// estimateRegexPieces bounds regexSplit/regexFindAll(regex, s, n): the matches are counted up
// to MaxSequenceLength+1.
func estimateRegexPieces(_ *evaluation, args []reflect.Value) (int64, error) {
	re, err := regexp.Compile(strArg(args, 0))
	if err != nil {
		return 0, nil // the function reports the invalid expression
	}
	s := strArg(args, 1)
	limit := MaxSequenceLength + 1
	if n := intArg(args, 2); n >= 0 && n < int64(limit) {
		limit = int(n)
	}
	matches := len(re.FindAllStringIndex(s, limit))
	if matches >= MaxSequenceLength {
		return 0, limitErr("more than %d matches", MaxSequenceLength-1)
	}
	return int64(len(s)) + int64(matches+1)*16, nil
}

// estimateRegexReplace bounds regexReplaceAll(regex, s, repl): at most one replacement per
// byte (plus one), and with expansion every $ reference copies at most the whole input
// (matches do not overlap).
func estimateRegexReplace(expand bool) func(*evaluation, []reflect.Value) (int64, error) {
	return func(_ *evaluation, args []reflect.Value) (int64, error) {
		s, repl := strArg(args, 1), strArg(args, 2)
		est := int64(len(s)) + mulLimit(int64(len(s))+1, int64(len(repl)))
		if expand {
			est += mulLimit(int64(strings.Count(repl, "$")), int64(len(s)))
		}
		return est, nil
	}
}

// maxFmtWidth is fmt's own limit on widths and precisions (larger values print %!(BADWIDTH)).
const maxFmtWidth = 1_000_000

// estimatePrintf bounds fmt.Sprintf(format, args...): the format, plus for every verb the
// largest of its width and precision for each value printed (a slice or map applies them to
// every element) and the formatted size of its argument.
func estimatePrintf(e *evaluation, args []reflect.Value) (int64, error) {
	format := strArg(args, 0)
	var operands []reflect.Value
	if len(args) > 1 {
		if v := indirect(args[1]); v.Kind() == reflect.Slice {
			for i := 0; i < v.Len(); i++ {
				operands = append(operands, v.Index(i))
			}
		}
	}
	est := int64(len(format))
	argNum := 0
	argIndex := func(i int) (int, int) {
		if i < len(format) && format[i] == '[' {
			end := strings.IndexByte(format[i:], ']')
			if end > 0 {
				if n, err := strconv.Atoi(format[i+1 : i+end]); err == nil && n > 0 {
					return n - 1, i + end + 1
				}
				return argNum, i + end + 1
			}
		}
		return argNum, i
	}
	number := func(i int) (int64, int) {
		if i < len(format) && format[i] == '*' {
			var w int64
			if argNum < len(operands) {
				w = intArg(operands, argNum)
				if w < 0 {
					w = -w
				}
			}
			argNum++
			return w, i + 1
		}
		start := i
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			i++
		}
		w, _ := strconv.ParseInt(format[start:i], 10, 64)
		return w, i
	}
	for i := 0; i < len(format); {
		if format[i] != '%' {
			i++
			continue
		}
		i++
		for i < len(format) && strings.IndexByte("+-# 0", format[i]) >= 0 {
			i++
		}
		argNum, i = argIndex(i)
		var width, prec int64
		width, i = number(i)
		if i < len(format) && format[i] == '.' {
			argNum, i = argIndex(i + 1)
			prec, i = number(i)
		}
		argNum, i = argIndex(i)
		if i >= len(format) {
			break
		}
		_, size := utf8.DecodeRuneInString(format[i:])
		verb := format[i]
		i += size
		if verb == '%' {
			est++
			continue
		}
		pad := min(max(width, prec), maxFmtWidth)
		if argNum < len(operands) {
			m := measure{}
			if err := e.measureValue(operands[argNum], &m, 0); err != nil {
				return 0, err
			}
			est += 4*m.bytes + 32*m.nodes + mulLimit(pad, m.nodes)
		} else {
			est += pad + 32
		}
		argNum++
		if est > MaxValueBytes {
			return est, nil
		}
	}
	return est, nil
}
