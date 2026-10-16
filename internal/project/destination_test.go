package project

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

type dest = v1alpha1.ApplicationDestination

func projWithDestinations(dests ...dest) *v1alpha1.AppProject {
	return &v1alpha1.AppProject{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.AppProjectSpec{Destinations: dests}}
}

func cluster(name, server string, mutate ...func(*v1alpha1.Cluster)) v1alpha1.Cluster {
	c := v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol"}, Spec: v1alpha1.ClusterSpec{Name: name, Server: server}}
	for _, m := range mutate {
		m(&c)
	}
	return c
}

func permitted(t *testing.T, proj *v1alpha1.AppProject, d Destination, lister ProjectClusterLister) bool {
	t.Helper()
	ok, err := IsDestinationPermitted(context.Background(), proj, d, lister)
	require.NoError(t, err)
	return ok
}

// Ports the semantics of upstream TestAppProject_IsDestinationPermitted.
func TestIsDestinationPermitted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		proj  []dest
		dest  Destination
		allow bool
	}{
		{"server and namespace match", []dest{{Server: "https://kubernetes.default.svc", Namespace: "default"}}, Destination{Server: "https://kubernetes.default.svc", Namespace: "default"}, true},
		{"namespace does not match", []dest{{Server: "https://kubernetes.default.svc", Namespace: "default"}}, Destination{Server: "https://kubernetes.default.svc", Namespace: "kube-system"}, false},
		{"server does not match", []dest{{Server: "https://my-cluster", Namespace: "default"}}, Destination{Server: "https://kubernetes.default.svc", Namespace: "default"}, false},
		{"wildcard namespace", []dest{{Server: "https://kubernetes.default.svc", Namespace: "*"}}, Destination{Server: "https://kubernetes.default.svc", Namespace: "kube-system"}, true},
		{"wildcard server", []dest{{Server: "https://*.default.svc", Namespace: "default"}}, Destination{Server: "https://kubernetes.default.svc", Namespace: "default"}, true},
		{"wildcard server no match", []dest{{Server: "https://team1-*", Namespace: "default"}}, Destination{Server: "https://test2-dev-cluster", Namespace: "default"}, false},
		{"namespace prefix", []dest{{Server: "https://kubernetes.default.svc", Namespace: "test-*"}}, Destination{Server: "https://kubernetes.default.svc", Namespace: "test-foo"}, true},
		{"namespace prefix no match", []dest{{Server: "https://kubernetes.default.svc", Namespace: "test-*"}}, Destination{Server: "https://kubernetes.default.svc", Namespace: "test"}, false},
		{"all wildcards", []dest{{Server: "*", Namespace: "*"}}, Destination{Server: "https://kubernetes.default.svc", Namespace: "test"}, true},
		{"name", []dest{{Namespace: "*", Name: "test"}}, Destination{Name: "test", Namespace: "test"}, true},
		{"other name", []dest{{Namespace: "*", Name: "test2"}}, Destination{Name: "test", Namespace: "test"}, false},
		{"negated namespace set", []dest{
			{Name: "host-cluster", Server: "https://kubernetes.default.svc", Namespace: "!{kube-system,argocd}"},
			{Name: "destination-cluster-01", Server: "https://eks.example.com", Namespace: "*"},
		}, Destination{Server: "https://eks.example.com", Namespace: "kube-system"}, true},
		// TEN-006 required: team-* namespaces.
		{"team glob", []dest{{Server: "*", Namespace: "team-*"}}, Destination{Server: "https://c1", Namespace: "team-a"}, true},
		{"team glob prod", []dest{{Server: "*", Namespace: "team-*"}}, Destination{Server: "https://c1", Namespace: "prod"}, false},
		// TEN-007: a name entry matches the cluster name whatever the app used.
		{"name entry, in-cluster", []dest{{Name: "in-cluster", Namespace: "*"}}, Destination{Server: v1alpha1.KubernetesInternalAPIServerAddr, Name: "in-cluster", Namespace: "x"}, true},
		{"name glob", []dest{{Name: "prod-*", Namespace: "*"}}, Destination{Server: "https://eu", Name: "prod-eu", Namespace: "x"}, true},
		// Empty namespace (cluster-scoped) matches only "*" or empty patterns.
		{"empty namespace star", []dest{{Server: "*", Namespace: "*"}}, Destination{Server: "https://c1"}, true},
		{"empty namespace empty pattern", []dest{{Server: "*"}}, Destination{Server: "https://c1"}, true},
		{"empty namespace other", []dest{{Server: "*", Namespace: "default"}}, Destination{Server: "https://c1"}, false},
		{"no cluster", []dest{{Server: "*", Namespace: "*"}}, Destination{Namespace: "x"}, false},
		{"no destinations", nil, Destination{Server: "https://c1", Namespace: "x"}, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.allow, permitted(t, projWithDestinations(tt.proj...), tt.dest, nil), tt.name)
	}
}

