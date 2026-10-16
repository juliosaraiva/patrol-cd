package tracking

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestInstanceName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, appName, appNamespace, controlPlane, want string
	}{
		{"app outside control-plane namespace", "appname", "appns", "defaultns", "appns_appname"},
		{"app in control-plane namespace", "appname", "appns", "appns", "appname"},
		{"app without namespace", "appname", "", "appns", "appname"},
		{"no control-plane namespace configured", "appname", "appns", "", "appns_appname"},
		{"nothing set", "appname", "", "", "appname"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, InstanceName(tc.appName, tc.appNamespace, tc.controlPlane))
		})
	}
}

func TestParseInstanceName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, defaultNS, wantName, wantNS string
	}{
		{"qualified without default", "namespace_name", "", "name", "namespace"},
		{"unqualified without default", "name", "", "name", ""},
		{"qualified with default", "namespace_name", "namespace2", "name", "namespace"},
		{"unqualified with default", "name", "namespace2", "name", "namespace2"},
		{"slash is not a separator", "namespace/name", "", "namespace/name", ""},
		{"splits on the first underscore", "a_b_c", "", "b_c", "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotName, gotNS := ParseInstanceName(tc.in, tc.defaultNS)
			assert.Equal(t, tc.wantName, gotName)
			assert.Equal(t, tc.wantNS, gotNS)
		})
	}
}

func TestInstanceNameFromQualified(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, defaultNS, want string
	}{
		{"qualified, other namespace", "appns/appname", "defaultns", "appns_appname"},
		{"qualified, control-plane namespace", "defaultns/appname", "defaultns", "appname"},
		{"unqualified", "appname", "defaultns", "appname"},
		{"unqualified without default", "appname", "", "appname"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, InstanceNameFromQualified(tc.in, tc.defaultNS))
		})
	}
}

func TestDisplayName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "team/app1", DisplayName("team_app1"))
	assert.Equal(t, "app1", DisplayName("app1"))
	// Every underscore is replaced, as upstream (only reachable with foreign label values).
	assert.Equal(t, "my/helm/release", DisplayName("my_helm_release"))
}

func TestApp(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "team/web", App{Name: "web", Namespace: "team"}.QualifiedName())
	assert.Equal(t, "web", App{Name: "web"}.QualifiedName())
	assert.Equal(t, App{}, AppOf(nil))
	a := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team"}}
	assert.Equal(t, App{Name: "web", Namespace: "team"}, AppOf(a))

	tr := newTestTracker()
	assert.Equal(t, "team_web", tr.InstanceName(AppOf(a)))
	assert.Equal(t, "web", tr.InstanceName(App{Name: "web", Namespace: testControlPlane}))
}
