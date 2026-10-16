package helm_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

var fixtureFiles = []string{"Chart.yaml", "crds/widgets.yaml", "templates/configmap.yaml", "templates/tests/test-connection.yaml", "values.schema.json", "values.yaml"}

func TestPullChartHTTP(t *testing.T) {
	t.Parallel()
	repo := repoWithVersions(t, true, "admin", "s3cret", "0.1.0", "0.2.0")
	c := newClient(t)
	creds := helm.Creds{Username: "admin", Password: "s3cret", CAData: repo.caPEM()}
	ctx := context.Background()

	dir, err := c.PullChart(ctx, repo.URL(), "mychart", "0.2.0", creds, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := treeFiles(t, dir); !slices.Equal(got, fixtureFiles) {
		t.Fatalf("extracted %v", got)
	}
	chartYAML, _ := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	mustContain(t, string(chartYAML), "version: 0.2.0")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	// The archive is cached: a second pull extracts again without downloading.
	dir2, err := c.PullChart(ctx, repo.URL(), "mychart", "0.2.0", creds, false)
	if err != nil {
		t.Fatal(err)
	}
	if dir2 == dir {
		t.Fatal("PullChart reused an extraction directory")
	}
	if n := repo.count("/mychart-0.2.0.tgz"); n != 1 {
		t.Fatalf("chart downloaded %d times, want 1", n)
	}
	// Hard refresh removes the cached archive.
	if err := c.CleanChartCache(repo.URL(), "mychart", "0.2.0", creds, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PullChart(ctx, repo.URL(), "mychart", "0.2.0", creds, false); err != nil {
		t.Fatal(err)
	}
	if n := repo.count("/mychart-0.2.0.tgz"); n != 2 {
		t.Fatalf("chart downloaded %d times after CleanChartCache, want 2", n)
	}
	// Other credentials never get the cached archive.
	if _, err := c.PullChart(ctx, repo.URL(), "mychart", "0.2.0", helm.Creds{CAData: repo.caPEM()}, false); err == nil {
		t.Fatal("anonymous pull succeeded")
	}
	// Unknown version.
	_, err = c.PullChart(ctx, repo.URL(), "mychart", "9.9.9", creds, false)
	if !errors.Is(err, helm.ErrVersionNotFound) {
		t.Fatalf("unknown version: %v", err)
	}
	mustContain(t, err.Error(), `error fetching chart: chart "mychart" version "9.9.9" not found in `)
	mustNotContain(t, err.Error(), "s3cret")
}

func TestPullChartNewVersionAfterCachedIndex(t *testing.T) {
	t.Parallel()
	repo := repoWithVersions(t, false, "", "", "0.1.0")
	c := newClient(t)
	ctx := context.Background()
	if _, err := c.ResolveChartVersion(ctx, repo.URL(), "mychart", "*", helm.Creds{}, false); err != nil {
		t.Fatal(err)
	}
	repo.addChart("mychart", "0.3.0", packageChart(t, fixtureChart, "mychart", "0.3.0"), "")
	if _, err := c.PullChart(ctx, repo.URL(), "mychart", "0.3.0", helm.Creds{}, false); err != nil {
		t.Fatalf("pin to a version newer than the cached index: %v", err)
	}
}

func TestPullChartCredentialScope(t *testing.T) {
	t.Parallel()
	// Charts hosted on another origin get credentials only with PassCredentials.
	charts := newChartRepo(t, false, "", "")
	archive := packageChart(t, fixtureChart, "mychart", "1.0.0")
	charts.setFile("/mychart-1.0.0.tgz", archive)
	index := newChartRepo(t, false, "", "")
	index.addChart("mychart", "1.0.0", archive, charts.URL()+"/mychart-1.0.0.tgz")
	index.addChart("mychart", "1.0.1", archive, "")
	index.setFile("/mychart-1.0.1.tgz", archive)

	ctx := context.Background()
	creds := helm.Creds{Username: "u", Password: "p"}
	c := newClient(t)
	if _, err := c.PullChart(ctx, index.URL(), "mychart", "1.0.0", creds, false); err != nil {
		t.Fatal(err)
	}
	if charts.sawAuth("/mychart-1.0.0.tgz") {
		t.Fatal("credentials sent to a foreign chart host without PassCredentials")
	}
	if _, err := c.PullChart(ctx, index.URL(), "mychart", "1.0.1", creds, false); err != nil {
		t.Fatal(err)
	}
	if !index.sawAuth("/mychart-1.0.1.tgz") {
		t.Fatal("credentials not sent to the repository host")
	}
	pass := creds
	pass.PassCredentials = true
	if _, err := c.PullChart(ctx, index.URL(), "mychart", "1.0.0", pass, false); err != nil {
		t.Fatal(err)
	}
	if !charts.sawAuth("/mychart-1.0.0.tgz") {
		t.Fatal("PassCredentials did not send credentials to the chart host")
	}
}

func TestPullChartRelativeURLs(t *testing.T) {
	t.Parallel()
	repo := newChartRepo(t, false, "", "")
	archive := packageChart(t, fixtureChart, "mychart", "1.0.0")
	repo.setFile("/sub/charts/mychart-1.0.0.tgz", archive)
	repo.setFile("/sub/index.yaml", []byte("apiVersion: v1\nentries:\n  mychart:\n  - name: mychart\n    version: 1.0.0\n    urls: [charts/mychart-1.0.0.tgz]\n"))
	c := newClient(t)
	if _, err := c.PullChart(context.Background(), repo.URL()+"/sub/", "mychart", "1.0.0", helm.Creds{}, false); err != nil {
		t.Fatal(err)
	}
}

func TestPullChartConcurrent(t *testing.T) {
	t.Parallel()
	repo := repoWithVersions(t, false, "", "", "1.0.0")
	c := newClient(t)
	var wg sync.WaitGroup
	dirs := make([]string, 8)
	for i := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := c.PullChart(context.Background(), repo.URL(), "mychart", "1.0.0", helm.Creds{}, false)
			if err != nil {
				t.Error(err)
			}
			dirs[i] = d
		}()
	}
	wg.Wait()
	if n := repo.count("/mychart-1.0.0.tgz"); n != 1 {
		t.Fatalf("concurrent pulls downloaded %d times", n)
	}
	slices.Sort(dirs)
	if len(slices.Compact(dirs)) != len(dirs) {
		t.Fatal("concurrent pulls shared an extraction directory")
	}
}

