package lua

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	glua "github.com/yuin/gopher-lua"
	"pgregory.net/rapid"
)

// genValue draws decoded-JSON values as found in unstructured objects.
func genValue(depth int) *rapid.Generator[any] {
	return rapid.Custom(func(t *rapid.T) any {
		max := 6
		if depth >= 3 {
			max = 4 // scalars only
		}
		switch rapid.IntRange(0, max).Draw(t, "kind") {
		case 0:
			return nil
		case 1:
			return rapid.Bool().Draw(t, "bool")
		case 2:
			return rapid.Int64Range(-1<<40, 1<<40).Draw(t, "int")
		case 3:
			return rapid.Float64Range(-1e9, 1e9).Draw(t, "float")
		case 4:
			return rapid.String().Draw(t, "string")
		case 5:
			return rapid.SliceOfN(genValue(depth+1), 0, 4).Draw(t, "list")
		default:
			return rapid.MapOfN(rapid.StringN(0, 8, -1), genValue(depth+1), 0, 4).Draw(t, "map")
		}
	})
}

// roundTripped is what a value looks like after Go → Lua → Go: numbers are float64, Lua
// tables cannot hold nil (nil list items are dropped, nil map values unset) and an empty
// table is an empty list.
func roundTripped(v any) any {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case []any:
		out := []any{}
		for _, item := range x {
			if item != nil {
				out = append(out, roundTripped(item))
			}
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, item := range x {
			if item != nil {
				out[k] = roundTripped(item)
			}
		}
		if len(out) == 0 {
			return []any{}
		}
		return out
	}
	return v
}

func TestConvertRoundTripProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		v := genValue(0).Draw(t, "value")
		L := glua.NewState(glua.Options{SkipOpenLibs: true})
		defer L.Close()
		lv, err := toLua(L, v, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fromLua(lv)
		if err != nil {
			t.Fatalf("fromLua: %v", err)
		}
		want := roundTripped(v)
		if !assert.ObjectsAreEqual(want, got) {
			t.Fatalf("round trip mismatch:\nwant %#v\ngot  %#v", want, got)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("not JSON encodable: %v", err)
		}
	})
}

func TestConvertEdgeCases(t *testing.T) {
	t.Parallel()
	L := glua.NewState(glua.Options{SkipOpenLibs: true})
	defer L.Close()

	lv, err := toLua(L, json.Number("12"), 0)
	require.NoError(t, err)
	assert.Equal(t, glua.LString("12"), lv)

	lv, err = toLua(L, struct{}{}, 0)
	require.NoError(t, err)
	assert.Equal(t, glua.LNil, lv)

	deep := any("leaf")
	for range maxValueDepth + 5 {
		deep = []any{deep}
	}
	_, err = toLua(L, deep, 0)
	require.ErrorIs(t, err, ErrValueTooDeep)

	_, err = fromLua(glua.LNumber(math.NaN()))
	require.Error(t, err)
	_, err = fromLua(glua.LNumber(math.Inf(1)))
	require.Error(t, err)
	_, err = fromLua(L.NewFunction(func(*glua.LState) int { return 0 }))
	require.ErrorIs(t, err, errUnsupportedLV)

	tbl := L.NewTable()
	tbl.RawSetInt(2, glua.LString("x"))
	_, err = fromLua(tbl)
	require.ErrorIs(t, err, ErrSparseArray)

	tbl = L.NewTable()
	tbl.RawSet(glua.LBool(true), glua.LString("x"))
	_, err = fromLua(tbl)
	require.ErrorIs(t, err, ErrInvalidKeys)
}

func TestScanFormat(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"": "", "plain": "", "%d": "d", "%5d": "d", "%-99s": "s", "%.2f": "f", "%99.99f": "f",
		"%%": "", "100%%": "", "%+ #0d": "d", "%s and %q": "sq", "[%s]": "s", "50%": "\x00",
	}
	bad := []string{
		"%100d", "%.100f", "%999999999d", "%-+ #0-d",
		// Go argument indexes and '*' widths would bypass the width limit.
		"%[1]s", "%[1]999999d", "%.[1]999999d", "%5.2[1]d", "%*d", "%.*f", "%-*d",
	}
	for f, verbs := range ok {
		got, err := scanFormat(f)
		require.NoError(t, err, f)
		assert.Equal(t, verbs, string(got), f)
	}
	for _, f := range bad {
		_, err := scanFormat(f)
		assert.Error(t, err, f)
	}
}

func TestScanFormatNeverPanics(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		_, _ = scanFormat(rapid.String().Draw(t, "format"))
	})
}
