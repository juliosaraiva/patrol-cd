package reposerver

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// TestLRUProperties checks the bounds and read-your-writes behavior of the LRU against
// a model under random operation sequences.
func TestLRUProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		maxEntries := rapid.IntRange(1, 8).Draw(rt, "maxEntries")
		maxBytes := rapid.Int64Range(1, 64).Draw(rt, "maxBytes")
		c := newLRU[int](maxEntries, maxBytes, nil)
		last := map[string]int{}
		ops := rapid.IntRange(1, 200).Draw(rt, "ops")
		for i := range ops {
			key := fmt.Sprint(rapid.IntRange(0, 12).Draw(rt, "key"))
			switch rapid.IntRange(0, 2).Draw(rt, "op") {
			case 0:
				size := rapid.Int64Range(0, 80).Draw(rt, "size")
				stored := c.Add(key, i, size, 0)
				if stored {
					last[key] = i
					// The value just added is always readable.
					v, ok := c.Get(key)
					if !ok || v != i {
						rt.Fatalf("value of %s not readable after Add", key)
					}
				} else {
					delete(last, key)
					if size <= maxBytes {
						rt.Fatalf("value of size %d <= %d refused", size, maxBytes)
					}
				}
			case 1:
				if v, ok := c.Get(key); ok && v != last[key] {
					rt.Fatalf("Get(%s) = %d, want last written %d", key, v, last[key])
				}
			case 2:
				c.Remove(key)
				delete(last, key)
			}
			if c.Len() > maxEntries {
				rt.Fatalf("len %d > %d", c.Len(), maxEntries)
			}
			if c.Bytes() > maxBytes {
				rt.Fatalf("bytes %d > %d", c.Bytes(), maxBytes)
			}
		}
	})
}

func TestLRUExpiry(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	c := newLRU[string](10, 0, func() time.Time { return now })
	c.Add("a", "x", 1, time.Minute)
	c.Add("b", "y", 1, 0)
	_, ok := c.Get("a")
	assert.True(t, ok)
	now = now.Add(time.Minute)
	_, ok = c.Get("a")
	assert.False(t, ok, "expired")
	_, ok = c.Get("b")
	assert.True(t, ok, "no expiry")
	assert.Equal(t, 1, c.Len())
}

func TestManifestCacheMoveRevision(t *testing.T) {
	t.Parallel()
	c := newManifestCache(100, 1<<20, time.Hour, nil)
	mk := func(spec, rev string, refs map[string]string, manifests ...string) *manifestEntry {
		return &manifestEntry{
			key: manifestKey(spec, rev, refs), specDigest: spec, identity: "id", revision: rev, refRevs: refs,
			resp: &apiclient.ManifestResponse{Revision: rev, Manifests: manifests},
		}
	}
	c.set(mk("spec1", "old", map[string]string{"values": "v1"}, "a"))
	c.set(mk("spec2", "old", map[string]string{"values": "v1"}, "b"))
	c.set(mk("spec1", "other", map[string]string{"values": "v1"}, "c"))
	c.set(&manifestEntry{key: manifestKey("spec3", "old", nil), specDigest: "spec3", identity: "id", revision: "old"}) // failure record

	n := c.moveRevision("id", true, "old", "new", map[string]string{"values": "v1"}, map[string]string{"values": "v2"})
	assert.Equal(t, 2, n)
	e, ok := c.get(manifestKey("spec1", "new", map[string]string{"values": "v2"}))
	require.True(t, ok)
	assert.Equal(t, []string{"a"}, e.resp.Manifests)
	assert.Equal(t, "new", e.resp.Revision)
	_, ok = c.get(manifestKey("spec2", "new", map[string]string{"values": "v2"}))
	assert.True(t, ok)
	_, ok = c.get(manifestKey("spec3", "new", nil))
	assert.False(t, ok, "failure records are not moved")
	verified := mk("spec4", "old", map[string]string{"values": "v1"}, "d")
	verified.resp.SourceIntegrityResult = &apiclient.SourceIntegrityResult{Checks: []apiclient.SourceIntegrityCheck{{Name: "GIT/GPG"}}}
	c.set(verified)
	c.moveRevision("id", true, "old", "new", map[string]string{"values": "v1"}, map[string]string{"values": "v2"})
	_, ok = c.get(manifestKey("spec4", "new", map[string]string{"values": "v2"}))
	assert.False(t, ok, "verified results are re-verified at the new revision")
	// Entries of other identities or revisions are untouched.
	assert.Equal(t, 0, c.moveRevision("unknown", true, "old", "new", nil, nil))
	_, ok = c.get(manifestKey("spec1", "other", map[string]string{"values": "v1"}))
	assert.True(t, ok)

	// Returned entries are private copies.
	e.resp.Manifests[0] = "mutated"
	e2, _ := c.get(manifestKey("spec1", "new", map[string]string{"values": "v2"}))
	assert.Equal(t, "a", e2.resp.Manifests[0])

	// The index follows evictions.
	small := newManifestCache(1, 1<<20, time.Hour, nil)
	small.set(mk("s", "r1", nil, "x"))
	small.set(&manifestEntry{key: "other", specDigest: "s", identity: "id2", revision: "r", resp: &apiclient.ManifestResponse{}})
	assert.Empty(t, small.index["id"])
}

