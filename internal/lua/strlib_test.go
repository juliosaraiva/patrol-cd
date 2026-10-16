package lua

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	glua "github.com/yuin/gopher-lua"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"
)

// serPrelude serializes any number of values (type and text), so that results can be
// compared between interpreters.
const serPrelude = `local function ser(...)
  local out = {}
  for i = 1, select('#', ...) do
    local v = select(i, ...)
    out[#out + 1] = type(v) .. ":" .. tostring(v)
  end
  return select('#', ...) .. "|" .. table.concat(out, ",")
end
`

// evalStock runs script with stock gopher-lua (all libraries, its own string library and
// OP_CONCAT) and returns its last value as text, or the error message.
func evalStock(script string) (string, string) {
	L := glua.NewState()
	defer L.Close()
	if err := L.DoString(script); err != nil {
		var apiErr *glua.ApiError
		if errors.As(err, &apiErr) && apiErr.Object != nil {
			return "", apiErr.Object.String()
		}
		return "", err.Error()
	}
	return glua.LVAsString(L.Get(-1)), ""
}

// evalOurs runs script with the Patrol runtime (safe profile, rewritten concatenation,
// replaced string functions) and returns its last value as text, or the error message.
func evalOurs(script string) (string, string) {
	proto, err := compile(script)
	if err != nil {
		return "", err.Error()
	}
	st, err := newState(profileSafe)
	if err != nil {
		return "", err.Error()
	}
	defer st.L.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st.L.SetContext(ctx)
	st.L.Push(st.L.NewFunctionFromProto(proto))
	st.L.Push(st.concat)
	if err := st.L.PCall(1, glua.MultRet, nil); err != nil {
		return "", scriptError(ctx, ctx, time.Second, err).Error()
	}
	return glua.LVAsString(st.L.Get(-1)), ""
}

func assertSameAsStock(t require.TestingT, script string) {
	wantOut, wantErr := evalStock(script)
	gotOut, gotErr := evalOurs(script)
	require.Equal(t, wantErr, gotErr, "error for script:\n%s", script)
	require.Equal(t, wantOut, gotOut, "result for script:\n%s", script)
}

