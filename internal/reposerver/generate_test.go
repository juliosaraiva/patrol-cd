package reposerver

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func dirRequest(r *remote, app, path, revision string) *apiclient.ManifestRequest {
	return &apiclient.ManifestRequest{
		Repo:      r.repo(),
		Revision:  revision,
		AppName:   app,
		Namespace: "default",
		Source:    v1alpha1.ApplicationSource{RepoURL: r.url(), Path: path, TargetRevision: revision},
	}
}

func TestGenerateManifestHTTPRoundTrip(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	sha := r.commit("init", map[string]string{
		"app/cm.yaml":      configMap("cm1", map[string]string{"k": "v"}),
		"app/crd.yaml":     "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n",
		"app/README.md":    "not a manifest",
		"other/other.yaml": configMap("other", nil),
	})
	s := newTestService(t)
	c := startServer(t, s)

	req := dirRequest(r, "guestbook", "app", "main")
	req.InstanceLabelKey = "app.kubernetes.io/instance"
	req.TrackingMethod = "label"
	resp, err := c.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, sha, resp.Revision)
	assert.Equal(t, "Directory", resp.SourceType)
	objs := objects(t, resp)
	assert.ElementsMatch(t, []string{"ConfigMap/cm1", "CustomResourceDefinition/widgets.example.com"}, names(objs))
	// Tracking metadata on everything but CRDs (SRC-121).
	assert.Equal(t, "guestbook", findObj(t, objs, "ConfigMap", "cm1").GetLabels()["app.kubernetes.io/instance"])
	assert.Empty(t, findObj(t, objs, "CustomResourceDefinition", "widgets.example.com").GetLabels())

	// Unknown revision: upstream message, mapped through problem+json.
	_, err = c.GenerateManifest(bg(), dirRequest(r, "guestbook", "app", "does-not-exist"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unable to resolve 'does-not-exist' to a commit SHA")
	var apiErr *apiclient.Error
	require.ErrorAs(t, err, &apiErr)

	// Missing app path.
	_, err = c.GenerateManifest(bg(), dirRequest(r, "guestbook", "missing", "main"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing: app path does not exist")
	assert.True(t, apiclient.IsCode(err, apiclient.CodeInvalidArgument), "%v", err)
}

func TestGenerateManifestCache(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	sha1 := r.commit("v1", map[string]string{"app/cm.yaml": configMap("cm", map[string]string{"v": "1"})})
	s := newTestService(t)
	hits := func() float64 { return counter(s.metrics.cacheRequests, "manifest", "hit") }
	misses := func() float64 { return counter(s.metrics.cacheRequests, "manifest", "miss") }

	resp, err := s.GenerateManifest(bg(), dirRequest(r, "app", "app", "main"))
	require.NoError(t, err)
	assert.Equal(t, sha1, resp.Revision)
	require.Equal(t, 0.0, hits())
	missesAfterFirst := misses()

	// Same request: cache hit, no render.
	renders := renderCount(s)
	resp2, err := s.GenerateManifest(bg(), dirRequest(r, "app", "app", "main"))
	require.NoError(t, err)
	assert.Equal(t, resp.Manifests, resp2.Manifests)
	assert.Equal(t, 1.0, hits())
	assert.Equal(t, renders, renderCount(s))

	// Resolving the same commit through another spelling hits the same entry.
	_, err = s.GenerateManifest(bg(), dirRequest(r, "app", "app", sha1))
	require.NoError(t, err)
	assert.Equal(t, 2.0, hits())

	// Different API versions or destination namespace: new key.
	req := dirRequest(r, "app", "app", "main")
	req.APIVersions = []string{"v1"}
	_, err = s.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, 2.0, hits())
	assert.Greater(t, misses(), missesAfterFirst)

	// A new commit invalidates by revision (ref cache bypassed with NoRevisionCache).
	sha2 := r.commit("v2", map[string]string{"app/cm.yaml": configMap("cm", map[string]string{"v": "2"})})
	req = dirRequest(r, "app", "app", "main")
	req.NoRevisionCache = true
	resp3, err := s.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, sha2, resp3.Revision)
	assert.Equal(t, "2", findObj(t, objects(t, resp3), "ConfigMap", "cm").Object["data"].(map[string]any)["v"])

	// The old revision is still cached.
	before := hits()
	resp4, err := s.GenerateManifest(bg(), dirRequest(r, "app", "app", sha1))
	require.NoError(t, err)
	assert.Equal(t, sha1, resp4.Revision)
	assert.Equal(t, before+1, hits())

	// NoCache skips the read and re-renders.
	renders = renderCount(s)
	req = dirRequest(r, "app", "app", sha1)
	req.NoCache = true
	_, err = s.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, renders+1, renderCount(s))
}

