package lua

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	glua "github.com/yuin/gopher-lua"
)

// String limits. gopher-lua has no allocation hook, so every standard function that can
// build a string much larger than its inputs in one call, and the .. operator (see
// rewrite.go), is replaced by a version that checks the size before allocating:
//
//	string.rep, string.gsub, string.format, string.upper, string.lower, string.reverse,
//	table.concat, os.date and ..
//
// Each evaluation also has a cumulative budget for the strings these build, so a loop
// cannot pile up many strings that are individually under the cap.
const (
	// maxStringBuild caps one string built by a script.
	maxStringBuild = 4 << 20
	// maxStringAlloc caps the total bytes of strings built by one evaluation.
	maxStringAlloc = 64 << 20
	// maxFormatSpec is Lua 5.1's limit on format width/precision digits.
	maxFormatSpec = 2
	// formatArgSlack bounds the formatted size of one non-string argument (a number
	// with width and precision of at most 99, or a type name and address).
	formatArgSlack = 512
)

// allocBudget accounts the strings built by one evaluation. It belongs to one Lua state.
type allocBudget struct {
	used int
}

// check raises a Lua error when a string of n bytes built by op would exceed the
// per-string cap or the remaining budget. It does not charge.
func (b *allocBudget) check(L *glua.LState, op string, n int) {
	if n > maxStringBuild {
		L.RaiseError("%s result exceeds %d bytes", op, maxStringBuild)
	}
	if n > maxStringAlloc-b.used {
		L.RaiseError("%s exceeds the string allocation limit of %d bytes per evaluation", op, maxStringAlloc)
	}
}

// charge checks and records a string of n bytes.
func (b *allocBudget) charge(L *glua.LState, op string, n int) {
	b.check(L, op, n)
	b.used += n
}

// installStringLimits replaces the string building and pattern matching functions of the
// string, table and os libraries opened in L (global tables; require and method calls
// resolve to the same tables) and returns the function that implements the .. operator
// for scripts compiled by compile.
func installStringLimits(L *glua.LState) (*glua.LFunction, error) {
	b := &allocBudget{}
	if mod, ok := L.GetGlobal(glua.StringLibName).(*glua.LTable); ok {
		origFormat, ok := mod.RawGetString("format").(*glua.LFunction)
		if !ok || !origFormat.IsG {
			return nil, errors.New("lua string library has no native format function")
		}
		set := func(name string, fn glua.LGFunction) { mod.RawSetString(name, L.NewFunction(fn)) }
		set("find", strFind)
		set("match", strMatch)
		gmatch := L.NewFunction(strGmatch)
		mod.RawSetString("gmatch", gmatch)
		mod.RawSetString("gfind", gmatch)
		set("gsub", b.gsub)
		set("rep", b.rep)
		set("format", b.format(origFormat.GFunction))
		set("upper", b.mapString("string.upper", strings.ToUpper))
		set("lower", b.mapString("string.lower", strings.ToLower))
		set("reverse", b.mapString("string.reverse", reverseBytes))
		mod.RawSetString("dump", glua.LNil)
	}
	if mod, ok := L.GetGlobal(glua.TabLibName).(*glua.LTable); ok {
		mod.RawSetString("concat", L.NewFunction(b.tableConcat))
	}
	if mod, ok := L.GetGlobal(glua.OsLibName).(*glua.LTable); ok {
		if date, ok := mod.RawGetString("date").(*glua.LFunction); ok && date.IsG {
			mod.RawSetString("date", L.NewFunction(b.osDate(date.GFunction)))
		}
	}
	return L.NewFunction(b.concat), nil
}

// raisePatternError reports a matcher error the way gopher-lua does.
func raisePatternError(L *glua.LState, err error) {
	var pe *patternError
	if errors.As(err, &pe) && pe.goRuntime {
		L.Error(glua.LString(pe.msg), 0)
		return
	}
	L.RaiseError("%s", err.Error())
}

func checkPattern(L *glua.LState, p string) *luaPattern {
	pat, err := compileLuaPattern(p)
	if err != nil {
		raisePatternError(L, err)
		return nil
	}
	return pat
}