func TestStringLibraryMatchesGopherLua(t *testing.T) {
	t.Parallel()
	scripts := map[string]string{
		"find":                `return ser(string.find("hello world", "o w"), ("abc"):find("b", 1, true), ("abc"):find("(b)(c)"), ("abc"):find("()b()"))`,
		"find init":           `return ser(("abcabc"):find("b", -2), ("abcabc"):find("b", 0), ("abcabc"):find("b", 10), ("abc"):find("", 10), ("abc"):find("c", -10))`,
		"find plain past end": `return ser(pcall(string.find, "abc", "b", 10, true))`,
		"find plain flag":     `return ser(("a.c"):find(".", 1, true), ("a.c"):find(".", 1, false), ("a.c"):find(".", 1, nil))`,
		"find errors":         `return ser(pcall(string.find, "abc", "(")) .. ser(pcall(string.find, "abc", "%0")) .. ser(pcall(string.find, "abc", "[a")) .. ser(pcall(string.find, "abc", ")"))`,
		"find back ref":       `return ser(pcall(string.find, "xaa", "((a)%1)"))`,
		"match":               `return ser(string.match("key=value", "(%w+)=(%w+)"), string.match("abc", "b"), string.match("abc", "()b"), select('#', string.match("abc", "x")))`,
		"match offset":        `return ser(("abcabc"):match("a.", 2), ("abcabc"):match("a.", -3), ("abc"):match("a", 10))`,
		"match anchors":       `return ser(("abc"):match("^b"), ("abc"):match("c$"), ("a$c"):match("a$c"), ("abc"):match("^abc$"))`,
		"gmatch":              `local r = {} for k, v in string.gmatch("a=1, b=2, c=3", "(%w+)=(%w+)") do r[#r+1] = k .. v end return table.concat(r, ";")`,
		"gmatch whole":        `local r = {} for w in ("one two  three"):gmatch("%a+") do r[#r+1] = w end return table.concat(r, ",")`,
		"gmatch empty":        `local r = {} for w in ("abc"):gmatch("%w*") do r[#r+1] = "[" .. w .. "]" end return table.concat(r)`,
		"gmatch anchored":     `local r = {} for w in ("aaa"):gmatch("^a") do r[#r+1] = w end return #r`,
		"gmatch positions":    `local r = {} for p in ("abab"):gmatch("()b") do r[#r+1] = p end return table.concat(r, ",")`,
		"gfind alias":         `local r = {} for w in string.gfind("a b", "%a") do r[#r+1] = w end return table.concat(r)`,
		"gsub string":         `return ser(string.gsub("hello world", "o", "0"), ("hello"):gsub("l", "L", 1), ("abc"):gsub("", "-"))`,
		"gsub escapes":        `return ser(("abc"):gsub("(b)", "[%1%0%%%x]"), ("abc"):gsub("b", "%"), ("abc"):gsub("b", "50%"), ("abc"):gsub("(a)(b)", "%2%1"))`,
		"gsub position cap":   `return ser(("abc"):gsub("()b", "%1"), ("abc"):gsub("()", "%1"))`,
		"gsub limits":         `return ser(("aaa"):gsub("a", "b", 0), ("baa"):gsub("a", "b", 0), ("aaa"):gsub("a", "b", -5), ("aaa"):gsub("^a", "b"))`,
		"gsub no match":       `return ser(string.gsub(123, "x", "y"), string.gsub("abc", "x", "y"))`,
		"gsub table":          `local t = {a = "1", b = true, c = false} return ser(("abcd"):gsub("%a", t), ("k=v"):gsub("(%w)=(%w)", {k = "K"}))`,
		"gsub table index":    `local t = setmetatable({}, {__index = function(_, k) return k:upper() end}) return ser(("ab"):gsub("%a", t))`,
		"gsub table number":   `return ser(("ab"):gsub("()", {[1] = "one", [3] = 3}))`,
		"gsub function":       `local n = 0 return ser(("a1b2"):gsub("%d", function(d) n = n + 1 return d * 2 end), n, ("ab"):gsub("(a)(b)", function(x, y) return y .. x end), ("ab"):gsub("a", function() return nil end))`,
		"gsub errors":         `return ser(pcall(string.gsub, "abc", "b", "%2")) .. ser(pcall(string.gsub, "abc", "b", 5)) .. ser(pcall(string.gsub, "abc", "(", "x"))`,
		"gsub empty matches":  `return ser(("abc"):gsub("%w*", "x"), ("a b"):gsub("%s*", "-"))`,
		"rep":                 `return ser(("ab"):rep(3), string.rep("x", 0), string.rep("x", -1), string.rep("", 5))`,
		"format":              `return ser(string.format("%5.2f|%d|%s|%q|%x|%%|%-4s|", 3.14159, 42, "s", "q\"", 255, "ab"))`,
		"format missing":      `return ser(string.format("%s %s", "a"), string.format("%d%%", 5), string.format("plain", 1))`,
		"upper lower reverse": `return ser(("aBc"):upper(), ("aBc"):lower(), ("abc"):reverse(), string.reverse(""))`,
		"table.concat":        `local t = {1, "b", 3.5} return ser(table.concat(t), table.concat(t, ","), table.concat(t, ",", 2), table.concat(t, ",", 2, 3), table.concat(t, ",", 5), table.concat(t, ",", 0, 2), table.concat({}, ","), table.concat(t, ",", 3, 1))`,
		"table.concat error":  `return ser(pcall(table.concat, {1, {}, 3}))`,
		"os.date":             `return ser(os.date("!%Y-%m-%dT%H:%M:%SZ", 0), os.date("!*t", 0).year, os.date("!%c", 86400))`,
		"string metatable":    `local s = "abc" return ser(s:len(), s:sub(2), s:byte(1), #s:rep(2))`,
	}
	for name, body := range scripts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertSameAsStock(t, serPrelude+body)
		})
	}
}

