package project

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

type cri = v1alpha1.ClusterResourceRestrictionItem

func gk(group, kind string) schema.GroupKind { return schema.GroupKind{Group: group, Kind: kind} }

// Ports the semantics of upstream TestAppProject_IsGroupKindPermitted plus the TEN-011..014
// required cases.
func TestIsGroupKindNamePermitted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		spec       v1alpha1.AppProjectSpec
		gk         schema.GroupKind
		resName    string
		namespaced bool
		want       bool
	}{
		{"empty ns whitelist denies", v1alpha1.AppProjectSpec{NamespaceResourceWhitelist: []metav1.GroupKind{}, NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "apps", Kind: "Deployment"}}}, gk("apps", "ReplicaSet"), "", true, false},
		{"empty ns whitelist, blacklisted", v1alpha1.AppProjectSpec{NamespaceResourceWhitelist: []metav1.GroupKind{}, NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "apps", Kind: "Deployment"}}}, gk("apps", "Deployment"), "", true, false},
		{"ns whitelisted", v1alpha1.AppProjectSpec{NamespaceResourceWhitelist: []metav1.GroupKind{{Group: "apps", Kind: "ReplicaSet"}}, NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "apps", Kind: "Deployment"}}}, gk("apps", "ReplicaSet"), "", true, true},
		{"ns not whitelisted", v1alpha1.AppProjectSpec{NamespaceResourceWhitelist: []metav1.GroupKind{{Group: "apps", Kind: "ReplicaSet"}}}, gk("apps", "Action"), "", true, false},
		{"cluster blacklist only", v1alpha1.AppProjectSpec{ClusterResourceBlacklist: []cri{{Group: "", Kind: "Namespace"}}}, gk("", "Namespace"), "", false, false},
		{"cluster */* both lists", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "*", Kind: "*"}}, ClusterResourceBlacklist: []cri{{Group: "*", Kind: "*"}}}, gk("", "Namespace"), "", false, false},
		{"ns unaffected by cluster lists", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "*", Kind: "*"}}, ClusterResourceBlacklist: []cri{{Group: "*", Kind: "*"}}}, gk("apps", "Action"), "", true, true},
		{"empty cluster whitelist", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{}, NamespaceResourceWhitelist: []metav1.GroupKind{{Group: "*", Kind: "*"}}}, gk("", "Namespace"), "", false, false},
		{"ns */*", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{}, NamespaceResourceWhitelist: []metav1.GroupKind{{Group: "*", Kind: "*"}}}, gk("apps", "Action"), "", true, true},
		{"empty spec cluster", v1alpha1.AppProjectSpec{}, gk("", "Namespace"), "", false, false},
		{"empty spec namespaced", v1alpha1.AppProjectSpec{}, gk("apps", "Action"), "", true, true},
		{"named whitelist other", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "", Kind: "Namespace", Name: "team1-*"}}}, gk("", "Namespace"), "other-namespace", false, false},
		{"named whitelist match", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "", Kind: "Namespace", Name: "team1-*"}}}, gk("", "Namespace"), "team1-namespace", false, true},
		{"named whitelist empty name", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "", Kind: "Namespace", Name: "team1-*"}}}, gk("", "Namespace"), "", false, false},
		// TEN-011
		{"default project ClusterRole", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "*", Kind: "*"}}}, gk("rbac.authorization.k8s.io", "ClusterRole"), "admin", false, true},
		{"kind glob ClusterRole", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "rbac.authorization.k8s.io", Kind: "Cluster*"}}}, gk("rbac.authorization.k8s.io", "ClusterRole"), "x", false, true},
		{"kind glob ClusterRoleBinding", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "rbac.authorization.k8s.io", Kind: "Cluster*"}}}, gk("rbac.authorization.k8s.io", "ClusterRoleBinding"), "x", false, true},
		{"kind glob other kind", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "rbac.authorization.k8s.io", Kind: "Cluster*"}}}, gk("rbac.authorization.k8s.io", "Role"), "x", false, false},
		// TEN-012
		{"blacklist ClusterRole", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "*", Kind: "*"}}, ClusterResourceBlacklist: []cri{{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}}}, gk("rbac.authorization.k8s.io", "ClusterRole"), "x", false, false},
		{"blacklist ClusterRole, binding ok", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "*", Kind: "*"}}, ClusterResourceBlacklist: []cri{{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}}}, gk("rbac.authorization.k8s.io", "ClusterRoleBinding"), "x", false, true},
		{"name-scoped deny", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "*", Kind: "*"}}, ClusterResourceBlacklist: []cri{{Kind: "Namespace", Name: "kube-*"}}}, gk("", "Namespace"), "kube-system", false, false},
		{"name-scoped deny other", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "*", Kind: "*"}}, ClusterResourceBlacklist: []cri{{Kind: "Namespace", Name: "kube-*"}}}, gk("", "Namespace"), "team", false, true},
		// TEN-013 / TEN-014
		{"whitelist Deployment denies ConfigMap", v1alpha1.AppProjectSpec{NamespaceResourceWhitelist: []metav1.GroupKind{{Group: "apps", Kind: "Deployment"}}}, gk("", "ConfigMap"), "", true, false},
		{"blacklist ResourceQuota", v1alpha1.AppProjectSpec{NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "", Kind: "ResourceQuota"}}}, gk("", "ResourceQuota"), "", true, false},
		{"wildcard ns deny", v1alpha1.AppProjectSpec{NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "*", Kind: "*"}}}, gk("apps", "Deployment"), "", true, false},
		{"malformed pattern", v1alpha1.AppProjectSpec{ClusterResourceWhitelist: []cri{{Group: "[", Kind: "*"}}}, gk("", "Namespace"), "", false, false},
	}
	for _, tt := range tests {
		proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: tt.spec}
		assert.Equal(t, tt.want, IsGroupKindNamePermitted(proj, tt.gk, tt.resName, tt.namespaced), tt.name)
	}
	assert.False(t, IsGroupKindNamePermitted(nil, gk("apps", "Deployment"), "", true))
	assert.True(t, IsGroupKindPermitted(&v1alpha1.AppProject{}, gk("apps", "Deployment"), true))
}