// renderCount is the number of renders observed by the metrics.
func renderCount(s *Service) int {
	ch := make(chan prometheus.Metric, 64)
	s.metrics.renderDuration.Collect(ch)
	close(ch)
	total := 0
	for m := range ch {
		var d dto.Metric
		if err := m.Write(&d); err == nil {
			total += int(d.GetHistogram().GetSampleCount())
		}
	}
	return total
}

func TestGenerateManifestParallelRenders(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	shas := []string{}
	for i := range 3 {
		shas = append(shas, r.commit(fmt.Sprintf("c%d", i), map[string]string{
			"app/cm.yaml": configMap("cm", map[string]string{"rev": fmt.Sprint(i)}),
		}))
	}
	s := newTestService(t, func(c *Config) { c.ParallelismLimit = 4 })
	var wg sync.WaitGroup
	errs := make(chan error, 48)
	for i := range 48 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rev := i % 3
			req := dirRequest(r, fmt.Sprintf("app-%d", i%7), "app", shas[rev])
			req.NoCache = i%5 == 0
			resp, err := s.GenerateManifest(bg(), req)
			if err != nil {
				errs <- err
				return
			}
			objs, err := manifestsOf(resp)
			if err != nil {
				errs <- err
				return
			}
			got := objs[0].Object["data"].(map[string]any)["rev"]
			if got != fmt.Sprint(rev) || resp.Revision != shas[rev] {
				errs <- fmt.Errorf("revision %s rendered %v", shas[rev], got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assert.Equal(t, 1, s.workdirs.count(), "one working copy per repository and credentials")
}

func TestGenerateManifestCredentialSeparation(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	sha := r.commit("init", map[string]string{"app/cm.yaml": configMap("cm", nil)})
	s := newTestService(t)
	for _, creds := range []apiclient.Repository{
		{URL: r.url()},
		{URL: r.url(), Username: "alice", Password: "secret-a"},
		{URL: r.url(), Username: "bob", Password: "secret-b"},
		{URL: r.url(), Username: "alice", Password: "secret-a"},
	} {
		req := dirRequest(r, "app", "app", sha)
		req.Repo = creds
		req.NoCache = true // the manifest cache is per application, not per credential
		_, err := s.GenerateManifest(bg(), req)
		require.NoError(t, err)
	}
	paths := s.workdirs.paths()
	require.Len(t, paths, 3, "different credentials never share a working copy")
	for _, p := range paths {
		st, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0), st.Mode().Perm(), "idle working copies are not accessible")
	}
	// Two different credential sets never resolve to the same directory.
	fpA := s.fp.of(apiclient.Repository{URL: r.url(), Username: "alice", Password: "secret-a"})
	fpB := s.fp.of(apiclient.Repository{URL: r.url(), Username: "alice", Password: "secret-b"})
	assert.NotEqual(t, fpA, fpB)
	assert.NotContains(t, fpA, "secret")
}