// firstMatch returns the captures of the first match at or after init.
func firstMatch(L *glua.LState, src, pattern string, init int) captures {
	pat := checkPattern(L, pattern)
	caps, err := newPatIter(L.Context(), pat, src, init, 1).next()
	if err != nil {
		raisePatternError(L, err)
	}
	return caps
}

// luaStringIndex converts a Lua string index (1-based, negative from the end) as
// gopher-lua does; start indexes are not clamped to the length.
func luaStringIndex(s string, i int, start bool) int {
	if start && i != 0 {
		i--
	}
	if i < 0 {
		i = len(s) + i + 1
	}
	i = max(i, 0)
	if !start && i > len(s) {
		i = len(s)
	}
	return i
}

func pushCapture(L *glua.LState, src string, caps captures, i int) {
	if caps.isPosition(i) {
		L.Push(glua.LNumber(caps.pos(i)))
		return
	}
	L.Push(glua.LString(src[caps.pos(i):caps.pos(i+1)]))
}

func strFind(L *glua.LState) int {
	str := L.CheckString(1)
	pattern := L.CheckString(2)
	if pattern == "" {
		L.Push(glua.LNumber(1))
		L.Push(glua.LNumber(0))
		return 2
	}
	init := luaStringIndex(str, L.OptInt(3, 1), true)
	plain := false
	if L.GetTop() == 4 {
		plain = glua.LVAsBool(L.Get(4))
	}
	if plain {
		if init > len(str) {
			// gopher-lua slices out of range here and reports the Go runtime error.
			L.Error(glua.LString(fmt.Sprintf("runtime error: slice bounds out of range [%d:%d]", init, len(str))), 0)
			return 0
		}
		pos := strings.Index(str[init:], pattern)
		if pos < 0 {
			L.Push(glua.LNil)
			return 1
		}
		L.Push(glua.LNumber(init + pos + 1))
		L.Push(glua.LNumber(init + pos + len(pattern)))
		return 2
	}
	caps := firstMatch(L, str, pattern, init)
	if caps == nil {
		L.Push(glua.LNil)
		return 1
	}
	L.Push(glua.LNumber(caps.pos(0) + 1))
	L.Push(glua.LNumber(caps.pos(1)))
	for i := 2; i < caps.count(); i += 2 {
		pushCapture(L, str, caps, i)
	}
	return caps.count()/2 + 1
}

func strMatch(L *glua.LState) int {
	str := L.CheckString(1)
	pattern := L.CheckString(2)
	offset := L.OptInt(3, 1)
	if offset < 0 {
		offset = len(str) + offset + 1
	}
	offset = max(offset-1, 0)
	caps := firstMatch(L, str, pattern, offset)
	if caps == nil {
		// gopher-lua returns no value (not nil) when nothing matches.
		return 0
	}
	if caps.count() == 2 {
		L.Push(glua.LString(str[caps.pos(0):caps.pos(1)]))
		return 1
	}
	for i := 2; i < caps.count(); i += 2 {
		pushCapture(L, str, caps, i)
	}
	return caps.count()/2 - 1
}

// strGmatch returns an iterator that finds matches lazily, so memory stays bounded and
// the deadline applies between and within matches. (gopher-lua collects every match up
// front and returns an iterator that only works in a generic for; this one also works
// when called directly.)
func strGmatch(L *glua.LState) int {
	str := L.CheckString(1)
	pat := checkPattern(L, L.CheckString(2))
	it := newPatIter(L.Context(), pat, str, 0, -1)
	L.Push(L.NewFunction(func(L *glua.LState) int {
		caps, err := it.next()
		if err != nil {
			raisePatternError(L, err)
			return 0
		}
		if caps == nil {
			return 0
		}
		if caps.count() == 2 {
			L.Push(glua.LString(str[caps.pos(0):caps.pos(1)]))
			return 1
		}
		for i := 2; i < caps.count(); i += 2 {
			pushCapture(L, str, caps, i)
		}
		return caps.count()/2 - 1
	}))
	return 1
}

