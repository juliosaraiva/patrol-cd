// Package lua runs resource health checks and resource actions written in Lua inside a
// sandboxed gopher-lua runtime.
//
// Scripts come from two places, consulted in the same order as Argo CD v3.5.5
// (util/lua): resource customizations configured in settings (see
// [ParseResourceOverrides]) and the built-in customizations embedded from
// customizations/ (Apache-2.0 data copied from upstream resource_customizations/).
//
// Every evaluation gets a fresh Lua state with a restricted library set, an execution
// deadline, a bounded call stack and a bounded value stack, bounded string building and a
// bounded result. The deadline is enforced twice: the VM and the pattern matcher check
// the context while they run, and a watchdog abandons an evaluation stuck in a Go call
// that cannot be interrupted (see exec). Compiled scripts are cached by content hash. A
// Runtime is immutable and safe for concurrent use; build a new one when settings change.
package lua

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/glob"
	lru "github.com/hashicorp/golang-lru/v2"
	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DefaultTimeout is the execution deadline of one script evaluation (upstream: 1s).
const DefaultTimeout = time.Second

// chunkName is the source name reported in Lua error positions ("<string>:3: ...").
const chunkName = "<string>"

// Sentinel errors.
var (
	// ErrTimeout is matched by errors of scripts that exceeded the execution deadline.
	ErrTimeout = errors.New("lua script execution timed out")
	// ErrActionNotFound means no configured or built-in script exists for an action.
	ErrActionNotFound = errors.New("resource action not found")
	// ErrNoDiscovery means ListActions was called for a kind without action discovery.
	ErrNoDiscovery = errors.New("no action discovery script")
	// ErrInvalidOutput means a script returned a value of the wrong shape.
	ErrInvalidOutput = errors.New("invalid lua script output")
	// ErrResultTooLarge means a script returned a table with more than maxResultNodes
	// values or maxResultBytes bytes of strings.
	ErrResultTooLarge = errors.New("lua script result too large")
	// ErrOverloaded means evaluations are refused because too many abandoned ones (see
	// exec) are still running in the process.
	ErrOverloaded = errors.New("too many lua evaluations still running past their deadline")
)

// Watchdog settings.
const (
	// minStallGrace and maxStallGrace bound how long exec waits after the deadline for
	// the evaluation to stop on its own before abandoning it (half the timeout in
	// between). The VM and the matcher stop within microseconds; the grace only absorbs
	// scheduling delays (CPU throttling, GC) so that slow-but-interruptible evaluations
	// are not counted as stalled.
	minStallGrace = 100 * time.Millisecond
	maxStallGrace = time.Second
	// maxStalled is the number of abandoned, still running evaluations at which new
	// evaluations are refused with ErrOverloaded.
	maxStalled = 16
)

func stallGrace(timeout time.Duration) time.Duration {
	return min(max(timeout/2, minStallGrace), maxStallGrace)
}

// Process-wide watchdog counters (see RegisterMetrics).
var (
	stalledEvaluations   atomic.Int64 // abandoned and still running
	abandonedEvaluations atomic.Int64 // abandoned since process start
)

// ScriptError is a failure reported by the Lua runtime (syntax error, runtime error,
// timeout). Error returns the Lua message without a stack trace, exactly as upstream, so
// that messages shown to users match.
type ScriptError struct {
	// Message is the Lua error message, e.g. "<string>:4: attempt to index a nil value".
	Message string
	// Timeout is set when the script exceeded its deadline.
	Timeout bool

	cause  error // *glua.ApiError
	ctxErr error // context error when the evaluation was cut short
}

func (e *ScriptError) Error() string { return e.Message }

// Unwrap exposes the underlying *lua.ApiError and, if any, the context error.
func (e *ScriptError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	if e.ctxErr != nil {
		errs = append(errs, e.ctxErr)
	}
	return errs
}

// Is reports ErrTimeout for timed-out evaluations.
func (e *ScriptError) Is(target error) bool { return target == ErrTimeout && e.Timeout }

// Runtime evaluates health and action scripts. The zero value is not usable; call New.
type Runtime struct {
	overrides map[string]ResourceOverride
	wildcards []wildcardOverride
	builtins  *builtinFS
	timeout   time.Duration
	logger    *slog.Logger
	// stateHook runs on every new Lua state before the script (tests).
	stateHook func(*glua.LState)
}

type wildcardOverride struct {
	key     string
	pattern *glob.Pattern
}

// Option configures a Runtime.
type Option func(*Runtime)

