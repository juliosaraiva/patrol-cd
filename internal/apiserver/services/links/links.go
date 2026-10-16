// Package links evaluates the deep links of patrol-cm (application.links, resource.links,
// project.links; API-045) for the API services that expose them (projects, applications,
// resources).
//
// A link is kept when its optional expr-lang condition ("if") evaluates to true against the
// context, and its URL is a Go text/template rendered with the sprig functions minus env,
// expandenv and getHostByName (upstream server/deeplinks). Evaluation problems skip the
// link and are returned as messages for the caller to log; they are never sent to clients.
//
// Deliberate difference from upstream: a rendered URL that is not an absolute http(s) URL is
// skipped (threat model T15: links are rendered as anchors by the UI, so a "javascript:"
// link would be script injection).
package links

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/expr-lang/expr"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Context keys available to link conditions and templates (upstream names).
const (
	KeyResource     = "resource"
	KeyApplication  = "application"
	KeyApp          = "app"
	KeyCluster      = "cluster"
	KeyProject      = "project"
	KeyManagedByURL = "managedByURL"
)

// MaxURLBytes bounds a rendered link URL; longer renderings are skipped.
const MaxURLBytes = 16 << 10

// funcMap is built once: the sprig text functions without those that reveal the server's
// environment (upstream removes the same three).
var funcMap = func() template.FuncMap {
	f := sprig.TxtFuncMap()
	delete(f, "env")
	delete(f, "expandenv")
	delete(f, "getHostByName")
	return f
}()

// Input holds the objects a link context is built from; nil objects are left out.
type Input struct {
	// Resource is a live resource as a JSON map; callers mask Secret data first.
	Resource map[string]any
	// Application is the Application (both "application" and "app").
	Application *v1alpha1.Application
	// Cluster is the destination cluster; only its non-sensitive identity fields are
	// exposed (server, name, namespaces, shard, project, labels, annotations).
	Cluster *v1alpha1.Cluster
	// Project is the AppProject; its status (token metadata) is removed.
	Project *v1alpha1.AppProject
}

// NewContext returns the evaluation context of in (upstream CreateDeepLinksObject). The
// "managedByURL" key is set from the Application's cd.patrol.example/managed-by-url
// annotation when it is an absolute http(s) URL.
func NewContext(in Input) (map[string]any, error) {
	out := map[string]any{}
	if in.Resource != nil {
		out[KeyResource] = in.Resource
	}
	if in.Application != nil {
		app := in.Application.DeepCopy()
		app.APIVersion, app.Kind = v1alpha1.GroupVersion.String(), "Application"
		m, err := toMap(app)
		if err != nil {
			return nil, fmt.Errorf("error getting application: %w", err)
		}
		out[KeyApplication] = m
		out[KeyApp] = m
		if u, ok := in.Application.Annotations[v1alpha1.AnnotationKeyManagedByURL]; ok && u != "" && settings.ValidateExternalURL(u) == nil {
			out[KeyManagedByURL] = u
		}
	}
	if in.Cluster != nil {
		out[KeyCluster] = ClusterData(in.Cluster)
	}
	if in.Project != nil {
		p := in.Project.DeepCopy()
		p.Status = v1alpha1.AppProjectStatus{}
		p.APIVersion, p.Kind = v1alpha1.GroupVersion.String(), "AppProject"
		m, err := toMap(p)
		if err != nil {
			return nil, fmt.Errorf("error getting project: %w", err)
		}
		out[KeyProject] = m
	}
	return out, nil
}

// ClusterData is the cluster as exposed to links (upstream ClusterLinksData): never its
// configuration or credentials.
func ClusterData(c *v1alpha1.Cluster) map[string]any {
	m := map[string]any{"server": c.Spec.Server, "name": c.Spec.Name}
	if len(c.Spec.Namespaces) > 0 {
		ns := make([]any, len(c.Spec.Namespaces))
		for i, n := range c.Spec.Namespaces {
			ns[i] = n
		}
		m["namespaces"] = ns
	}
	if c.Spec.Shard != nil {
		m["shard"] = *c.Spec.Shard
	}
	if c.Spec.Project != "" {
		m["project"] = c.Spec.Project
	}
	if len(c.Labels) > 0 {
		m["labels"] = stringMap(c.Labels)
	}
	if len(c.Annotations) > 0 {
		m["annotations"] = stringMap(c.Annotations)
	}
	return m
}

func stringMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func toMap(obj runtime.Object) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

// Evaluate returns the links of defs whose condition holds for ctx, rendered, in
// configuration order (upstream EvaluateDeepLinksResponse), and one message per skipped
// link. name identifies the evaluated object in the messages.
func Evaluate(ctx map[string]any, name string, defs []settings.DeepLink) (*apiclient.LinksResponse, []string) {
	out := &apiclient.LinksResponse{Items: []apiclient.LinkInfo{}}
	var problems []string
	env := maps.Clone(ctx)
	if env == nil {
		env = map[string]any{}
	}
	for _, l := range defs {
		if l.Condition != nil {
			res, err := expr.Eval(*l.Condition, env)
			if err != nil {
				problems = append(problems, fmt.Sprintf("failed to evaluate link condition '%v' with resource %v, error=%v", *l.Condition, name, err))
				continue
			}
			ok, isBool := res.(bool)
			if !isBool {
				problems = append(problems, fmt.Sprintf("link condition '%v' evaluated to non-boolean value for resource %v", *l.Condition, name))
				continue
			}
			if !ok {
				continue
			}
		}
		t, err := template.New("deep-link").Funcs(funcMap).Parse(l.URL)
		if err != nil {
			problems = append(problems, fmt.Sprintf("failed to parse link template '%v', error=%v", l.URL, err))
			continue
		}
		var b limitedBuilder
		if err := t.Execute(&b, env); err != nil {
			problems = append(problems, fmt.Sprintf("failed to evaluate link template '%v' with resource %v, error=%v", l.URL, name, err))
			continue
		}
		rendered := b.String()
		if !isHTTPURL(rendered) {
			problems = append(problems, fmt.Sprintf("link '%v' for resource %v does not render an absolute http(s) URL", l.Title, name))
			continue
		}
		out.Items = append(out.Items, apiclient.LinkInfo{Title: l.Title, URL: rendered, Description: cloneString(l.Description), IconClass: cloneString(l.IconClass)})
	}
	return out, problems
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// isHTTPURL accepts absolute http and https URLs with a host.
func isHTTPURL(s string) bool {
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") && u.Host != ""
}

var errTooLong = errors.New("rendered URL exceeds the size limit")

// limitedBuilder is a strings.Builder that fails once MaxURLBytes are exceeded, which
// stops template execution.
type limitedBuilder struct{ strings.Builder }

func (b *limitedBuilder) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxURLBytes {
		return 0, errTooLong
	}
	return b.Builder.Write(p)
}
