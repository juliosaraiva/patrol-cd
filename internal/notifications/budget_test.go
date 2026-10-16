package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/sprig/v3"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// renderMessage renders a one-field template against the application fixture.
func renderMessage(t *testing.T, tmpl string, opts ...Option) (string, error) {
	t.Helper()
	def, err := json.Marshal(map[string]string{"message": tmpl})
	if err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(t, map[string]string{"template.t": string(def)}, nil, nil, opts...)
	if w := eng.Warnings(); len(w) > 0 {
		t.Fatalf("template does not compile: %v", w)
	}
	n, err := eng.Render(context.Background(), appFixture(), []string{"t"}, dest("slack", ""))
	if err != nil {
		return "", err
	}
	return n.Message, nil
}

// TestTemplateLimitsRejectAmplification: templates (tenant controlled with self-service
// notifications) cannot make the controller allocate or compute without bound, nor build
// values that crash it (cyclic values overflow the stack of fmt).
func TestTemplateLimitsRejectAmplification(t *testing.T) {
	t.Parallel()
	manyWidths := strings.Repeat("%0999999d", 40)
	manyArgs := strings.TrimSpace(strings.Repeat(" 1", 40))
	hundredRefs := strings.TrimSpace(strings.Repeat(" $m", 100))
	tests := map[string]string{
		"repeat":                    `{{ $x := repeat 200000000 "a" }}{{ len $x }}`,
		"until":                     `{{ len (until 100000000) }}`,
		"untilStep":                 `{{ len (untilStep 0 1000000000 1) }}`,
		"seq":                       `{{ seq 100000000 }}`,
		"indent":                    `{{ indent 100000000 "a" }}`,
		"nindent":                   `{{ nindent 100000000 "a" }}`,
		"randAlpha":                 `{{ randAlpha 100000000 }}`,
		"randBytes":                 `{{ randBytes 100000000 }}`,
		"printf widths":             `{{ printf "` + manyWidths + `" ` + manyArgs + ` }}`,
		"print doubling":            `{{ $x := "0123456789" }}{{ range until 40 }}{{ $x = print $x $x }}{{ end }}{{ len $x }}`,
		"printf doubling":           `{{ $x := "0123456789" }}{{ range until 40 }}{{ $x = printf "%s%s" $x $x }}{{ end }}{{ len $x }}`,
		"cat doubling":              `{{ $x := "0123456789" }}{{ range until 40 }}{{ $x = cat $x $x }}{{ end }}{{ len $x }}`,
		"replace":                   `{{ replace "" (repeat 1000 "y") (repeat 100000 "x") }}`,
		"regexReplaceAll":           `{{ regexReplaceAll "" (repeat 100000 "x") (repeat 1000 "y") }}`,
		"regexReplaceAll refs":      `{{ regexReplaceAll ".*" (repeat 1000000 "x") (repeat 100 "$0") }}`,
		"split":                     `{{ len (split "" (repeat 1000000 "x")) }}`,
		"aliased list":              `{{ $s := repeat 4000000 "a" }}{{ $l := list $s $s $s $s $s $s $s $s $s $s }}{{ len $l }}`,
		"exponential aliasing":      `{{ $l := list "aaaaaaaaaa" }}{{ range until 60 }}{{ $l = list $l $l }}{{ end }}{{ len $l }}`,
		"deep nesting":              `{{ $d := dict }}{{ range until 1000 }}{{ $d = dict "a" $d }}{{ end }}{{ $d }}`,
		"deep fromJson":             `{{ fromJson (print (repeat 5000 "[") (repeat 5000 "]")) }}`,
		"set cycle":                 `{{ $d := dict }}{{ $_ := set $d "self" $d }}{{ $d }}`,
		"set indirect cycle":        `{{ $a := dict }}{{ $b := dict "a" $a }}{{ $_ := set $a "b" $b }}{{ $a }}`,
		"set cycle in app":          `{{ $_ := set .app.metadata "app" .app }}{{ .app }}`,
		"merge cycle":               `{{ $a := dict "x" (dict) }}{{ $_ := merge $a (dict "x" (dict "y" (get $a "x"))) }}{{ $a }}`,
		"growing shared data":       `{{ $s := repeat 1000000 "a" }}{{ range until 200 }}{{ $_ := set $.app.metadata (printf "k%d" .) $s }}{{ end }}{{ $.app }}`,
		"mutating an aliased dict":  `{{ $m := dict }}{{ $l := list ` + hundredRefs + ` }}{{ $_ := set $m "k" (repeat 1000000 "a") }}{{ $l }}`,
		"range over int":            `{{ range 100000000000 }}{{ end }}`,
		"template recursion":        `{{ define "x" }}{{ if lt . 40 }}{{ template "x" (add . 1) }}{{ template "x" (add . 1) }}{{ end }}{{ end }}{{ template "x" 0 }}`,
		"uniq":                      `{{ len (uniq (until 100000)) }}`,
		"key generation":            `{{ range until 10 }}{{ genPrivateKey "ecdsa" }}{{ end }}`,
		"strings.ReplaceAll helper": `{{ call .strings.ReplaceAll (repeat 1000 "x") "" (repeat 10000 "y") }}`,
		"toJson escaping":           `{{ $s := repeat 3000000 "<" }}{{ toJson $s }}`,
		"join separator":            `{{ join (repeat 1000000 "x") (until 1000) }}`,
		"printf indexed arguments":  `{{ printf (repeat 1000 "%[1]s") (repeat 100000 "x") }}`,
		"pretty json":               `{{ $l := list }}{{ range until 2000 }}{{ $l = list $l }}{{ end }}{{ toPrettyJson $l }}`,
	}
	for name, tmpl := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			// A lower step limit keeps the step-bound cases fast under -race; the memory
			// bound cases trip long before it.
			out, err := renderMessage(t, tmpl, WithTemplateLimits(TemplateLimits{MaxSteps: 20_000}))
			if err == nil {
				t.Fatalf("rendered %d bytes, want an error", len(out))
			}
			if !errors.Is(err, ErrTemplateLimitExceeded) {
				t.Fatalf("want ErrTemplateLimitExceeded, got %v", err)
			}
			if d := time.Since(start); d > 20*time.Second {
				t.Fatalf("limit enforced after %s", d)
			}
		})
	}
}

