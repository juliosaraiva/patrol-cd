package clusters

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestIsSkipReconcile covers REPO-097's annotation parsing.
func TestIsSkipReconcile(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"true": true, "True": true, "1": true, "t": true, "false": false, "yes": false, "": false} {
		c := cluster("c", "https://c", func(c *v1alpha1.Cluster) {
			c.Annotations = map[string]string{v1alpha1.AnnotationKeySkipReconcile: v}
		})
		assert.Equal(t, want, IsSkipReconcile(c), "value %q", v)
	}
	assert.False(t, IsSkipReconcile(nil), "the in-cluster cluster (nil) is always managed")
	assert.False(t, IsSkipReconcile(cluster("c", "https://c")))
}

// TestRefreshRequestedAt covers REPO-096: a malformed annotation is ignored.
func TestRefreshRequestedAt(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c := cluster("c", "https://c", func(c *v1alpha1.Cluster) {
		c.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: ts.Format(time.RFC3339)}
	})
	got, ok, err := RefreshRequestedAt(c)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, got.Equal(ts))
	before, after := ts.Add(-time.Minute), ts.Add(time.Minute)
	assert.True(t, NeedsCacheInvalidation(c, &before))
	assert.False(t, NeedsCacheInvalidation(c, &after))
	assert.False(t, NeedsCacheInvalidation(c, nil), "a cache that never synced is not forced")

	c.Annotations[v1alpha1.AnnotationKeyRefresh] = "yesterday"
	_, ok, err = RefreshRequestedAt(c)
	require.Error(t, err)
	assert.False(t, ok)
	assert.False(t, NeedsCacheInvalidation(c, &before))

	_, ok, err = RefreshRequestedAt(cluster("c", "https://c"))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDiffCacheSettings(t *testing.T) {
	t.Parallel()
	a := cluster("c", "https://c", func(c *v1alpha1.Cluster) { c.Spec.Namespaces = []string{"a", "b"} })
	b := a.DeepCopy()
	assert.False(t, DiffCacheSettings(a, b).Any())
	b.Spec.Namespaces = []string{" a ", "", "b"}
	assert.False(t, DiffCacheSettings(a, b).Any(), "namespaces compare after normalization")
	b.Spec.Namespaces = []string{"a"}
	assert.Equal(t, CacheSettingsChange{Namespaces: true}, DiffCacheSettings(a, b))
	b = a.DeepCopy()
	b.Spec.ClusterResources = true
	b.Spec.Config.ProxyURL = "http://p"
	assert.Equal(t, CacheSettingsChange{Config: true, ClusterResources: true}, DiffCacheSettings(a, b))
	assert.True(t, DiffCacheSettings(nil, a).Any())
}

// TestNormalizeNamespaces covers REPO-079's parsing rule (" a , ,b " -> [a b]).
func TestNormalizeNamespaces(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"a", "b"}, NormalizeNamespaces([]string{" a ", " ", "b"}))
	assert.Nil(t, NormalizeNamespaces([]string{"", "  "}))
	assert.Nil(t, NormalizeNamespaces(nil))
}

// TestObjectNameForServer covers REPO-104's naming with upstream's known answers.
func TestObjectNameForServer(t *testing.T) {
	t.Parallel()
	for server, want := range map[string]string{
		"http://foo": "cluster-foo-752281925",
		"http://thelongestdomainnameintheworld.argocd-project.com:3000": "cluster-thelongestdomainnameintheworld.argocd-project.com-2721640553",
		"http://[fe80::1ff:fe23:4567:890a]":                             "cluster-fe80--1ff-fe23-4567-890a-3877258831",
		"http://[fe80::1ff:fe23:4567:890a]:8000":                        "cluster-fe80--1ff-fe23-4567-890a-664858999",
		"http://[FE80::1FF:FE23:4567:890A]:8000":                        "cluster-fe80--1ff-fe23-4567-890a-682802007",
		"http://:/abc":                                                  "cluster--1969338796",
	} {
		got, err := ObjectNameForServer(server)
		require.NoError(t, err, server)
		assert.Equal(t, want, got, server)
	}
	_, err := ObjectNameForServer("not a url")
	require.Error(t, err)
}

// TestObjectNameForServerProperties: names are deterministic, lower-case and distinguish
// servers that differ only in port.
func TestObjectNameForServerProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		host := rapid.StringMatching(`[A-Za-z][A-Za-z0-9-]{0,20}(\.[A-Za-z0-9-]{1,10}){0,3}`).Draw(rt, "host")
		port := rapid.IntRange(1, 65535).Draw(rt, "port")
		server := "https://" + host + ":" + itoa(port)
		a, err := ObjectNameForServer(server)
		require.NoError(rt, err)
		b, err := ObjectNameForServer(server)
		require.NoError(rt, err)
		assert.Equal(rt, a, b)
		assert.Regexp(rt, `^cluster-[a-z0-9.-]+-[0-9]+$`, a)
		other, err := ObjectNameForServer("https://" + host + ":" + itoa(port%65535+1))
		require.NoError(rt, err)
		assert.NotEqual(rt, a, other)
	})
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestAutoLabelKubernetesVersion(t *testing.T) {
	t.Parallel()
	c := cluster("c", "https://c", func(c *v1alpha1.Cluster) {
		c.Labels = map[string]string{v1alpha1.LabelKeyAutoLabelClusterInfo: "true"}
	})
	assert.True(t, AutoLabelKubernetesVersion(c, "v1.36.0"))
	assert.Equal(t, "v1.36.0", c.Labels[v1alpha1.LabelKeyClusterKubernetesVersion])
	assert.False(t, AutoLabelKubernetesVersion(c, "v1.36.0"), "unchanged")
	assert.True(t, AutoLabelKubernetesVersion(c, "v1.37.1"))
	off := cluster("c", "https://c", func(c *v1alpha1.Cluster) { c.Labels = map[string]string{} })
	assert.False(t, AutoLabelKubernetesVersion(off, "v1.36.0"))
	assert.False(t, AutoLabelKubernetesVersion(nil, "v1.36.0"))
	assert.False(t, AutoLabelKubernetesVersion(c, ""), "no version to record")
	assert.Equal(t, "v1.37.1", c.Labels[v1alpha1.LabelKeyClusterKubernetesVersion])
}

func TestInClusterEnabledSetting(t *testing.T) {
	t.Parallel()
	assert.True(t, InClusterEnabled(nil), "unreadable ConfigMap means enabled")
	assert.True(t, InClusterEnabled(map[string]string{}))
	assert.True(t, InClusterEnabled(map[string]string{SettingInClusterEnabled: "no"}))
	assert.True(t, InClusterEnabled(map[string]string{SettingInClusterEnabled: "False"}))
	assert.False(t, InClusterEnabled(map[string]string{SettingInClusterEnabled: "false"}))
}

func TestValidateRegistration(t *testing.T) {
	t.Parallel()
	require.NoError(t, ValidateRegistration(InClusterServer, true))
	require.NoError(t, ValidateRegistration("https://a", false))
	err := ValidateRegistration(InClusterServer+"/", false)
	require.EqualError(t, err, "cannot register cluster: in-cluster has been disabled")
	require.ErrorIs(t, err, ErrInClusterDisabled)
}