func TestConcatRewriteMatchesGopherLua(t *testing.T) {
	t.Parallel()
	scripts := map[string]string{
		"strings and numbers":    `return "x" .. 1 .. 2.5 .. "y" .. -0 .. 1e20`,
		"left nested":            `local a = ("a" .. "b") .. ("c" .. "d") return a .. (a .. a)`,
		"in expressions":         `local t = {["k" .. 1] = "v" .. 2} return t.k1 .. #("ab" .. "c") .. tostring(("x" .. "y") == "xy")`,
		"closures":               `local function g(a) return function(b) return a .. b end end return g("p")("q") .. g(1)(2)`,
		"loops":                  `local s = "" for i = 1, 5 do s = s .. i end while #s < 8 do s = s .. "w" end repeat s = s .. "r" until #s > 9 return s`,
		"methods":                `local o = {n = "o"} function o:name(x) return self.n .. x end return o:name("!") .. o.name(o, "?")`,
		"metamethods":            `local mt = {__concat = function(a, b) return (type(a) == "table" and "T" or a) .. "+" .. (type(b) == "table" and "T" or b) end} local t = setmetatable({}, mt) return (t .. "x") .. "|" .. ("x" .. t) .. "|" .. ("a" .. "b" .. t .. "c" .. "d")`,
		"metamethod on right":    `local t = setmetatable({}, {__concat = function(a, b) return "R" end}) return 1 .. t`,
		"concat nil":             "local a = 1\nlocal b\nreturn a ..\n b",
		"concat table":           `return "x" .. {}`,
		"concat bool in chain":   "local s = 'a'\nreturn s .. 'b' .. true .. 'c'",
		"pcall concat error":     `return select(2, pcall(function() return "a" .. nil end))`,
		"non function mm":        `local t = setmetatable({}, {__concat = 5}) return "x" .. t`,
		"main varargs":           `return select('#', ...) .. "|" .. #{...} .. "|" .. tostring((...))`,
		"local varargs":          `local a, b = ... return tostring(a) .. tostring(b)`,
		"function varargs":       `local function f(...) return select('#', ...) .. table.concat({...}, ",") end return f(1, 2, 3) .. f()`,
		"vararg in table tail":   `local t = {1, ...} return #t`,
		"return concat count":    `local function f() return "a" .. "b" end return select('#', f()) .. f()`,
		"multiple assignment":    `local a, b = "x" .. "y" return tostring(a) .. tostring(b)`,
		"concat as call arg":     `return string.format("%s|%s", "a" .. "b", "c" .. "d")`,
		"global named arg":       `return tostring(arg)`,
		"error line in function": "local function f(x)\n  local y = x\n  return y .. nil\nend\nreturn f('a')",
		"numeric for":            `local s = "" for i = 1, 3, 1 do s = s .. i end return s`,
		"generic for":            `local s = "" for k, v in pairs({a = 1}) do s = s .. k .. v end return s`,
		"if branches":            `local s = "x" if s .. "" == "x" then s = s .. "1" elseif s then s = s .. "2" else s = s .. "3" end return s`,
		"logical operand":        `local f, a = false, nil return "x" .. (f or "q" .. "r") .. "!" .. (a and "q" .. "r" or "z") .. (not ("a" .. "b") and "1" or "2")`,
		"hidden local unnamed":   `return tostring(_G["(patrol concat)"]) .. tostring(rawget(_G, "(patrol concat)"))`,
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertSameAsStock(t, script)
		})
	}
}

// luaQuote writes s as a Lua string literal using only decimal escapes.
func luaQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ' {
			sb.WriteByte(c)
			continue
		}
		fmt.Fprintf(&sb, "\\%03d", c)
	}
	sb.WriteByte('"')
	return sb.String()
}

// TestPatternFunctionsParityProperty runs the four pattern functions through both
// interpreters on generated patterns, subjects and replacement strings.
func TestPatternFunctionsParityProperty(t *testing.T) {
	t.Parallel()
	patAlphabet := []byte("%[]^$().*+-?abc01fxDSw")
	srcAlphabet := []byte("abc01 ()-x%")
	replAlphabet := []byte("%01ab")
	rapid.Check(t, func(t *rapid.T) {
		p := luaQuote(string(rapid.SliceOfN(rapid.SampledFrom(patAlphabet), 0, 8).Draw(t, "pattern")))
		s := luaQuote(string(rapid.SliceOfN(rapid.SampledFrom(srcAlphabet), 0, 12).Draw(t, "subject")))
		r := luaQuote(string(rapid.SliceOfN(rapid.SampledFrom(replAlphabet), 0, 5).Draw(t, "replacement")))
		init := rapid.IntRange(-4, 14).Draw(t, "init")
		limit := rapid.IntRange(-1, 3).Draw(t, "limit")
		script := serPrelude + fmt.Sprintf(`local S, P, R = %s, %s, %s
local out = {}
out[1] = ser(pcall(string.find, S, P, %d))
out[2] = ser(pcall(string.match, S, P, %d))
out[3] = ser(pcall(string.gsub, S, P, R, %d))
out[4] = ser(pcall(string.gsub, S, P, R))
out[5] = ser(pcall(function() local r = {} for a, b in string.gmatch(S, P) do r[#r+1] = ser(a, b) end return table.concat(r, ";") end))
out[6] = ser(pcall(string.find, S, P, %d, true))
return table.concat(out, "\n")`, p, s, r, init, init, limit, init)
		wantOut, wantErr := evalStock(script)
		gotOut, gotErr := evalOurs(script)
		if wantErr != gotErr || wantOut != gotOut {
			t.Fatalf("script:\n%s\nstock: %q %q\nours:  %q %q", script, wantOut, wantErr, gotOut, gotErr)
		}
	})
}

