package oci_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

const plainConfig = "application/vnd.oci.image.config.v1+json"

// listTree returns "path" (files with content "path=content") for every entry
// under root, sorted.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		switch {
		case d.Type()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out = append(out, rel+"->"+target)
		case d.IsDir():
			out = append(out, rel+"/")
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out = append(out, rel+"="+string(b))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out
}

func TestPullArtifact(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{})
	c := newClient(t, reg, oci.Options{})
	repoURL := "oci://" + reg.Host() + "/apps/demo"
	ctx := context.Background()

	manifests := buildTgz(t,
		tarEntry{name: "base/", typeflag: tar.TypeDir},
		tarEntry{name: "base/deploy.yaml", body: "kind: Deployment\n"},
		tarEntry{name: "kustomization.yaml", body: "resources: [base/deploy.yaml]\n"},
		tarEntry{name: "link.yaml", typeflag: tar.TypeSymlink, linkname: "base/deploy.yaml"},
	)
	chart := buildTgz(t,
		tarEntry{name: "nginx/Chart.yaml", body: "name: nginx\n"},
		tarEntry{name: "nginx/templates/cm.yaml", body: "kind: ConfigMap\n"},
	)
	attestation := ocitest.Layer{MediaType: "application/vnd.in-toto+json", Data: []byte(`{}`)}

	tests := []struct {
		name     string
		push     func() ocispec.Descriptor
		limits   oci.Limits
		want     []string
		wantHelm bool
		wantErr  error
		errText  string
	}{
		{
			name: "gzip layer",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "gz", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: manifests}}, nil)
			},
			want: []string{"base/", "base/deploy.yaml=kind: Deployment\n", "kustomization.yaml=resources: [base/deploy.yaml]\n", "link.yaml->base/deploy.yaml"},
		},
		{
			name: "plain tar layer with attestation",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "tar", plainConfig, nil, []ocitest.Layer{
					{MediaType: oci.MediaTypeImageLayer, Data: buildTar(t, tarEntry{name: "a.yaml", body: "a: 1\n"})},
					attestation,
				}, nil)
			},
			want: []string{"a.yaml=a: 1\n"},
		},
		{
			name: "helm chart flattened",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "chart", oci.MediaTypeHelmConfig, []byte(`{}`), []ocitest.Layer{
					{MediaType: oci.MediaTypeHelmChartContent, Data: chart},
					{MediaType: "application/vnd.cncf.helm.chart.provenance.v1.prov", Data: []byte("prov")},
				}, nil)
			},
			want:     []string{"Chart.yaml=name: nginx\n", "templates/", "templates/cm.yaml=kind: ConfigMap\n"},
			wantHelm: true,
		},
		{
			name: "helm chart internal symlinks kept",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "chartlinks", oci.MediaTypeHelmConfig, nil, []ocitest.Layer{
					{MediaType: oci.MediaTypeHelmChartContent, Data: buildTgz(t,
						tarEntry{name: "nginx/Chart.yaml", body: "name: nginx\n"},
						tarEntry{name: "nginx/templates/cm.yaml", body: "kind: ConfigMap\n"},
						tarEntry{name: "nginx/link.yaml", typeflag: tar.TypeSymlink, linkname: "templates/cm.yaml"},
						tarEntry{name: "nginx/templates/self", typeflag: tar.TypeSymlink, linkname: ".."},
					)},
				}, nil)
			},
			want:     []string{"Chart.yaml=name: nginx\n", "link.yaml->templates/cm.yaml", "templates/", "templates/cm.yaml=kind: ConfigMap\n", "templates/self->.."},
			wantHelm: true,
		},
		// The chart directory becomes dest, so a link that is inside the layer
		// but outside the chart directory would point at dest's parent.
		{
			name: "helm chart symlink to layer root",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "chartup", oci.MediaTypeHelmConfig, nil, []ocitest.Layer{
					{MediaType: oci.MediaTypeHelmChartContent, Data: buildTgz(t,
						tarEntry{name: "nginx/Chart.yaml", body: "name: nginx\n"},
						tarEntry{name: "nginx/up", typeflag: tar.TypeSymlink, linkname: ".."},
					)},
				}, nil)
			},
			wantErr: sandbox.ErrPathEscape,
			errText: "oci image contains out-of-bounds symlinks. file: up",
		},
		{
			name: "helm chart nested symlink to layer root",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "chartupnested", oci.MediaTypeHelmConfig, nil, []ocitest.Layer{
					{MediaType: oci.MediaTypeHelmChartContent, Data: buildTgz(t,
						tarEntry{name: "nginx/Chart.yaml", body: "name: nginx\n"},
						tarEntry{name: "nginx/a/", typeflag: tar.TypeDir},
						tarEntry{name: "nginx/a/b", typeflag: tar.TypeSymlink, linkname: "../.."},
					)},
				}, nil)
			},
			wantErr: sandbox.ErrPathEscape,
			errText: "oci image contains out-of-bounds symlinks. file: a/b",
		},
		{
			name: "helm chart with two top-level entries",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "badchart", oci.MediaTypeHelmConfig, nil, []ocitest.Layer{
					{MediaType: oci.MediaTypeHelmChartContent, Data: buildTgz(t, tarEntry{name: "a/Chart.yaml", body: "x"}, tarEntry{name: "b", body: "y"})},
				}, nil)
			},
			wantErr: oci.ErrInvalidHelmChart,
			errText: "expected 1 directory, found 2",
		},
		{
			name: "two content layers",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "two", plainConfig, nil, []ocitest.Layer{
					{MediaType: oci.MediaTypeImageLayerGzip, Data: manifests},
					{MediaType: oci.MediaTypeImageLayer, Data: buildTar(t, tarEntry{name: "a", body: "x"})},
				}, nil)
			},
			wantErr: oci.ErrContentLayerCount,
			errText: "expected only a single oci content layer, got 2",
		},
		{
			name: "no content layer",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "none", plainConfig, nil, []ocitest.Layer{attestation}, nil)
			},
			wantErr: oci.ErrContentLayerCount,
			errText: "expected only a single oci content layer, got 0",
		},
		{
			name: "media type not allowed",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "custom", plainConfig, nil, []ocitest.Layer{{MediaType: "application/x-custom.tar", Data: buildTar(t, tarEntry{name: "a", body: "x"})}}, nil)
			},
			wantErr: oci.ErrMediaTypeNotAllowed,
			errText: "oci layer media type application/x-custom.tar is not in the list of allowed media types",
		},
		{
			name: "custom media type allowlisted",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "custom2", plainConfig, nil, []ocitest.Layer{{MediaType: "application/x-custom.tar", Data: buildTar(t, tarEntry{name: "a", body: "x"})}}, nil)
			},
			limits: oci.Limits{AllowedMediaTypes: []string{"application/x-custom.tar"}},
			want:   []string{"a=x"},
		},
		{
			name: "too many layers",
			push: func() ocispec.Descriptor {
				var ls []ocitest.Layer
				for range 11 {
					ls = append(ls, attestation)
				}
				return reg.PushArtifact("apps/demo", "many", plainConfig, nil, ls, nil)
			},
			wantErr: oci.ErrTooManyLayers,
			errText: "expected no more than 10 oci layers, got 11",
		},
		{
			name: "layer larger than limit",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "large", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: manifests}}, nil)
			},
			limits:  oci.Limits{MaxLayerBytes: 10},
			wantErr: oci.ErrLayerTooLarge,
		},
		{
			name: "extracted size over limit",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "bomb", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip,
					Data: buildTgz(t, tarEntry{name: "big", body: strings.Repeat("0", 1<<20)})}}, nil)
			},
			limits:  oci.Limits{Archive: sandbox.ArchiveLimits{MaxTotalBytes: 1 << 10}},
			wantErr: sandbox.ErrArchiveLimit,
			errText: "cannot extract contents of oci image with revision",
		},
		{
			name: "extraction limit disabled",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "bomb2", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip,
					Data: buildTgz(t, tarEntry{name: "big", body: strings.Repeat("0", 2048)})}}, nil)
			},
			limits: oci.Limits{Archive: sandbox.ArchiveLimits{MaxTotalBytes: -1, MaxFileBytes: -1}},
			want:   []string{"big=" + strings.Repeat("0", 2048)},
		},
		{
			name: "path traversal entry",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "slip", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip,
					Data: buildTgz(t, tarEntry{name: "../evil.yaml", body: "x"})}}, nil)
			},
			wantErr: sandbox.ErrUnsafeArchiveEntry,
		},
		{
			name: "absolute entry",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "abs", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip,
					Data: buildTgz(t, tarEntry{name: "/etc/evil", body: "x"})}}, nil)
			},
			wantErr: sandbox.ErrAbsolutePath,
		},
		{
			name: "escaping symlink",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "symlink", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip,
					Data: buildTgz(t, tarEntry{name: "passwd", typeflag: tar.TypeSymlink, linkname: "../../../../etc/passwd"})}}, nil)
			},
			wantErr: sandbox.ErrPathEscape,
		},
		{
			name: "write through symlinked dir",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "symdir", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip,
					Data: buildTgz(t,
						tarEntry{name: "d", typeflag: tar.TypeSymlink, linkname: "."},
						tarEntry{name: "d/x", body: "x"})}}, nil)
			},
			wantErr: sandbox.ErrUnsafeArchiveEntry,
		},
		{
			name: "corrupt gzip",
			push: func() ocispec.Descriptor {
				return reg.PushArtifact("apps/demo", "corrupt", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: []byte("not gzip")}}, nil)
			},
			wantErr: sandbox.ErrInvalidArchive,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.push()
			parent := t.TempDir()
			dest := filepath.Join(parent, "out")
			art, err := c.PullArtifact(ctx, repoURL, d.Digest.String(), dest, tt.limits)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("PullArtifact error = %v, want %v", err, tt.wantErr)
				}
				if tt.errText != "" && !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("error %q does not contain %q", err, tt.errText)
				}
				// Nothing is left behind: no destination, no staging directory.
				entries, _ := os.ReadDir(parent)
				if len(entries) != 0 {
					t.Fatalf("leftovers after failure: %v", entries)
				}
				return
			}
			if err != nil {
				t.Fatalf("PullArtifact: %v", err)
			}
			if art.IsHelmChart != tt.wantHelm || art.Digest != d.Digest.String() {
				t.Fatalf("artifact = %+v", art)
			}
			if got := listTree(t, dest); strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("tree = %q, want %q", got, tt.want)
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 1 {
				t.Fatalf("staging left behind: %v", entries)
			}
		})
	}
}

