package lua

import (
	"context"
	"fmt"
)

// This file implements Lua pattern matching for string.find, string.match,
// string.gmatch and string.gsub.
//
// gopher-lua's own matcher (package pm) cannot be interrupted: one call runs to
// completion however long it takes, and backtracking patterns such as "(%d+)/(%d+)" take
// quadratic time on long subjects. A built-in health script fed a crafted object could
// therefore hold a controller worker far past its deadline. This matcher has the same
// observable semantics as gopher-lua v1.1 (the version upstream Argo CD uses), quirks
// included, and checks the evaluation context while it runs:
//
//   - '^' anchors only at the start of a top-level pattern; '$' anchors only as the last
//     byte of a top-level pattern;
//   - a trailing '%' yields a class that never matches, "%b" with missing delimiters never
//     matches, and %f is a literal 'f' (no frontier patterns);
//   - in a set, a range whose ends are not plain bytes never matches, and a dangling '-'
//     is literal;
//   - ')' at top level, '%0', unfinished captures and an unterminated set are errors with
//     pm's message texts and positions;
//   - matching recurses at most 1,000,000 levels ("pattern/input too complex").
//
// The parity tests compare this matcher with pm on generated patterns and subjects.

// patEOS marks the end of the pattern in the scanner and in error positions.
const patEOS = -1

const (
	// maxPatternDepth is pm's recursion limit, kept for identical errors.
	maxPatternDepth = 1_000_000
	// patternCheckEvery is the number of matcher steps between context checks.
	patternCheckEvery = 1024
)

// patternError carries pm-compatible error texts.
type patternError struct {
	msg string
	pos int
	// positional errors print " at <pos>" or " at EOS".
	positional bool
	// goRuntime marks the one case where pm hits a Go runtime panic (a back reference to
	// an enclosing capture); gopher-lua reports the panic text without a position.
	goRuntime bool
}

func (e *patternError) Error() string {
	switch {
	case !e.positional:
		return e.msg
	case e.pos == patEOS:
		return e.msg + " at EOS"
	default:
		return fmt.Sprintf("%s at %d", e.msg, e.pos)
	}
}

// patScanner walks a pattern byte by byte. pos is the index of the last consumed byte
// (0 before the first one) or patEOS once a read went past the end; error messages
// report it, so its quirks are kept.
type patScanner struct {
	src     string
	pos     int
	started bool
}

func (s *patScanner) nextPos() int {
	if s.pos == patEOS || s.pos >= len(s.src)-1 {
		return patEOS
	}
	if !s.started {
		return 0
	}
	return s.pos + 1
}

func (s *patScanner) next() int {
	if !s.started {
		s.started = true
		if len(s.src) == 0 {
			s.pos = patEOS
		}
	} else {
		s.pos = s.nextPos()
	}
	if s.pos == patEOS {
		return patEOS
	}
	return int(s.src[s.pos])
}

// peek returns the next byte without consuming it. Once the scanner has run past the end
// it stays there.
func (s *patScanner) peek() int {
	wasEOS := s.pos == patEOS
	ch := s.next()
	if wasEOS {
		return ch
	}
	if s.pos == patEOS {
		s.pos = len(s.src) - 1
		return ch
	}
	s.pos--
	if s.pos < 0 {
		s.pos = 0
		s.started = false
	}
	return ch
}

func (s *patScanner) fail(msg string) error {
	return &patternError{msg: msg, pos: s.pos, positional: true}
}

// byteSet is a 256-bit membership table for one character class.
type byteSet [4]uint64

func (b *byteSet) add(c int) { b[c>>6] |= 1 << (uint(c) & 63) }

func (b *byteSet) has(c byte) bool { return b[c>>6]&(1<<(uint(c)&63)) != 0 }

type classKind uint8

const (
	classAny    classKind = iota // '.'
	classByte                    // a literal byte
	classEscape                  // %x
	classSet                     // [...]
	classRange                   // x-y inside a set
)