func TestTemplateNegativeCounts(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{`{{ repeat -1 "a" }}`, `{{ indent -1 "a" }}`} {
		if _, err := renderMessage(t, tmpl); err == nil || !strings.Contains(err.Error(), "negative") {
			t.Errorf("%s: got %v", tmpl, err)
		}
	}
}

// TestTemplateLimitsBoundMemory reproduces the reported case: the allocation is refused
// before it happens instead of being bounded only by the output size.
func TestTemplateLimitsBoundMemory(t *testing.T) { //nolint:paralleltest // measures process allocations
	def, _ := json.Marshal(map[string]string{"message": `{{ $x := repeat 200000000 "a" }}{{ len $x }}`})
	eng := newTestEngine(t, map[string]string{"template.t": string(def)}, nil, nil)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := eng.Render(context.Background(), appFixture(), []string{"t"}, dest("slack", ""))
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrTemplateLimitExceeded) {
		t.Fatalf("want ErrTemplateLimitExceeded, got %v", err)
	}
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 32<<20 {
		t.Fatalf("render allocated %d MiB", delta>>20)
	}
}

// TestTemplateLimitsAllowNormalTemplates: ordinary templates keep the baseline output.
func TestTemplateLimitsAllowNormalTemplates(t *testing.T) {
	t.Parallel()
	tests := []struct{ tmpl, want string }{
		{`{{ range until 3 }}{{ . }}{{ end }}`, "012"},
		{`{{ until -3 }}`, "[0 -1 -2]"},
		{`{{ untilStep 0 10 3 }}|{{ untilStep 10 0 -3 }}|{{ untilStep 0 10 -1 }}`, "[0 3 6 9]|[10 7 4 1]|[]"},
		{`{{ untilStep 0 9223372036854775807 4611686018427387904 }}`, "[0 4611686018427387904]"},
		{`{{ seq 3 }}|{{ seq 2 4 }}|{{ seq 3 1 }}|{{ seq 0 2 6 }}|{{ seq 6 -2 0 }}|{{ seq 1 2 3 4 }}`, "1 2 3|2 3 4|3 2 1|0 2 4 6|6 4 2 0|"},
		{`{{ indent 2 "a\nb" }}|{{ nindent 1 "c" }}`, "  a\n  b|\n c"},
		{`{{ repeat 3 "ab" }}{{ "x" | repeat 2 }}`, "ababab" + "xx"},
		{`{{ printf "%05d|%-*d|%.2f|%[1]d" 42 4 7 3.14159 }}`, "00042|7   |3.14|42"},
		{`{{ print 1 2 }}|{{ println "a" }}|{{ html "<a>" }}|{{ js "'" }}|{{ urlquery "a b" }}`, "1 2|a\n|&lt;a&gt;|\\'|a+b"},
		{`{{ replace "a" "bb" "banana" }}|{{ regexReplaceAll "a(x*)b" "-ab-axxb-" "${1}W" }}|{{ regexReplaceAllLiteral "a" "aa" "$1" }}`, "bbbnbbnbb|-W-xxW-|$1$1"},
		{`{{ toJson (dict "a" (list 1 2) "b" "x") }}|{{ toPrettyJson .app.metadata.labels }}`, "{\"a\":[1,2],\"b\":\"x\"}|{\n  \"team\": \"a\"\n}"},
		{`{{ $d := dict }}{{ range until 300 }}{{ $_ := set $d (toString .) . }}{{ end }}{{ len $d }}`, "300"},
		{`{{ $l := list }}{{ range until 500 }}{{ $l = append $l . }}{{ end }}{{ len $l }}`, "500"},
		{`{{ $_ := set .app.metadata "x" "y" }}{{ .app.metadata.x }}`, "y"},
		{`{{ merge (dict "a" 1) (dict "b" 2) | toJson }}|{{ mergeOverwrite (dict "a" 1) (dict "a" 2) | toJson }}`, `{"a":1,"b":2}|{"a":2}`},
		{`{{ deepCopy .app.metadata.labels | toJson }}|{{ unset (dict "a" 1 "b" 2) "a" | toJson }}`, `{"team":"a"}|{"b":2}`},
		{`{{ call .strings.ReplaceAll "a-b" "-" "_" }}`, "a_b"},
		{`{{ define "item" }}<{{ . }}>{{ end }}{{ range list 1 2 }}{{ template "item" . }}{{ end }}`, "<1><2>"},
		{`{{ $s := splitList "," "a,b,c" }}{{ join "-" $s }}|{{ (split "/" "x/y")._1 }}|{{ wrapWith 2 "|" "abcd" }}`, "a-b-c|y|ab|cd"},
		{`{{ uniq (list 1 1 2) }}|{{ without (list 1 2 3) 2 }}|{{ len (randAlpha 5) }}|{{ quote "a" }}`, `[1 2]|[1 3]|5|"a"`},
	}
	for _, tc := range tests {
		t.Run(tc.tmpl, func(t *testing.T) {
			t.Parallel()
			got, err := renderMessage(t, tc.tmpl)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTemplateLimitsDefaultSteps: an unbounded loop stops at the default step limit.
func TestTemplateLimitsDefaultSteps(t *testing.T) {
	t.Parallel()
	_, err := renderMessage(t, `{{ range 100000000000 }}{{ end }}`)
	if !errors.Is(err, ErrTemplateLimitExceeded) || !strings.Contains(err.Error(), fmt.Sprint(DefaultTemplateMaxSteps)) {
		t.Fatalf("got %v", err)
	}
}

func TestTemplateLimitsConfigurable(t *testing.T) {
	t.Parallel()
	loop := `{{ range until 1000 }}{{ end }}done`
	if _, err := renderMessage(t, loop, WithTemplateLimits(TemplateLimits{MaxSteps: 100})); !errors.Is(err, ErrTemplateLimitExceeded) {
		t.Fatalf("MaxSteps: got %v", err)
	}
	if got, err := renderMessage(t, loop, WithTemplateLimits(TemplateLimits{MaxSteps: 2000})); err != nil || got != "done" {
		t.Fatalf("within MaxSteps: %q %v", got, err)
	}
	if _, err := renderMessage(t, `{{ repeat 2000 "a" }}`, WithTemplateLimits(TemplateLimits{MaxBytes: 1000})); !errors.Is(err, ErrTemplateLimitExceeded) {
		t.Fatalf("MaxBytes: got %v", err)
	}
}

func TestTemplateLimitsDeadlineAndCancellation(t *testing.T) {
	t.Parallel()
	b := newRenderBudget(context.Background(), TemplateLimits{Timeout: time.Second}, nil)
	now := time.Now()
	b.now = func() time.Time { return now }
	b.deadline = now.Add(time.Second)
	if err := b.step(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := b.step(); !errors.Is(err, ErrTemplateLimitExceeded) {
		t.Fatalf("deadline: got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	def, _ := json.Marshal(map[string]string{"message": `{{ range until 10 }}{{ . }}{{ end }}`})
	eng := newTestEngine(t, map[string]string{"template.t": string(def)}, nil, nil)
	if _, err := eng.Render(ctx, appFixture(), []string{"t"}, dest("slack", "")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: got %v", err)
	}
}

// TestTemplateMutationsAreIsolatedPerRender: a template modifying the application data
// (set/unset/merge) cannot influence the render for another destination.
func TestTemplateMutationsAreIsolatedPerRender(t *testing.T) {
	t.Parallel()
	svc := &recordingService{}
	cm := map[string]string{
		"trigger.on-any": "- when: 'true'\n  send: [mutating]",
		"template.mutating": "message: '{{ if hasKey .app.metadata \"seen\" }}seen{{ else }}fresh{{ end }}" +
			"{{ $_ := set .app.metadata \"seen\" true }}'",
	}
	eng := newTestEngine(t, cm, nil, map[string]services.Service{"slack": svc})
	app := appFixture(withAnnotation(testPrefix+"/subscribe.on-any.slack", "a;b;c"))
	_, res := process(t, eng, app, nil)
	if statuses(res) != "on-any/slack:a/Sent,on-any/slack:b/Sent,on-any/slack:c/Sent" {
		t.Fatalf("results %s", statuses(res))
	}
	for _, d := range svc.deliveries() {
		if d.n.Message != "fresh" {
			t.Fatalf("render for %s saw a mutation from another render: %q", d.dest, d.n.Message)
		}
	}
}

// TestBoundedSequencesMatchSprig: until/untilStep/seq replacements keep sprig's results.
func TestBoundedSequencesMatchSprig(t *testing.T) {
	t.Parallel()
	sp := sprig.TxtFuncMap()
	spUntil := sp["until"].(func(int) []int)
	spUntilStep := sp["untilStep"].(func(int, int, int) []int)
	spSeq := sp["seq"].(func(...int) string)
	rapid.Check(t, func(t *rapid.T) {
		start := rapid.IntRange(-50, 50).Draw(t, "start")
		stop := rapid.IntRange(-50, 50).Draw(t, "stop")
		step := rapid.IntRange(-7, 7).Draw(t, "step")
		if got, want := untilStep(start, stop, step), spUntilStep(start, stop, step); !reflect.DeepEqual(got, want) {
			t.Fatalf("untilStep(%d, %d, %d) = %v, want %v", start, stop, step, got, want)
		}
		if n := stepCount(int64(start), int64(stop), int64(step)); n != uint64(len(spUntilStep(start, stop, step))) {
			t.Fatalf("stepCount(%d, %d, %d) = %d", start, stop, step, n)
		}
		if got, want := until(stop), spUntil(stop); !reflect.DeepEqual(got, want) {
			t.Fatalf("until(%d) = %v, want %v", stop, got, want)
		}
		params := rapid.SliceOfN(rapid.IntRange(-20, 20), 0, 4).Draw(t, "params")
		if len(params) == 3 && params[1] == 0 {
			return // sprig loops forever on a zero step in seq
		}
		if got, want := seq(params...), spSeq(params...); got != want {
			t.Fatalf("seq(%v) = %q, want %q", params, got, want)
		}
	})
}

func TestPrintfPadding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		format string
		args   []any
		want   int64
	}{
		{"%d", []any{1}, 0},
		{"%05d|%-*d", []any{1, 7, 2}, 12},
		{"%.*f", []any{3, 1.0}, 3},
		{"%[2]*[1]d", []any{1, 9}, 9},
		{"%0999999d%0999999d", []any{1, 2}, 1999998},
		{"%01000001d", []any{1}, 0}, // fmt refuses widths over 1e6
		{"%%%d%%", []any{1}, 0},
	}
	for _, tc := range tests {
		if got := printfPadding(tc.format, tc.args); got != tc.want {
			t.Errorf("printfPadding(%q) = %d, want %d", tc.format, got, tc.want)
		}
		_ = fmt.Sprintf(tc.format, tc.args...)
	}
}

func TestBoundedReplaceAll(t *testing.T) {
	t.Parallel()
	if got, err := boundedReplaceAll("a-b-c", "-", "+"); err != nil || got != "a+b+c" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := boundedReplaceAll(strings.Repeat("x", 1000), "", strings.Repeat("y", 10000)); !errors.Is(err, ErrTemplateLimitExceeded) {
		t.Fatalf("got %v", err)
	}
	// Also refused in triggers.
	cm := map[string]string{
		"trigger.big":   "- when: len(strings.ReplaceAll(app.metadata.name, '', '" + strings.Repeat("y", 500000) + "')) > 0\n  send: [t]",
		"trigger.small": "- when: strings.ReplaceAll(app.metadata.name, 'book', 'x') == 'guestx'\n  send: [t]",
		"template.t":    "message: x",
	}
	eng := newTestEngine(t, cm, nil, nil)
	if res, err := eng.RunTrigger(context.Background(), "small", appFixture()); err != nil || len(res) != 1 || !res[0].Triggered {
		t.Fatalf("trigger results %+v %v", res, err)
	}
	res, err := eng.RunTrigger(context.Background(), "big", appFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Triggered || !errors.Is(res[0].Err, ErrTemplateLimitExceeded) {
		t.Fatalf("trigger results %+v", res)
	}
}
