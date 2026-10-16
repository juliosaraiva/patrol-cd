package applicationsets

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestContinueTokenRoundTrip(t *testing.T) {
	t.Parallel()
	k := listKey{Name: "guestbook", Namespace: "tenant-1"}
	got, err := decodeContinue(encodeContinue(k))
	require.NoError(t, err)
	require.Equal(t, k, got)
	for _, bad := range []string{"!!", "e30", strings.Repeat("A", maxContinueLen+1), "bm90IGpzb24"} {
		_, err := decodeContinue(bad)
		require.Error(t, err, bad)
	}
}

func TestNamespaceEnabled(t *testing.T) {
	t.Parallel()
	s := New(Deps{Config: Config{Namespace: "patrol", Namespaces: []string{"team-*", "/^env-[0-9]+$/", "exact"}}})
	cases := map[string]bool{
		"patrol": true, "team-a": true, "exact": true, "env-12": true,
		"env-x": false, "other": false, "": false, "teamx": false,
	}
	for ns, want := range cases {
		require.Equal(t, want, s.namespaceEnabled(ns), ns)
	}
	none := New(Deps{Config: Config{Namespace: "patrol"}})
	require.True(t, none.namespaceEnabled("patrol"))
	require.False(t, none.namespaceEnabled("team-a"))
}

func TestRBACObject(t *testing.T) {
	t.Parallel()
	s := New(Deps{Config: Config{Namespace: "patrol"}})
	as := func(ns, proj string) *v1alpha1.ApplicationSet {
		a := &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: ns}}
		a.Spec.Template.Spec.Project = proj
		return a
	}
	require.Equal(t, "team/set", s.object(as("patrol", "team")))
	require.Equal(t, "team/set", s.object(as("", "team")))
	require.Equal(t, "default/set", s.object(as("patrol", "")))
	require.Equal(t, "team/tenant/set", s.object(as("tenant", "team")))
	// The audited object keeps a templated project (created with kubectl) literally;
	// authorization uses objects(), which fails closed (TestRBACObjects).
	require.Equal(t, "{{.p}}/set", s.object(as("patrol", "{{.p}}")))
}

func TestProjectFilter(t *testing.T) {
	t.Parallel()
	mk := func(p string) *v1alpha1.ApplicationSet {
		a := &v1alpha1.ApplicationSet{}
		a.Spec.Template.Spec.Project = p
		return a
	}
	require.True(t, projectFilter(nil, mk("x")))
	require.True(t, projectFilter([]string{"x"}, mk("x")))
	require.False(t, projectFilter([]string{"y"}, mk("x")))
	require.True(t, projectFilter([]string{"default"}, mk("")))
}

func TestSameDesiredState(t *testing.T) {
	t.Parallel()
	base := &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}}
	base.Spec.GoTemplate = true
	same := base.DeepCopy()
	same.Labels = nil
	require.True(t, sameDesiredState(base, same), "nil and empty label maps are equal")
	diff := base.DeepCopy()
	diff.Spec.GoTemplate = false
	require.False(t, sameDesiredState(base, diff))
	fin := base.DeepCopy()
	fin.Finalizers = []string{"x"}
	require.False(t, sameDesiredState(base, fin))
}

func TestMergeMaps(t *testing.T) {
	t.Parallel()
	require.Nil(t, mergeMaps(nil, nil))
	require.Equal(t, map[string]string{"a": "1", "b": "3"}, mergeMaps(map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "3"}))
	require.Equal(t, map[string]string{"b": "3"}, mergeMaps(nil, map[string]string{"b": "3"}))
}

func TestBuildTreeAndStatuses(t *testing.T) {
	t.Parallel()
	as := &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns", UID: "u"}}
	require.Empty(t, buildTree(as).Nodes)
	require.NotNil(t, buildTree(as).Nodes, "empty list, not null")
	apps := []v1alpha1.Application{
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}},
	}
	apps[0].Status.Sync.Status = v1alpha1.SyncStatusCodeSynced
	st := resourceStatuses(apps)
	require.Equal(t, []string{"a", "b"}, []string{st[0].Name, st[1].Name})
	require.Equal(t, v1alpha1.SyncStatusCodeSynced, st[1].Status)
	as.Status.Resources = st
	tree := buildTree(as)
	require.Equal(t, "a", tree.Nodes[0].Name)
	require.Nil(t, tree.Nodes[0].Info)
	require.Equal(t, "Synced", tree.Nodes[1].Info[0].Value)
	require.Equal(t, "u", tree.Nodes[1].ParentRefs[0].UID)
}

func TestSanitizeForCreate(t *testing.T) {
	t.Parallel()
	now := metav1.Now()
	as := &v1alpha1.ApplicationSet{ObjectMeta: metav1.ObjectMeta{Name: "s", ResourceVersion: "5", UID: "x", Generation: 3, DeletionTimestamp: &now,
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "m"}}}}
	as.Status.ResourcesCount = 4
	sanitizeForCreate(as)
	require.Empty(t, as.ResourceVersion)
	require.Empty(t, as.UID)
	require.Zero(t, as.Generation)
	require.Nil(t, as.DeletionTimestamp)
	require.Nil(t, as.ManagedFields)
	require.Zero(t, as.Status.ResourcesCount)
	require.Equal(t, "s", as.Name)
}