type patClass struct {
	kind    classKind
	ch      int // classByte: the byte; classEscape: the byte after '%' (may be patEOS)
	negated bool
	members []*patClass
	lo, hi  *patClass
}

func (c *patClass) matches(ch int) bool {
	switch c.kind {
	case classAny:
		return true
	case classByte:
		return c.ch == ch
	case classEscape:
		return escapeMatches(c.ch, ch)
	case classSet:
		for _, m := range c.members {
			if m.matches(ch) {
				return !c.negated
			}
		}
		return c.negated
	case classRange:
		if c.lo.kind != classByte || c.hi.kind != classByte {
			return false
		}
		return c.lo.ch <= ch && ch <= c.hi.ch
	}
	return false
}

// escapeMatches implements %a, %d, ... (upper case negates) and the literal escapes.
func escapeMatches(class, ch int) bool {
	var in bool
	switch class {
	case 'a', 'A':
		in = 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z'
	case 'c', 'C':
		in = ch <= 0x1f || ch == 0x7f
	case 'd', 'D':
		in = '0' <= ch && ch <= '9'
	case 'l', 'L':
		in = 'a' <= ch && ch <= 'z'
	case 'p', 'P':
		in = 0x21 <= ch && ch <= 0x2f || 0x3a <= ch && ch <= 0x40 || 0x5b <= ch && ch <= 0x60 || 0x7b <= ch && ch <= 0x7e
	case 's', 'S':
		in = ch == ' ' || ch == '\f' || ch == '\n' || ch == '\r' || ch == '\t' || ch == '\v'
	case 'u', 'U':
		in = 'A' <= ch && ch <= 'Z'
	case 'w', 'W':
		in = '0' <= ch && ch <= '9' || 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z'
	case 'x', 'X':
		in = '0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f' || 'A' <= ch && ch <= 'F'
	case 'z', 'Z':
		in = ch == 0
	default:
		return ch == class
	}
	if 'A' <= class && class <= 'Z' {
		return !in
	}
	return in
}

func (c *patClass) table() byteSet {
	var b byteSet
	for ch := 0; ch < 256; ch++ {
		if c.matches(ch) {
			b.add(ch)
		}
	}
	return b
}

type nodeKind uint8

const (
	nodeSingle nodeKind = iota
	nodeRepeat
	nodePosCapture
	nodeCapture
	nodeBackref
	nodeBrace
)

type patNode struct {
	kind        nodeKind
	class       *patClass
	op          int // nodeRepeat: '*', '+', '-' or '?'
	sub         []patNode
	n           int // nodeBackref: capture number 1..9
	open, close int // nodeBrace: delimiters, possibly patEOS
}

func parseClass(s *patScanner, allowSet bool) (*patClass, error) {
	ch := s.next()
	switch ch {
	case '%':
		return &patClass{kind: classEscape, ch: s.next()}, nil
	case '.':
		if allowSet {
			return &patClass{kind: classAny}, nil
		}
	case '[':
		if allowSet {
			return parseSet(s)
		}
	case patEOS:
		return nil, s.fail("unexpected EOS")
	}
	return &patClass{kind: classByte, ch: ch}, nil
}

func parseSet(s *patScanner) (*patClass, error) {
	set := &patClass{kind: classSet}
	if s.peek() == '^' {
		set.negated = true
		s.next()
	}
	inRange := false
	for {
		ch := s.peek()
		switch {
		case ch == patEOS:
			return nil, s.fail("unexpected EOS")
		case ch == ']' && len(set.members) > 0:
			s.next()
			if inRange {
				set.members = append(set.members, &patClass{kind: classByte, ch: '-'})
			}
			return set, nil
		case ch == '-' && len(set.members) > 0:
			s.next()
			inRange = true
			continue
		}
		m, err := parseClass(s, false)
		if err != nil {
			return nil, err
		}
		set.members = append(set.members, m)
		if inRange {
			n := len(set.members)
			set.members = append(set.members[:n-2], &patClass{kind: classRange, lo: set.members[n-2], hi: set.members[n-1]})
			inRange = false
		}
	}
}