func liveObj(group, version, kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func TestIsLiveResourcePermitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{
		Destinations:             []v1alpha1.ApplicationDestination{{Server: "https://c1", Namespace: "apps"}},
		ClusterResourceWhitelist: []cri{{Group: "", Kind: "Namespace"}},
	}}
	c1 := &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://c1", Name: "c1"}}
	tests := []struct {
		obj  *unstructured.Unstructured
		want bool
	}{
		{liveObj("apps", "v1", "Deployment", "apps", "web"), true},
		{liveObj("apps", "v1", "Deployment", "other", "web"), false},
		{liveObj("", "v1", "Namespace", "", "apps"), true},
		{liveObj("rbac.authorization.k8s.io", "v1", "ClusterRole", "", "admin"), false},
		{nil, false},
	}
	for _, tt := range tests {
		ok, err := IsLiveResourcePermitted(ctx, proj, tt.obj, c1, nil)
		require.NoError(t, err)
		assert.Equal(t, tt.want, ok, "%v", tt.obj)
	}
	assert.Equal(t, `Failed to check if live resource "Deployment" is permitted in project "p": boom`,
		LiveResourceCheckErrorMessage("Deployment", "p", errors.New("boom")))
}

func TestValidateSyncPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	proj := &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{
		Destinations:               []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "!kube-system"}, {Server: "*", Namespace: "*"}},
		NamespaceResourceBlacklist: []metav1.GroupKind{{Group: "", Kind: "ResourceQuota"}},
	}}
	c1 := &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://c1", Name: "c1"}}
	namespaced := ResourceScope{Known: true, Namespaced: true}
	clusterScoped := ResourceScope{Known: true}
	tests := []struct {
		obj   *unstructured.Unstructured
		scope ResourceScope
		err   string
	}{
		{liveObj("apps", "v1", "Deployment", "apps", "web"), namespaced, ""},
		{liveObj("apps", "v1", "Deployment", "kube-system", "web"), namespaced, "namespace kube-system is not permitted in project 'p'"},
		{liveObj("", "v1", "ResourceQuota", "apps", "q"), namespaced, "resource :ResourceQuota is not permitted in project p"},
		// TEN-041: ClusterRole in a project without cluster whitelist; the manifest namespace is ignored.
		{liveObj("rbac.authorization.k8s.io", "v1", "ClusterRole", "apps", "admin"), clusterScoped, "resource rbac.authorization.k8s.io:ClusterRole is not permitted in project p"},
		{liveObj("example.com", "v1", "Widget", "apps", "w"), ResourceScope{}, "failed to get API resource info for example.com/Widget: unable to verify permissions"},
	}
	for _, tt := range tests {
		err := ValidateSyncPermissions(ctx, proj, tt.obj, tt.scope, c1, nil)
		if tt.err == "" {
			assert.NoError(t, err)
			continue
		}
		assert.EqualError(t, err, tt.err)
	}
	assert.Error(t, ValidateSyncPermissions(ctx, proj, nil, namespaced, c1, nil))
}

func TestMissingNamespaceMessage(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "Namespace for web apps/v1, Kind=Deployment is missing.",
		MissingNamespaceMessage("web", schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}))
}
