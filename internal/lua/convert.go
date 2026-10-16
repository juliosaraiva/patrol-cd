package lua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	glua "github.com/yuin/gopher-lua"
)

// maxValueDepth bounds nesting when converting between Go and Lua values. Kubernetes
// objects are far shallower; the cap protects the Go stack from scripts that build deeply
// nested tables.
const maxValueDepth = 1000

// Limits of a script result. Kubernetes objects are far smaller (etcd stores at most a
// few MiB per object). Bytes count string values and object keys, so a string referenced
// many times counts every time.
const (
	maxResultNodes = 1 << 20
	maxResultBytes = 16 << 20
	// resultCheckEvery is the number of values converted between context checks.
	resultCheckEvery = 4096
)

// Errors from converting Lua values to JSON (texts follow the gopher-json encoder that
// upstream uses).
var (
	ErrNestedTable   = errors.New("cannot encode recursively nested tables to JSON")
	ErrSparseArray   = errors.New("cannot encode sparse array")
	ErrInvalidKeys   = errors.New("cannot encode mixed or invalid key types")
	ErrValueTooDeep  = errors.New("value nesting exceeds maximum depth")
	errUnsupportedLV = errors.New("unsupported Lua value")
)

// toLua converts a decoded JSON/YAML value (as found in unstructured objects) to a Lua
// value. Unsupported Go types become nil, as upstream. Nil elements of slices are dropped
// and nil map values leave the key unset (Lua tables cannot hold nil).
func toLua(L *glua.LState, v any, depth int) (glua.LValue, error) {
	if depth > maxValueDepth {
		return glua.LNil, ErrValueTooDeep
	}
	switch x := v.(type) {
	case nil:
		return glua.LNil, nil
	case bool:
		return glua.LBool(x), nil
	case string:
		return glua.LString(x), nil
	case json.Number:
		return glua.LString(string(x)), nil
	case float64:
		return glua.LNumber(x), nil
	case float32:
		return glua.LNumber(x), nil
	case int:
		return glua.LNumber(x), nil
	case int32:
		return glua.LNumber(x), nil
	case int64:
		return glua.LNumber(x), nil
	case []any:
		t := L.CreateTable(len(x), 0)
		for _, item := range x {
			lv, err := toLua(L, item, depth+1)
			if err != nil {
				return glua.LNil, err
			}
			t.Append(lv)
		}
		return t, nil
	case map[string]any:
		// Keys are inserted in sorted order so that pairs() iterates deterministically
		// (gopher-lua iterates hash keys in insertion order). Upstream inserts in Go map
		// order, which makes order-dependent scripts flap between evaluations.
		t := L.CreateTable(0, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			lv, err := toLua(L, x[k], depth+1)
			if err != nil {
				return glua.LNil, err
			}
			t.RawSetH(glua.LString(k), lv)
		}
		return t, nil
	}
	return glua.LNil, nil
}

// fromLua converts a Lua value into a JSON-compatible Go value with the semantics of the
// gopher-json encoder used upstream:
//
//   - an empty table is an empty array;
//   - a table whose first key is a number must be a sequence 1..n (else ErrSparseArray, or
//     ErrInvalidKeys on a non-number key);
//   - a table whose first key is a string must only have string keys (else ErrInvalidKeys);
//   - functions, userdata, threads and channels cannot be encoded.
//
// Unlike that encoder, a table referenced twice without forming a cycle is accepted; a
// cycle yields ErrNestedTable. Results larger than maxResultNodes values or
// maxResultBytes bytes fail with ErrResultTooLarge.
func fromLua(v glua.LValue) (any, error) {
	return fromLuaContext(context.Background(), v)
}

// fromLuaContext is fromLua that also stops with ctx's error when ctx ends.
func fromLuaContext(ctx context.Context, v glua.LValue) (any, error) {
	c := &luaConverter{ctx: ctx, onPath: map[*glua.LTable]struct{}{}}
	return c.value(v, 0)
}

type luaConverter struct {
	ctx    context.Context
	onPath map[*glua.LTable]struct{}
	nodes  int
	bytes  int
}

// count accounts one value (and n bytes of strings) against the result limits.
func (c *luaConverter) count(n int) error {
	c.nodes++
	c.bytes += n
	if c.nodes > maxResultNodes || c.bytes > maxResultBytes {
		return fmt.Errorf("%w: more than %d values or %d bytes", ErrResultTooLarge, maxResultNodes, maxResultBytes)
	}
	if c.nodes%resultCheckEvery == 0 {
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (c *luaConverter) value(v glua.LValue, depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, ErrValueTooDeep
	}
	switch x := v.(type) {
	case *glua.LNilType:
		return nil, c.count(0)
	case glua.LBool:
		return bool(x), c.count(0)
	case glua.LString:
		return string(x), c.count(len(x))
	case glua.LNumber:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("json: unsupported value: %v", f)
		}
		return f, c.count(0)
	case *glua.LTable:
		if _, ok := c.onPath[x]; ok {
			return nil, ErrNestedTable
		}
		if err := c.count(0); err != nil {
			return nil, err
		}
		c.onPath[x] = struct{}{}
		defer delete(c.onPath, x)
		return c.table(x, depth)
	}
	return nil, fmt.Errorf("%w: cannot encode %s to JSON", errUnsupportedLV, v.Type())
}

func (c *luaConverter) table(t *glua.LTable, depth int) (any, error) {
	key, val := t.Next(glua.LNil)
	switch key.Type() {
	case glua.LTNil:
		return []any{}, nil
	case glua.LTNumber:
		arr := make([]any, 0, min(t.Len(), maxResultNodes))
		expected := glua.LNumber(1)
		for key != glua.LNil {
			n, ok := key.(glua.LNumber)
			if !ok {
				return nil, ErrInvalidKeys
			}
			if n != expected {
				return nil, ErrSparseArray
			}
			gv, err := c.value(val, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, gv)
			expected++
			key, val = t.Next(key)
		}
		return arr, nil
	case glua.LTString:
		obj := map[string]any{}
		for key != glua.LNil {
			s, ok := key.(glua.LString)
			if !ok {
				return nil, ErrInvalidKeys
			}
			c.bytes += len(s)
			gv, err := c.value(val, depth+1)
			if err != nil {
				return nil, err
			}
			obj[string(s)] = gv
			key, val = t.Next(key)
		}
		return obj, nil
	}
	return nil, ErrInvalidKeys
}

// toJSON encodes a Lua value as JSON with fromLua semantics.
func toJSON(v glua.LValue) ([]byte, error) {
	return toJSONContext(context.Background(), v)
}

// toJSONContext is toJSON bounded by ctx.
func toJSONContext(ctx context.Context, v glua.LValue) ([]byte, error) {
	gv, err := fromLuaContext(ctx, v)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(gv)
	if err != nil {
		return nil, fmt.Errorf("encode lua value: %w", err)
	}
	return b, nil
}