// Ports the semantics of upstream TestAppProject_IsNegatedDestinationPermitted.
func TestIsNegatedDestinationPermitted(t *testing.T) {
	t.Parallel()
	const local = "https://kubernetes.default.svc"
	tests := []struct {
		proj  []dest
		dest  Destination
		allow bool
	}{
		{[]dest{{Server: "!" + local, Namespace: "default"}}, Destination{Server: local, Namespace: "default"}, false},
		{[]dest{{Server: local, Namespace: "!default"}}, Destination{Server: local, Namespace: "kube-system"}, true},
		{[]dest{{Server: "!https://my-cluster", Namespace: "default"}}, Destination{Server: local, Namespace: "default"}, true},
		{[]dest{{Server: "!" + local, Namespace: "*"}}, Destination{Server: local, Namespace: "kube-system"}, false},
		{[]dest{{Server: "!https://*.default.svc", Namespace: "default"}}, Destination{Server: local, Namespace: "default"}, false},
		{[]dest{{Server: "!https://team1-*", Namespace: "default"}}, Destination{Server: "https://test2-dev-cluster", Namespace: "default"}, true},
		{[]dest{{Server: local, Namespace: "!test-*"}}, Destination{Server: local, Namespace: "test-foo"}, false},
		{[]dest{{Server: local, Namespace: "!test-*"}}, Destination{Server: local, Namespace: "test"}, true},
		{[]dest{{Namespace: "*", Name: "!test"}}, Destination{Name: "test", Namespace: "test"}, false},
		{[]dest{{Namespace: "*", Name: "!test2"}}, Destination{Name: "test", Namespace: "test"}, true},
		{[]dest{{Server: "*", Namespace: "kube-system"}, {Server: "*", Namespace: "!kube-system"}}, Destination{Server: local, Namespace: "kube-system"}, false},
		{[]dest{{Server: "*", Namespace: "*"}, {Server: "*", Namespace: "!kube-system"}}, Destination{Server: local, Namespace: "kube-system"}, false},
		{[]dest{{Server: local, Namespace: "*"}, {Server: "!" + local, Namespace: "*"}}, Destination{Server: local, Namespace: "kube-system"}, false},
		{[]dest{{Server: "*", Namespace: "*"}, {Server: "!" + local, Namespace: "kube-system"}}, Destination{Server: local, Namespace: "kube-system"}, false},
		{[]dest{{Server: "*", Namespace: "*"}, {Server: "!" + local, Namespace: "kube-system"}, {Server: "*", Namespace: "!kube-system"}}, Destination{Server: local, Namespace: "kube-system"}, false},
		{[]dest{{Server: "*", Namespace: "*"}, {Server: "!" + local, Namespace: "kube-system"}, {Server: "*", Namespace: "!kube-system"}}, Destination{Server: local, Namespace: "default"}, true},
		{[]dest{{Server: "*", Namespace: "*"}, {Server: "!" + local, Namespace: "kube-system"}, {Server: "*", Namespace: "!kube-system"}}, Destination{Server: "https://test-dev-cluster", Namespace: "kube-system"}, false},
		{[]dest{{Namespace: "*", Name: "test"}, {Namespace: "*", Name: "!test"}}, Destination{Name: "test", Namespace: "test"}, false},
		{[]dest{{Server: "*", Namespace: "test"}, {Server: "!https://test-server", Namespace: "other"}}, Destination{Server: "https://test-server", Namespace: "test"}, true},
		{[]dest{{Server: "*", Namespace: "*"}, {Server: "https://test-server", Namespace: "!other"}}, Destination{Server: "https://other-test-server", Namespace: "other"}, true},
		// A negated name rejects regardless of namespace (A || (B && C) precedence) even when
		// another entry allows.
		{[]dest{{Server: "*", Namespace: "*"}, {Name: "!prod", Namespace: "other"}}, Destination{Server: "https://p", Name: "prod", Namespace: "default"}, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.allow, permitted(t, projWithDestinations(tt.proj...), tt.dest, nil), "%+v vs %+v", tt.proj, tt.dest)
	}
}

// TEN-008: the documentation example evaluated against a server/namespace matrix.
func TestNegatedDestinationDocExample(t *testing.T) {
	t.Parallel()
	proj := projWithDestinations(
		dest{Namespace: "!kube-system", Server: "*"},
		dest{Namespace: "*", Server: "!https://team1-*"},
		dest{Namespace: "*", Server: "*"},
	)
	tests := []struct {
		server, namespace string
		allow             bool
	}{
		{"https://team2-a", "default", true},
		{"https://team2-a", "kube-system", false},
		{"https://team1-a", "default", false},
		{"https://team1-a", "kube-system", false},
		{"https://kubernetes.default.svc", "apps", true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.allow, permitted(t, proj, Destination{Server: tt.server, Name: "c", Namespace: tt.namespace}, nil), "%+v", tt)
	}
}