func TestPullArtifactRejectsTamperedLayer(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{})
	c := newClient(t, reg, oci.Options{})
	real := buildTgz(t, tarEntry{name: "a", body: "real"})
	evil := buildTgz(t, tarEntry{name: "a", body: "evil"})
	if len(real) != len(evil) {
		t.Fatalf("fixture sizes differ: %d vs %d", len(real), len(evil))
	}
	d := reg.PushArtifact("repo", "x", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: real}}, nil)
	// The registry now serves different bytes (same length) for the layer digest.
	reg.PutBlobRaw("repo", digest.FromBytes(real), evil)
	dest := filepath.Join(t.TempDir(), "out")
	_, err := c.PullArtifact(context.Background(), reg.Host()+"/repo", d.Digest.String(), dest, oci.Limits{})
	if err == nil || !strings.Contains(err.Error(), "mismatched digest") {
		t.Fatalf("tampered layer: err = %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination created for tampered layer: %v", err)
	}
}

func TestPullArtifactArguments(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{})
	c := newClient(t, reg, oci.Options{})
	d := reg.PushArtifact("repo", "1.0.0", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayer, Data: buildTar(t, tarEntry{name: "a", body: "x"})}}, nil)
	if _, err := c.PullArtifact(context.Background(), reg.Host()+"/repo", "1.0.0", t.TempDir(), oci.Limits{}); !errors.Is(err, oci.ErrInvalidReference) {
		t.Fatalf("tag accepted as digest: %v", err)
	}
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PullArtifact(context.Background(), reg.Host()+"/repo", d.Digest.String(), nonEmpty, oci.Limits{}); !errors.Is(err, oci.ErrDestinationExists) {
		t.Fatalf("non-empty destination accepted: %v", err)
	}
	empty := t.TempDir()
	if _, err := c.PullArtifact(context.Background(), reg.Host()+"/repo", d.Digest.String(), empty, oci.Limits{}); err != nil {
		t.Fatalf("empty destination rejected: %v", err)
	}
	if got := listTree(t, empty); len(got) != 1 || got[0] != "a=x" {
		t.Fatalf("tree = %v", got)
	}
}