func TestManifestKeyStability(t *testing.T) {
	t.Parallel()
	s := &Service{serverStamp: "srv", cfg: Config{DisableGPG: true}}
	req := &apiclient.ManifestRequest{
		Repo: apiclient.Repository{URL: "https://git.example.com/a.git", Password: "p1"}, AppName: "app",
		APIVersions: []string{"v1", "apps/v1"},
		Source:      v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/a.git", Path: "x", TargetRevision: "main"},
		RefSources:  map[string]apiclient.RefTarget{"$v": {Repo: apiclient.Repository{URL: "https://git.example.com/v.git", Password: "secret"}, TargetRevision: "main"}},
	}
	d1 := digestJSON(s.renderSpecOf(req, req.Source, kindGit))
	// Credentials, API version order and the git target revision do not change the key.
	req2 := *req
	req2.Repo.Password = "p2"
	req2.APIVersions = []string{"apps/v1", "v1"}
	req2.Source.TargetRevision = "HEAD"
	req2.RefSources = map[string]apiclient.RefTarget{"$v": {Repo: apiclient.Repository{URL: "https://git.example.com/v.git", Password: "other"}, TargetRevision: "main"}}
	assert.Equal(t, d1, digestJSON(s.renderSpecOf(&req2, req2.Source, kindGit)))
	// Every rendering input does.
	for name, mutate := range map[string]func(*apiclient.ManifestRequest){
		"namespace": func(r *apiclient.ManifestRequest) { r.Namespace = "other" },
		"kube":      func(r *apiclient.ManifestRequest) { r.KubeVersion = "1.30" },
		"apis":      func(r *apiclient.ManifestRequest) { r.APIVersions = []string{"v1"} },
		"tracking":  func(r *apiclient.ManifestRequest) { r.TrackingMethod = "label" },
		"label":     func(r *apiclient.ManifestRequest) { r.InstanceLabelKey = "x" },
		"install":   func(r *apiclient.ManifestRequest) { r.InstallationID = "i" },
		"app":       func(r *apiclient.ManifestRequest) { r.AppName = "other" },
		"appns":     func(r *apiclient.ManifestRequest) { r.AppNamespace = "other" },
		"path":      func(r *apiclient.ManifestRequest) { r.Source.Path = "y" },
		"kustomize": func(r *apiclient.ManifestRequest) { r.KustomizeOptions.BuildOptions = "--enable-helm" },
		"helm":      func(r *apiclient.ManifestRequest) { r.HelmOptions.ValuesFileSchemes = []string{"https"} },
		"enabled":   func(r *apiclient.ManifestRequest) { r.EnabledSourceTypes = map[string]bool{"Helm": false} },
		"ref target": func(r *apiclient.ManifestRequest) {
			r.RefSources = map[string]apiclient.RefTarget{"$v": {TargetRevision: "dev"}}
		},
		"plugin env":  func(r *apiclient.ManifestRequest) { r.Source.Plugin = &v1alpha1.ApplicationSourcePlugin{Name: "p"} },
		"integrity":   func(r *apiclient.ManifestRequest) { r.SourceIntegrity = &v1alpha1.SourceIntegrity{} },
		"multisource": func(r *apiclient.ManifestRequest) { r.HasMultipleSources = true },
	} {
		r := *req
		mutate(&r)
		assert.NotEqual(t, d1, digestJSON(s.renderSpecOf(&r, r.Source, kindGit)), name)
	}
	assert.NotEqual(t, manifestKey(d1, "a", nil), manifestKey(d1, "b", nil))
	assert.NotEqual(t, manifestKey(d1, "a", map[string]string{"v": "1"}), manifestKey(d1, "a", map[string]string{"v": "2"}))
}

func TestWorkdirsEviction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	tick := func() { mu.Lock(); now = now.Add(time.Second); mu.Unlock() }
	w := newWorkdirs(root, 2, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fp := "anon"
	p1, lock1, rel1, err := w.acquire(apiclient.Repository{URL: "https://h/a"}, fp)
	require.NoError(t, err)
	tick()
	p2, _, rel2, err := w.acquire(apiclient.Repository{URL: "https://h/b"}, fp)
	require.NoError(t, err)
	tick()
	p3, _, rel3, err := w.acquire(apiclient.Repository{URL: "https://h/c"}, fp)
	require.NoError(t, err)
	assert.Equal(t, 3, w.count(), "held working copies are never evicted")
	rel2()
	tick()
	rel1()
	tick()
	// Releasing triggers eviction of the least recently used idle copy (b).
	assert.Equal(t, 2, w.count())
	_, err = os.Stat(p2)
	assert.True(t, os.IsNotExist(err))
	for _, p := range []string{p1, p3} {
		_, err := os.Stat(p)
		require.NoError(t, err)
	}
	rel3()
	// Same key: same directory.
	again, lockAgain, rel, err := w.acquire(apiclient.Repository{URL: "https://h/a"}, fp)
	require.NoError(t, err)
	assert.Equal(t, p1, again)
	assert.Same(t, lock1, lockAgain, "a working copy keeps its repository lock")
	rel()

	// Expired short-lived credentials get a fresh directory.
	exp := now.Add(time.Minute)
	pe, locke, rele, err := w.acquire(apiclient.Repository{URL: "https://h/e", CredentialsExpireAt: &exp}, "tok")
	require.NoError(t, err)
	rele()
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	pe2, locke2, rele2, err := w.acquire(apiclient.Repository{URL: "https://h/e"}, "tok")
	require.NoError(t, err)
	defer rele2()
	assert.NotEqual(t, pe, pe2)
	assert.NotSame(t, locke, locke2, "a fresh working copy gets a fresh repository lock")

	// Stale directories of a previous process are removed; other names are kept.
	require.NoError(t, os.Mkdir(filepath.Join(root, "r-stale"), 0o000))
	require.NoError(t, os.Mkdir(filepath.Join(root, "keep"), 0o700))
	cleanStale(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err = os.Stat(filepath.Join(root, "r-stale"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "keep"))
	require.NoError(t, err)
	for _, p := range w.paths() {
		_ = removeTree(p)
	}
}