func TestGenerateManifestOutOfBoundsSymlinks(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.commit("init", map[string]string{
		"app/cm.yaml": configMap("cm", nil),
		"app/passwd":  "->/etc/passwd",
		"app/up":      "->../../outside",
	})
	s := newTestService(t)
	_, err := s.GenerateManifest(bg(), dirRequest(r, "app", "app", "main"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository contains out-of-bounds symlinks. file: app/")

	allow := newTestService(t, func(c *Config) { c.AllowOutOfBoundsSymlinks = true })
	resp, err := allow.GenerateManifest(bg(), dirRequest(r, "app", "app", "main"))
	require.NoError(t, err)
	assert.Equal(t, []string{"ConfigMap/cm"}, names(objects(t, resp)))
}

func TestGenerateManifestErrorCaching(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	r.commit("bad", map[string]string{"app/bad.json": `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}{"x":1}`})
	s := newTestService(t, func(c *Config) { c.PauseGenerationAfterFailedAttempts = 2; c.PauseGenerationRequests = 2 })
	req := dirRequest(r, "app", "app", "main")
	for range 2 {
		_, err := s.GenerateManifest(bg(), req)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), cachedManifestGenerationPrefix)
	}
	_, err := s.GenerateManifest(bg(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), cachedManifestGenerationPrefix+": ")

	// NoCache bypasses the error cache.
	noCache := *req
	noCache.NoCache = true
	_, err = s.GenerateManifest(bg(), &noCache)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), cachedManifestGenerationPrefix)

	// After PauseGenerationMinutes, generation is retried.
	clock := time.Now()
	var mu sync.Mutex
	timed := newTestService(t, func(c *Config) {
		c.PauseGenerationAfterFailedAttempts = 1
		c.PauseGenerationMinutes = 5
		c.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	})
	_, err = timed.GenerateManifest(bg(), req)
	require.Error(t, err)
	_, err = timed.GenerateManifest(bg(), req)
	require.ErrorContains(t, err, cachedManifestGenerationPrefix)
	mu.Lock()
	clock = clock.Add(5 * time.Minute)
	mu.Unlock()
	_, err = timed.GenerateManifest(bg(), req)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), cachedManifestGenerationPrefix)

	// After PauseGenerationRequests cached answers, generation is retried.
	_, err = s.GenerateManifest(bg(), req)
	require.ErrorContains(t, err, cachedManifestGenerationPrefix)
	_, err = s.GenerateManifest(bg(), req)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), cachedManifestGenerationPrefix)
}

// TestGenerateManifestErrorCachingSkipsContentFailures checks that failures to obtain
// the main source's content (git fetch, OCI pull, Helm chart download) are not paused
// by the manifest-generation error cache (upstream returns them before generation):
// once the remote recovers, the next request renders.
func TestGenerateManifestErrorCachingSkipsContentFailures(t *testing.T) {
	t.Parallel()
	pause := func(c *Config) { c.PauseGenerationAfterFailedAttempts = 1 }

	t.Run("git fetch", func(t *testing.T) {
		t.Parallel()
		r := newRemote(t)
		r.commit("init", map[string]string{"app/cm.yaml": configMap("a", nil)})
		s := newTestService(t, pause)
		_, err := s.GenerateManifest(bg(), dirRequest(r, "app", "app", "main"))
		require.NoError(t, err)
		sha := r.commit("second", map[string]string{"app/cm2.yaml": configMap("b", nil)})
		moved := r.bare + ".moved"
		require.NoError(t, os.Rename(r.bare, moved))
		req := dirRequest(r, "app", "app", sha) // a full SHA needs no ls-remote
		for range 3 {
			_, err := s.GenerateManifest(bg(), req)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), cachedManifestGenerationPrefix)
			assert.Contains(t, err.Error(), "failed to initialize repository resources")
		}
		require.NoError(t, os.Rename(moved, r.bare))
		resp, err := s.GenerateManifest(bg(), req)
		require.NoError(t, err, "a recovered remote renders at once")
		assert.Equal(t, []string{"ConfigMap/a", "ConfigMap/b"}, names(objects(t, resp)))
	})

	t.Run("oci pull", func(t *testing.T) {
		t.Parallel()
		reg := ocitest.New(t, ocitest.Options{})
		layer := tarGz(t, [][2]string{{"app/cm.yaml", configMap("from-oci", nil)}})
		d := reg.PushArtifact("team/app", "1.0.0", "application/vnd.oci.image.config.v1+json", []byte("{}"),
			[]ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: layer}}, nil)
		host, fail := flakyProxy(t, reg.URL())
		url := "oci://" + host + "/team/app"
		s := newTestService(t, pause)
		req := &apiclient.ManifestRequest{
			Repo:    apiclient.Repository{URL: url, Type: v1alpha1.RepositoryTypeOCI, InsecureOCIForceHTTP: true},
			AppName: "app", Source: v1alpha1.ApplicationSource{RepoURL: url, Path: "app", TargetRevision: d.Digest.String()},
		}
		fail.Store(true)
		for range 3 {
			_, err := s.GenerateManifest(bg(), req)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), cachedManifestGenerationPrefix)
		}
		fail.Store(false)
		resp, err := s.GenerateManifest(bg(), req)
		require.NoError(t, err)
		assert.Equal(t, []string{"ConfigMap/from-oci"}, names(objects(t, resp)))
	})

	t.Run("helm chart download", func(t *testing.T) {
		t.Parallel()
		requireHelm(t)
		cr := newChartRepo(t)
		s := newTestService(t, pause)
		req := &apiclient.ManifestRequest{
			Repo: cr.repo(), AppName: "release", Namespace: "ns",
			Source: v1alpha1.ApplicationSource{RepoURL: cr.url, Chart: "demo", TargetRevision: "1.0.0"},
		}
		cr.failCharts.Store(true)
		for range 3 {
			_, err := s.GenerateManifest(bg(), req)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), cachedManifestGenerationPrefix)
			assert.True(t, strings.HasPrefix(err.Error(), chartFetchErrorPrefix), "render reports chart pulls with %q: %v", chartFetchErrorPrefix, err)
		}
		cr.failCharts.Store(false)
		resp, err := s.GenerateManifest(bg(), req)
		require.NoError(t, err)
		assert.Equal(t, "1.0.0", resp.Revision)
	})

	t.Run("ref source checkout is recorded", func(t *testing.T) {
		t.Parallel()
		app := newRemote(t)
		app.commit("init", map[string]string{"chart/Chart.yaml": "apiVersion: v2\nname: c\nversion: 0.1.0\n", "chart/templates/cm.yaml": configMap("c", nil)})
		values := newRemote(t)
		valuesSHA := values.commit("values", map[string]string{"prod.yaml": "x: 1\n"})
		s := newTestService(t, pause)
		req := &apiclient.ManifestRequest{
			Repo: app.repo(), AppName: "app", HasMultipleSources: true,
			Source: v1alpha1.ApplicationSource{RepoURL: app.url(), Path: "chart", TargetRevision: "main",
				Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/prod.yaml"}}},
			RefSources: map[string]apiclient.RefTarget{"$values": {Repo: values.repo(), TargetRevision: valuesSHA}},
		}
		require.NoError(t, os.Rename(values.bare, values.bare+".moved"))
		_, err := s.GenerateManifest(bg(), req)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), cachedManifestGenerationPrefix)
		_, err = s.GenerateManifest(bg(), req)
		require.ErrorContains(t, err, cachedManifestGenerationPrefix, "upstream counts referenced-source checkout failures")
	})
}