// WithTimeout sets the per-evaluation execution deadline (default [DefaultTimeout]).
// Non-positive values are ignored.
func WithTimeout(d time.Duration) Option {
	return func(r *Runtime) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// WithLogger sets the logger used for configuration warnings.
func WithLogger(l *slog.Logger) Option {
	return func(r *Runtime) {
		if l != nil {
			r.logger = l
		}
	}
}

// withStateHook registers a function that prepares every new Lua state (tests).
func withStateHook(fn func(*glua.LState)) Option {
	return func(r *Runtime) { r.stateHook = fn }
}

// withBuiltinFS replaces the embedded customizations (tests).
func withBuiltinFS(fsys fs.FS) Option {
	return func(r *Runtime) { r.builtins = newBuiltinFS(fsys) }
}

// New returns a Runtime for the given configured customizations (keyed "group/kind",
// wildcards allowed). The map is copied. Keys that are not valid glob patterns are logged
// and only ever match exactly, as upstream.
func New(overrides map[string]ResourceOverride, opts ...Option) *Runtime {
	r := &Runtime{
		overrides: make(map[string]ResourceOverride, len(overrides)),
		timeout:   DefaultTimeout,
		logger:    slog.Default(),
	}
	for _, o := range opts {
		o(r)
	}
	if r.builtins == nil {
		r.builtins = defaultBuiltins()
	}
	keys := make([]string, 0, len(overrides))
	for k, v := range overrides {
		r.overrides[k] = v
		keys = append(keys, k)
	}
	// Upstream iterates a Go map (random order) when several wildcard keys match; sorting
	// makes the choice deterministic.
	slices.Sort(keys)
	for _, k := range keys {
		g, err := glob.Compile(k)
		if err != nil {
			r.logger.Warn("ignoring invalid resource customization key pattern", "key", k, "error", err)
			continue
		}
		r.wildcards = append(r.wildcards, wildcardOverride{key: k, pattern: g})
	}
	return r
}

// protoCacheSize bounds the number of cached compiled scripts.
const protoCacheSize = 512

// protoCache holds compiled scripts keyed by the SHA-256 of their source. Compiled
// prototypes are immutable and safely shared between Lua states. A nil cache (which only
// happens for a non-positive size) disables caching.
var protoCache = sync.OnceValue(func() *lru.Cache[[sha256.Size]byte, *glua.FunctionProto] {
	c, err := lru.New[[sha256.Size]byte, *glua.FunctionProto](protoCacheSize)
	if err != nil {
		return nil
	}
	return c
})

func compile(script string) (*glua.FunctionProto, error) {
	key := sha256.Sum256([]byte(script))
	cache := protoCache()
	if cache != nil {
		if p, ok := cache.Get(key); ok {
			return p, nil
		}
	}
	chunk, err := parse.Parse(strings.NewReader(script), chunkName)
	if err != nil {
		return nil, &ScriptError{Message: err.Error(), cause: err}
	}
	proto, err := glua.Compile(rewriteChunk(chunk), chunkName)
	if err != nil {
		return nil, &ScriptError{Message: err.Error(), cause: err}
	}
	if cache != nil {
		cache.Add(key, proto)
	}
	return proto, nil
}

// ActionParameter is a named action input exposed to scripts as actionParams[name]
// (always a string, as upstream).
type ActionParameter struct {
	Name  string
	Value string
}

// result is the last value returned by a script, encoded as JSON when it is a table.
type result struct {
	typ  glua.LValueType
	json []byte
}

// execOutcome is what the evaluation goroutine reports.
type execOutcome struct {
	res result
	err error
}

// exec runs script with obj (and params) bound as globals and returns its last return
// value. Library profile, deadline, string limits and stack limits are applied per call.
// A nil Runtime uses the defaults.
//
// The script runs on its own goroutine. The VM and the pattern matcher stop at the
// deadline by themselves; if the evaluation is still running stallGrace(timeout) after the
// deadline (a Go library call that cannot be interrupted, such as table.sort on a huge
// table or, with useOpenLibs, a blocking os.execute), exec returns the timeout error and
// abandons the goroutine, which exits when that call returns. Abandoned evaluations are
// counted; while maxStalled of them are still running, exec fails fast with
// ErrOverloaded.
func (r *Runtime) exec(ctx context.Context, obj *unstructured.Unstructured, script string, profile libProfile, params []ActionParameter) (result, error) {
	if stalledEvaluations.Load() >= maxStalled {
		return result{}, ErrOverloaded
	}
	proto, err := compile(script)
	if err != nil {
		return result{}, err
	}
	st, err := newState(profile)
	if err != nil {
		return result{}, err
	}
	L := st.L
	handedOff := false
	defer func() {
		if !handedOff {
			L.Close()
		}
	}()

	timeout := DefaultTimeout
	logger := slog.Default()
	if r != nil {
		if r.timeout > 0 {
			timeout = r.timeout
		}
		logger = r.logger
		if r.stateHook != nil {
			r.stateHook(L)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	L.SetContext(runCtx)

	// Globals are prepared on the caller's goroutine: the evaluation goroutine must not
	// touch obj, which the caller owns again once exec returns.
	paramTable := L.CreateTable(0, len(params))
	for _, p := range params {
		paramTable.RawSetString(p.Name, glua.LString(p.Value))
	}
	L.SetGlobal("actionParams", paramTable)
	objValue := glua.LNil
	if obj != nil {
		objValue, err = toLua(L, obj.Object, 0)
		if err != nil {
			return result{}, fmt.Errorf("convert object for lua: %w", err)
		}
	}
	L.SetGlobal("obj", objValue)
	fn := L.NewFunctionFromProto(proto)

	done := make(chan execOutcome, 1)
	handedOff = true
	go func() {
		defer L.Close()
		done <- run(ctx, runCtx, timeout, L, fn, st.concat)
	}()
	select {
	case out := <-done:
		return out.res, out.err
	case <-runCtx.Done():
	}
	grace := time.NewTimer(stallGrace(timeout))
	defer grace.Stop()
	select {
	case out := <-done:
		return out.res, out.err
	case <-grace.C:
	}
	stalled := stalledEvaluations.Add(1)
	abandonedEvaluations.Add(1)
	go func() {
		<-done
		stalledEvaluations.Add(-1)
	}()
	logger.Warn("abandoned a lua evaluation still running past its deadline",
		"timeout", timeout, "stalledEvaluations", stalled)
	return result{}, withDeadline(&ScriptError{}, ctx, runCtx, timeout)
}

// run executes the compiled chunk on L and converts its last return value. It runs on
// the evaluation goroutine and never panics.
func run(parent, runCtx context.Context, timeout time.Duration, L *glua.LState, fn, concat *glua.LFunction) (out execOutcome) {
	defer func() {
		if p := recover(); p != nil {
			out = execOutcome{err: fmt.Errorf("lua evaluation failed: %v", p)}
		}
	}()
	L.Push(fn)
	L.Push(concat) // bound to the chunk's hidden concatenation local (rewrite.go)
	if err := L.PCall(1, glua.MultRet, nil); err != nil {
		return execOutcome{err: scriptError(parent, runCtx, timeout, err)}
	}
	ret := glua.LNil
	if L.GetTop() > 0 {
		ret = L.Get(-1)
	}
	res := result{typ: ret.Type()}
	if res.typ == glua.LTTable {
		b, err := toJSONContext(runCtx, ret)
		if err != nil {
			if runCtx.Err() != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
				return execOutcome{err: withDeadline(&ScriptError{}, parent, runCtx, timeout)}
			}
			return execOutcome{err: err}
		}
		res.json = b
	}
	return execOutcome{res: res}
}

// scriptError converts a gopher-lua error into a ScriptError without stack trace.
// parent is the caller's context, runCtx the derived context carrying the script deadline.
func scriptError(parent, runCtx context.Context, timeout time.Duration, err error) error {
	se := &ScriptError{Message: err.Error(), cause: err}
	var apiErr *glua.ApiError
	if errors.As(err, &apiErr) {
		// Stack traces reveal runtime internals and are dropped from errors that reach
		// users, as upstream does.
		apiErr.StackTrace = ""
		if apiErr.Object != nil {
			se.Message = apiErr.Object.String()
		}
	}
	return withDeadline(se, parent, runCtx, timeout)
}

// withDeadline marks se as a timeout or cancellation when runCtx has ended.
func withDeadline(se *ScriptError, parent, runCtx context.Context, timeout time.Duration) error {
	ctxErr := runCtx.Err()
	if ctxErr == nil {
		return se
	}
	se.ctxErr = ctxErr
	se.Timeout = errors.Is(ctxErr, context.DeadlineExceeded)
	switch {
	case parent.Err() != nil:
		se.Message = "lua script execution canceled: " + parent.Err().Error()
	case se.Timeout:
		se.Message = fmt.Sprintf("lua script exceeded execution timeout of %s", timeout)
	}
	return se
}