func tgz(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range entries {
		body := ""
		if h.Typeflag == tar.TypeReg || h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
			body = strings.Repeat("x", int(h.Size))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPullChartRejectsMaliciousArchives(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		archive func(t *testing.T) []byte
		client  func(c *helm.Client)
		wantErr error
	}{
		{name: "path traversal", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "mychart/Chart.yaml", Size: 4}, tar.Header{Name: "mychart/../../../escape.txt", Size: 4})
		}, wantErr: sandbox.ErrUnsafeArchiveEntry},
		{name: "absolute path", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "/tmp/patrol-escape.txt", Size: 4})
		}, wantErr: sandbox.ErrAbsolutePath},
		{name: "symlink escape", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "mychart/Chart.yaml", Size: 4},
				tar.Header{Name: "mychart/values.yaml", Typeflag: tar.TypeSymlink, Linkname: "../../../../../etc/passwd"})
		}, wantErr: sandbox.ErrPathEscape},
		// Links that stay inside the archive but leave the chart directory would
		// point into WorkDir/work once the chart directory is moved there.
		{name: "symlink to archive root", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "mychart/Chart.yaml", Size: 4},
				tar.Header{Name: "mychart/up", Typeflag: tar.TypeSymlink, Linkname: ".."})
		}, wantErr: sandbox.ErrPathEscape},
		{name: "nested symlink to archive root", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "mychart/Chart.yaml", Size: 4},
				tar.Header{Name: "mychart/a/", Typeflag: tar.TypeDir, Mode: 0o755},
				tar.Header{Name: "mychart/a/b", Typeflag: tar.TypeSymlink, Linkname: "../.."})
		}, wantErr: sandbox.ErrPathEscape},
		{name: "symlink to sibling archive dir", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "other/secret", Size: 4},
				tar.Header{Name: "mychart/Chart.yaml", Size: 4},
				tar.Header{Name: "mychart/values.yaml", Typeflag: tar.TypeSymlink, Linkname: "../other/secret"})
		}, wantErr: sandbox.ErrPathEscape},
		{name: "chart dir is a symlink", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "real/Chart.yaml", Size: 4}, tar.Header{Name: "mychart", Typeflag: tar.TypeSymlink, Linkname: "real"})
		}, wantErr: helm.ErrInvalidChart},
		{name: "device file", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "mychart/dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3})
		}, wantErr: sandbox.ErrUnsafeArchiveEntry},
		{name: "extracted size over limit", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "mychart/Chart.yaml", Size: 4}, tar.Header{Name: "mychart/big", Size: 64 << 10})
		}, client: func(c *helm.Client) { c.Archive = sandbox.ArchiveLimits{MaxTotalBytes: 1 << 10} }, wantErr: sandbox.ErrArchiveLimit},
		{name: "archive over download cap", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "mychart/Chart.yaml", Size: 4})
		}, client: func(c *helm.Client) { c.MaxChartBytes = 8 }, wantErr: helm.ErrChartTooLarge},
		{name: "wrong top-level directory", archive: func(t *testing.T) []byte {
			return tgz(t, tar.Header{Name: "other/Chart.yaml", Size: 4})
		}, wantErr: helm.ErrInvalidChart},
		{name: "not gzip", archive: func(t *testing.T) []byte { return []byte("plain text") }, wantErr: sandbox.ErrInvalidArchive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := newChartRepo(t, false, "", "")
			repo.addChart("mychart", "1.0.0", tt.archive(t), "")
			c := newClient(t)
			if tt.client != nil {
				tt.client(c)
			}
			dir, err := c.PullChart(context.Background(), repo.URL(), "mychart", "1.0.0", helm.Creds{}, false)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("PullChart = %q, %v; want %v", dir, err, tt.wantErr)
			}
			// Nothing but the cache and empty work dirs remain.
			work := filepath.Join(c.WorkDir, "work")
			if entries, _ := os.ReadDir(work); len(entries) != 0 {
				t.Fatalf("work dir not cleaned: %v", entries)
			}
			if _, err := os.Lstat("/tmp/patrol-escape.txt"); err == nil {
				t.Fatal("archive wrote outside the extraction dir")
			}
			if _, err := os.Lstat(filepath.Join(c.WorkDir, "escape.txt")); err == nil {
				t.Fatal("archive wrote outside the extraction dir")
			}
		})
	}
}

