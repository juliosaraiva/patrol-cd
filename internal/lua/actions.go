package lua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	glua "github.com/yuin/gopher-lua"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ActionParam describes an input of an action, as reported by discovery.
type ActionParam struct {
	Name string `json:"name,omitempty"`
}

// Action is an action offered for an object by a discovery script.
type Action struct {
	Name        string        `json:"name,omitempty"`
	Params      []ActionParam `json:"params,omitempty"`
	Disabled    bool          `json:"disabled,omitempty"`
	IconClass   string        `json:"iconClass,omitempty"`
	DisplayName string        `json:"displayName,omitempty"`
}

// Operation is what the caller must do with an impacted resource.
type Operation string

const (
	// OperationPatch: merge-patch the source object to the returned state.
	OperationPatch Operation = "patch"
	// OperationCreate: create the returned object.
	OperationCreate Operation = "create"
)

// UnmarshalJSON accepts only "patch" and "create" (upstream message on failure).
func (o *Operation) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case `"create"`:
		*o = OperationCreate
	case `"patch"`:
		*o = OperationPatch
	default:
		return fmt.Errorf("unsupported operation: %s", b)
	}
	return nil
}

// ImpactedResource is one object produced by an action.
type ImpactedResource struct {
	Object    *unstructured.Unstructured
	Operation Operation
}

// DiscoveryScripts returns the action discovery scripts for obj in evaluation order: the
// configured discovery script (if the key has actions configured) followed, unless the
// configuration replaces built-ins (mergeBuiltinActions=false), by the built-in one.
// Configured actions match the exact group/kind key only (no wildcards), as upstream.
func (r *Runtime) DiscoveryScripts(obj *unstructured.Unstructured) ([]string, error) {
	if r == nil || obj == nil {
		return nil, nil
	}
	gk := obj.GroupVersionKind().GroupKind()
	var scripts []string
	if o, ok := r.overrides[OverrideKey(gk)]; ok && o.Actions != "" {
		actions, err := o.GetActions()
		if err != nil {
			return nil, err
		}
		if !actions.MergeBuiltinActions {
			return []string{actions.ActionDiscoveryLua}, nil
		}
		scripts = append(scripts, actions.ActionDiscoveryLua)
	}
	s, _, err := r.builtins.readFor(gk, "actions", discoveryScriptFile)
	switch {
	case err == nil:
		scripts = append(scripts, s)
	case !errors.Is(err, errNoScript):
		return nil, fmt.Errorf("error while fetching predefined lua scripts: %w", err)
	}
	return scripts, nil
}

// ListActions returns the actions available for obj, sorted by name. It returns an empty
// list when the kind has no discovery script.
func (r *Runtime) ListActions(ctx context.Context, obj *unstructured.Unstructured) ([]Action, error) {
	scripts, err := r.DiscoveryScripts(obj)
	if err != nil {
		return nil, err
	}
	if len(scripts) == 0 {
		return []Action{}, nil
	}
	return r.ExecuteDiscovery(ctx, obj, scripts)
}