type clustersFunc func(ctx context.Context, project string) ([]*v1alpha1.Cluster, error)

func (f clustersFunc) GetProjectClusters(ctx context.Context, project string) ([]*v1alpha1.Cluster, error) {
	return f(ctx, project)
}

// Ports the semantics of upstream TestAppProject_IsDestinationPermitted_PermitOnlyProjectScopedClusters.
func TestPermitOnlyProjectScopedClusters(t *testing.T) {
	t.Parallel()
	scoped := func(cs ...*v1alpha1.Cluster) ProjectClusterLister {
		return clustersFunc(func(context.Context, string) ([]*v1alpha1.Cluster, error) { return cs, nil })
	}
	byServer := func(s string) *v1alpha1.Cluster { return &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: s}} }
	byName := func(n string) *v1alpha1.Cluster { return &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Name: n}} }
	tests := []struct {
		proj   []dest
		dest   Destination
		lister ProjectClusterLister
		allow  bool
	}{
		{[]dest{{Server: "https://team1-*", Namespace: "default"}}, Destination{Server: "https://team1-something.com", Namespace: "default"}, scoped(), false},
		{[]dest{{Server: "https://my-cluster.123.com", Namespace: "default"}}, Destination{Server: "https://my-cluster.123.com", Namespace: "default"}, scoped(byServer("https://my-cluster.123.com")), true},
		{[]dest{{Server: "https://my-cluster.123.com", Namespace: "default"}}, Destination{Server: "https://other.example.com", Namespace: "default"}, scoped(byServer("https://my-cluster.123.com")), false},
		{[]dest{{Name: "some-server", Namespace: "default"}}, Destination{Name: "some-server", Namespace: "default"}, scoped(), false},
		{[]dest{{Name: "some-other-server", Namespace: "default"}}, Destination{Name: "some-other-server", Namespace: "default"}, scoped(byName("some-other-server")), true},
		{[]dest{{Name: "some-server", Namespace: "default"}}, Destination{Name: "some-other-server", Namespace: "default"}, scoped(byName("some-server")), false},
		// TEN-028: match by name when servers differ.
		{[]dest{{Server: "*", Namespace: "*"}}, Destination{Server: "https://a", Name: "shared", Namespace: "x"}, scoped(&v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Name: "shared", Server: "https://b"}}), true},
	}
	for _, tt := range tests {
		proj := projWithDestinations(tt.proj...)
		proj.Spec.PermitOnlyProjectScopedClusters = true
		assert.Equal(t, tt.allow, permitted(t, proj, tt.dest, tt.lister), "%+v", tt.dest)
	}

	proj := projWithDestinations(dest{Server: "https://my-cluster.123.com", Namespace: "default"})
	proj.Spec.PermitOnlyProjectScopedClusters = true
	_, err := IsDestinationPermitted(context.Background(), proj, Destination{Server: "https://my-cluster.123.com", Namespace: "default"},
		clustersFunc(func(context.Context, string) ([]*v1alpha1.Cluster, error) { return nil, errors.New("some error") }))
	assert.ErrorContains(t, err, "could not retrieve project clusters: some error")

	// Not consulted when the destination does not match anyway.
	ok, err := IsDestinationPermitted(context.Background(), proj, Destination{Server: "https://x", Namespace: "default"},
		clustersFunc(func(context.Context, string) ([]*v1alpha1.Cluster, error) { return nil, errors.New("boom") }))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestResolveDestination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	idx := NewClusterIndex([]v1alpha1.Cluster{
		cluster("prod", "https://prod.example.com"),
		cluster("dup-a", "https://dup-a.example.com", func(c *v1alpha1.Cluster) { c.Spec.Name = "dup" }),
		cluster("dup-b", "https://dup-b.example.com", func(c *v1alpha1.Cluster) { c.Spec.Name = "dup" }),
		cluster("slash", "https://slash.example.com/"),
	}, true)

	c, err := ResolveDestination(ctx, dest{Server: "https://prod.example.com/"}, idx)
	require.NoError(t, err)
	assert.Equal(t, "prod", c.Spec.Name)

	c, err = ResolveDestination(ctx, dest{Name: "prod"}, idx)
	require.NoError(t, err)
	assert.Equal(t, "https://prod.example.com", c.Spec.Server)

	c, err = ResolveDestination(ctx, dest{Server: "https://slash.example.com"}, idx)
	require.NoError(t, err)
	assert.Equal(t, "slash", c.Spec.Name)
	assert.Equal(t, "https://slash.example.com", DestinationOf(c, "ns").Server)

	c, err = ResolveDestination(ctx, dest{Name: "in-cluster"}, idx)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.KubernetesInternalAPIServerAddr, c.Spec.Server)

	tests := []struct {
		dest dest
		msg  string
	}{
		{dest{Name: "prod", Server: "https://prod.example.com"}, "application destination can't have both name and server defined: prod https://prod.example.com"},
		{dest{}, "Destination server missing from app spec"},
		{dest{Name: "nope"}, "there are no clusters with this name: nope"},
		{dest{Name: "dup"}, "there are 2 clusters with the same name: [https://dup-a.example.com https://dup-b.example.com]"},
		{dest{Server: "https://unknown"}, `error getting cluster by server "https://unknown": cluster "https://unknown" not found`},
	}
	for _, tt := range tests {
		_, err := ResolveDestination(ctx, tt.dest, idx)
		require.Error(t, err, tt.msg)
		assert.Contains(t, err.Error(), tt.msg)
	}
	_, err = ResolveDestination(ctx, dest{Server: "https://unknown"}, idx)
	assert.ErrorIs(t, err, ErrClusterNotFound)
	_, err = ResolveDestination(ctx, dest{}, idx)
	assert.ErrorIs(t, err, ErrDestinationMissing)
}

func TestClusterIndexInCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const local = v1alpha1.KubernetesInternalAPIServerAddr

	enabled := NewClusterIndex(nil, true)
	c, err := enabled.GetCluster(ctx, local)
	require.NoError(t, err)
	assert.Equal(t, InClusterName, c.Spec.Name)
	servers, err := enabled.GetClusterServersByName(ctx, InClusterName)
	require.NoError(t, err)
	assert.Equal(t, []string{local}, servers)

	disabled := NewClusterIndex([]v1alpha1.Cluster{cluster("in-cluster", local, func(c *v1alpha1.Cluster) { c.Spec.Project = "p" })}, false)
	_, err = disabled.GetCluster(ctx, local)
	require.ErrorIs(t, err, ErrClusterNotFound)
	assert.Contains(t, err.Error(), `cluster "https://kubernetes.default.svc" is disabled`)
	servers, err = disabled.GetClusterServersByName(ctx, InClusterName)
	require.NoError(t, err)
	assert.Empty(t, servers)
	scoped, err := disabled.GetProjectClusters(ctx, "p")
	require.NoError(t, err)
	assert.Empty(t, scoped)

	// A Cluster object registering the in-cluster address takes precedence over the
	// synthesized one.
	named := NewClusterIndex([]v1alpha1.Cluster{cluster("local", local)}, true)
	c, err = named.GetCluster(ctx, local)
	require.NoError(t, err)
	assert.Equal(t, "local", c.Spec.Name)
	servers, err = named.GetClusterServersByName(ctx, InClusterName)
	require.NoError(t, err)
	assert.Equal(t, []string{local}, servers, "no Cluster is named in-cluster: the default applies")

	// The index is isolated from its input.
	in := []v1alpha1.Cluster{cluster("a", "https://a")}
	idx := NewClusterIndex(in, true)
	in[0].Spec.Server = "https://mutated"
	_, err = idx.GetCluster(ctx, "https://a")
	require.NoError(t, err)
}

func TestIsAppDestinationPermitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	idx := NewClusterIndex([]v1alpha1.Cluster{cluster("prod", "https://prod")}, true)
	proj := projWithDestinations(dest{Server: "https://prod", Namespace: "apps"})

	ok, c, err := IsAppDestinationPermitted(ctx, proj, dest{Name: "prod", Namespace: "apps"}, idx)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "prod", c.Spec.Name)

	ok, _, err = IsAppDestinationPermitted(ctx, proj, dest{Name: "prod", Namespace: "other"}, idx)
	require.NoError(t, err)
	assert.False(t, ok)

	_, c, err = IsAppDestinationPermitted(ctx, proj, dest{Name: "nope", Namespace: "apps"}, idx)
	require.Error(t, err)
	assert.Nil(t, c)
}

func TestNilClusterIndex(t *testing.T) {
	t.Parallel()
	var idx *ClusterIndex
	_, err := ResolveDestination(context.Background(), dest{Server: "https://x"}, idx)
	require.ErrorIs(t, err, ErrClusterNotFound)
	_, err = ResolveDestination(context.Background(), dest{Name: "x"}, idx)
	require.EqualError(t, err, "there are no clusters with this name: x")
	cs, err := idx.GetProjectClusters(context.Background(), "p")
	require.NoError(t, err)
	assert.Empty(t, cs)
}