// capturedString returns capture idx (slot index) as a replacement value, as gopher-lua.
func capturedString(L *glua.LState, src string, caps captures, idx int) string {
	if idx > 2 && idx >= caps.count() {
		L.RaiseError("invalid capture index")
	}
	if idx >= caps.count() && idx == 2 {
		idx = 0
	}
	if caps.isPosition(idx) {
		return strconv.Itoa(caps.pos(idx))
	}
	return src[caps.pos(idx):caps.pos(idx+1)]
}

// gsub builds its result in one pass (gopher-lua rebuilds the whole string for every
// replacement) and checks the size before every append.
func (b *allocBudget) gsub(L *glua.LState) int {
	const op = "string.gsub"
	str := L.CheckString(1)
	pattern := L.CheckString(2)
	L.CheckTypes(3, glua.LTString, glua.LTTable, glua.LTFunction)
	repl := L.CheckAny(3)
	limit := L.OptInt(4, -1)
	pat := checkPattern(L, pattern)
	it := newPatIter(L.Context(), pat, str, 0, limit)
	var out strings.Builder
	last, matches := 0, 0
	for {
		caps, err := it.next()
		if err != nil {
			raisePatternError(L, err)
			return 0
		}
		if caps == nil {
			break
		}
		matches++
		start, end := caps.pos(0), caps.pos(1)
		base := out.Len() + start - last
		var piece string
		switch r := repl.(type) {
		case glua.LString:
			piece = b.expandReplacement(L, str, caps, string(r), base)
		case *glua.LTable:
			idx := 0
			if caps.count() > 2 {
				idx = 2
			}
			var v glua.LValue
			if caps.isPosition(idx) {
				v = L.GetTable(r, glua.LNumber(caps.pos(idx)))
			} else {
				v = L.GetField(r, str[caps.pos(idx):caps.pos(idx+1)])
			}
			if glua.LVIsFalse(v) {
				continue
			}
			piece = glua.LVAsString(v)
		case *glua.LFunction:
			L.Push(r)
			nargs := 0
			if caps.count() > 2 {
				for i := 2; i < caps.count(); i += 2 {
					if caps.isPosition(i) {
						L.Push(glua.LNumber(caps.pos(i)))
					} else {
						L.Push(glua.LString(capturedString(L, str, caps, i)))
					}
					nargs++
				}
			} else {
				L.Push(glua.LString(capturedString(L, str, caps, 0)))
				nargs++
			}
			L.Call(nargs, 1)
			v := L.Get(-1)
			L.Pop(1)
			if glua.LVIsFalse(v) {
				continue
			}
			piece = glua.LVAsString(v)
		}
		b.check(L, op, base+len(piece))
		out.WriteString(str[last:start])
		out.WriteString(piece)
		last = end
	}
	if matches == 0 {
		L.SetTop(1)
		L.Push(glua.LNumber(0))
		return 2
	}
	b.charge(L, op, out.Len()+len(str)-last)
	out.WriteString(str[last:])
	L.Push(glua.LString(out.String()))
	L.Push(glua.LNumber(matches))
	return 2
}

// expandReplacement expands %0-%9 in a gsub replacement string. As in gopher-lua, "%%"
// is a percent sign, a trailing '%' is literal and '%' before any other byte keeps both
// bytes. base is the size of the result built so far, for the limit checks.
func (b *allocBudget) expandReplacement(L *glua.LState, src string, caps captures, repl string, base int) string {
	if strings.IndexByte(repl, '%') < 0 {
		return repl
	}
	var sb strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		switch {
		case c != '%':
			sb.WriteByte(c)
		case i+1 < len(repl) && repl[i+1] == '%':
			sb.WriteByte('%')
			i++
		case i == len(repl)-1:
			sb.WriteByte('%')
		default:
			i++
			d := repl[i]
			if d < '0' || d > '9' {
				sb.WriteByte('%')
				sb.WriteByte(d)
				continue
			}
			s := capturedString(L, src, caps, 2*int(d-'0'))
			b.check(L, "string.gsub", base+sb.Len()+len(s))
			sb.WriteString(s)
		}
	}
	return sb.String()
}

