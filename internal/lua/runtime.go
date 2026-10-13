// Package lua runs resource health checks and resource actions written in Lua inside a
// sandboxed gopher-lua runtime.
//
// Scripts come from two places, consulted in the same order as Argo CD v3.5.5
// (util/lua): resource customizations configured in settings (see
// [ParseResourceOverrides]) and the built-in customizations embedded from
// customizations/ (Apache-2.0 data copied from upstream resource_customizations/).
//
// Every evaluation gets a fresh Lua state with a restricted library set, an execution
// deadline, a bounded call stack and a bounded value stack. Compiled scripts are cached by
// content hash. A Runtime is immutable and safe for concurrent use; build a new one when
// settings change.
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
	proto, err := glua.Compile(chunk, chunkName)
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

// exec runs script with obj (and params) bound as globals and returns its last return
// value. Library profile, deadline and stack limits are applied per call. A nil Runtime
// uses the defaults.
func (r *Runtime) exec(ctx context.Context, obj *unstructured.Unstructured, script string, profile libProfile, params []ActionParameter) (result, error) {
	proto, err := compile(script)
	if err != nil {
		return result{}, err
	}
	L, err := newState(profile)
	if err != nil {
		return result{}, err
	}
	defer L.Close()

	timeout := DefaultTimeout
	if r != nil && r.timeout > 0 {
		timeout = r.timeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	L.SetContext(runCtx)

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

	L.Push(L.NewFunctionFromProto(proto))
	if err := L.PCall(0, glua.MultRet, nil); err != nil {
		return result{}, scriptError(ctx, runCtx, timeout, err)
	}
	ret := glua.LNil
	if L.GetTop() > 0 {
		ret = L.Get(-1)
	}
	out := result{typ: ret.Type()}
	if out.typ == glua.LTTable {
		if out.json, err = toJSON(ret); err != nil {
			return result{}, err
		}
	}
	return out, nil
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
