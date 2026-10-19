package deploy_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// resolvedParams resolves a container's patrol-cmd-params-cm parameters as the component does
// at startup: command-line flags (presented as their environment variable, like
// server.ParamFlags), then literal environment variables, then the rendered ConfigMap, then
// the compiled defaults. Values projected from the ConfigMap into the environment are the
// ConfigMap values, so the ConfigMap alone stands for them.
func resolvedParams(pc parsedContainer, cm map[string]string) (*settings.Params, []settings.Issue) {
	env := map[string]string{}
	for _, e := range pc.c.Env {
		if e.ValueFrom == nil {
			env[e.Name] = e.Value
		}
	}
	for envName, spec := range pc.params {
		if spec.Flag == "" {
			continue
		}
		if f := pc.fs.Lookup(spec.Flag); f != nil && pc.fs.Changed(spec.Flag) {
			env[envName] = f.Value.String()
		}
	}
	return settings.ResolveParams(pc.spec.settings, cm, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
}

// cmdParams returns the data of the rendered patrol-cmd-params-cm (nil when not rendered).
func cmdParams(objs []object) map[string]string {
	o := find(objs, "ConfigMap", settings.DefaultCmdParamsConfigMapName)
	if o == nil {
		return nil
	}
	out := map[string]string{}
	if data, ok := o.u.Object["data"].(map[string]any); ok {
		for k, v := range data {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// TestParamsResolve resolves every component's parameters from each profile's rendered
// patrol-cmd-params-cm, environment and flags with the binaries' own resolver: errors fail
// (a component would refuse to start), warnings (a value ignored) fail too. It also checks
// startup preconditions that depend on several parameters.
func TestParamsResolve(t *testing.T) {
	t.Parallel()
	for _, p := range profiles(t) {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			objs := renderProfile(t, p)
			cm := cmdParams(objs)
			checked := 0
			pts := podTemplates(t, objs)
			for i := range pts {
				pt := &pts[i]
				for _, c := range append(slices.Clone(pt.tmpl.Spec.InitContainers), pt.tmpl.Spec.Containers...) {
					if !isPatrolcd(c) {
						continue
					}
					pc, err := parseContainer(pt, c, false)
					if err != nil {
						t.Errorf("%s/%s: %v", pt.owner, c.Name, err)
						continue
					}
					if pc.spec.settings == "" {
						continue
					}
					params, issues := resolvedParams(pc, cm)
					for _, is := range issues {
						t.Errorf("%s: %s %s: %s", pc.id(), is.Severity, is.Key, is.Message)
					}
					checked++
					if pc.sub == "appset-controller" {
						checkAppSetPreconditions(t, pc, params)
					}
				}
			}
			t.Logf("%s: parameters of %d containers resolved", p.name, checked)
		})
	}
}

// checkAppSetPreconditions mirrors the ApplicationSet controller's startup check
// (internal/appset/config.go ErrSCMProvidersRequireAllowList): ApplicationSets in other
// namespaces with SCM providers enabled need an allow-list, else the controller exits.
func checkAppSetPreconditions(t *testing.T, pc parsedContainer, p *settings.Params) {
	t.Helper()
	release := strings.Split(pc.pod.owner, "/")[1] // kind/namespace/name
	multi := slices.ContainsFunc(p.Strings("applicationsetcontroller.namespaces"), func(n string) bool { return n != release })
	if multi && p.Bool("applicationsetcontroller.enable.scm.providers") && len(p.Strings("applicationsetcontroller.allowed.scm.providers")) == 0 {
		t.Errorf("%s: applicationsetcontroller.namespaces=%v with SCM providers enabled and no applicationsetcontroller.allowed.scm.providers: the ApplicationSet controller exits at startup (%q)",
			pc.id(), p.Strings("applicationsetcontroller.namespaces"), "you must either set --enable-scm-providers=false or specify --allowed-scm-providers")
	}
}