// TestWorkingCopyLockStateIsReleased checks that repository lock state does not
// outlive its working copy: with rotating credentials every rotation is a new working
// copy, and evicted ones must not leave lock state behind for the life of the process.
func TestWorkingCopyLockStateIsReleased(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	sha := r.commit("init", map[string]string{"app/cm.yaml": configMap("a", nil)})
	s := newTestService(t, func(c *Config) { c.MaxWorkingCopies = 1 })
	var locks []weak.Pointer[git.RepoLock]
	for i := range 4 {
		repo := r.repo()
		repo.Username, repo.Password = "u", fmt.Sprintf("rotated-token-%d", i)
		req := dirRequest(r, "app", "app", sha)
		req.Repo, req.NoCache = repo, true
		_, err := s.GenerateManifest(bg(), req)
		require.NoError(t, err)
		_, lock, release, err := s.workdirs.acquire(repo, s.fp.of(repo))
		require.NoError(t, err)
		locks = append(locks, weak.Make(lock))
		release()
	}
	assert.Equal(t, 1, s.workdirs.count(), "working copies of rotated credentials are evicted")
	runtime.GC()
	runtime.GC()
	for i, w := range locks[:len(locks)-1] {
		assert.Nil(t, w.Value(), "lock state of rotation %d is released with its working copy", i)
	}
	assert.NotNil(t, locks[len(locks)-1].Value(), "the live working copy keeps its lock")
}

