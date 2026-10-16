package links_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/links"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func str(s string) *string { return &s }

func TestEvaluate(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "patrol", Labels: map[string]string{"team/name": "blue"},
		Annotations: map[string]string{v1alpha1.AnnotationKeyManagedByURL: "https://other-patrol.example.com"}}}
	app.Spec.Destination = v1alpha1.ApplicationDestination{Server: "https://kube.example.com", Namespace: "apps"}
	cluster := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Labels: map[string]string{"region": "eu"}}, Spec: v1alpha1.ClusterSpec{
		Server: "https://kube.example.com", Name: "prod", Project: "p",
		Config: v1alpha1.ClusterConfig{ProxyURL: "http://proxy:3128", CredentialsRef: &v1alpha1.LocalSecretReference{Name: "creds"}},
	}}
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Status: v1alpha1.AppProjectStatus{JWTTokensByRole: map[string]v1alpha1.JWTTokens{"r": {Items: []v1alpha1.JWTToken{{ID: "tok-id", IssuedAt: 1}}}}}}
	ctx, err := links.NewContext(links.Input{
		Resource:    map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "web"}},
		Application: app, Cluster: cluster, Project: proj,
	})
	require.NoError(t, err)
	assert.Equal(t, "https://other-patrol.example.com", ctx[links.KeyManagedByURL])
	assert.Equal(t, map[string]any{}, ctx[links.KeyProject].(map[string]any)["status"], "project status emptied (upstream)")
	assert.Equal(t, map[string]any{"server": "https://kube.example.com", "name": "prod", "project": "p", "labels": map[string]any{"region": "eu"}}, ctx[links.KeyCluster], "cluster configuration never exposed")

	defs := []settings.DeepLink{
		{Title: "app", URL: "https://logs.example.com/{{.app.metadata.name}}?ns={{.application.spec.destination.namespace}}", Description: str("d"), IconClass: str("fa")},
		{Title: "label", URL: `https://x.example.com/{{ index .app.metadata.labels "team/name" | upper }}`},
		{Title: "cond-true", URL: "https://c.example.com/{{.cluster.name}}", Condition: str(`cluster.labels.region == "eu"`)},
		{Title: "cond-false", URL: "https://c.example.com/", Condition: str(`resource.kind == "Service"`)},
		{Title: "non-bool", URL: "https://c.example.com/", Condition: str(`resource.kind`)},
		{Title: "cond-error", URL: "https://c.example.com/", Condition: str(`nosuchvar.x == 1`)},
		{Title: "env", URL: `https://e.example.com/{{ env "HOME" }}`},
		{Title: "dns", URL: `https://e.example.com/{{ getHostByName "example.com" }}`},
		{Title: "exec-err", URL: `https://e.example.com/{{ fail "boom" }}`},
		{Title: "javascript", URL: "javascript:alert(document.cookie)"},
		{Title: "relative", URL: "/relative/path"},
		{Title: "managed", URL: "{{.managedByURL}}/applications/{{.app.metadata.name}}"},
		{Title: "too-long", URL: `https://l.example.com/{{ repeat 20000 "a" }}`},
		{Title: "token", URL: "https://t.example.com/{{.project.status}}"},
	}
	out, problems := links.Evaluate(ctx, "web", defs)
	var titles []string
	for _, l := range out.Items {
		titles = append(titles, l.Title)
	}
	assert.Equal(t, []string{"app", "label", "cond-true", "managed", "token"}, titles)
	assert.Equal(t, "https://logs.example.com/web?ns=apps", out.Items[0].URL)
	assert.Equal(t, "d", *out.Items[0].Description)
	assert.Equal(t, "fa", *out.Items[0].IconClass)
	assert.Equal(t, "https://x.example.com/BLUE", out.Items[1].URL)
	assert.Equal(t, "https://c.example.com/prod", out.Items[2].URL)
	assert.Equal(t, "https://other-patrol.example.com/applications/web", out.Items[3].URL)
	assert.NotContains(t, out.Items[4].URL, "tok-id")
	assert.Len(t, problems, 8)
	joined := strings.Join(problems, "\n")
	assert.Contains(t, joined, "evaluated to non-boolean value for resource web")
	assert.Contains(t, joined, `function "env" not defined`)
	assert.Contains(t, joined, `function "getHostByName" not defined`)
	assert.Contains(t, joined, "does not render an absolute http(s) URL")
	assert.NotNil(t, out.Items)

	empty, problems := links.Evaluate(nil, "x", nil)
	assert.Empty(t, problems)
	assert.NotNil(t, empty.Items, "items is [] not null")
}

func TestManagedByURLValidated(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Annotations: map[string]string{v1alpha1.AnnotationKeyManagedByURL: "javascript:alert(1)"}}}
	ctx, err := links.NewContext(links.Input{Application: app})
	require.NoError(t, err)
	assert.NotContains(t, ctx, links.KeyManagedByURL)
	assert.Contains(t, ctx, links.KeyApp)
	assert.Contains(t, ctx, links.KeyApplication)
}
