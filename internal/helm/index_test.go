package helm_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
)

func repoWithVersions(t *testing.T, useTLS bool, user, pass string, versions ...string) *chartRepo {
	t.Helper()
	r := newChartRepo(t, useTLS, user, pass)
	for _, v := range versions {
		r.addChart("mychart", v, packageChart(t, fixtureChart, "mychart", v), "")
	}
	return r
}

func TestResolveChartVersionHTTP(t *testing.T) {
	t.Parallel()
	repo := repoWithVersions(t, false, "", "", "0.1.0", "0.2.0", "1.0.0", "1.1.0", "2.0.0-rc.1")
	c := newClient(t)
	ctx := context.Background()
	tests := []struct {
		constraint string
		chart      string
		want       string
		wantErr    error
		errText    string
	}{
		{constraint: "1.*", want: "1.1.0"},
		{constraint: "*", want: "1.1.0"},
		{constraint: "*-0", want: "2.0.0-rc.1"},
		{constraint: ">=0.1.0 <1.0.0", want: "0.2.0"},
		{constraint: "^0.1", want: "0.1.0"},
		{constraint: "^0", want: "0.2.0"},
		{constraint: "~1.0", want: "1.0.0"},
		{constraint: "0.1.0", want: "0.1.0"},
		{constraint: "9.9.9", want: "9.9.9"}, // exact pins are not checked against the index
		{constraint: "v1.2", want: "v1.2"},
		{constraint: "3.*", wantErr: helm.ErrNoMatchingVersion, errText: "invalid revision: version matching constraint not found in 5 tags"},
		{constraint: "not-a-version", wantErr: helm.ErrInvalidConstraint, errText: "invalid revision: failed to determine semver constraint"},
		{constraint: "1.*", chart: "other", wantErr: helm.ErrChartNotFound, errText: "chart 'other' not found in index"},
	}
	for _, tt := range tests {
		chart := tt.chart
		if chart == "" {
			chart = "mychart"
		}
		got, err := c.ResolveChartVersion(ctx, repo.URL(), chart, tt.constraint, helm.Creds{}, false)
		if tt.wantErr != nil {
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("%s: got %q, %v; want %v", tt.constraint, got, err, tt.wantErr)
			} else if !strings.HasPrefix(err.Error(), tt.errText) {
				t.Errorf("%s: error %q does not start with %q", tt.constraint, err, tt.errText)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%s: got %q, %v; want %q", tt.constraint, got, err, tt.want)
		}
	}
	// One index request served every constraint (TTL cache).
	if n := repo.count("/index.yaml"); n != 1 {
		t.Fatalf("index fetched %d times, want 1", n)
	}
}

func TestIndexCacheTTLAndETag(t *testing.T) {
	t.Parallel()
	repo := repoWithVersions(t, false, "", "", "1.0.0")
	c := &helm.Client{WorkDir: t.TempDir(), IndexTTL: 50 * time.Millisecond, UserAgent: "patrol-test/1"}
	ctx := context.Background()
	if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{}, false); err != nil {
		t.Fatal(err)
	}
	if n := repo.count("/index.yaml"); n != 1 {
		t.Fatalf("within TTL: %d requests", n)
	}
	// Hard refresh always asks the server (answered 304 here).
	if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{}, true); err != nil {
		t.Fatal(err)
	}
	if n := repo.count("/index.yaml"); n != 2 {
		t.Fatalf("after noCache: %d requests", n)
	}
	// After the TTL the index is revalidated; a new version becomes visible.
	repo.addChart("mychart", "1.1.0", packageChart(t, fixtureChart, "mychart", "1.1.0"), "")
	time.Sleep(60 * time.Millisecond)
	v, err := c.ResolveChartVersion(ctx, repo.URL(), "mychart", "1.*", helm.Creds{}, false)
	if err != nil || v != "1.1.0" {
		t.Fatalf("after TTL: %q, %v", v, err)
	}
	for _, ua := range repo.agents() {
		if ua != "patrol-test/1" {
			t.Fatalf("user agent %q", ua)
		}
	}
	// Different credentials never share a cache entry.
	if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{Username: "x", Password: "y"}, false); err != nil {
		t.Fatal(err)
	}
	if n := repo.count("/index.yaml"); n != 4 {
		t.Fatalf("credential-scoped cache: %d requests, want 4", n)
	}
	c.InvalidateIndex(repo.URL())
	if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{}, false); err != nil {
		t.Fatal(err)
	}
	if n := repo.count("/index.yaml"); n != 5 {
		t.Fatalf("after InvalidateIndex: %d requests, want 5", n)
	}
}