func (b *allocBudget) rep(L *glua.LState) int {
	const op = "string.rep"
	s := L.CheckString(1)
	n := L.CheckInt(2)
	if n <= 0 || s == "" {
		L.Push(glua.LString(""))
		return 1
	}
	if n > maxStringBuild/len(s) {
		L.RaiseError("%s result exceeds %d bytes", op, maxStringBuild)
		return 0
	}
	b.charge(L, op, len(s)*n)
	L.Push(glua.LString(strings.Repeat(s, n)))
	return 1
}

// format enforces Lua 5.1's format limits, bounds the result before formatting and
// charges the actual size afterwards.
func (b *allocBudget) format(orig glua.LGFunction) glua.LGFunction {
	return func(L *glua.LState) int {
		const op = "string.format"
		f := L.CheckString(1)
		verbs, err := scanFormat(f)
		if err != nil {
			L.RaiseError("%s", err.Error())
			return 0
		}
		b.check(L, op, formatBound(L, f, verbs))
		n := orig(L)
		if s, ok := L.Get(-1).(glua.LString); ok {
			b.charge(L, op, len(s))
		}
		return n
	}
}

// scanFormat validates a format string with Lua 5.1's limits (at most 5 flags, at most
// two digits each for width and precision) and rejects Go's argument indexes and '*'
// widths, which gopher-lua would otherwise pass to fmt (Lua has neither). It returns
// the conversion byte of every directive (0 when the format ends after the '%').
func scanFormat(f string) ([]byte, error) {
	var verbs []byte
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		i++
		if i < len(f) && f[i] == '%' {
			continue
		}
		flags := 0
		for i < len(f) && strings.IndexByte("-+ #0", f[i]) >= 0 {
			flags++
			i++
		}
		if flags > 5 {
			return nil, errors.New("invalid format (repeated flags)")
		}
		width := 0
		for i < len(f) && f[i] >= '0' && f[i] <= '9' {
			width++
			i++
		}
		if width > maxFormatSpec {
			return nil, errors.New("invalid format (width or precision too long)")
		}
		if i < len(f) && f[i] == '.' {
			i++
			prec := 0
			for i < len(f) && f[i] >= '0' && f[i] <= '9' {
				prec++
				i++
			}
			if prec > maxFormatSpec {
				return nil, errors.New("invalid format (width or precision too long)")
			}
		}
		if i >= len(f) {
			verbs = append(verbs, 0)
			break
		}
		if f[i] == '[' || f[i] == '*' {
			return nil, errors.New("invalid format (argument indexes and '*' are not supported)")
		}
		verbs = append(verbs, f[i])
	}
	return verbs, nil
}

// formatBound is an upper bound of the size of string.format's result. Arguments are
// consumed in order (scanFormat rejects indexes); gopher-lua passes as many arguments as
// the format has '%' signs, and fmt prints unused ones as %!(EXTRA ...).
func formatBound(L *glua.LState, f string, verbs []byte) int {
	passed := min(max(strings.Count(f, "%")-strings.Count(f, "%%"), 0), L.GetTop()-1)
	argLen := func(i int) int {
		if s, ok := L.Get(i + 2).(glua.LString); ok {
			return len(s)
		}
		return 0
	}
	bound := len(f)
	for i, v := range verbs {
		if i >= passed {
			bound += formatArgSlack // %!v(MISSING)
			continue
		}
		mult := 1
		switch v {
		case 'x', 'X': // "% #x": "0x41 " per byte
			mult = 5
		case 'q', 'v': // quoted, %#v: at most "\xff" per byte
			mult = 4
		}
		bound += mult*argLen(i) + formatArgSlack
		if bound > maxStringBuild {
			return bound
		}
	}
	for i := len(verbs); i < passed; i++ {
		bound += argLen(i) + formatArgSlack
	}
	return bound
}

