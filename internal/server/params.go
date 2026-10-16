package server

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/pflag"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// ParamFlags binds the patrol-cmd-params-cm parameters of one component to command-line
// flags and resolves them with the precedence
//
//	flag > environment variable > patrol-cmd-params-cm > compiled default.
//
// Every parameter with a flag name in the settings register (settings.ParamsForComponent)
// becomes a flag; parameters without one are read from the environment and the ConfigMap
// only. Flag values are validated by the same parser as every other source, and an invalid
// flag value is an error (other sources fall back to the default with a warning, as
// upstream).
type ParamFlags struct {
	component settings.Component
	specs     []settings.ParamSpec
	// values holds the flag of each flagged key (several keys may share a flag name).
	values map[string]*paramValue
}

// paramValue is the pflag.Value of one parameter flag.
type paramValue struct {
	spec settings.ParamSpec
	raw  string
	set  bool
}

func (v *paramValue) String() string {
	if v.set {
		return v.raw
	}
	return v.spec.Default
}

func (v *paramValue) Set(s string) error {
	v.raw, v.set = s, true
	return nil
}

// Type names the value type in --help.
func (v *paramValue) Type() string {
	switch v.spec.Type {
	case settings.ParamBool:
		return "bool"
	case settings.ParamInt:
		return "int"
	case settings.ParamFloat:
		return "float"
	case settings.ParamDuration:
		return "duration"
	case settings.ParamQuantity:
		return "quantity"
	case settings.ParamStringList:
		return "strings"
	case settings.ParamStringMap:
		return "key=value"
	case settings.ParamLogLevel:
		return "level"
	default:
		return "string"
	}
}

// BindParamFlags registers the parameter flags of component c on fs. Flags already
// defined on fs (by the command itself) are left alone and their parameter is then only
// read from the environment and the ConfigMap.
func BindParamFlags(fs *pflag.FlagSet, c settings.Component) *ParamFlags {
	pf := &ParamFlags{component: c, specs: settings.ParamsForComponent(c), values: map[string]*paramValue{}}
	byFlag := map[string]*paramValue{}
	for _, spec := range pf.specs {
		if spec.Flag == "" {
			continue
		}
		if v, ok := byFlag[spec.Flag]; ok {
			pf.values[spec.Key] = v
			continue
		}
		if fs.Lookup(spec.Flag) != nil {
			continue
		}
		v := &paramValue{spec: spec}
		byFlag[spec.Flag] = v
		pf.values[spec.Key] = v
		f := fs.VarPF(v, spec.Flag, "", paramUsage(spec))
		if spec.Type == settings.ParamBool {
			f.NoOptDefVal = "true"
		}
		if spec.Deprecated != "" {
			f.Hidden = true
			f.Deprecated = spec.Deprecated
		}
		if spec.NotApplicable != "" {
			f.Hidden = true
		}
	}
	return pf
}

// paramUsage documents a parameter flag: description, environment variable and key.
func paramUsage(spec settings.ParamSpec) string {
	var b strings.Builder
	b.WriteString(upperFirst(spec.Description))
	if len(spec.Allowed) > 0 && spec.Type != settings.ParamLogLevel {
		b.WriteString(" (one of ")
		var allowed []string
		for _, a := range spec.Allowed {
			if a == "" {
				a = `""`
			}
			allowed = append(allowed, a)
		}
		b.WriteString(strings.Join(allowed, ", "))
		b.WriteString(")")
	}
	b.WriteString("; ")
	if spec.Env != "" {
		b.WriteString("env " + spec.Env + ", ")
	}
	b.WriteString("patrol-cmd-params-cm " + spec.Key)
	return b.String()
}