func TestIndexConcurrentFetchCollapsed(t *testing.T) {
	t.Parallel()
	repo := repoWithVersions(t, false, "", "", "1.0.0")
	c := newClient(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ResolveChartVersion(context.Background(), repo.URL(), "mychart", "*", helm.Creds{}, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := repo.count("/index.yaml"); n > 2 {
		t.Fatalf("concurrent resolutions fetched the index %d times", n)
	}
}

func TestIndexErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("basic auth", func(t *testing.T) {
		t.Parallel()
		repo := repoWithVersions(t, false, "admin", "pw", "1.0.0")
		c := newClient(t)
		_, err := c.GetIndex(ctx, repo.URL(), helm.Creds{}, false)
		if !errors.Is(err, helm.ErrIndexFetch) || !strings.Contains(err.Error(), "failed to get index: 401 Unauthorized") {
			t.Fatalf("anonymous: %v", err)
		}
		if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{Username: "admin", Password: "pw"}, false); err != nil {
			t.Fatalf("authorized: %v", err)
		}
	})
	t.Run("size cap", func(t *testing.T) {
		t.Parallel()
		repo := repoWithVersions(t, false, "", "", "1.0.0")
		c := &helm.Client{WorkDir: t.TempDir(), MaxIndexBytes: 16}
		if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{}, false); !errors.Is(err, helm.ErrIndexTooLarge) {
			t.Fatalf("expected ErrIndexTooLarge, got %v", err)
		}
	})
	t.Run("invalid yaml", func(t *testing.T) {
		t.Parallel()
		repo := newChartRepo(t, false, "", "")
		repo.setFile("/index.yaml", []byte("entries: [unclosed"))
		c := newClient(t)
		if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{}, false); err == nil || !strings.Contains(err.Error(), "error decoding index") {
			t.Fatalf("expected decode error, got %v", err)
		}
	})
	t.Run("tls with ca", func(t *testing.T) {
		t.Parallel()
		repo := repoWithVersions(t, true, "", "", "1.0.0")
		c := newClient(t)
		if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{}, false); err == nil {
			t.Fatal("untrusted TLS repository accepted")
		}
		if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{CAData: repo.caPEM()}, false); err != nil {
			t.Fatalf("with CA: %v", err)
		}
		if _, err := c.GetIndex(ctx, repo.URL(), helm.Creds{InsecureSkipVerify: true}, false); err != nil {
			t.Fatalf("insecure: %v", err)
		}
	})
	t.Run("bad url", func(t *testing.T) {
		t.Parallel()
		c := newClient(t)
		if _, err := c.GetIndex(ctx, "ftp://example.com/charts", helm.Creds{}, false); !errors.Is(err, helm.ErrInvalidArgument) {
			t.Fatalf("expected ErrInvalidArgument, got %v", err)
		}
	})
	t.Run("missing work dir", func(t *testing.T) {
		t.Parallel()
		c := &helm.Client{}
		if _, err := c.GetIndex(ctx, "https://example.com", helm.Creds{}, false); !errors.Is(err, helm.ErrInvalidArgument) {
			t.Fatalf("expected ErrInvalidArgument, got %v", err)
		}
	})
}