func otelCollector(statusReplicas string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "opentelemetry.io/v1beta1",
		"kind":       "OpenTelemetryCollector",
		"metadata":   map[string]any{"name": "collector", "namespace": "observability"},
		"status":     map[string]any{"scale": map[string]any{"statusReplicas": statusReplicas}},
	}}
}

// TestBuiltinDeadlineOnHostileInput is the regression test for built-in scripts whose
// pattern matching takes quadratic time on crafted status values: the default deadline
// now bounds them.
func TestBuiltinDeadlineOnHostileInput(t *testing.T) {
	t.Parallel()
	rt := New(nil) // default 1s deadline, as in production

	t.Run("OpenTelemetryCollector statusReplicas", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		_, err := rt.GetResourceHealth(otelCollector(strings.Repeat("1", 60_000)))
		elapsed := time.Since(start)
		require.ErrorIs(t, err, ErrTimeout)
		assert.Contains(t, err.Error(), "exceeded execution timeout of 1s")
		assert.Less(t, elapsed, 3*time.Second, "deadline must bound the evaluation")
	})

	t.Run("OpenTelemetryCollector still healthy", func(t *testing.T) {
		t.Parallel()
		hs, err := rt.GetResourceHealth(otelCollector("2/2"))
		require.NoError(t, err)
		assert.Equal(t, "Healthy", string(hs.Status))
	})

	t.Run("AdvancedCronJob lastScheduleTime", func(t *testing.T) {
		t.Parallel()
		cj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps.kruise.io/v1alpha1",
			"kind":       "AdvancedCronJob",
			"metadata":   map[string]any{"name": "cj", "namespace": "ns"},
			"status":     map[string]any{"lastScheduleTime": strings.Repeat("1", 60_000)},
		}}
		start := time.Now()
		_, err := rt.GetResourceHealth(cj)
		elapsed := time.Since(start)
		require.ErrorIs(t, err, ErrTimeout)
		assert.Less(t, elapsed, 3*time.Second, "deadline must bound the evaluation")
	})

	t.Run("caller cancellation stops matching", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := rt.GetResourceHealthContext(ctx, otelCollector(strings.Repeat("1", 60_000)))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(start), 2*time.Second)
	})
}

// TestStringLimits is the regression test for allocation amplification: every function
// that can build a string much larger than its input fails fast once it would exceed the
// per-string cap or the per-evaluation budget, and never times out doing so.
func TestStringLimits(t *testing.T) {
	t.Parallel()
	rt := New(nil, WithTimeout(30*time.Second))
	failing := map[string]struct{ script, msg string }{
		"gsub amplification":      {`local s = string.rep("x", 4096) string.gsub(s, ".", s)`, "string.gsub result exceeds 4194304 bytes"},
		"gsub on a large input":   {`local s = string.rep("x", 4194304) string.gsub(s, ".", s)`, "string.gsub result exceeds 4194304 bytes"},
		"gsub capture expansion":  {`local s = string.rep("x", 65536) string.gsub(s, ".+", string.rep("%0", 100))`, "string.gsub result exceeds 4194304 bytes"},
		"gsub function results":   {`local big = string.rep("y", 1048576) string.gsub(string.rep("x", 10), ".", function() return big end)`, "string.gsub result exceeds 4194304 bytes"},
		"concat doubling":         {`local s = "x" for i = 1, 40 do s = s .. s end`, "string concatenation result exceeds 4194304 bytes"},
		"concat chain":            {`local s = string.rep("x", 1048576) local t = s .. s .. s .. s .. s`, "string concatenation result exceeds 4194304 bytes"},
		"table.concat":            {`local s = string.rep("x", 1048576) local t = {} for i = 1, 8 do t[i] = s end table.concat(t)`, "table.concat result exceeds 4194304 bytes"},
		"table.concat separator":  {`local t = {} for i = 1, 1000 do t[i] = "" end table.concat(t, string.rep("x", 1048576))`, "table.concat result exceeds 4194304 bytes"},
		"format arguments":        {`local s = string.rep("x", 1048576) string.format("%s%s%s%s%s", s, s, s, s, s)`, "string.format result exceeds 4194304 bytes"},
		"format hex":              {`string.format("%x", string.rep("x", 1048576))`, "string.format result exceeds 4194304 bytes"},
		"format argument index":   {`string.format("%[1]s%[1]s", "x")`, "argument indexes"},
		"format star width":       {`string.format("%*d", 5, 1)`, "argument indexes"},
		"os.date format":          {`os.date(string.rep("%c", 1048576))`, "os.date result exceeds 4194304 bytes"},
		"rep":                     {`string.rep("x", 4194305)`, "string.rep result exceeds 4194304 bytes"},
		"upper of a large string": {`local s = string.rep("\255", 1468007) s:upper()`, "string.upper result exceeds 4194304 bytes"},
		"budget across calls":     {`local t = {} for i = 1, 20 do t[i] = string.rep("x", 4194304) end`, "string.rep exceeds the string allocation limit of 67108864 bytes per evaluation"},
		"budget across concat":    {`local s = string.rep("x", 2097152) local t = {} for i = 1, 40 do t[i] = s .. i end`, "string concatenation exceeds the string allocation limit"},
	}
	for name, tc := range failing {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			_, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), tc.script, false)
			require.Error(t, err)
			var se *ScriptError
			require.ErrorAs(t, err, &se)
			assert.False(t, se.Timeout, "must fail fast, not time out: %v", err)
			assert.Contains(t, err.Error(), tc.msg)
			t.Logf("failed after %v", time.Since(start))
		})
	}

	ok := map[string]string{
		"string of 4 MiB":            `local s = string.rep("x", 4194304) return {status = "Healthy", message = tostring(#s)}`,
		"gsub on 1 MiB":              `local s = string.rep("ab", 524288):gsub("a", "c") return {status = "Healthy", message = tostring(#s)}`,
		"concat in a loop":           `local s = "" for i = 1, 10000 do s = s .. "x" end return {status = "Healthy", message = tostring(#s)}`,
		"table.concat of 100k":       `local t = {} for i = 1, 100000 do t[i] = i end return {status = "Healthy", message = tostring(#table.concat(t, ","))}`,
		"format of a 1 MiB argument": `return {status = "Healthy", message = tostring(#string.format("[%s]", string.rep("x", 1048576)))}`,
	}
	want := map[string]string{
		"string of 4 MiB":            "4194304",
		"gsub on 1 MiB":              "1048576",
		"concat in a loop":           "10000",
		"table.concat of 100k":       "588894",
		"format of a 1 MiB argument": "1048578",
	}
	for name, script := range ok {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := rt.ExecuteHealth(context.Background(), obj(t, rolloutYAML), script, false)
			require.NoError(t, err)
			assert.Equal(t, want[name], got.Message)
		})
	}
}