func TestPullArtifactCache(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{Username: "u", Password: "p"})
	cacheDir := t.TempDir()
	c := newClient(t, reg, oci.Options{Username: "u", Password: "p", CacheDir: cacheDir})
	layer := buildTgz(t, tarEntry{name: "a", body: "x"})
	d := reg.PushArtifact("repo", "1", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: layer}}, nil)
	repoURL := reg.Host() + "/repo"
	blobKey := "blob:" + digest.FromBytes(layer).String()
	for i := range 2 {
		if _, err := c.PullArtifact(context.Background(), repoURL, d.Digest.String(), filepath.Join(t.TempDir(), "o"), oci.Limits{}); err != nil {
			t.Fatalf("pull %d: %v", i, err)
		}
	}
	if n := reg.Requests(blobKey); n != 1 {
		t.Fatalf("layer downloaded %d times, want 1", n)
	}
	// The cache never bypasses authorization: the manifest is fetched again.
	anon := newClient(t, reg, oci.Options{CacheDir: cacheDir})
	if _, err := anon.PullArtifact(context.Background(), repoURL, d.Digest.String(), filepath.Join(t.TempDir(), "o"), oci.Limits{}); !errors.Is(err, oci.ErrUnauthorized) {
		t.Fatalf("anonymous pull served from cache: %v", err)
	}
	if err := c.CleanCache(repoURL); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PullArtifact(context.Background(), repoURL, d.Digest.String(), filepath.Join(t.TempDir(), "o"), oci.Limits{}); err != nil {
		t.Fatal(err)
	}
	if n := reg.Requests(blobKey); n != 2 {
		t.Fatalf("layer downloaded %d times after CleanCache, want 2", n)
	}
}