func parseSeq(s *patScanner, top bool) (seq []patNode, mustHead, mustTail bool, err error) {
	if top && s.peek() == '^' {
		s.next()
		mustHead = true
	}
	for {
		ch := s.peek()
		switch ch {
		case '%':
			saved := *s
			s.next()
			switch c := s.peek(); {
			case c == '0':
				return nil, false, false, s.fail("invalid capture index")
			case '1' <= c && c <= '9':
				seq = append(seq, patNode{kind: nodeBackref, n: s.next() - '0'})
			case c == 'b':
				s.next()
				open := s.next()
				closing := s.next()
				seq = append(seq, patNode{kind: nodeBrace, open: open, close: closing})
			default:
				*s = saved
				cl, err := parseClass(s, true)
				if err != nil {
					return nil, false, false, err
				}
				seq = append(seq, patNode{kind: nodeSingle, class: cl})
			}
		case '.', '[', ']':
			cl, err := parseClass(s, true)
			if err != nil {
				return nil, false, false, err
			}
			seq = append(seq, patNode{kind: nodeSingle, class: cl})
		case ')':
			if top {
				return nil, false, false, s.fail("invalid ')'")
			}
			return seq, mustHead, mustTail, nil
		case '(':
			s.next()
			if s.peek() == ')' {
				s.next()
				seq = append(seq, patNode{kind: nodePosCapture})
				continue
			}
			sub, _, _, err := parseSeq(s, false)
			if err != nil {
				return nil, false, false, err
			}
			if s.peek() != ')' {
				return nil, false, false, s.fail("unfinished capture")
			}
			s.next()
			seq = append(seq, patNode{kind: nodeCapture, sub: sub})
		case '*', '+', '-', '?':
			s.next()
			if n := len(seq); n > 0 && seq[n-1].kind == nodeSingle {
				seq[n-1] = patNode{kind: nodeRepeat, op: ch, class: seq[n-1].class}
				continue
			}
			seq = append(seq, patNode{kind: nodeSingle, class: &patClass{kind: classByte, ch: ch}})
		case '$':
			if np := s.nextPos(); top && (np == len(s.src)-1 || np == patEOS) {
				mustTail = true
			} else {
				seq = append(seq, patNode{kind: nodeSingle, class: &patClass{kind: classByte, ch: ch}})
			}
			s.next()
		case patEOS:
			s.next()
			return seq, mustHead, mustTail, nil
		default:
			s.next()
			seq = append(seq, patNode{kind: nodeSingle, class: &patClass{kind: classByte, ch: ch}})
		}
	}
}

type patOp uint8

const (
	opByte      patOp = iota // consume one byte of the class
	opMatch                  // success
	opTailMatch              // success only at the end of the subject
	opJump                   // pc = a
	opSplit                  // try a; on failure continue at b
	opSave                   // record position in capture slot a
	opPosSave                // record a position capture in slots a, a+1
	opBrace                  // %b: a = open, b = close
	opBackref                // %1..%9: a = capture number
)

type patInst struct {
	op   patOp
	set  *byteSet
	a, b int
}

// luaPattern is a compiled pattern. It is immutable and may be shared.
type luaPattern struct {
	code     []patInst
	anchored bool
}

func compileLuaPattern(p string) (*luaPattern, error) {
	s := &patScanner{src: p}
	seq, mustHead, mustTail, err := parseSeq(s, true)
	if err != nil {
		return nil, err
	}
	c := patCompiler{code: []patInst{{op: opSave, a: 0}}, capture: 2}
	c.seq(seq)
	if mustTail {
		c.code = append(c.code, patInst{op: opSave, a: 1}, patInst{op: opTailMatch})
	}
	c.code = append(c.code, patInst{op: opSave, a: 1}, patInst{op: opMatch})
	return &luaPattern{code: c.code, anchored: mustHead}, nil
}

type patCompiler struct {
	code    []patInst
	capture int
}

func (c *patCompiler) seq(nodes []patNode) {
	for i := range nodes {
		c.node(&nodes[i])
	}
}

