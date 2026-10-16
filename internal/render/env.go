package render

import (
	"net/url"
	"os"
	"strings"
)

// Build environment variable names (SRC-091). Every ARGOCD_APP_* variable also has a
// PATROL_APP_* twin with the same value, so plugins and build steps written for either
// product work unchanged.
var buildEnvNames = []string{
	"APP_NAME",
	"APP_NAMESPACE",
	"APP_PROJECT_NAME",
	"APP_REVISION",
	"APP_REVISION_SHORT",
	"APP_REVISION_SHORT_8",
	"APP_SOURCE_REPO_URL",
	"APP_SOURCE_PATH",
	"APP_SOURCE_TARGET_REVISION",
}

// envVar is one NAME=VALUE entry.
type envVar struct{ name, value string }

// buildEnv is the ordered build environment of one call.
type buildEnv struct {
	vars []envVar
}

func newBuildEnv(in Input) *buildEnv {
	values := []string{
		in.AppName,
		in.Namespace,
		in.ProjectName,
		in.Revision,
		shortenRevision(in.Revision, 7),
		shortenRevision(in.Revision, 8),
		in.Repo.URL,
		in.Source.Path,
		in.Source.TargetRevision,
	}
	e := &buildEnv{}
	for _, prefix := range []string{"ARGOCD_", "PATROL_"} {
		for i, n := range buildEnvNames {
			e.vars = append(e.vars, envVar{prefix + n, values[i]})
		}
	}
	return e
}

// newEnvFromList builds an environment from NAME=VALUE strings (entries without '='
// are dropped).
func newEnvFromList(list []string) *buildEnv {
	e := &buildEnv{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		e.vars = append(e.vars, envVar{k, v})
	}
	return e
}

func shortenRevision(rev string, n int) string {
	if len(rev) > n {
		return rev[:n]
	}
	return rev
}

// Environ returns NAME=VALUE strings in order. Like upstream Env.Environ, entries with
// a name and an empty value are kept.
func (e *buildEnv) Environ() []string {
	out := make([]string, 0, len(e.vars))
	for _, v := range e.vars {
		if v.name == "" && v.value == "" {
			continue
		}
		out = append(out, v.name+"="+v.value)
	}
	return out
}

// subset returns the entries whose names are in names (both prefixes), in order.
func (e *buildEnv) subset(names ...string) *buildEnv {
	keep := map[string]bool{}
	for _, n := range names {
		keep["ARGOCD_"+n] = true
		keep["PATROL_"+n] = true
	}
	out := &buildEnv{}
	for _, v := range e.vars {
		if keep[v.name] {
			out.vars = append(out.vars, v)
		}
	}
	return out
}

// Envsubst expands $VAR and ${VAR} with os.Expand semantics: unknown variables expand
// to the empty string and "$$" yields "$" (upstream Env.Envsubst).
func (e *buildEnv) Envsubst(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	vals := make(map[string]string, len(e.vars))
	for _, v := range e.vars {
		vals[v.name] = v.value
	}
	return os.Expand(s, func(name string) string {
		if name == "$" {
			return "$"
		}
		return vals[name]
	})
}

// redactURL removes user info from a URL for logs.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
}