func TestFetchHelmChart(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{})
	c := newClient(t, reg, oci.Options{})
	tgz := buildTgz(t, tarEntry{name: "mychart/Chart.yaml", body: "name: mychart\n"})
	d := reg.PushHelmChart("charts/mychart", "1.0.0+build.7", tgz)
	var buf bytes.Buffer
	got, err := c.FetchHelmChart(context.Background(), reg.Host()+"/charts/mychart", "1.0.0+build.7", &buf, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got != d.Digest.String() || !bytes.Equal(buf.Bytes(), tgz) {
		t.Fatalf("FetchHelmChart = %q (%d bytes)", got, buf.Len())
	}
	if _, err := c.FetchHelmChart(context.Background(), reg.Host()+"/charts/mychart", "1.0.0+build.7", &bytes.Buffer{}, 10); !errors.Is(err, oci.ErrLayerTooLarge) {
		t.Fatalf("expected ErrLayerTooLarge, got %v", err)
	}
	if _, err := c.FetchHelmChart(context.Background(), reg.Host()+"/charts/mychart", "2.0.0", &bytes.Buffer{}, -1); !errors.Is(err, oci.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	reg.PushArtifact("charts/notachart", "1.0.0", plainConfig, nil, []ocitest.Layer{{MediaType: oci.MediaTypeImageLayerGzip, Data: tgz}}, nil)
	if _, err := c.FetchHelmChart(context.Background(), reg.Host()+"/charts/notachart", "1.0.0", &bytes.Buffer{}, -1); !errors.Is(err, oci.ErrContentLayerCount) {
		t.Fatalf("expected ErrContentLayerCount, got %v", err)
	}
}
