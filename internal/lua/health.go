package lua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	glua "github.com/yuin/gopher-lua"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/internal/health"
)

// InvalidHealthStatusMessage is the message of the Unknown status reported when a script
// returns an unrecognized status code (upstream text).
const InvalidHealthStatusMessage = "Lua returned an invalid health status"

// ApplicationHealthLua is the documented customization that reports a Patrol Application's
// recorded health (configure it as resource.customizations.health.cd.patrol.example_Application).
// It is the Patrol equivalent of upstream's argoproj.io_Application snippet and yields the
// same results as health.ApplicationHealth.
const ApplicationHealthLua = `hs = {}
hs.status = "Progressing"
hs.message = ""
if obj.status ~= nil then
  if obj.status.health ~= nil then
    hs.status = obj.status.health.status
    if obj.status.health.message ~= nil then
      hs.message = obj.status.health.message
    end
  end
end
return hs
`

// ScriptSource tells where a script was found.
type ScriptSource string

const (
	// SourceConfigured: settings key equal to the object's group/kind.
	SourceConfigured ScriptSource = "configured"
	// SourceConfiguredWildcard: settings key with a wildcard matching group/kind.
	SourceConfiguredWildcard ScriptSource = "configured-wildcard"
	// SourceBuiltin: built-in customization directory for group/kind.
	SourceBuiltin ScriptSource = "builtin"
	// SourceBuiltinWildcard: built-in wildcard directory (for example _.crossplane.io/_).
	SourceBuiltinWildcard ScriptSource = "builtin-wildcard"
)

// HealthScript is the health script selected for an object.
type HealthScript struct {
	Script string
	// UseOpenLibs is true only for configured scripts that opted in. Built-in scripts run
	// with the safe library profile (upstream runs them with all libraries; they only need
	// string/table/math/os.time/os.date).
	UseOpenLibs bool
	Source      ScriptSource
	// Key is the matching settings key or built-in directory.
	Key string
}

// HealthScript returns the health script for obj, or nil when none applies. Lookup order
// (upstream): configured key equal to group/kind with a non-empty script; first configured
// wildcard key (sorted) matching group/kind with a non-empty script; built-in
// <group>/<kind>/health.lua; first built-in wildcard directory (sorted) matching.
func (r *Runtime) HealthScript(obj *unstructured.Unstructured) (*HealthScript, error) {
	if r == nil || obj == nil {
		return nil, nil
	}
	gk := obj.GroupVersionKind().GroupKind()
	key := OverrideKey(gk)
	if o, ok := r.overrides[key]; ok && o.HealthLua != "" {
		return &HealthScript{Script: o.HealthLua, UseOpenLibs: o.UseOpenLibs, Source: SourceConfigured, Key: key}, nil
	}
	for _, w := range r.wildcards {
		o := r.overrides[w.key]
		if o.HealthLua != "" && w.pattern.Match(key) {
			return &HealthScript{Script: o.HealthLua, UseOpenLibs: o.UseOpenLibs, Source: SourceConfiguredWildcard, Key: w.key}, nil
		}
	}
	script, dirKey, err := r.builtins.readFor(gk, healthScriptFile)
	switch {
	case err == nil:
		return &HealthScript{Script: script, Source: SourceBuiltin, Key: dirKey}, nil
	case !errors.Is(err, errNoScript):
		return nil, err
	}
	script, dir, err := r.builtins.wildcardHealth(key)
	if err != nil {
		return nil, fmt.Errorf("error while fetching built-in health script: %w", err)
	}
	if script == "" {
		return nil, nil
	}
	return &HealthScript{Script: script, Source: SourceBuiltinWildcard, Key: dir}, nil
}

// GetResourceHealth implements health.Overrides with the default timeout and no caller
// context. It returns (nil, nil) when no script applies to obj.
func (r *Runtime) GetResourceHealth(obj *unstructured.Unstructured) (*health.HealthStatus, error) {
	return r.GetResourceHealthContext(context.Background(), obj)
}

// GetResourceHealthContext is GetResourceHealth bounded by ctx as well as the runtime
// timeout.
func (r *Runtime) GetResourceHealthContext(ctx context.Context, obj *unstructured.Unstructured) (*health.HealthStatus, error) {
	hs, err := r.HealthScript(obj)
	if err != nil || hs == nil {
		return nil, err
	}
	return r.ExecuteHealth(ctx, obj, hs.Script, hs.UseOpenLibs)
}

// ExecuteHealth runs a health script against obj and interprets its result as upstream:
//
//   - a table is decoded as {status, message} (keys case-insensitive); a table that
//     cannot be decoded (empty table, wrong field types) yields an empty HealthStatus;
//   - an unrecognized status yields Unknown with [InvalidHealthStatusMessage];
//   - nil (or no return value) yields an empty HealthStatus;
//   - any other type is an error wrapping [ErrInvalidOutput].
func (r *Runtime) ExecuteHealth(ctx context.Context, obj *unstructured.Unstructured, script string, useOpenLibs bool) (*health.HealthStatus, error) {
	profile := profileSafe
	if useOpenLibs {
		profile = profileOpen
	}
	res, err := r.exec(ctx, obj, script, profile, nil)
	if err != nil {
		return nil, err
	}
	switch res.typ {
	case glua.LTNil:
		return &health.HealthStatus{}, nil
	case glua.LTTable:
		var hs health.HealthStatus
		if err := json.Unmarshal(res.json, &hs); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				return &health.HealthStatus{}, nil
			}
			return nil, fmt.Errorf("decode health status: %w", err)
		}
		if !hs.Status.Valid() {
			return &health.HealthStatus{Status: health.StatusUnknown, Message: InvalidHealthStatusMessage}, nil
		}
		return &hs, nil
	}
	return nil, outputTypeError("table", res.typ)
}

// outputError keeps upstream's message while matching ErrInvalidOutput.
type outputError struct{ msg string }

func (e *outputError) Error() string        { return e.msg }
func (e *outputError) Is(target error) bool { return target == ErrInvalidOutput }

func outputTypeError(want string, got glua.LValueType) error {
	return &outputError{msg: fmt.Sprintf("expect %s output from Lua script, not %s", want, got)}
}

// Compile-time check that Runtime satisfies health.Overrides.
var _ health.Overrides = (*Runtime)(nil)
