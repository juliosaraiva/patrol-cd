package notifications

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	texttemplate "text/template"

	"github.com/Masterminds/sprig/v3"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// maxRenderedFieldBytes bounds the output of any single template field.
const maxRenderedFieldBytes = 1 << 20

// Default templates applied to service sections that leave these fields empty (baseline
// behavior).
const (
	defaultRepoURLTemplate   = "{{.app.spec.source.repoURL}}"
	defaultRevisionTemplate  = "{{.app.status.operationState.syncResult.revision}}"
	defaultNewRelicChangelog = "{{(call .repo.GetCommitMetadata .app.status.sync.revision).Message}}"
	defaultNewRelicUser      = "{{(call .repo.GetCommitMetadata .app.status.sync.revision).Author}}"
)

// templateFuncs is sprig's text function map without functions that read the controller
// environment (env, expandenv) or perform network lookups (getHostByName).
var templateFuncs = func() texttemplate.FuncMap {
	f := sprig.TxtFuncMap()
	delete(f, "env")
	delete(f, "expandenv")
	delete(f, "getHostByName")
	return f
}()

// TemplateFuncNames returns the names of the functions available to templates (besides the
// text/template builtins).
func TemplateFuncNames() []string {
	out := make([]string, 0, len(templateFuncs))
	for k := range templateFuncs {
		out = append(out, k)
	}
	return out
}

type renderFn func(vars map[string]any) (reflect.Value, error)

type compiledSection struct {
	field  int
	isMap  bool
	render renderFn
}

type compiledTemplate struct {
	name     string
	message  renderFn
	sections []compiledSection
}

var notificationType = reflect.TypeOf(services.Notification{})

// applyTemplateDefaults fills default field templates of present sections.
func applyTemplateDefaults(name string, n services.Notification) services.Notification {
	if n.GitHub != nil {
		gh := *n.GitHub
		if gh.RepoURLPath == "" {
			gh.RepoURLPath = defaultRepoURLTemplate
		}
		if gh.RevisionPath == "" {
			gh.RevisionPath = defaultRevisionTemplate
		}
		n.GitHub = &gh
	}
	if n.NewRelic != nil {
		nr := *n.NewRelic
		if nr.Revision == "" {
			nr.Revision = defaultRevisionTemplate
		}
		if nr.Changelog == "" {
			nr.Changelog = defaultNewRelicChangelog
		}
		if nr.User == "" {
			nr.User = defaultNewRelicUser
		}
		n.NewRelic = &nr
	}
	if n.Alertmanager != nil {
		am := *n.Alertmanager
		labels := make(map[string]string, len(am.Labels)+1)
		for k, v := range am.Labels {
			labels[k] = v
		}
		if _, ok := labels["alertname"]; !ok {
			labels["alertname"] = name
		}
		am.Labels = labels
		if am.GeneratorURL == "" {
			am.GeneratorURL = defaultRepoURLTemplate
		}
		n.Alertmanager = &am
	}
	return n
}

// validateTemplate reports definitions the baseline rejects when building templates.
func validateTemplate(def services.Notification) error {
	if gh := def.GitHub; gh != nil && gh.Status == nil && gh.Deployment == nil && gh.PullRequestComment == nil && gh.CheckRun == nil {
		return errors.New("github: at least one of {status, deployment, pullRequestComment, checkRun} must be specified")
	}
	return nil
}

// compileTemplate parses every string of the template definition once.
func compileTemplate(name string, def services.Notification) (*compiledTemplate, error) {
	if err := validateTemplate(def); err != nil {
		return nil, err
	}
	def = applyTemplateDefaults(name, def)
	v := reflect.ValueOf(def)
	ct := &compiledTemplate{name: name}
	for i := 0; i < notificationType.NumField(); i++ {
		f := notificationType.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if f.Name == "Message" {
			fn, err := compileValue(name, fv)
			if err != nil {
				return nil, fmt.Errorf("message: %w", err)
			}
			ct.message = fn
			continue
		}
		if fv.IsZero() {
			continue
		}
		fn, err := compileValue(name, fv)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", jsonName(f), err)
		}
		ct.sections = append(ct.sections, compiledSection{field: i, isMap: fv.Kind() == reflect.Map, render: fn})
	}
	return ct, nil
}

func jsonName(f reflect.StructField) string {
	if tag := f.Tag.Get("json"); tag != "" {
		if n, _, _ := strings.Cut(tag, ","); n != "" {
			return n
		}
	}
	return f.Name
}

