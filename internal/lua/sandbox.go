package lua

import (
	"fmt"
	"strings"
	"sync"

	glua "github.com/yuin/gopher-lua"
)

// libProfile selects the Lua libraries available to a script.
type libProfile int

const (
	// profileSafe: base (without file/code loading, GC control or printing), table, string,
	// math and a time-only os (time, date, clock, difftime) reachable as the global os and
	// through require("os"). Used for every built-in script, every action and discovery
	// script, and configured health scripts without useOpenLibs.
	profileSafe libProfile = iota
	// profileOpen: the complete gopher-lua standard library (package, io, full os, debug,
	// coroutine, channel, ...). Used only for configured health scripts with
	// useOpenLibs=true, mirroring upstream.
	profileOpen
)

// Resource limits applied to every Lua state.
const (
	callStackSize    = 256       // nested Lua calls ("lua callstack overflow" beyond)
	registrySize     = 1024      // initial value stack slots (grows on demand)
	registryMaxSize  = 256 * 256 // hard cap on value stack slots ("registry overflow")
	registryGrowStep = 1024
	// maxStringBuild caps results of string.rep, the one standard function that can build a
	// huge string from tiny input in a single (uninterruptible) Go call.
	maxStringBuild = 4 << 20
	// maxFormatSpec is Lua 5.1's limit on format width/precision digits.
	maxFormatSpec = 2
)

// safeBaseRemoved lists base functions removed in the safe profile: file and code loading,
// module system, GC control and debug printing.
var safeBaseRemoved = []string{
	"dofile", "loadfile", "load", "loadstring", "require", "module",
	"collectgarbage", "_printregs", "print",
}

// safeOsFuncNames are the os functions exposed in the safe profile.
var safeOsFuncNames = []string{"time", "date", "clock", "difftime"}

// safeOsFuncs extracts the time functions from gopher-lua's os library once, so the safe
// profile never instantiates the rest of the os library.
var safeOsFuncs = sync.OnceValue(func() map[string]glua.LGFunction {
	L := glua.NewState(glua.Options{SkipOpenLibs: true})
	defer L.Close()
	out := map[string]glua.LGFunction{}
	if err := L.CallByParam(glua.P{Fn: L.NewFunction(glua.OpenOs), NRet: 1, Protect: true}, glua.LString(glua.OsLibName)); err != nil {
		return out
	}
	mod, ok := L.Get(-1).(*glua.LTable)
	if !ok {
		return out
	}
	for _, name := range safeOsFuncNames {
		if fn, ok := mod.RawGetString(name).(*glua.LFunction); ok && fn.IsG {
			out[name] = fn.GFunction
		}
	}
	return out
})

// newState creates a Lua state with the libraries of profile p and the package limits.
func newState(p libProfile) (*glua.LState, error) {
	L := glua.NewState(glua.Options{
		SkipOpenLibs:        true,
		CallStackSize:       callStackSize,
		RegistrySize:        registrySize,
		RegistryMaxSize:     registryMaxSize,
		RegistryGrowStep:    registryGrowStep,
		MinimizeStackMemory: true,
	})
	var err error
	switch p {
	case profileOpen:
		err = openLibs(L, []libOpener{
			{glua.LoadLibName, glua.OpenPackage},
			{glua.BaseLibName, glua.OpenBase},
			{glua.TabLibName, glua.OpenTable},
			{glua.IoLibName, glua.OpenIo},
			{glua.OsLibName, glua.OpenOs},
			{glua.StringLibName, glua.OpenString},
			{glua.MathLibName, glua.OpenMath},
			{glua.DebugLibName, glua.OpenDebug},
			{glua.ChannelLibName, glua.OpenChannel},
			{glua.CoroutineLibName, glua.OpenCoroutine},
		})
	default:
		err = openSafe(L)
	}
	if err != nil {
		L.Close()
		return nil, err
	}
	if err := hardenString(L); err != nil {
		L.Close()
		return nil, err
	}
	// print never reaches the controller's stdout: scripts process untrusted objects and
	// their output would be an unbounded, unstructured log channel.
	L.SetGlobal("print", L.NewFunction(func(*glua.LState) int { return 0 }))
	return L, nil
}