func TestPullChartOutOfBoundsSymlinkMessage(t *testing.T) {
	t.Parallel()
	repo := newChartRepo(t, false, "", "")
	repo.addChart("mychart", "1.0.0", tgz(t, tar.Header{Name: "mychart/Chart.yaml", Size: 4},
		tar.Header{Name: "mychart/up", Typeflag: tar.TypeSymlink, Linkname: ".."}), "")
	c := newClient(t)
	_, err := c.PullChart(context.Background(), repo.URL(), "mychart", "1.0.0", helm.Creds{}, false)
	var oob *sandbox.OutOfBoundsSymlinkError
	if !errors.As(err, &oob) || oob.Path != "up" {
		t.Fatalf("PullChart = %v, want *OutOfBoundsSymlinkError for up", err)
	}
	mustContain(t, err.Error(), "chart contains out-of-bounds symlinks. file: up")
}

func TestPullChartKeepsInternalSymlinks(t *testing.T) {
	t.Parallel()
	repo := newChartRepo(t, false, "", "")
	repo.addChart("mychart", "1.0.0", tgz(t,
		tar.Header{Name: "mychart/Chart.yaml", Size: 4},
		tar.Header{Name: "mychart/files/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "mychart/files/real.yaml", Size: 4},
		tar.Header{Name: "mychart/values.yaml", Typeflag: tar.TypeSymlink, Linkname: "files/real.yaml"},
		tar.Header{Name: "mychart/files/self", Typeflag: tar.TypeSymlink, Linkname: ".."},
	), "")
	c := newClient(t)
	dir, err := c.PullChart(context.Background(), repo.URL(), "mychart", "1.0.0", helm.Creds{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for link, want := range map[string]string{"values.yaml": "files/real.yaml", "files/self": "."} {
		got, err := filepath.EvalSymlinks(filepath.Join(dir, link))
		if err != nil {
			t.Fatal(err)
		}
		wantAbs, _ := filepath.EvalSymlinks(filepath.Join(dir, want))
		if got != wantAbs {
			t.Fatalf("%s resolves to %s, want %s", link, got, wantAbs)
		}
	}
}

// TestPullChartVersionFallback checks Helm's IndexFile.Get semantics for HTTP
// repositories: an exact index match wins, otherwise the highest entry that
// satisfies the requested version read as a semver constraint ("v1.2.3" means
// =1.2.3, "1.2" means 1.2.x) is pulled. ResolveChartVersion returns such pins
// verbatim, so PullChart must accept them.
func TestPullChartVersionFallback(t *testing.T) {
	t.Parallel()
	repo := repoWithVersions(t, false, "", "", "1.2.0", "1.2.3", "1.3.0", "1.2.4-rc.1", "2.0.0")
	c := newClient(t)
	ctx := context.Background()
	tests := []struct {
		version string
		want    string
		wantErr error
	}{
		{version: "v1.2.3", want: "1.2.3"},
		{version: "1.2", want: "1.2.3"},
		{version: "v1.2", want: "1.2.3"},
		{version: "1", want: "1.3.0"},
		{version: "1.2.0", want: "1.2.0"},
		{version: "1.2.4-rc.1", want: "1.2.4-rc.1"},
		{version: "2", want: "2.0.0"},
		{version: "1.4", wantErr: helm.ErrVersionNotFound},
		{version: "v1.2.5", wantErr: helm.ErrVersionNotFound},
		{version: "not-a-version", wantErr: helm.ErrVersionNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			resolved, err := c.ResolveChartVersion(ctx, repo.URL(), "mychart", tt.version, helm.Creds{}, false)
			if tt.wantErr == nil && (err != nil || resolved != tt.version) {
				t.Fatalf("ResolveChartVersion = %q, %v; want the pin verbatim", resolved, err)
			}
			dir, err := c.PullChart(ctx, repo.URL(), "mychart", tt.version, helm.Creds{}, false)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("PullChart = %q, %v; want %v", dir, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			chartYAML, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			mustContain(t, string(chartYAML), "version: "+tt.want+"\n")
		})
	}
}

func TestPullChartArguments(t *testing.T) {
	t.Parallel()
	c := newClient(t)
	for _, tc := range []struct{ chart, version string }{{"", "1.0.0"}, {"mychart", ""}, {"..", "1.0.0"}, {"a/..", "1.0.0"}} {
		if _, err := c.PullChart(context.Background(), "https://example.invalid", tc.chart, tc.version, helm.Creds{}, false); !errors.Is(err, helm.ErrInvalidArgument) {
			t.Errorf("PullChart(%q, %q) = %v", tc.chart, tc.version, err)
		}
	}
}

func TestPullChartOCI(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{Username: "u", Password: "p"})
	reg.PushHelmChart("charts/mychart", "1.2.3+meta", packageChart(t, fixtureChart, "mychart", "1.2.3+meta"))
	c := newClient(t)
	creds := helm.Creds{Username: "u", Password: "p", PlainHTTP: true}
	ctx := context.Background()
	dir, err := c.PullChart(ctx, reg.Host()+"/charts", "mychart", "1.2.3+meta", creds, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := treeFiles(t, dir); !slices.Equal(got, fixtureFiles) {
		t.Fatalf("extracted %v", got)
	}
	if _, err := c.PullChart(ctx, reg.Host()+"/charts", "mychart", "1.2.3+meta", creds, true); err != nil {
		t.Fatal(err)
	}
	if n := reg.Requests("blob"); n != 1 {
		t.Fatalf("chart layer downloaded %d times, want 1", n)
	}
	_, err = c.PullChart(ctx, reg.Host()+"/charts", "mychart", "1.2.3+meta", helm.Creds{PlainHTTP: true}, true)
	if err == nil || !strings.HasPrefix(err.Error(), "error pulling OCI chart: ") {
		t.Fatalf("anonymous OCI pull: %v", err)
	}
	reg.PushHelmChart("charts/big", "1.0.0", packageChart(t, fixtureChart, "big", "1.0.0"))
	small := &helm.Client{WorkDir: t.TempDir(), MaxChartBytes: 10}
	if _, err := small.PullChart(ctx, reg.Host()+"/charts", "big", "1.0.0", creds, true); !errors.Is(err, helm.ErrChartTooLarge) {
		t.Fatalf("expected ErrChartTooLarge, got %v", err)
	}
}