// compileValue returns a renderer mirroring v's structure in which every string is the
// output of executing it as a template. Map keys and non-string scalars are copied.
func compileValue(name string, v reflect.Value) (renderFn, error) {
	switch v.Kind() {
	case reflect.String:
		src := v.String()
		typ := v.Type()
		if !strings.Contains(src, "{{") {
			out := reflect.ValueOf(src).Convert(typ)
			return func(map[string]any) (reflect.Value, error) { return out, nil }, nil
		}
		t, err := texttemplate.New(name).Funcs(templateFuncs).Parse(src)
		if err != nil {
			return nil, err
		}
		return func(vars map[string]any) (reflect.Value, error) {
			var buf limitedBuffer
			if err := t.Execute(&buf, vars); err != nil {
				return reflect.Value{}, err
			}
			return reflect.ValueOf(buf.String()).Convert(typ), nil
		}, nil
	case reflect.Pointer:
		typ := v.Type()
		if v.IsNil() {
			zero := reflect.Zero(typ)
			return func(map[string]any) (reflect.Value, error) { return zero, nil }, nil
		}
		elem, err := compileValue(name, v.Elem())
		if err != nil {
			return nil, err
		}
		return func(vars map[string]any) (reflect.Value, error) {
			ev, err := elem(vars)
			if err != nil {
				return reflect.Value{}, err
			}
			p := reflect.New(typ.Elem())
			p.Elem().Set(ev)
			return p, nil
		}, nil
	case reflect.Struct:
		typ := v.Type()
		fields := make([]renderFn, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			if !typ.Field(i).IsExported() {
				continue
			}
			fn, err := compileValue(name, v.Field(i))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", jsonName(typ.Field(i)), err)
			}
			fields[i] = fn
		}
		return func(vars map[string]any) (reflect.Value, error) {
			out := reflect.New(typ).Elem()
			for i, fn := range fields {
				if fn == nil {
					continue
				}
				fv, err := fn(vars)
				if err != nil {
					return reflect.Value{}, err
				}
				out.Field(i).Set(fv)
			}
			return out, nil
		}, nil
	case reflect.Slice:
		typ := v.Type()
		if v.IsNil() {
			zero := reflect.Zero(typ)
			return func(map[string]any) (reflect.Value, error) { return zero, nil }, nil
		}
		elems := make([]renderFn, v.Len())
		for i := 0; i < v.Len(); i++ {
			fn, err := compileValue(name, v.Index(i))
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			elems[i] = fn
		}
		return func(vars map[string]any) (reflect.Value, error) {
			out := reflect.MakeSlice(typ, len(elems), len(elems))
			for i, fn := range elems {
				ev, err := fn(vars)
				if err != nil {
					return reflect.Value{}, err
				}
				out.Index(i).Set(ev)
			}
			return out, nil
		}, nil
	case reflect.Map:
		typ := v.Type()
		if v.IsNil() {
			zero := reflect.Zero(typ)
			return func(map[string]any) (reflect.Value, error) { return zero, nil }, nil
		}
		type entry struct {
			key reflect.Value
			fn  renderFn
		}
		var entries []entry
		iter := v.MapRange()
		for iter.Next() {
			fn, err := compileValue(name, iter.Value())
			if err != nil {
				return nil, fmt.Errorf("%v: %w", iter.Key().Interface(), err)
			}
			entries = append(entries, entry{key: iter.Key(), fn: fn})
		}
		return func(vars map[string]any) (reflect.Value, error) {
			out := reflect.MakeMapWithSize(typ, len(entries))
			for _, e := range entries {
				ev, err := e.fn(vars)
				if err != nil {
					return reflect.Value{}, err
				}
				out.SetMapIndex(e.key, ev)
			}
			return out, nil
		}, nil
	default:
		cp := reflect.New(v.Type()).Elem()
		cp.Set(v)
		return func(map[string]any) (reflect.Value, error) { return cp, nil }, nil
	}
}

// apply renders the template into out. Message is overwritten only when the rendered
// message is non-empty; every section present in the template replaces the corresponding
// section of out (webhook entries are merged per webhook name).
func (ct *compiledTemplate) apply(out *services.Notification, vars map[string]any) error {
	mv, err := ct.message(vars)
	if err != nil {
		return err
	}
	if msg := mv.String(); msg != "" {
		out.Message = msg
	}
	dst := reflect.ValueOf(out).Elem()
	for _, s := range ct.sections {
		rv, err := s.render(vars)
		if err != nil {
			return err
		}
		field := dst.Field(s.field)
		if s.isMap {
			if field.IsNil() {
				field.Set(reflect.MakeMap(field.Type()))
			}
			iter := rv.MapRange()
			for iter.Next() {
				field.SetMapIndex(iter.Key(), iter.Value())
			}
			continue
		}
		field.Set(rv)
	}
	return nil
}

// limitedBuffer is a bytes.Buffer that refuses to grow past maxRenderedFieldBytes.
type limitedBuffer struct {
	buf bytes.Buffer
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > maxRenderedFieldBytes {
		return 0, ErrTemplateOutputTooLarge
	}
	return l.buf.Write(p)
}

func (l *limitedBuffer) String() string { return l.buf.String() }
