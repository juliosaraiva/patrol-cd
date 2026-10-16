package tracking

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseTrackingID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    string
		want  *AppInstanceValue
		valid bool
	}{
		{
			name: "namespaced core resource", in: "my-app:/ConfigMap:default/my-cm", valid: true,
			want: &AppInstanceValue{ApplicationName: "my-app", Group: "", Kind: "ConfigMap", Namespace: "default", Name: "my-cm"},
		},
		{
			name: "grouped resource", in: "my-app:apps/Deployment:default/my-deployment", valid: true,
			want: &AppInstanceValue{ApplicationName: "my-app", Group: "apps", Kind: "Deployment", Namespace: "default", Name: "my-deployment"},
		},
		{
			name: "qualified instance name", in: "team_app1:apps/Deployment:team/web", valid: true,
			want: &AppInstanceValue{ApplicationName: "team_app1", Group: "apps", Kind: "Deployment", Namespace: "team", Name: "web"},
		},
		{
			name: "upstream placeholders", in: "app:<group>/<kind>:<namespace>/<name>", valid: true,
			want: &AppInstanceValue{ApplicationName: "app", Group: "<group>", Kind: "<kind>", Namespace: "<namespace>", Name: "<name>"},
		},
		{
			name: "colon in name is kept", in: "app:<group>/<kind>:<namespace>/<name>:<colon>", valid: true,
			want: &AppInstanceValue{ApplicationName: "app", Group: "<group>", Kind: "<kind>", Namespace: "<namespace>", Name: "<name>:<colon>"},
		},
		{
			name: "cluster role with colon", in: "app:rbac.authorization.k8s.io/ClusterRole:dest/system:aggregate-to-admin", valid: true,
			want: &AppInstanceValue{ApplicationName: "app", Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Namespace: "dest", Name: "system:aggregate-to-admin"},
		},
		{
			name: "all parts empty", in: ":/:/", valid: true,
			want: &AppInstanceValue{},
		},
		{name: "empty", in: ""},
		{name: "app only", in: "app"},
		{name: "semicolon separators", in: "app;group/kind/ns"},
		{name: "one colon", in: "app:apps/Deployment"},
		{name: "group without kind", in: "app:apps:default/name"},
		{name: "too many slashes in group/kind", in: "app:apps/v1/Deployment:default/name"},
		{name: "namespace without name", in: "app:apps/Deployment:default"},
		{name: "too many slashes in namespace/name", in: "app:apps/Deployment:default/a/b"},
		{name: "legacy label value", in: "guestbook"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTrackingID(tc.in)
			if !tc.valid {
				require.ErrorIs(t, err, ErrWrongResourceTrackingFormat)
				assert.EqualError(t, err, "wrong resource tracking format, should be <application-name>:<group>/<kind>:<namespace>/<name>")
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.in, BuildTrackingID(*got), "build must invert parse")
			assert.Equal(t, tc.in, got.String())
		})
	}
}

func TestBuildTrackingID(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "my-app:apps/Deployment:default/my-deployment", BuildTrackingID(AppInstanceValue{
		ApplicationName: "my-app", Group: "apps", Kind: "Deployment", Namespace: "default", Name: "my-deployment",
	}))
	assert.Equal(t, ":/:/", BuildTrackingID(AppInstanceValue{}))
}

func TestNewAppInstanceValue(t *testing.T) {
	t.Parallel()
	clusterRole := func() *unstructured.Unstructured {
		return newObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "admin")
	}
	tests := []struct {
		name      string
		obj       *unstructured.Unstructured
		defaultNS string
		want      string
	}{
		{name: "object namespace wins", obj: configMap("own", "cm"), defaultNS: "dest", want: "app:/ConfigMap:own/cm"},
		{name: "default namespace for cluster-scoped", obj: clusterRole(), defaultNS: "dest", want: "app:rbac.authorization.k8s.io/ClusterRole:dest/admin"},
		{name: "no namespace at all", obj: clusterRole(), defaultNS: "", want: "app:rbac.authorization.k8s.io/ClusterRole:/admin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, NewAppInstanceValue(tc.obj, "app", tc.defaultNS).String())
		})
	}
}

func TestAppInstanceValueReferences(t *testing.T) {
	t.Parallel()
	id := AppInstanceValue{ApplicationName: "app", Group: "apps", Kind: "Deployment", Namespace: "ns", Name: "web"}
	clusterID := AppInstanceValue{ApplicationName: "app", Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Namespace: "dest", Name: "admin"}
	tests := []struct {
		name string
		id   AppInstanceValue
		obj  *unstructured.Unstructured
		want bool
	}{
		{name: "same object", id: id, obj: newObj("apps/v1", "Deployment", "ns", "web"), want: true},
		{name: "other version same group", id: id, obj: newObj("apps/v1beta2", "Deployment", "ns", "web"), want: true},
		{name: "other namespace (copied)", id: id, obj: newObj("apps/v1", "Deployment", "copy", "web"), want: false},
		{name: "other name", id: id, obj: newObj("apps/v1", "Deployment", "ns", "web2"), want: false},
		{name: "other kind", id: id, obj: newObj("apps/v1", "StatefulSet", "ns", "web"), want: false},
		{name: "other group", id: id, obj: newObj("extensions/v1beta1", "Deployment", "ns", "web"), want: false},
		{name: "cluster-scoped ignores id namespace", id: clusterID, obj: newObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "admin"), want: true},
		{name: "nil object", id: id, obj: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.id.References(tc.obj))
		})
	}
}