func TestListCharts(t *testing.T) {
	t.Parallel()
	repo := repoWithVersions(t, false, "", "", "0.1.0", "0.2.0")
	repo.addChart("another", "1.0.0", packageChart(t, fixtureChart, "another", "1.0.0"), "")
	c := newClient(t)
	got, err := c.ListCharts(context.Background(), repo.URL(), helm.Creds{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "another" || got[1].Name != "mychart" || strings.Join(got[1].Versions, ",") != "0.1.0,0.2.0" {
		t.Fatalf("ListCharts = %+v", got)
	}
}

func TestResolveChartVersionOCI(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{Username: "u", Password: "p"})
	for _, v := range []string{"1.0.0", "1.2.0", "1.2.1+build.5", "2.0.0"} {
		reg.PushHelmChart("charts/mychart", v, packageChart(t, fixtureChart, "mychart", v))
	}
	c := newClient(t)
	creds := helm.Creds{Username: "u", Password: "p", PlainHTTP: true}
	ctx := context.Background()
	// enableOCI with a schemeless URL, and implicit OCI for a schemeless URL that
	// parses without a host ("localhost:port/..."; an IP:port does not parse and
	// is not detected, as upstream).
	_, port, _ := strings.Cut(reg.Host(), ":")
	for _, tc := range []struct {
		url    string
		enable bool
	}{{reg.Host() + "/charts", true}, {"localhost:" + port + "/charts", false}} {
		got, err := c.ResolveChartVersion(ctx, tc.url, "mychart", "1.*", creds, tc.enable)
		if err != nil || got != "1.2.1+build.5" {
			t.Fatalf("%s enableOCI=%v: %q, %v", tc.url, tc.enable, got, err)
		}
	}
	if n := reg.Requests("tags"); n != 2 {
		t.Fatalf("tags listed %d times, want 2", n)
	}
	if _, err := c.ResolveChartVersion(ctx, reg.Host()+"/charts", "mychart", "1.*", creds, true); err != nil {
		t.Fatal(err)
	}
	if n := reg.Requests("tags"); n != 2 {
		t.Fatalf("tags listed %d times, want 2 (cached)", n)
	}
	if _, err := c.ResolveChartVersion(ctx, reg.Host()+"/charts", "mychart", "1.*", helm.Creds{PlainHTTP: true}, true); !errors.Is(err, oci.ErrUnauthorized) ||
		!strings.HasPrefix(err.Error(), "unable to get tags: ") {
		t.Fatalf("anonymous: %v", err)
	}
	if got, err := c.ResolveChartVersion(ctx, reg.Host()+"/charts", "mychart", "3.0.0", helm.Creds{}, true); err != nil || got != "3.0.0" {
		t.Fatalf("exact pin: %q, %v", got, err)
	}
	if !helm.IsHelmOCIRepo("registry.example.com/charts") || !helm.IsHelmOCIRepo("localhost:5000/charts") ||
		helm.IsHelmOCIRepo("127.0.0.1:5000/charts") || helm.IsHelmOCIRepo("https://charts.example.com") || helm.IsHelmOCIRepo("") {
		t.Fatal("IsHelmOCIRepo")
	}
}

// TestClosestIndexEntryProperties checks the constraint fallback used by
// PullChart: the selected entry satisfies the request, no satisfying entry has a
// higher version, the choice does not depend on index order, and a request that
// matches nothing (or is not a constraint) selects nothing.
func TestClosestIndexEntryProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		gen := rapid.Custom(func(rt *rapid.T) string {
			v := fmt.Sprintf("%d.%d.%d", rapid.IntRange(0, 3).Draw(rt, "maj"), rapid.IntRange(0, 3).Draw(rt, "min"), rapid.IntRange(0, 3).Draw(rt, "pat"))
			if rapid.IntRange(0, 4).Draw(rt, "pre") == 0 {
				v += "-rc.1"
			}
			return v
		})
		versions := rapid.SliceOfNDistinct(gen, 0, 12, func(s string) string { return s }).Draw(rt, "versions")
		versions = append(versions, rapid.SliceOfN(rapid.SampledFrom([]string{"latest", "", "x.y"}), 0, 2).Draw(rt, "junk")...)
		request := rapid.SampledFrom([]string{
			"0", "1", "2", "3", "1.2", "v1.2", "0.0", "2.3", "v1.2.3", "1.2.3", "3.3.3", "0.0.0", "1.2.3-rc.1", "~1.1", "^2", ">=1.0.0 <2.0.0", "*", "not a constraint",
		}).Draw(rt, "request")

		entries := make([]helm.ChartVersion, len(versions))
		for i, v := range versions {
			entries[i] = helm.ChartVersion{Name: "c", Version: v}
		}
		got, ok := helm.ClosestIndexEntry(entries, request)

		constraint, cerr := semver.NewConstraint(request)
		var satisfying []*semver.Version
		for _, v := range versions {
			if sv, err := semver.NewVersion(v); err == nil && cerr == nil && constraint.Check(sv) {
				satisfying = append(satisfying, sv)
			}
		}
		if ok != (len(satisfying) > 0) {
			rt.Fatalf("ok = %v with %d satisfying entries", ok, len(satisfying))
		}
		if !ok {
			return
		}
		gv := semver.MustParse(got.Version)
		if !constraint.Check(gv) {
			rt.Fatalf("selected %s does not satisfy %q", got.Version, request)
		}
		for _, sv := range satisfying {
			if sv.GreaterThan(gv) {
				rt.Fatalf("selected %s but %s also satisfies %q", got.Version, sv.Original(), request)
			}
		}
		shuffled := slices.Clone(entries)
		slices.Reverse(shuffled)
		if again, _ := helm.ClosestIndexEntry(shuffled, request); again.Version != got.Version {
			rt.Fatalf("order dependent: %s vs %s", got.Version, again.Version)
		}
	})
}