func (b *allocBudget) mapString(op string, fn func(string) string) glua.LGFunction {
	return func(L *glua.LState) int {
		out := fn(L.CheckString(1))
		b.charge(L, op, len(out))
		L.Push(glua.LString(out))
		return 1
	}
}

func reverseBytes(s string) string {
	out := make([]byte, len(s))
	for i := range len(s) {
		out[len(s)-1-i] = s[i]
	}
	return string(out)
}

// tableConcat is table.concat with gopher-lua's index handling and error text; the
// result size is computed before joining.
func (b *allocBudget) tableConcat(L *glua.LState) int {
	const op = "table.concat"
	tbl := L.CheckTable(1)
	sep := L.OptString(2, "")
	n := tbl.Len()
	i := L.OptInt(3, 1)
	j := L.OptInt(4, n)
	if L.GetTop() == 3 && (i > n || i < 1) {
		L.Push(glua.LString(""))
		return 1
	}
	i = max(min(i, n), 1)
	j = min(j, n)
	if i > j {
		L.Push(glua.LString(""))
		return 1
	}
	parts := make([]string, 0, j-i+1)
	size := 0
	for k := i; k <= j; k++ {
		v := tbl.RawGetInt(k)
		if !glua.LVCanConvToString(v) {
			L.RaiseError("invalid value (%s) at index %d in table for concat", v.Type().String(), k)
			return 0
		}
		s := glua.LVAsString(v)
		size += len(s)
		if k < j {
			size += len(sep)
		}
		b.check(L, op, size)
		parts = append(parts, s)
	}
	b.charge(L, op, size)
	L.Push(glua.LString(strings.Join(parts, sep)))
	return 1
}

// osDate bounds os.date's output: each directive expands to at most ~21 bytes.
func (b *allocBudget) osDate(orig glua.LGFunction) glua.LGFunction {
	return func(L *glua.LState) int {
		const op = "os.date"
		if L.GetTop() >= 1 {
			b.check(L, op, 16*len(L.CheckString(1))+64)
		}
		n := orig(L)
		if s, ok := L.Get(-1).(glua.LString); ok && n > 0 {
			b.charge(L, op, len(s))
		}
		return n
	}
}

// concat implements the .. operator for rewritten scripts with the semantics of
// gopher-lua's OP_CONCAT over its operands: right to left, runs of strings and numbers
// are joined at once, other operands go through __concat, and the error text is the
// same. Called with no arguments (the rewritten top-level ...), it returns nothing.
func (b *allocBudget) concat(L *glua.LState) int {
	const op = "string concatenation"
	top := L.GetTop()
	if top == 0 {
		return 0
	}
	rhs := L.Get(top)
	i := top - 1
	for i >= 1 {
		lhs := L.Get(i)
		if !glua.LVCanConvToString(lhs) || !glua.LVCanConvToString(rhs) {
			mm := L.GetMetaField(lhs, "__concat")
			if mm == glua.LNil {
				mm = L.GetMetaField(rhs, "__concat")
			}
			fn, ok := mm.(*glua.LFunction)
			if !ok {
				L.RaiseError("cannot perform concat operation between %v and %v", lhs.Type().String(), rhs.Type().String())
				return 0
			}
			L.Push(fn)
			L.Push(lhs)
			L.Push(rhs)
			L.Call(2, 1)
			rhs = L.Get(-1)
			L.Pop(1)
			i--
			continue
		}
		// Join the run of strings and numbers that ends with rhs.
		first := i
		size := len(glua.LVAsString(rhs))
		for first >= 1 && glua.LVCanConvToString(L.Get(first)) {
			size += len(glua.LVAsString(L.Get(first)))
			if size > maxStringBuild {
				break
			}
			first--
		}
		b.charge(L, op, size)
		var sb strings.Builder
		sb.Grow(size)
		for k := first + 1; k <= i; k++ {
			sb.WriteString(glua.LVAsString(L.Get(k)))
		}
		sb.WriteString(glua.LVAsString(rhs))
		rhs = glua.LString(sb.String())
		i = first
	}
	L.Push(rhs)
	return 1
}
