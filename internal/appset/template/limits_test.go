package template

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// allocated returns the bytes allocated while fn runs. Callers must not run in parallel with
// other tests (top-level tests without t.Parallel run alone).
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestEvaluationLimitsHostileTemplates: tiny templates that would allocate gigabytes, loop for
// hours or crash the process fail with a limit error after a bounded amount of work.
func TestEvaluationLimitsHostileTemplates(t *testing.T) {
	params := map[string]any{"name": "x"}
	go1 := Options{GoTemplate: true}
	tests := []struct {
		name string
		tmpl string
	}{
		{"until", `{{ len (until 30000000) }}`},
		{"until negative", `{{ len (until -30000000) }}`},
		{"untilStep", `{{ len (untilStep 0 1000000000 1) }}`},
		{"seq", `{{ len (seq 1000000000) }}`},
		{"seq with step", `{{ len (seq 1 1 1000000000) }}`},
		// sprig's loop would overflow past the stop value and never terminate.
		{"untilStep overflow", `{{ len (untilStep 0 9223372036854775807 4611686018427387904) }}`},
		{"untilStep negative overflow", `{{ len (untilStep 9223372036854775807 -9223372036854775808 -9223372036854775808) }}`},
		{"seq overflow", `{{ seq 9223372036854775806 2 9223372036854775807 }}`},
		{"repeat", `{{ len (repeat 200000000 "a") }}`},
		{"repeat negative", `{{ repeat -1 "a" }}`},
		{"indent", `{{ len (indent 1000000000 "x") }}`},
		{"nindent many lines", `{{ len (nindent 100000 (repeat 100000 "\n")) }}`},
		{"replace amplification", `{{ $s := repeat 1000 "a" }}{{ len (replace "a" $s (replace "a" $s $s)) }}`},
		{"replace empty old", `{{ $s := repeat 100000 "a" }}{{ len (replace "" $s $s) }}`},
		{"regexReplaceAll expansion", `{{ $s := repeat 1000000 "a" }}{{ len (regexReplaceAll "a" $s "$0$0$0$0$0$0$0$0$0$0") }}`},
		{"wrapWith separator", `{{ len (wrapWith 1 (repeat 100000 "-") (repeat 1000 "a")) }}`},
		{"randAlpha", `{{ len (randAlpha 1000000000) }}`},
		{"randBytes", `{{ len (randBytes 1000000000) }}`},
		{"printf width", `{{ len (printf (repeat 100 "%1000000d") 1 2 3) }}`},
		{"printf width per element", `{{ len (printf "%1000000v" (until 10000)) }}`},
		{"printf star width with argument indexes", `{{ len (printf (repeat 100 "%[1]*[2]d") 1000000 1) }}`},
		{"join separator", `{{ len (join (repeat 10000 "-") (until 10000)) }}`},
		{"shared references serialized", `{{ $l := list (repeat 1000000 "a") }}{{ $l = list $l $l $l $l }}{{ $l = list $l $l $l $l }}{{ len (toJson $l) }}`},
		{"print shared references", `{{ $s := repeat 4000000 "a" }}{{ len (print $s $s $s) }}`},
		{"splitList pieces", `{{ len (splitList "" (repeat 1000000 "a")) }}`},
		{"split pieces", `{{ len (split "," (repeat 1000000 ",")) }}`},
		{"regexSplit pieces", `{{ len (regexSplit "a" (repeat 1000000 "a") -1) }}`},
		{"regexFindAll matches", `{{ len (regexFindAll "a" (repeat 1000000 "a") -1) }}`},
		{"deep nesting", `{{ toPrettyJson (fromJson (print (repeat 5000 "[") (repeat 5000 "]"))) }}`},
		{"range over int", `{{ range 100000000000 }}{{ end }}`},
		{"nested ranges without output", `{{ range until 10000 }}{{ range until 10000 }}{{ end }}{{ end }}`},
		{"range body work", `{{ range until 10000 }}{{ $a := 1 }}{{ $a = 2 }}{{ $a = 3 }}{{ $a = 4 }}{{ $a = 5 }}{{ $a = 6 }}{{ $a = 7 }}{{ $a = 8 }}{{ $a = 9 }}{{ $a = 10 }}{{ $a = 11 }}{{ $a = 12 }}{{ $a = 13 }}{{ $a = 14 }}{{ $a = 15 }}{{ $a = 16 }}{{ $a = 17 }}{{ $a = 18 }}{{ $a = 19 }}{{ $a = 20 }}{{ $a = 21 }}{{ $a = 22 }}{{ $a = 23 }}{{ $a = 24 }}{{ $a = 25 }}{{ end }}`},
		{"exponential recursion", `{{ define "r" }}{{ if lt (len .) 40 }}{{ template "r" (append . 1) }}{{ template "r" (append . 1) }}{{ end }}{{ end }}{{ template "r" list }}`},
		{"recursion without functions", `{{ define "r" }}{{ template "r" . }}{{ template "r" . }}{{ end }}{{ template "r" . }}`},
		{"cyclic set", `{{ $d := dict }}{{ $_ := set $d "self" $d }}{{ $d }}`},
		{"cyclic set through a list", `{{ $d := dict }}{{ $_ := set $d "self" (list $d) }}{{ toYaml $d }}`},
		{"cyclic merge", `{{ $d := dict "a" 1 }}{{ $_ := mergeOverwrite $d (dict "k" $d) }}{{ toYaml $d }}`},
		{"cyclic merge two steps", `{{ $a := dict }}{{ $b := dict "a" $a }}{{ $_ := merge $a (dict "b" $b) }}{{ $a }}`},
		{"uniq quadratic", `{{ len (uniq (until 10000)) }}`},
		{"key generation", `{{ genPrivateKey "ecdsa" }}{{ genPrivateKey "ecdsa" }}{{ genPrivateKey "ecdsa" }}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			alloc := allocated(func() { _, err = Replace(tc.tmpl, params, go1) })
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrLimitExceeded) || errors.Is(err, ErrOutputTooLarge) || strings.Contains(err.Error(), "negative"),
				"limit error expected, got %v", err)
			assert.Less(t, alloc, uint64(96<<20), "allocated %d MiB", alloc>>20)
		})
	}
}

// TestEvaluationLimitsFasttemplate: many tags referring to a large parameter fail while
// rendering, without building the oversized string.
func TestEvaluationLimitsFasttemplate(t *testing.T) {
	params := map[string]any{"big": strings.Repeat("a", 512<<10)}
	tmpl := strings.Repeat("{{big}}", 10_000)
	var err error
	alloc := allocated(func() { _, err = Replace(tmpl, params, Options{}) })
	require.ErrorIs(t, err, ErrOutputTooLarge)
	assert.Less(t, alloc, uint64(16<<20), "allocated %d MiB", alloc>>20)
}

// TestEvaluationLimitsApplicationTotal: the strings of one Application share one evaluation:
// many fields that each stay under the per-string cap cannot add up without limit.
func TestEvaluationLimitsApplicationTotal(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{}
	app.Name = "{{ name }}"
	app.Annotations = map[string]string{}
	for i := range 8 {
		app.Annotations["a"+strings.Repeat("x", i)] = "{{ big }}"
	}
	params := map[string]any{"name": "n", "big": strings.Repeat("a", 900<<10)}
	_, err := RenderApplication(app, nil, params, Options{})
	require.ErrorIs(t, err, ErrLimitExceeded)
	assert.ErrorContains(t, err, "rendered output exceeds")

	app.Annotations = map[string]string{"a": "{{ big }}", "b": "{{ big }}"}
	_, err = RenderApplication(app, nil, params, Options{})
	require.NoError(t, err, "a few large fields stay within the evaluation output")

	// Go templates: the step budget is shared by the fields too.
	app.Annotations = map[string]string{}
	for i := range 30 {
		app.Annotations["k"+strings.Repeat("x", i)] = `{{ range until 10000 }}{{ $a := 1 }}{{ end }}`
	}
	_, err = RenderApplication(app, nil, map[string]any{"name": "n"}, Options{GoTemplate: true})
	require.ErrorIs(t, err, ErrLimitExceeded)
	assert.ErrorContains(t, err, "steps")
}

// TestEvaluationLimitsAllowOrdinaryTemplates: common template idioms are unaffected.
func TestEvaluationLimitsAllowOrdinaryTemplates(t *testing.T) {
	t.Parallel()
	params := map[string]any{
		"name":   "guestbook",
		"values": map[string]any{"replicas": 2, "image": map[string]any{"tag": "v1"}},
		"list":   []any{"b", "a", "b"},
		"csv":    "x,y,z",
	}
	tests := []struct{ tmpl, want string }{
		{`{{ range $i := until 3 }}{{ $i }}{{ end }}`, "012"},
		{`{{ range $i, $e := .list }}{{ $i }}={{ $e }};{{ end }}`, "0=b;1=a;2=b;"},
		{`{{ range 3 }}x{{ end }}`, "xxx"},
		{`{{ define "t" }}<{{ . }}>{{ end }}{{ template "t" .name }}{{ template "t" "y" }}`, "<guestbook><y>"},
		{`{{ define "r" }}{{ if lt (len .) 3 }}{{ template "r" (append . 1) }}{{ else }}{{ len . }}{{ end }}{{ end }}{{ template "r" list }}`, "3"},
		{"{{ toYaml .values | nindent 2 }}", "\n  image:\n    tag: v1\n  replicas: 2"},
		{`{{ printf "%05d-%s" 7 .name }}`, "00007-guestbook"},
		{`{{ list 1 "a" | toJson }}`, `[1,"a"]`},
		{`{{ uniq .list | sortAlpha | join "," }}`, "a,b"},
		{`{{ $d := dict "a" 1 }}{{ $_ := set $d "b" 2 }}{{ $_ := merge $d (dict "c" 3) }}{{ keys $d | sortAlpha | join "" }}`, "abc"},
		{`{{ splitList "," .csv | len }}`, "3"},
		{`{{ regexReplaceAll "(b)" "abc" "<$1>" }}`, "a<b>c"},
		{`{{ seq 3 }}|{{ repeat 3 "ab" }}|{{ indent 2 "x" }}`, "1 2 3|ababab|  x"},
		{`{{ replace "-" "_" .name }}{{ upper .name }}`, "guestbookGUESTBOOK"},
		{`{{ (fromJson "{\"a\":[1,2]}").a | len }}`, "2"},
		{`{{ deepCopy .values | toJson }}`, `{"image":{"tag":"v1"},"replicas":2}`},
		{`{{ print .name 1 }}{{ println }}{{ html "<" }}{{ js "'" }}{{ urlquery "a b" }}`, "guestbook1\n&lt;\\'a+b"},
	}
	for _, tc := range tests {
		got, err := Replace(tc.tmpl, params, Options{GoTemplate: true})
		require.NoError(t, err, tc.tmpl)
		assert.Equal(t, tc.want, got, tc.tmpl)
	}
}

// TestEvaluationStepAccounting pins how work is charged.
func TestEvaluationStepAccounting(t *testing.T) {
	t.Parallel()
	ev := newEvaluation()
	_, err := ev.replace(`{{ range until 1000 }}{{ end }}`, map[string]any{}, Options{GoTemplate: true})
	require.NoError(t, err)
	used := ev.steps
	assert.Greater(t, used, int64(1000), "every iteration is charged")
	assert.Less(t, used, int64(10_000))

	ev = newEvaluation()
	_, err = ev.replace(`{{ range until 1000 }}{{ $a := 1 }}{{ $a = 2 }}{{ $a = 3 }}{{ end }}`, map[string]any{}, Options{GoTemplate: true})
	require.NoError(t, err)
	assert.Greater(t, ev.steps, 3*used, "an iteration is charged by the size of its body")

	require.ErrorIs(t, ev.step(MaxEvaluationSteps), ErrLimitExceeded)
	require.ErrorIs(t, ev.step(1), ErrLimitExceeded, "an exhausted evaluation stays exhausted")
}

func TestSequenceLength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		start, stop, step int64
		want              uint64
	}{
		{0, 10, 1, 10}, {0, 10, 3, 4}, {10, 0, -1, 10}, {10, 0, 1, 0}, {0, 10, -1, 0}, {0, 10, 0, 0}, {5, 5, 1, 0},
		{-1 << 63, 1<<63 - 1, 1, 1<<64 - 1}, {1<<63 - 1, -1 << 63, -1 << 63, 2},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, sequenceLength(tc.start, tc.stop, tc.step), "%d %d %d", tc.start, tc.stop, tc.step)
	}
}