func (c *patCompiler) node(n *patNode) {
	switch n.kind {
	case nodeSingle:
		set := n.class.table()
		c.code = append(c.code, patInst{op: opByte, set: &set})
	case nodeRepeat:
		set := n.class.table()
		at := len(c.code)
		switch n.op {
		case '*':
			c.code = append(c.code, patInst{op: opSplit, a: at + 1, b: at + 3}, patInst{op: opByte, set: &set}, patInst{op: opJump, a: at})
		case '+':
			c.code = append(c.code, patInst{op: opByte, set: &set}, patInst{op: opSplit, a: at, b: at + 2})
		case '-':
			c.code = append(c.code, patInst{op: opSplit, a: at + 3, b: at + 1}, patInst{op: opByte, set: &set}, patInst{op: opJump, a: at})
		case '?':
			c.code = append(c.code, patInst{op: opSplit, a: at + 1, b: at + 2}, patInst{op: opByte, set: &set})
		}
	case nodePosCapture:
		c.code = append(c.code, patInst{op: opPosSave, a: c.capture})
		c.capture += 2
	case nodeCapture:
		open, closing := c.capture, c.capture+1
		c.capture += 2
		c.code = append(c.code, patInst{op: opSave, a: open})
		c.seq(n.sub)
		c.code = append(c.code, patInst{op: opSave, a: closing})
	case nodeBrace:
		c.code = append(c.code, patInst{op: opBrace, a: n.open, b: n.close})
	case nodeBackref:
		c.code = append(c.code, patInst{op: opBackref, a: n.n})
	}
}

// Capture slots hold position<<1, with the low bit set for position captures. Slots 0 and
// 1 delimit the whole match; capture k uses slots 2k and 2k+1.
type captures []int

func (c captures) count() int            { return len(c) }
func (c captures) pos(i int) int         { return c[i] >> 1 }
func (c captures) isPosition(i int) bool { return c[i]&1 == 1 }

// backtrack frames: a split resumes at (pc, sp); a save restores slot to old.
type patFrame struct {
	split   bool
	pc, sp  int
	slot    int
	oldSlot int
}

// patMatcher runs a compiled pattern against one subject. It is not safe for concurrent
// use.
type patMatcher struct {
	pat   *luaPattern
	src   string
	ctx   context.Context
	fuel  int
	caps  captures
	stack []patFrame
}

func newPatMatcher(ctx context.Context, pat *luaPattern, src string) *patMatcher {
	return &patMatcher{pat: pat, src: src, ctx: ctx, fuel: patternCheckEvery}
}

// spend accounts n units of work and checks the context when the fuel runs out.
func (m *patMatcher) spend(n int) error {
	m.fuel -= n
	if m.fuel > 0 {
		return nil
	}
	m.fuel = patternCheckEvery
	if m.ctx == nil {
		return nil
	}
	select {
	case <-m.ctx.Done():
		return m.ctx.Err()
	default:
		return nil
	}
}

func (m *patMatcher) setSlot(slot, v int) int {
	for slot >= len(m.caps) {
		m.caps = append(m.caps, 0)
	}
	old := m.caps[slot]
	m.caps[slot] = v
	return old
}

func (m *patMatcher) push(f patFrame) error {
	if len(m.stack)+2 > maxPatternDepth {
		return &patternError{msg: "pattern/input too complex"}
	}
	m.stack = append(m.stack, f)
	return nil
}

