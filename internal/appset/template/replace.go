package template

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	gotemplate "text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/valyala/fasttemplate"
)

// MaxRenderedBytes caps the output of one rendered string. It bounds output only; the work and
// the values a template builds are bounded by the evaluation limits (limits.go).
const MaxRenderedBytes = 1 << 20

// ErrOutputTooLarge is returned when a rendered string exceeds MaxRenderedBytes.
var ErrOutputTooLarge = errors.New("rendered template exceeds the size limit")

// Options selects the templating engine.
type Options struct {
	// GoTemplate selects Go text/template (spec.goTemplate); fasttemplate otherwise.
	GoTemplate bool
	// GoTemplateOptions are text/template options (spec.goTemplateOptions), e.g.
	// "missingkey=error". Ignored in fasttemplate mode.
	GoTemplateOptions []string
}

// isTemplated mirrors upstream's ".*{{.*}}.*" check: fasttemplate only renders strings with an
// opening and a closing tag on the same line.
var isTemplated = regexp.MustCompile(".*{{.*}}.*")

var (
	baseOnce sync.Once
	baseTmpl *gotemplate.Template
	funcMap  gotemplate.FuncMap
)

// FuncMap returns a copy of the Go template function map: Sprig's generic functions without
// env, expandenv and getHostByName, plus normalize, slugify, toYaml, fromYaml and fromYamlArray.
// These are the unbounded implementations; Replace executes them through the evaluation limits.
func FuncMap() gotemplate.FuncMap {
	initBase()
	out := make(gotemplate.FuncMap, len(funcMap))
	for k, v := range funcMap {
		out[k] = v
	}
	return out
}

func initBase() {
	baseOnce.Do(func() {
		m := sprig.GenericFuncMap()
		// Functions that reveal information about the controller's environment.
		delete(m, "env")
		delete(m, "expandenv")
		delete(m, "getHostByName")
		m["normalize"] = SanitizeName
		m["slugify"] = Slugify
		m["toYaml"] = toYAML
		m["fromYaml"] = fromYAML
		m["fromYamlArray"] = fromYAMLArray
		funcMap = gotemplate.FuncMap(m)
		baseTmpl = gotemplate.New("base").Funcs(funcMap)
	})
}

// Replace renders one templated string with the given parameters, as one evaluation (see the
// evaluation limits).
//
// fasttemplate: strings without "{{...}}" on one line are returned unchanged; each tag is
// replaced by the parameter whose key equals the trimmed tag content when that parameter is a
// string, and kept verbatim otherwise. Go templates: the string is parsed and executed against
// params; parse errors are "failed to parse template <tmpl>: ..." and execution errors "failed
// to execute go template <tmpl>: ...".
func Replace(tmpl string, params map[string]any, o Options) (string, error) {
	return newEvaluation().replace(tmpl, params, o)
}

func (e *evaluation) replace(tmpl string, params map[string]any, o Options) (string, error) {
	// A string without an action renders to itself in both engines; skip the work.
	if !strings.Contains(tmpl, "{{") {
		return tmpl, nil
	}
	if o.GoTemplate {
		return e.replaceGo(tmpl, params, o.GoTemplateOptions)
	}
	return e.replaceFast(tmpl, params)
}

func (e *evaluation) replaceGo(tmpl string, params map[string]any, options []string) (string, error) {
	initBase()
	t, err := baseTmpl.Clone()
	if err != nil {
		return "", fmt.Errorf("failed to clone base template: %w", err)
	}
	for _, opt := range options {
		if t, err = applyOption(t, opt); err != nil {
			return "", err
		}
	}
	parsed, err := t.Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", tmpl, err)
	}
	// Execute the bounded implementations of the functions in use, and charge every template
	// invocation and range iteration.
	parsed.Funcs(e.funcs(instrument(parsed)))
	w := e.newBuffer()
	if err := parsed.Execute(w, params); err != nil {
		return "", fmt.Errorf("failed to execute go template %s: %w", tmpl, err)
	}
	if err := e.output(w.Len()); err != nil {
		return "", err
	}
	return w.String(), nil
}

// applyOption calls Template.Option, which panics on unknown options, and converts the panic
// into an error so an invalid goTemplateOptions entry cannot crash the controller.
func applyOption(t *gotemplate.Template, opt string) (out *gotemplate.Template, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = fmt.Errorf("invalid goTemplateOptions entry %q: %v", opt, r)
		}
	}()
	return t.Option(opt), nil
}

func (e *evaluation) replaceFast(tmpl string, params map[string]any) (string, error) {
	if !isTemplated.MatchString(tmpl) {
		return tmpl, nil
	}
	ft, err := fasttemplate.NewTemplate(tmpl, "{{", "}}")
	if err != nil {
		return "", fmt.Errorf("invalid template: %w", err)
	}
	// Written to a bounded buffer as it renders: many tags referring to a large parameter
	// cannot build an oversized string first.
	w := e.newBuffer()
	if _, err := ft.ExecuteFunc(w, func(w io.Writer, tag string) (int, error) {
		trimmed := strings.TrimSpace(tag)
		replacement, ok := params[trimmed].(string)
		if trimmed == "" || !ok {
			return fmt.Fprintf(w, "{{%s}}", tag)
		}
		return io.WriteString(w, replacement)
	}); err != nil {
		if errors.Is(err, ErrOutputTooLarge) || errors.Is(err, ErrLimitExceeded) {
			return "", err
		}
		return "", fmt.Errorf("invalid template: %w", err)
	}
	if err := e.output(w.Len()); err != nil {
		return "", err
	}
	return w.String(), nil
}

// newBuffer returns the output buffer of one string: at most MaxRenderedBytes and what remains
// of the evaluation's output.
func (e *evaluation) newBuffer() *limitedBuffer {
	if rest := e.remainingOutput(); rest < MaxRenderedBytes {
		return &limitedBuffer{max: max(rest, 0), err: limitErr("rendered output exceeds %d bytes", MaxEvaluationBytes)}
	}
	return &limitedBuffer{max: MaxRenderedBytes, err: fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, MaxRenderedBytes)}
}

// limitedBuffer is an io.Writer that fails with err once more than max bytes were written.
type limitedBuffer struct {
	bytes.Buffer
	max int
	err error
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, b.err
	}
	return b.Buffer.Write(p)
}

// IsMissingKeyError reports whether err is a text/template missing-key or missing-field
// error (used to keep values that refer to parameters not yet available).
func IsMissingKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "map has no entry for key") || strings.Contains(msg, "can't evaluate field")
}
