package settings

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Component is a Patrol CD process that reads patrol-cmd-params-cm.
type Component string

// Components (patrolcd subcommands; ComponentDex is the optional Dex launcher).
const (
	ComponentController              Component = "controller"
	ComponentServer                  Component = "server"
	ComponentRepoServer              Component = "repo-server"
	ComponentAppSetController        Component = "appset-controller"
	ComponentNotificationsController Component = "notifications-controller"
	ComponentCommitServer            Component = "commit-server"
	ComponentDex                     Component = "dex"
)

// Components returns every component in a stable order.
func Components() []Component {
	return []Component{
		ComponentController, ComponentServer, ComponentRepoServer, ComponentAppSetController,
		ComponentNotificationsController, ComponentCommitServer, ComponentDex,
	}
}

// ParamType is the value type of a parameter.
type ParamType string

// Parameter types and their parsing (upstream util/env semantics):
const (
	// ParamString is used verbatim.
	ParamString ParamType = "string"
	// ParamBool accepts "true"/"false" in any case.
	ParamBool ParamType = "bool"
	// ParamInt is a base-10 integer.
	ParamInt ParamType = "int"
	// ParamFloat is a floating point number.
	ParamFloat ParamType = "float"
	// ParamDuration is a Go duration or "<n><s|m|h|d>".
	ParamDuration ParamType = "duration"
	// ParamQuantity is a Kubernetes quantity ("10M", "1G"); the value is in bytes.
	ParamQuantity ParamType = "quantity"
	// ParamStringList is split on Separator with items trimmed.
	ParamStringList ParamType = "list"
	// ParamStringMap is "k1=v1<sep>k2=v2" with keys and values trimmed.
	ParamStringMap ParamType = "map"
	// ParamLogLevel is a logrus level name in any letter case (upstream passes the value to
	// logrus.ParseLevel). The value is the log/slog level name used for it: "warning" is
	// "warn", "trace" is "debug", and "fatal" and "panic" are "error" (slog has no such
	// levels; the last three are reported as warnings).
	ParamLogLevel ParamType = "loglevel"
)

// logLevels maps the logrus level names (upstream logrus.ParseLevel) to slog level names.
var logLevels = map[string]string{
	"panic": "error", "fatal": "error", "error": "error", "warn": "warn", "warning": "warn",
	"info": "info", "debug": "debug", "trace": "debug",
}

// logLevelNames lists the logrus level names in logrus order.
var logLevelNames = []string{"panic", "fatal", "error", "warn", "warning", "info", "debug", "trace"}

// ParamSpec describes how one patrol-cmd-params-cm key reaches one component.
type ParamSpec struct {
	// Key is the patrol-cmd-params-cm key (identical to the argocd-cmd-params-cm key).
	Key string `json:"key"`
	// Component reads the key.
	Component Component `json:"component"`
	// Env is the environment variable the key is projected into by the install manifests and
	// that the component reads (ARGOCD_ renamed to PATROL_). Empty when the key is not read
	// from the environment (profile.enabled keys are projected as a file).
	Env string `json:"env,omitempty"`
	// UpstreamEnv is the Argo CD environment variable.
	UpstreamEnv string `json:"upstreamEnv,omitempty"`
	// Flag is the command-line flag whose default the key sets ("" when env only).
	Flag string `json:"flag,omitempty"`
	// Type is the value type.
	Type ParamType `json:"type"`
	// Default is the compiled default in the type's syntax.
	Default string `json:"default"`
	// Min and Max bound numeric and duration values ("" = unbounded).
	Min string `json:"min,omitempty"`
	Max string `json:"max,omitempty"`
	// Allowed lists the valid values of an enumerated string ("" included when allowed), or
	// the accepted names of a ParamLogLevel.
	Allowed []string `json:"allowed,omitempty"`
	// CaseInsensitive makes an enumerated string match Allowed in any letter case; the value
	// is then the Allowed spelling (upstream lower-cases *.log.format).
	CaseInsensitive bool `json:"caseInsensitive,omitempty"`
	// Separator splits list and map values.
	Separator string `json:"separator,omitempty"`
	// AllowEmpty makes an explicitly empty value override the default.
	AllowEmpty bool `json:"allowEmpty,omitempty"`
	// NotApplicable explains why the key has no effect in Patrol CD.
	NotApplicable string `json:"notApplicable,omitempty"`
	// Deprecated explains a deprecation.
	Deprecated string `json:"deprecated,omitempty"`
	// Description is a one-line summary.
	Description string `json:"description"`
}