// ExecuteDiscovery runs discovery scripts in order and merges their results; an action
// name reported by an earlier script wins. Each script must return a table mapping action
// names to action descriptors ({} or a table with iconClass, displayName, disabled,
// params).
func (r *Runtime) ExecuteDiscovery(ctx context.Context, obj *unstructured.Unstructured, scripts []string) ([]Action, error) {
	if len(scripts) == 0 {
		return nil, ErrNoDiscovery
	}
	byName := map[string]Action{}
	for _, script := range scripts {
		res, err := r.exec(ctx, obj, script, profileSafe, nil)
		if err != nil {
			return nil, err
		}
		if res.typ != glua.LTTable {
			return nil, outputTypeError("table", res.typ)
		}
		if string(res.json) == "[]" { // empty table: no actions
			continue
		}
		raw := map[string]json.RawMessage{}
		if err := json.Unmarshal(res.json, &raw); err != nil {
			return nil, fmt.Errorf("error unmarshaling action table: %w", err)
		}
		for name, value := range raw {
			if _, exists := byName[name]; exists {
				continue
			}
			a, err := decodeAction(name, value)
			if err != nil {
				return nil, err
			}
			byName[name] = a
		}
	}
	out := make([]Action, 0, len(byName))
	for _, a := range byName {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Action) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// decodeAction decodes one discovery entry. A JSON array (an empty Lua table) is an action
// with defaults; an object is decoded over {Name: name}, so an explicit "name" field wins.
func decodeAction(name string, value json.RawMessage) (Action, error) {
	a := Action{Name: name}
	if len(value) > 0 && value[0] == '[' {
		return a, nil
	}
	if err := json.Unmarshal(value, &a); err != nil {
		return Action{}, fmt.Errorf("error unmarshaling resource action: %w", err)
	}
	return a, nil
}

// ActionScript returns the script of action name for obj: a configured definition with
// that name first, then the built-in <group>/<kind>/actions/<name>/action.lua. It returns
// ErrActionNotFound when neither exists.
func (r *Runtime) ActionScript(obj *unstructured.Unstructured, name string) (ActionDefinition, error) {
	if r == nil || obj == nil {
		return ActionDefinition{}, ErrActionNotFound
	}
	gk := obj.GroupVersionKind().GroupKind()
	if o, ok := r.overrides[OverrideKey(gk)]; ok && o.Actions != "" {
		actions, err := o.GetActions()
		if err != nil {
			return ActionDefinition{}, err
		}
		for _, d := range actions.Definitions {
			if d.Name == name {
				return d, nil
			}
		}
	}
	if !validElem(name) {
		return ActionDefinition{}, fmt.Errorf("%w: %q", ErrActionNotFound, name)
	}
	s, _, err := r.builtins.readFor(gk, "actions", name, actionScriptFile)
	if err != nil {
		if errors.Is(err, errNoScript) {
			return ActionDefinition{}, fmt.Errorf("%w: %q for %s", ErrActionNotFound, name, OverrideKey(gk))
		}
		return ActionDefinition{}, err
	}
	return ActionDefinition{Name: name, ActionLua: s}, nil
}

// ExecuteAction runs action name against obj. It does not consult discovery (callers that
// enforce "disabled" must check ListActions first, as upstream leaves this to the API).
func (r *Runtime) ExecuteAction(ctx context.Context, obj *unstructured.Unstructured, name string, params []ActionParameter) ([]ImpactedResource, error) {
	def, err := r.ActionScript(obj, name)
	if err != nil {
		return nil, err
	}
	return r.ExecuteActionScript(ctx, obj, def.ActionLua, params)
}

// impactedJSON is the wire shape of a new-style action result entry.
type impactedJSON struct {
	Resource  *unstructured.Unstructured `json:"resource"`
	Operation Operation                  `json:"operation"`
}

// ExecuteActionScript runs an action script against obj (with the safe library profile)
// and interprets its result as upstream:
//
//   - an object is the new state of obj (operation patch);
//   - an array lists {operation = "create"|"patch", resource = object} entries.
//
// Patch results are cleaned so that tables that were objects in obj and came back as empty
// arrays (Lua cannot tell {} apart) are objects again. Patrol additionally requires every
// patch to target obj itself (same group, kind, namespace and name) and every entry to
// carry a resource and an operation.
func (r *Runtime) ExecuteActionScript(ctx context.Context, obj *unstructured.Unstructured, script string, params []ActionParameter) ([]ImpactedResource, error) {
	if obj == nil {
		return nil, fmt.Errorf("%w: nil object", ErrInvalidOutput)
	}
	res, err := r.exec(ctx, obj, script, profileSafe, params)
	if err != nil {
		return nil, err
	}
	if res.typ != glua.LTTable {
		return nil, outputTypeError("table", res.typ)
	}
	out := res.json
	if len(out) < 2 {
		return nil, &outputError{msg: "Lua output was not a valid json object or array"}
	}
	var impacted []ImpactedResource
	if out[0] == '[' && out[len(out)-1] == ']' {
		var entries []impactedJSON
		if err := json.Unmarshal(out, &entries); err != nil {
			return nil, err
		}
		impacted = make([]ImpactedResource, 0, len(entries))
		for i, e := range entries {
			if e.Resource == nil {
				return nil, &outputError{msg: fmt.Sprintf("impacted resource %d has no resource", i)}
			}
			if e.Operation == "" {
				return nil, &outputError{msg: fmt.Sprintf("impacted resource %d has no operation", i)}
			}
			impacted = append(impacted, ImpactedResource{Object: e.Resource, Operation: e.Operation})
		}
	} else {
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(out, u); err != nil {
			return nil, err
		}
		impacted = []ImpactedResource{{Object: u, Operation: OperationPatch}}
	}
	for _, ir := range impacted {
		if ir.Operation != OperationPatch {
			continue
		}
		if err := samePatchTarget(obj, ir.Object); err != nil {
			return nil, err
		}
		ir.Object.Object = cleanReturnedObj(ir.Object.Object, obj.Object)
	}
	return impacted, nil
}

func samePatchTarget(src, got *unstructured.Unstructured) error {
	if src.GroupVersionKind().GroupKind() != got.GroupVersionKind().GroupKind() ||
		src.GetNamespace() != got.GetNamespace() || src.GetName() != got.GetName() {
		return &outputError{msg: fmt.Sprintf("patch target %s %s/%s differs from action source %s %s/%s",
			got.GroupVersionKind().GroupKind(), got.GetNamespace(), got.GetName(),
			src.GroupVersionKind().GroupKind(), src.GetNamespace(), src.GetName())}
	}
	return nil
}

// cleanReturnedObj restores empty objects that the Lua round trip turned into empty
// arrays, recursing through maps and arrays present in both orig and newObj.
func cleanReturnedObj(newObj, orig map[string]any) map[string]any {
	for key, oldVal := range orig {
		newVal, ok := newObj[key]
		if !ok {
			continue
		}
		switch nv := newVal.(type) {
		case map[string]any:
			if ov, ok := oldVal.(map[string]any); ok {
				newObj[key] = cleanReturnedObj(nv, ov)
			}
		case []any:
			switch ov := oldVal.(type) {
			case map[string]any:
				if len(nv) == 0 {
					newObj[key] = map[string]any{}
				}
			case []any:
				newObj[key] = cleanReturnedArray(nv, ov)
			}
		}
	}
	return newObj
}

func cleanReturnedArray(newArr, orig []any) []any {
	for i := range newArr {
		if i >= len(orig) {
			break
		}
		switch nv := newArr[i].(type) {
		case map[string]any:
			if ov, ok := orig[i].(map[string]any); ok {
				newArr[i] = cleanReturnedObj(nv, ov)
			}
		case []any:
			if ov, ok := orig[i].([]any); ok {
				newArr[i] = cleanReturnedArray(nv, ov)
			}
		}
	}
	return newArr
}