type libOpener struct {
	name string
	fn   glua.LGFunction
}

func openLibs(L *glua.LState, libs []libOpener) error {
	for _, lib := range libs {
		if err := L.CallByParam(glua.P{Fn: L.NewFunction(lib.fn), NRet: 0, Protect: true}, glua.LString(lib.name)); err != nil {
			return fmt.Errorf("open lua library %q: %w", lib.name, err)
		}
	}
	return nil
}

func openSafe(L *glua.LState) error {
	if err := openLibs(L, []libOpener{
		{glua.BaseLibName, glua.OpenBase},
		{glua.TabLibName, glua.OpenTable},
		{glua.StringLibName, glua.OpenString},
		{glua.MathLibName, glua.OpenMath},
	}); err != nil {
		return err
	}
	for _, name := range safeBaseRemoved {
		L.SetGlobal(name, glua.LNil)
	}
	osMod := L.SetFuncs(L.NewTable(), safeOsFuncs())
	L.SetGlobal(glua.OsLibName, osMod)

	modules := map[string]glua.LValue{
		glua.OsLibName:     osMod,
		glua.StringLibName: L.GetGlobal(glua.StringLibName),
		glua.TabLibName:    L.GetGlobal(glua.TabLibName),
		glua.MathLibName:   L.GetGlobal(glua.MathLibName),
	}
	// require only resolves the preloaded safe modules; it never touches the filesystem.
	L.SetGlobal("require", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		mod, ok := modules[name]
		if !ok {
			L.RaiseError("module %q not found (only os, string, table and math are available)", name)
			return 0
		}
		L.Push(mod)
		return 1
	}))
	return nil
}

// hardenString replaces string.rep with a size-capped version, rejects over-long format
// widths (as Lua 5.1 does) and removes string.dump. The string metatable indexes the same
// table, so method calls ("x"):rep(n) are covered too.
func hardenString(L *glua.LState) error {
	mod, ok := L.GetGlobal(glua.StringLibName).(*glua.LTable)
	if !ok {
		return nil
	}
	origFormat, ok := mod.RawGetString("format").(*glua.LFunction)
	if !ok || !origFormat.IsG {
		return fmt.Errorf("lua string library has no native format function")
	}
	format := origFormat.GFunction
	mod.RawSetString("rep", L.NewFunction(func(L *glua.LState) int {
		s := L.CheckString(1)
		n := L.CheckInt(2)
		if n <= 0 || s == "" {
			L.Push(glua.LString(""))
			return 1
		}
		if n > maxStringBuild/len(s) {
			L.RaiseError("string.rep result exceeds %d bytes", maxStringBuild)
			return 0
		}
		L.Push(glua.LString(strings.Repeat(s, n)))
		return 1
	}))
	mod.RawSetString("format", L.NewFunction(func(L *glua.LState) int {
		if err := checkFormat(L.CheckString(1)); err != nil {
			L.RaiseError("%s", err.Error())
			return 0
		}
		return format(L)
	}))
	mod.RawSetString("dump", glua.LNil)
	return nil
}

// checkFormat enforces Lua 5.1's string.format limits: at most 5 flag characters and at
// most two digits each for width and precision.
func checkFormat(f string) error {
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
			return fmt.Errorf("invalid format (repeated flags)")
		}
		digits := 0
		for i < len(f) && f[i] >= '0' && f[i] <= '9' {
			digits++
			i++
		}
		if i < len(f) && f[i] == '.' {
			i++
			prec := 0
			for i < len(f) && f[i] >= '0' && f[i] <= '9' {
				prec++
				i++
			}
			if prec > maxFormatSpec {
				return fmt.Errorf("invalid format (width or precision too long)")
			}
		}
		if digits > maxFormatSpec {
			return fmt.Errorf("invalid format (width or precision too long)")
		}
	}
	return nil
}