// ParamSource tells where a resolved value came from.
type ParamSource string

// Parameter value sources, in decreasing precedence (a command-line flag, applied by the
// caller, overrides all of them).
const (
	ParamFromEnv       ParamSource = "env"
	ParamFromConfigMap ParamSource = "configmap"
	ParamFromDefault   ParamSource = "default"
)

// ParamValue is one resolved parameter.
type ParamValue struct {
	Spec ParamSpec
	// Raw is the configured text ("" for defaults).
	Raw string
	// Source is where Raw came from; ParamFromDefault when the default is used (also when
	// the configured value was invalid).
	Source ParamSource
	value  any
}

// ParamsForComponent returns the parameters of component c, sorted by key. Command wiring
// uses them to set flag defaults: for each spec, the flag default is the resolved value of
// spec.Key (see ResolveParams).
func ParamsForComponent(c Component) []ParamSpec {
	var out []ParamSpec
	for _, d := range paramDefs {
		for _, ce := range d.envs {
			if ce.c == c {
				out = append(out, d.spec(ce))
			}
		}
	}
	slices.SortFunc(out, func(a, b ParamSpec) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

// ParamSpecs returns the parameters of every component.
func ParamSpecs() []ParamSpec {
	var out []ParamSpec
	for _, c := range Components() {
		out = append(out, ParamsForComponent(c)...)
	}
	return out
}

// ParamKeys returns every patrol-cmd-params-cm key, sorted.
func ParamKeys() []string {
	keys := make([]string, 0, len(paramDefs))
	for _, d := range paramDefs {
		keys = append(keys, d.key)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// Params are the resolved parameters of one component.
type Params struct {
	component Component
	values    map[string]ParamValue
}

// ResolveParams resolves the parameters of component c. Precedence: the environment
// variable (when set, as the install manifests project patrol-cmd-params-cm into it), then
// cmData (patrol-cmd-params-cm read directly), then the compiled default. As upstream, an
// empty value selects the default unless the spec allows empty values. Invalid values select
// the default and are reported with upstream's wording (warning), except values upstream
// treats as fatal (quantities, enumerations), which are errors that command wiring should
// treat as startup failures. lookupEnv may be nil (os.LookupEnv is not consulted then).
func ResolveParams(c Component, cmData map[string]string, lookupEnv func(string) (string, bool)) (*Params, []Issue) {
	ps := &Params{component: c, values: map[string]ParamValue{}}
	var issues []Issue
	issue := func(key string, sev Severity, format string, args ...any) {
		issues = append(issues, Issue{Source: SourceCmdParamsConfigMap, Key: key, Severity: sev, Message: fmt.Sprintf(format, args...)})
	}
	for _, spec := range ParamsForComponent(c) {
		pv := ParamValue{Spec: spec, Source: ParamFromDefault}
		raw, src, set := "", ParamFromDefault, false
		if lookupEnv != nil && spec.Env != "" {
			if v, ok := lookupEnv(spec.Env); ok {
				raw, src, set = v, ParamFromEnv, true
			}
		}
		if !set {
			if v, ok := cmData[spec.Key]; ok {
				raw, src, set = v, ParamFromConfigMap, true
			}
		}
		name := spec.Env
		if src == ParamFromConfigMap || name == "" {
			name = spec.Key
		}
		if set && spec.NotApplicable != "" && raw != "" {
			issue(spec.Key, SeverityWarning, "%s has no effect in Patrol CD: %s", name, spec.NotApplicable)
		}
		if set && spec.Deprecated != "" && raw != "" {
			issue(spec.Key, SeverityWarning, "%s is deprecated: %s", name, spec.Deprecated)
		}
		def, err := parseParam(spec, spec.Default)
		if err != nil {
			// The table is covered by tests; keep going with the zero value.
			issue(spec.Key, SeverityError, "internal error: invalid default for %s: %v", spec.Key, err)
		}
		pv.value = def
		if set && (raw != "" || spec.AllowEmpty) {
			v, sev, msg := parseConfigured(spec, name, raw)
			if msg != "" {
				issue(spec.Key, sev, "%s", msg)
			} else {
				pv.value, pv.Raw, pv.Source = v, raw, src
				if used, ok := v.(string); ok && spec.Type == ParamLogLevel && approximatedLogLevel(raw) {
					issue(spec.Key, SeverityWarning, "log level '%s' in %s is used as '%s': Patrol CD logs with log/slog, which has no %s level", raw, name, used, strings.ToLower(raw))
				}
			}
		}
		ps.values[spec.Key] = pv
	}
	for _, k := range slices.Sorted(maps.Keys(cmData)) {
		if _, ok := LookupKey(SourceCmdParamsConfigMap, k); !ok {
			issue(k, SeverityWarning, "unknown key is ignored")
		}
	}
	return ps, issues
}

// parseConfigured parses a configured value, returning a message (with severity) when it is
// rejected and the default applies.
func parseConfigured(spec ParamSpec, name, raw string) (any, Severity, string) {
	v, err := parseParam(spec, raw)
	if err == nil {
		return v, "", ""
	}
	switch spec.Type {
	case ParamInt:
		if _, perr := strconv.ParseInt(raw, 10, 64); perr != nil {
			return nil, SeverityWarning, fmt.Sprintf("Could not parse '%s' as a number from environment %s", raw, name)
		}
		return nil, SeverityWarning, fmt.Sprintf("Value in %s is %s, %v", name, raw, err)
	case ParamFloat:
		if _, perr := strconv.ParseFloat(raw, 64); perr != nil {
			return nil, SeverityWarning, fmt.Sprintf("Could not parse '%s' as a float64 from environment %s", raw, name)
		}
		return nil, SeverityWarning, fmt.Sprintf("Value in %s is %s, %v", name, raw, err)
	case ParamDuration:
		if _, perr := parseDuration(raw); perr != nil {
			return nil, SeverityWarning, fmt.Sprintf("Could not parse '%s' as a duration from environment %s", raw, name)
		}
		return nil, SeverityWarning, fmt.Sprintf("Value in %s is %s, %v", name, raw, err)
	case ParamBool:
		return nil, SeverityWarning, fmt.Sprintf("Could not parse '%s' as a boolean from environment %s; using the default %s", raw, name, spec.Default)
	case ParamStringMap:
		var dup *duplicateKeyError
		if errors.As(err, &dup) {
			return nil, SeverityWarning, fmt.Sprintf("Duplicate key '%s' when parsing environment '%s' as a string map", dup.key, name)
		}
		return nil, SeverityWarning, fmt.Sprintf("Invalid key-value pair when parsing environment '%s' as a string map", name)
	case ParamQuantity:
		return nil, SeverityError, fmt.Sprintf("invalid quantity '%s' in %s: %v", raw, name, err)
	default:
		return nil, SeverityError, fmt.Sprintf("invalid value '%s' in %s: %v", raw, name, err)
	}
}

// parseParam parses raw according to spec.
func parseParam(spec ParamSpec, raw string) (any, error) {
	switch spec.Type {
	case ParamString:
		if len(spec.Allowed) == 0 {
			return raw, nil
		}
		for _, a := range spec.Allowed {
			if a == raw || (spec.CaseInsensitive && strings.EqualFold(a, raw)) {
				return a, nil
			}
		}
		return nil, fmt.Errorf("must be one of %s", quoteList(spec.Allowed))
	case ParamLogLevel:
		lvl, ok := logLevels[strings.ToLower(raw)]
		if !ok {
			// Upstream wording (logrus.ParseLevel).
			return nil, upstreamErrorf("not a valid logrus Level: %q", raw)
		}
		return lvl, nil
	case ParamBool:
		switch {
		case strings.EqualFold(raw, "true"):
			return true, nil
		case strings.EqualFold(raw, "false"):
			return false, nil
		}
		return nil, fmt.Errorf("not a boolean")
	case ParamInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		if spec.Min != "" {
			if lo, _ := strconv.ParseInt(spec.Min, 10, 64); n < lo {
				return nil, fmt.Errorf("which is less than minimum %d allowed", lo)
			}
		}
		if spec.Max != "" {
			if hi, _ := strconv.ParseInt(spec.Max, 10, 64); n > hi {
				return nil, fmt.Errorf("which is greater than maximum %d allowed", hi)
			}
		}
		return n, nil
	case ParamFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("not a finite number")
		}
		if spec.Min != "" {
			if lo, _ := strconv.ParseFloat(spec.Min, 64); f < lo {
				return nil, fmt.Errorf("which is less than minimum %v allowed", lo)
			}
		}
		return f, nil
	case ParamDuration:
		d, err := parseDuration(raw)
		if err != nil {
			return nil, err
		}
		if d < 0 {
			return nil, fmt.Errorf("which is less than minimum 0s allowed")
		}
		if spec.Min != "" {
			if lo, _ := parseDuration(spec.Min); d < lo {
				return nil, fmt.Errorf("which is less than minimum %s allowed", lo)
			}
		}
		if spec.Max != "" {
			if hi, _ := parseDuration(spec.Max); d > hi {
				return nil, fmt.Errorf("which is greater than maximum %s allowed", hi)
			}
		}
		return d, nil
	case ParamQuantity:
		q, err := resource.ParseQuantity(raw)
		if err != nil {
			return nil, err
		}
		if q.Sign() < 0 {
			return nil, fmt.Errorf("must not be negative")
		}
		return q.Value(), nil
	case ParamStringList:
		if raw == "" {
			return []string{}, nil
		}
		items := strings.Split(raw, cmp.Or(spec.Separator, ","))
		for i := range items {
			items[i] = strings.TrimSpace(items[i])
		}
		return items, nil
	case ParamStringMap:
		out := map[string]string{}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return out, nil
		}
		for pair := range strings.SplitSeq(raw, cmp.Or(spec.Separator, ",")) {
			kv := strings.Split(pair, "=")
			if len(kv) != 2 {
				return nil, errInvalidPair
			}
			k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
			if _, dup := out[k]; dup {
				return nil, &duplicateKeyError{key: k}
			}
			out[k] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown parameter type %q", spec.Type)
}

// approximatedLogLevel reports whether the logrus level name raw has no slog equivalent.
func approximatedLogLevel(raw string) bool {
	switch strings.ToLower(raw) {
	case "trace", "fatal", "panic":
		return true
	}
	return false
}

var errInvalidPair = errors.New("invalid key-value pair")

type duplicateKeyError struct{ key string }

func (e *duplicateKeyError) Error() string { return fmt.Sprintf("duplicate key %q", e.key) }

func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, it := range items {
		q[i] = strconv.Quote(it)
	}
	return strings.Join(q, ", ")
}

// Component returns the component the parameters were resolved for.
func (p *Params) Component() Component { return p.component }

// Lookup returns the resolved value of key.
func (p *Params) Lookup(key string) (ParamValue, bool) {
	v, ok := p.values[key]
	return v, ok
}

// Values returns every resolved value, sorted by key.
func (p *Params) Values() []ParamValue {
	out := make([]ParamValue, 0, len(p.values))
	for _, k := range slices.Sorted(maps.Keys(p.values)) {
		out = append(out, p.values[k])
	}
	return out
}

// String returns a string parameter ("" for unknown keys or other types).
func (p *Params) String(key string) string { v, _ := p.values[key].value.(string); return v }

// Bool returns a bool parameter.
func (p *Params) Bool(key string) bool { v, _ := p.values[key].value.(bool); return v }

// Int returns an int parameter.
func (p *Params) Int(key string) int64 { v, _ := p.values[key].value.(int64); return v }

// Float returns a float parameter.
func (p *Params) Float(key string) float64 { v, _ := p.values[key].value.(float64); return v }

// Duration returns a duration parameter.
func (p *Params) Duration(key string) time.Duration {
	v, _ := p.values[key].value.(time.Duration)
	return v
}

// Quantity returns a quantity parameter in bytes.
func (p *Params) Quantity(key string) int64 { v, _ := p.values[key].value.(int64); return v }

// Strings returns a list parameter.
func (p *Params) Strings(key string) []string {
	v, _ := p.values[key].value.([]string)
	return slices.Clone(v)
}

// StringMap returns a map parameter.
func (p *Params) StringMap(key string) map[string]string {
	v, _ := p.values[key].value.(map[string]string)
	return maps.Clone(v)
}

// FlagDefault returns the resolved value of key formatted for a flag default.
func (p *Params) FlagDefault(key string) string {
	v, ok := p.values[key]
	if !ok {
		return ""
	}
	switch x := v.value.(type) {
	case []string:
		return strings.Join(x, ",")
	case map[string]string:
		parts := make([]string, 0, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			parts = append(parts, k+"="+x[k])
		}
		return strings.Join(parts, ",")
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// parseCmdParams records patrol-cmd-params-cm and reports invalid values for every
// component (the values are resolved per component by ResolveParams).
func (p *parser) parseCmdParams() {
	src := SourceCmdParamsConfigMap
	data := p.cmData(src)
	if data == nil {
		return
	}
	p.s.cmdParams = data
	for k, v := range data {
		p.accept(src, k, v)
	}
	seen := map[string]bool{}
	for _, c := range Components() {
		_, issues := ResolveParams(c, data, nil)
		for _, is := range issues {
			id := is.Key + "\x00" + is.Message
			if seen[id] {
				continue
			}
			seen[id] = true
			p.addIssue(src, is.Key, is.Severity, is.Message)
		}
	}
}