// TestResultLimits is the regression test for unbounded result conversion: oversized
// results fail with ErrResultTooLarge and conversion honors the deadline.
func TestResultLimits(t *testing.T) {
	t.Parallel()
	rt := New(nil, WithTimeout(30*time.Second))
	cases := map[string]string{
		"nested values":    `local inner = {} for i = 1, 1100 do inner[i] = {i} end local t = {} for i = 1, 500 do t[i] = inner end return t`,
		"too many bytes":   `local s = string.rep("x", 1048576) local t = {} for i = 1, 17 do t[i] = s end return t`,
		"shared subtables": `local inner = {1, 2, 3, 4, 5, 6, 7, 8, 9, 10} local t = {} for i = 1, 100000 do t[i] = inner end return t`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := rt.ExecuteActionScript(context.Background(), obj(t, rolloutYAML), script, nil)
			require.ErrorIs(t, err, ErrResultTooLarge)
		})
	}

	t.Run("large legitimate result", func(t *testing.T) {
		t.Parallel()
		got, err := rt.exec(context.Background(), obj(t, rolloutYAML), `local t = {} for i = 1, 100000 do t[i] = "item" .. i end return t`, profileSafe, nil)
		require.NoError(t, err)
		assert.Greater(t, len(got.json), 1_000_000)
	})

	t.Run("flat array over the value limit", func(t *testing.T) {
		t.Parallel()
		L := glua.NewState(glua.Options{SkipOpenLibs: true})
		defer L.Close()
		tbl := L.CreateTable(maxResultNodes+1, 0)
		for i := range maxResultNodes + 1 {
			tbl.Append(glua.LNumber(i))
		}
		_, err := toJSON(tbl)
		require.ErrorIs(t, err, ErrResultTooLarge)
		tbl.Remove(-1)
		tbl.Remove(-1)
		_, err = toJSON(tbl)
		require.NoError(t, err, "exactly at the limit (the table itself is one value)")
	})

	t.Run("conversion stops at the deadline", func(t *testing.T) {
		t.Parallel()
		L := glua.NewState(glua.Options{SkipOpenLibs: true})
		defer L.Close()
		tbl := L.CreateTable(100_000, 0)
		for i := range 100_000 {
			tbl.Append(glua.LNumber(i))
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := toJSONContext(ctx, tbl)
		require.ErrorIs(t, err, context.Canceled)
	})
}