// upperFirst capitalizes a description, leaving acronym-like words ("mTLS") alone.
func upperFirst(s string) string {
	if s == "" || (len(s) > 1 && strings.ToUpper(s[1:2]) == s[1:2] && strings.ToLower(s[1:2]) != s[1:2]) {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Component returns the component the flags belong to.
func (p *ParamFlags) Component() settings.Component { return p.component }

// ChangedKeys returns the keys whose value was given on the command line, sorted.
func (p *ParamFlags) ChangedKeys() []string {
	var out []string
	for k, v := range p.values {
		if v.set {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// Resolve resolves the parameters from the flags, lookupEnv (os.LookupEnv; nil: no
// environment) and cmData (the data of patrol-cmd-params-cm; nil when it was not read).
// It returns the parameters, the issues of the environment and ConfigMap values (warnings
// are to be logged), and an error for invalid flag values and for values upstream treats as
// fatal (invalid quantities and enumerations).
func (p *ParamFlags) Resolve(cmData map[string]string, lookupEnv func(string) (string, bool)) (*settings.Params, []settings.Issue, error) {
	data := maps.Clone(cmData)
	if data == nil {
		data = map[string]string{}
	}
	envOverride := map[string]string{}
	fromFlag := map[string]bool{}
	for key, v := range p.values {
		if !v.set {
			continue
		}
		fromFlag[key] = true
		spec := v.spec
		if s, ok := p.spec(key); ok {
			spec = s
		}
		if spec.Env != "" {
			envOverride[spec.Env] = v.raw
		} else {
			data[key] = v.raw
		}
	}
	lookup := func(name string) (string, bool) {
		if s, ok := envOverride[name]; ok {
			return s, true
		}
		if lookupEnv != nil {
			return lookupEnv(name)
		}
		return "", false
	}
	params, issues := settings.ResolveParams(p.component, data, lookup)
	var errs []error
	var rest []settings.Issue
	for _, is := range issues {
		switch {
		case fromFlag[is.Key] && is.Severity != settings.SeverityWarning:
			errs = append(errs, fmt.Errorf("flag --%s: %s", p.values[is.Key].spec.Flag, is.Message))
		case fromFlag[is.Key] && isInvalidValueMessage(is.Message):
			errs = append(errs, fmt.Errorf("invalid value for flag --%s: %s", p.values[is.Key].spec.Flag, is.Message))
		case is.Severity == settings.SeverityError:
			errs = append(errs, fmt.Errorf("%s: %s", is.Key, is.Message))
		default:
			rest = append(rest, is)
		}
	}
	return params, rest, errors.Join(errs...)
}

// isInvalidValueMessage reports whether a warning rejected the configured value (as opposed
// to a deprecation or no-effect notice, which keep the value).
func isInvalidValueMessage(msg string) bool {
	for _, p := range []string{"Could not parse", "Value in ", "Invalid key-value pair", "Duplicate key"} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

func (p *ParamFlags) spec(key string) (settings.ParamSpec, bool) {
	for _, s := range p.specs {
		if s.Key == key {
			return s, true
		}
	}
	return settings.ParamSpec{}, false
}

// paramSource reports where the value of key came from: "flag", "env", "configmap" or
// "default".
func (p *ParamFlags) paramSource(params *settings.Params, key string) string {
	if v, ok := p.values[key]; ok && v.set {
		return "flag"
	}
	pv, ok := params.Lookup(key)
	if !ok {
		return "default"
	}
	return string(pv.Source)
}

// paramEnvLookup returns a lookup of environment variable names that answers with the
// resolved, explicitly configured parameter values (flag, environment or ConfigMap) of the
// parameters projected into those variables, then falls back to next. It feeds libraries
// that read PATROL_* variables themselves (Kubernetes client tuning).
func paramEnvLookup(params *settings.Params, next func(string) (string, bool)) func(string) (string, bool) {
	byEnv := map[string]string{}
	if params != nil {
		for _, v := range params.Values() {
			if v.Spec.Env != "" && v.Source != settings.ParamFromDefault {
				byEnv[v.Spec.Env] = v.Raw
			}
		}
	}
	return func(name string) (string, bool) {
		if s, ok := byEnv[name]; ok {
			return s, true
		}
		if next != nil {
			return next(name)
		}
		return "", false
	}
}
