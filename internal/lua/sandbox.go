package lua

import (
	"fmt"
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

// evalState is a fresh Lua state for one evaluation and the function that implements
// the .. operator of scripts compiled by compile (see rewrite.go).
type evalState struct {
	L      *glua.LState
	concat *glua.LFunction
}

// newState creates a Lua state with the libraries of profile p and the package limits.
func newState(p libProfile) (*evalState, error) {
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
	concat, err := installStringLimits(L)
	if err != nil {
		L.Close()
		return nil, err
	}
	// print never reaches the controller's stdout: scripts process untrusted objects and
	// their output would be an unbounded, unstructured log channel.
	L.SetGlobal("print", L.NewFunction(func(*glua.LState) int { return 0 }))
	return &evalState{L: L, concat: concat}, nil
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