func TestGenerateManifestMultiSourceRefs(t *testing.T) {
	t.Parallel()
	app := newRemote(t)
	app.commit("init", map[string]string{"cm.yaml": configMap("cm", nil)})
	values := newRemote(t)
	vsha := values.commit("values", map[string]string{"env/values.yaml": "replicas: 3\n"})
	s := newTestService(t)

	// Ref-only source: revision resolved, nothing checked out.
	ref := &apiclient.ManifestRequest{
		Repo: values.repo(), Revision: "main", AppName: "app", HasMultipleSources: true,
		Source: v1alpha1.ApplicationSource{RepoURL: values.url(), Ref: "values", TargetRevision: "main"},
	}
	resp, err := s.GenerateManifest(bg(), ref)
	require.NoError(t, err)
	assert.Equal(t, vsha, resp.Revision)
	assert.Empty(t, resp.Manifests)
	assert.Equal(t, 0, s.workdirs.count())

	base := func(valueFiles []string, refs map[string]apiclient.RefTarget) *apiclient.ManifestRequest {
		return &apiclient.ManifestRequest{
			Repo: app.repo(), Revision: "main", AppName: "app", HasMultipleSources: true, RefSources: refs,
			Source: v1alpha1.ApplicationSource{RepoURL: app.url(), Path: ".", TargetRevision: "main",
				Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: valueFiles}},
		}
	}
	valuesRef := apiclient.RefTarget{Repo: values.repo(), TargetRevision: "main"}
	for name, tc := range map[string]struct {
		req  *apiclient.ManifestRequest
		want string
	}{
		"no refs": {base([]string{"$values/env/values.yaml"}, nil),
			`source referenced "$values", but no source has a 'ref' field defined`},
		"unknown ref": {base([]string{"$other/x.yaml"}, map[string]apiclient.RefTarget{"$values": valuesRef, "$b": valuesRef}),
			`source referenced "$other", which is not one of the available sources ($b, $values)`},
		"chart ref": {base([]string{"$values/x.yaml"}, map[string]apiclient.RefTarget{"$values": {Repo: values.repo(), Chart: "c"}}),
			`source has a 'chart' field defined, but Helm charts are not yet not supported for 'ref' sources`},
		"multiple revisions": {base([]string{"$a/x.yaml", "$b/y.yaml"}, map[string]apiclient.RefTarget{
			"$a": {Repo: values.repo(), TargetRevision: "main"}, "$b": {Repo: values.repo(), TargetRevision: vsha}}),
			`cannot reference multiple revisions for the same repository ($b references "` + vsha + `" while $a references "main")`},
		"bad key": {base(nil, map[string]apiclient.RefTarget{"$va/lues": valuesRef}),
			`sources.ref va/lues cannot contain any special characters except '_' and '-'`},
	} {
		_, err := s.GenerateManifest(bg(), tc.req)
		require.Error(t, err, name)
		assert.Equal(t, tc.want, err.Error(), name)
		assert.Equal(t, apiclient.CodeInvalidArgument, ErrorCode(err), name)
	}

	// Same repository referenced at a different commit than the application.
	appSHA := app.git("rev-parse", "HEAD")
	app.commit("second", map[string]string{"cm2.yaml": configMap("cm2", nil)})
	req := base([]string{"$self/cm.yaml"}, map[string]apiclient.RefTarget{"$self": {Repo: app.repo(), TargetRevision: appSHA}})
	req.NoRevisionCache = true
	_, err = s.GenerateManifest(bg(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot reference a different revision of the same repository ($self references")
}

func TestGenerateManifestRefValuesHelm(t *testing.T) {
	t.Parallel()
	requireHelm(t)
	app := newRemote(t)
	app.commit("chart", map[string]string{
		"chart/Chart.yaml":        "apiVersion: v2\nname: demo\nversion: 0.1.0\n",
		"chart/values.yaml":       "replicas: 1\n",
		"chart/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\ndata:\n  replicas: {{ .Values.replicas | quote }}\n",
	})
	values := newRemote(t)
	v1 := values.commit("v1", map[string]string{"env/values.yaml": "replicas: 3\n"})
	s := newTestService(t)
	req := &apiclient.ManifestRequest{
		Repo: app.repo(), Revision: "main", AppName: "demo", HasMultipleSources: true,
		RefSources: map[string]apiclient.RefTarget{"$values": {Repo: values.repo(), TargetRevision: "main"}},
		Source: v1alpha1.ApplicationSource{RepoURL: app.url(), Path: "chart", TargetRevision: "main",
			Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/env/values.yaml"}}},
	}
	resp, err := s.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, "Helm", resp.SourceType)
	assert.Equal(t, "3", findObj(t, objects(t, resp), "ConfigMap", "demo").Object["data"].(map[string]any)["replicas"])
	require.Equal(t, 2, s.workdirs.count())
	_ = v1

	// A push to the values repository changes the cache key (ref SHA) without an
	// application change.
	values.commit("v2", map[string]string{"env/values.yaml": "replicas: 5\n"})
	req.NoRevisionCache = true
	resp, err = s.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, "5", findObj(t, objects(t, resp), "ConfigMap", "demo").Object["data"].(map[string]any)["replicas"])

	// App details resolve `$ref` value files from the checked-out ref source.
	details, err := s.GetAppDetails(bg(), &apiclient.AppDetailsQuery{
		Repo: app.repo(), Source: req.Source, RefSources: req.RefSources, NoRevisionCache: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "Helm", details.Type)
	assert.Contains(t, details.Helm.Parameters, v1alpha1.HelmParameter{Name: "replicas", Value: "5"})

	// Escaping the referenced repository is rejected.
	bad := *req
	bad.Source.Helm = &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/../../etc/passwd"}}
	_, err = s.GenerateManifest(bg(), &bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolved to outside repository root")
}

func TestErrorPathSanitization(t *testing.T) {
	t.Parallel()
	requireKustomize(t)
	r := newRemote(t)
	r.commit("broken", map[string]string{"app/kustomization.yaml": "resources:\n- missing.yaml\n"})
	s := newTestService(t)
	_, err := s.GenerateManifest(bg(), dirRequest(r, "app", "app", "main"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), s.root)
	assert.NotContains(t, err.Error(), strings.TrimPrefix(s.root, "/private"))
	assert.Contains(t, err.Error(), placeholderCachedSource+"/app/missing.yaml")
	// Unit level: every spelling of the root is replaced.
	ps := newPathSanitizer("/tmp/x/root", "/private/tmp/x/root")
	assert.Equal(t, "open "+placeholderCachedSource+"/app/a.yaml: no such file",
		ps.apply("open /private/tmp/x/root/r-0123abcd/app/a.yaml: no such file"))
	assert.Equal(t, placeholderCachedSource+" and "+placeholderCachedSource+"/b",
		ps.apply("/tmp/x/root/r-1 and /tmp/x/root/s-2/b"))
}

func TestGenerateManifestParallelismLimit(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	sha := r.commit("init", map[string]string{"app/cm.yaml": configMap("cm", nil)})
	s := newTestService(t, func(c *Config) { c.ParallelismLimit = 1 })
	_, err := s.GenerateManifest(bg(), dirRequest(r, "cached", "app", sha))
	require.NoError(t, err)

	// Occupy the only slot: cache hits still answer, misses wait.
	require.NoError(t, s.sem.Acquire(bg(), 1))
	_, err = s.GenerateManifest(bg(), dirRequest(r, "cached", "app", sha))
	require.NoError(t, err, "cache hits are not subject to the limit")
	ctx, cancel := context.WithTimeout(bg(), 300*time.Millisecond)
	defer cancel()
	_, err = s.GenerateManifest(ctx, dirRequest(r, "miss", "app", sha))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	s.sem.Release(1)
	_, err = s.GenerateManifest(bg(), dirRequest(r, "miss", "app", sha))
	require.NoError(t, err)
}

func TestCheckoutStrategy(t *testing.T) {
	t.Parallel()
	r := newRemote(t)
	c1 := r.commit("c1", map[string]string{"app/a.yaml": configMap("a", nil), "app/b.yaml": configMap("b", nil)})
	c2 := r.commit("c2", map[string]string{"app/b.yaml": ""})
	s := newTestService(t)
	fetches := func() float64 { return counter(s.metrics.gitOps, "fetch", "success") }

	resp, err := s.GenerateManifest(bg(), dirRequest(r, "app", "app", c2))
	require.NoError(t, err)
	assert.Equal(t, []string{"ConfigMap/a"}, names(objects(t, resp)))
	assert.Equal(t, 1.0, fetches())

	// A present revision is checked out without fetching; files of other revisions
	// never leak between checkouts.
	resp, err = s.GenerateManifest(bg(), dirRequest(r, "app", "app", c1))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ConfigMap/a", "ConfigMap/b"}, names(objects(t, resp)))
	resp, err = s.GenerateManifest(bg(), dirRequest(r, "app2", "app", c2))
	require.NoError(t, err)
	assert.Equal(t, []string{"ConfigMap/a"}, names(objects(t, resp)))
	assert.Equal(t, 1.0, fetches())

	// A new commit is fetched once.
	c3 := r.commit("c3", map[string]string{"app/c.yaml": configMap("c", nil)})
	req := dirRequest(r, "app", "app", "main")
	req.NoRevisionCache = true
	resp, err = s.GenerateManifest(bg(), req)
	require.NoError(t, err)
	assert.Equal(t, c3, resp.Revision)
	assert.Equal(t, 2.0, fetches())

	// Shallow repositories fetch the revision itself.
	shallow := newTestService(t)
	sreq := dirRequest(r, "app", "app", c1)
	sreq.Repo.Depth = 1
	resp, err = shallow.GenerateManifest(bg(), sreq)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ConfigMap/a", "ConfigMap/b"}, names(objects(t, resp)))
}
