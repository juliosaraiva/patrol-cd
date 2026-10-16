package applicationsets_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applicationsets"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/internal/miscapitest"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const inCluster = "https://kubernetes.default.svc"

// fixture is an API server with the ApplicationSet service against envtest: a control-plane
// namespace with projects default, team-a and team-b, an enabled tenant namespace (allowed
// by default and team-a) and a namespace that is not enabled.
type fixture struct {
	*miscapitest.Env
	tenant   string
	disabled string
}

type fixtureOptions struct {
	config    func(*applicationsets.Config)
	generator applicationsets.Generator
}

// users and their policies.
func policies(tenant string) map[string]string {
	return miscapitest.Policy(
		"g, admin, role:admin",
		"g, reader, role:readonly",
		"p, alice, applicationsets, *, team-a/*, allow",
		"p, bob, applicationsets, get, team-b/*, allow",
		"p, bob, applicationsets, delete, team-b/*, allow",
		"p, creator, applicationsets, create, team-a/*, allow",
		"p, creator, applicationsets, get, team-a/*, allow",
		"p, mover, applicationsets, create, team-b/*, allow",
		"p, mover, applicationsets, update, team-b/*, allow",
		"p, mover, applicationsets, *, team-a/*, allow",
		"p, mover2, applicationsets, create, team-b/*, allow",
		"p, mover2, applicationsets, update, team-b/*, allow",
		"p, mover2, applicationsets, create, team-a/*, allow",
		"p, mover2, applicationsets, get, team-a/*, allow",
		"p, tenantuser, applicationsets, *, team-a/"+tenant+"/*, allow",
		"g, sso-devs, role:readonly",
	)
}

func newFixture(t *testing.T, o fixtureOptions) *fixture {
	t.Helper()
	f := &fixture{tenant: miscapitest.Unique("tenant"), disabled: miscapitest.Unique("other")}
	f.Env = miscapitest.New(t, miscapitest.Options{
		Kube: true,
		CM:   miscapitest.Accounts("admin", "reader", "alice", "bob", "creator", "mover", "mover2", "tenantuser", "nobody"),
		RBAC: policies(f.tenant),
	}, func(env *miscapitest.Env) []miscapitest.Registrar {
		cfg := applicationsets.Config{Namespace: env.Namespace, Namespaces: []string{f.tenant}}
		if o.config != nil {
			o.config(&cfg)
		}
		gen := o.generator
		if gen == nil {
			gen = applicationsets.NewGenerator(generators.Config{Client: env.Kube, Namespace: env.Namespace})
		}
		return []miscapitest.Registrar{applicationsets.New(applicationsets.Deps{
			Config: cfg, Reader: env.Kube, APIReader: env.Kube, Writer: env.Kube, Watcher: env.Kube,
			Generator: gen, Recorder: env.Recorder,
		})}
	})
	f.CreateNamespace(f.tenant)
	f.CreateNamespace(f.disabled)
	ctx := context.Background()
	for _, p := range []struct {
		name    string
		sources []string
	}{{"default", []string{f.tenant}}, {"team-a", []string{f.tenant}}, {"team-b", nil}} {
		proj := &v1alpha1.AppProject{
			ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: f.Namespace},
			Spec: v1alpha1.AppProjectSpec{
				SourceRepos:      []string{"*"},
				Destinations:     []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
				SourceNamespaces: p.sources,
			},
		}
		require.NoError(t, f.Kube.Create(ctx, proj))
	}
	f.SetProjectRoles("team-a", rbac.Role{Name: "ci", Policies: []string{"p, proj:team-a:ci, applicationsets, *, team-a/*, allow"}})
	return f
}

// listAppSet returns an ApplicationSet with a List generator producing one Application per
// element ("<element>-<name>").
func listAppSet(ns, name, project string, elements ...string) *v1alpha1.ApplicationSet {
	var els []apiextensionsv1.JSON
	for _, e := range elements {
		b, _ := json.Marshal(map[string]string{"cluster": e})
		els = append(els, apiextensionsv1.JSON{Raw: b})
	}
	return &v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"team": project}},
		Spec: v1alpha1.ApplicationSetSpec{
			Generators: []v1alpha1.ApplicationSetGenerator{{List: &v1alpha1.ListGenerator{Elements: els}}},
			Template: v1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{Name: "{{cluster}}-" + name},
				Spec: v1alpha1.ApplicationSpec{
					Project:     project,
					Source:      &v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/apps.git", Path: "{{cluster}}"},
					Destination: v1alpha1.ApplicationDestination{Server: inCluster, Namespace: "{{cluster}}"},
				},
			},
		},
	}
}

// seed creates an ApplicationSet directly through the Kubernetes API.
func (f *fixture) seed(as *v1alpha1.ApplicationSet) *v1alpha1.ApplicationSet {
	f.T.Helper()
	require.NoError(f.T, f.Kube.Create(context.Background(), as))
	return as
}

func (f *fixture) exists(ns, name string) bool {
	f.T.Helper()
	err := f.Kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &v1alpha1.ApplicationSet{})
	if err == nil {
		return true
	}
	require.True(f.T, client.IgnoreNotFound(err) == nil, "unexpected error %v", err)
	return false
}

func apiErr(t *testing.T, err error) *apiclient.APIError {
	t.Helper()
	var ae *apiclient.APIError
	require.True(t, errors.As(err, &ae), "want *apiclient.APIError, got %T: %v", err, err)
	return ae
}

func appNames(apps []v1alpha1.Application) []string {
	out := make([]string, 0, len(apps))
	for _, a := range apps {
		out = append(out, a.Name)
	}
	return out
}

func setNames(items []v1alpha1.ApplicationSet) []string {
	out := make([]string, 0, len(items))
	for _, a := range items {
		out = append(out, a.Namespace+"/"+a.Name)
	}
	return out
}