// matchAt tries the pattern at byte offset start. On success it returns the end offset;
// the captures are in m.caps until the next call.
func (m *patMatcher) matchAt(start int) (bool, int, error) {
	m.caps = m.caps[:0]
	m.stack = m.stack[:0]
	code, src := m.pat.code, m.src
	pc, sp := 0, start
	for {
		if err := m.spend(1); err != nil {
			return false, 0, err
		}
		in := &code[pc]
		ok := true
		switch in.op {
		case opByte:
			if sp >= len(src) || !in.set.has(src[sp]) {
				ok = false
				break
			}
			pc++
			sp++
		case opMatch:
			return true, sp, nil
		case opTailMatch:
			if sp >= len(src) {
				return true, sp, nil
			}
			ok = false
		case opJump:
			pc = in.a
		case opSplit:
			if err := m.push(patFrame{split: true, pc: in.b, sp: sp}); err != nil {
				return false, 0, err
			}
			pc = in.a
		case opSave:
			old := m.setSlot(in.a, sp<<1)
			if err := m.push(patFrame{slot: in.a, oldSlot: old}); err != nil {
				return false, 0, err
			}
			pc++
		case opPosSave:
			v := (sp+1)<<1 | 1
			m.setSlot(in.a+1, v)
			m.caps[in.a] = v
			pc++
		case opBrace:
			end, matched := matchBrace(src, sp, in.a, in.b)
			if err := m.spend(end - sp); err != nil {
				return false, 0, err
			}
			if !matched {
				ok = false
				break
			}
			sp = end
			pc++
		case opBackref:
			idx := in.a * 2
			if idx >= len(m.caps)-1 {
				return false, 0, &patternError{msg: "invalid capture index"}
			}
			from, to := m.caps.pos(idx), m.caps.pos(idx+1)
			if from > to {
				// A back reference to an enclosing capture reads a close slot that is
				// still zero; pm panics slicing the subject and gopher-lua reports the
				// runtime error.
				return false, 0, &patternError{
					msg:       fmt.Sprintf("runtime error: slice bounds out of range [%d:%d]", from, to),
					goRuntime: true,
				}
			}
			ref := src[from:to]
			if err := m.spend(len(ref)); err != nil {
				return false, 0, err
			}
			if len(src)-sp < len(ref) || src[sp:sp+len(ref)] != ref {
				ok = false
				break
			}
			sp += len(ref)
			pc++
		}
		if ok {
			continue
		}
		// Backtrack: undo saves until a split offers an alternative.
		for {
			n := len(m.stack)
			if n == 0 {
				return false, sp, nil
			}
			f := m.stack[n-1]
			m.stack = m.stack[:n-1]
			if f.split {
				pc, sp = f.pc, f.sp
				break
			}
			m.caps[f.slot] = f.oldSlot
		}
	}
}

// matchBrace matches %b<open><close> at sp and returns the offset after the balanced
// run.
func matchBrace(src string, sp, open, closing int) (int, bool) {
	if sp >= len(src) || int(src[sp]) != open {
		return sp, false
	}
	depth := 1
	for i := sp + 1; i < len(src); i++ {
		c := int(src[i])
		if c == closing {
			depth--
		}
		if depth == 0 {
			return i + 1, true
		}
		if c == open {
			depth++
		}
	}
	return len(src), false
}

// patIter enumerates successive matches the way pm.Find does: every start position from
// init on is tried, the scan resumes after each match (at least one byte further), an
// anchored pattern is tried once, and the scan stops once limit matches were found
// (limit < 0: no limit). gopher-lua checks the limit only after the first attempt, so a
// limit of 0 still returns every match when the first attempt succeeds; that is kept.
type patIter struct {
	m     *patMatcher
	sp    int
	found int
	limit int
	done  bool
}

func newPatIter(ctx context.Context, pat *luaPattern, src string, init, limit int) *patIter {
	return &patIter{m: newPatMatcher(ctx, pat, src), sp: init, limit: limit}
}

// next returns the captures of the next match, or nil when there are no more.
func (it *patIter) next() (captures, error) {
	for !it.done && it.sp <= len(it.m.src) {
		ok, end, err := it.m.matchAt(it.sp)
		if err != nil {
			it.done = true
			return nil, err
		}
		it.sp++
		if ok {
			if it.sp < end {
				it.sp = end
			}
			it.found++
		}
		if it.found == it.limit || it.m.pat.anchored {
			it.done = true
		}
		if ok {
			return it.m.caps, nil
		}
	}
	it.done = true
	return nil, nil
}
