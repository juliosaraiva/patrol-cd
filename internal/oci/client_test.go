package oci_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/oci/ocitest"
)

func TestParseRepository(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in       string
		registry string
		path     string
		wantErr  bool
	}{
		{in: "oci://registry.example.com/charts/nginx", registry: "registry.example.com", path: "charts/nginx"},
		{in: "registry.example.com/charts/nginx", registry: "registry.example.com", path: "charts/nginx"},
		{in: "oci://localhost:5000/app", registry: "localhost:5000", path: "app"},
		{in: "oci://oci://localhost:5000/app", wantErr: true},
		{in: "https://registry.example.com/app", wantErr: true},
		{in: "oci://registry.example.com/app:1.0.0", wantErr: true},
		{in: "oci://registry.example.com/app@sha256:" + strings.Repeat("a", 64), wantErr: true},
		{in: "", wantErr: true},
		{in: "oci://", wantErr: true},
		{in: "oci://Registry.example.com/UPPER", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := oci.ParseRepository(tt.in)
			if tt.wantErr {
				if !errors.Is(err, oci.ErrInvalidReference) {
					t.Fatalf("ParseRepository(%q) = %+v, %v; want ErrInvalidReference", tt.in, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepository(%q) error %v", tt.in, err)
			}
			if got.Registry != tt.registry || got.Path != tt.path {
				t.Fatalf("ParseRepository(%q) = %+v", tt.in, got)
			}
		})
	}
	if got := oci.TrimScheme("oci://oci://x"); got != "oci://x" {
		t.Fatalf("TrimScheme trims more than one prefix: %q", got)
	}
}

func TestResolveReference(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{})
	layer := ocitest.Layer{MediaType: oci.MediaTypeImageLayerGzip, Data: buildTgz(t, tarEntry{name: "a.yaml", body: "a: 1\n"})}
	d100 := reg.PushArtifact("apps/guestbook", "1.0.0", "application/vnd.oci.image.config.v1+json", nil, []ocitest.Layer{layer}, nil)
	layer2 := ocitest.Layer{MediaType: oci.MediaTypeImageLayerGzip, Data: buildTgz(t, tarEntry{name: "a.yaml", body: "a: 2\n"})}
	d110 := reg.PushArtifact("apps/guestbook", "1.1.0", "application/vnd.oci.image.config.v1+json", nil, []ocitest.Layer{layer2}, nil)
	layer3 := ocitest.Layer{MediaType: oci.MediaTypeImageLayerGzip, Data: buildTgz(t, tarEntry{name: "a.yaml", body: "a: 3\n"})}
	d200b := reg.PushArtifact("apps/guestbook", "2.0.0_build.1", "application/vnd.oci.image.config.v1+json", nil, []ocitest.Layer{layer3}, nil)
	reg.PushArtifact("apps/guestbook", "latest", "application/vnd.oci.image.config.v1+json", nil, []ocitest.Layer{layer3}, nil)

	c := newClient(t, reg, oci.Options{})
	repoURL := "oci://" + reg.Host() + "/apps/guestbook"
	ctx := context.Background()
	tests := []struct {
		ref     string
		want    string
		wantErr error
	}{
		{ref: "1.0.0", want: d100.Digest.String()},
		{ref: "latest", want: d200b.Digest.String()},
		{ref: d110.Digest.String(), want: d110.Digest.String()},
		{ref: "1.*", want: d110.Digest.String()},
		{ref: ">=1.0.0 <1.1.0", want: d100.Digest.String()},
		{ref: "2.*", want: d200b.Digest.String()},
		{ref: "*", want: d200b.Digest.String()},
		{ref: "3.*", wantErr: oci.ErrNoMatchingVersion},
		{ref: "9.9.9", wantErr: oci.ErrNotFound},
		{ref: "unknown", wantErr: oci.ErrNotFound},
		{ref: "", wantErr: oci.ErrInvalidReference},
	}
	for _, tt := range tests {
		got, err := c.ResolveReference(ctx, repoURL, tt.ref)
		if tt.wantErr != nil {
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ResolveReference(%q) = %q, %v; want %v", tt.ref, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ResolveReference(%q) = %q, %v; want %q", tt.ref, got, err, tt.want)
		}
	}

	_, err := c.ResolveReference(ctx, repoURL, "unknown")
	if err == nil || !strings.HasPrefix(err.Error(), "cannot get digest for revision unknown: ") {
		t.Fatalf("unexpected error text %v", err)
	}
	_, err = c.ResolveReference(ctx, repoURL, "3.*")
	if err == nil || !strings.HasPrefix(err.Error(), "no version for constraints: version matching constraint not found in 4 tags") {
		t.Fatalf("unexpected error text %v", err)
	}
}

func TestListTagsPaginationAndConversion(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{PageSize: 2})
	layer := ocitest.Layer{MediaType: oci.MediaTypeImageLayer, Data: buildTar(t, tarEntry{name: "a", body: "x"})}
	for _, tg := range []string{"1.0.0", "1.0.0_build.1", "1.1.0", "2.0.0", "latest"} {
		reg.PushArtifact("repo", tg, "application/vnd.oci.image.config.v1+json", nil, []ocitest.Layer{layer}, nil)
	}
	c := newClient(t, reg, oci.Options{})
	tags, err := c.ListTags(context.Background(), reg.Host()+"/repo")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.0.0", "1.0.0+build.1", "1.1.0", "2.0.0", "latest"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
	if n := reg.Requests("tags"); n != 3 {
		t.Fatalf("tag pages requested = %d, want 3", n)
	}

	capped := newClient(t, reg, oci.Options{MaxTags: 3})
	if _, err := capped.ListTags(context.Background(), reg.Host()+"/repo"); !errors.Is(err, oci.ErrTooManyTags) {
		t.Fatalf("expected ErrTooManyTags, got %v", err)
	}
	if _, err := c.ListTags(context.Background(), reg.Host()+"/missing"); !errors.Is(err, oci.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	layer := func(t *testing.T) []ocitest.Layer {
		return []ocitest.Layer{{MediaType: oci.MediaTypeImageLayer, Data: buildTar(t, tarEntry{name: "a", body: "x"})}}
	}
	t.Run("basic", func(t *testing.T) {
		t.Parallel()
		reg := ocitest.New(t, ocitest.Options{Username: "alice", Password: "s3cret"})
		d := reg.PushArtifact("repo", "1.0.0", "application/vnd.oci.image.config.v1+json", nil, layer(t), nil)
		ok := newClient(t, reg, oci.Options{Username: "alice", Password: "s3cret"})
		got, err := ok.ResolveReference(context.Background(), reg.Host()+"/repo", "1.0.0")
		if err != nil || got != d.Digest.String() {
			t.Fatalf("authorized resolve = %q, %v", got, err)
		}
		bad := newClient(t, reg, oci.Options{Username: "alice", Password: "wrong"})
		_, err = bad.ResolveReference(context.Background(), reg.Host()+"/repo", "1.0.0")
		if !errors.Is(err, oci.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized, got %v", err)
		}
		if strings.Contains(err.Error(), "wrong") {
			t.Fatalf("error leaks password: %v", err)
		}
		anon := newClient(t, reg, oci.Options{})
		if _, err := anon.ListTags(context.Background(), reg.Host()+"/repo"); err == nil {
			t.Fatal("anonymous listing succeeded")
		}
		if err := ok.Ping(context.Background(), reg.Host()+"/repo"); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		if err := bad.Ping(context.Background(), reg.Host()+"/repo"); !errors.Is(err, oci.ErrUnauthorized) {
			t.Fatalf("Ping with bad creds: %v", err)
		}
	})
	t.Run("bearer via token service", func(t *testing.T) {
		t.Parallel()
		reg := ocitest.New(t, ocitest.Options{Username: "bob", Password: "pw", BearerToken: "tok-123"})
		d := reg.PushArtifact("team/repo", "1.0.0", "application/vnd.oci.image.config.v1+json", nil, layer(t), nil)
		c := newClient(t, reg, oci.Options{Username: "bob", Password: "pw"})
		got, err := c.ResolveReference(context.Background(), reg.Host()+"/team/repo", "1.0.0")
		if err != nil || got != d.Digest.String() {
			t.Fatalf("resolve = %q, %v", got, err)
		}
		if reg.Requests("token") == 0 {
			t.Fatal("token endpoint not used")
		}
	})
	t.Run("access token", func(t *testing.T) {
		t.Parallel()
		reg := ocitest.New(t, ocitest.Options{BearerToken: "direct-token"})
		d := reg.PushArtifact("repo", "1.0.0", "application/vnd.oci.image.config.v1+json", nil, layer(t), nil)
		c := newClient(t, reg, oci.Options{AccessToken: "direct-token"})
		got, err := c.ResolveReference(context.Background(), reg.Host()+"/repo", "1.0.0")
		if err != nil || got != d.Digest.String() {
			t.Fatalf("resolve = %q, %v", got, err)
		}
		if reg.Requests("token") != 0 {
			t.Fatal("access token was exchanged at the token endpoint")
		}
	})
	t.Run("tls with ca", func(t *testing.T) {
		t.Parallel()
		reg := ocitest.New(t, ocitest.Options{TLS: true})
		d := reg.PushArtifact("repo", "1.0.0", "application/vnd.oci.image.config.v1+json", nil, layer(t), nil)
		c := newClient(t, reg, oci.Options{})
		got, err := c.ResolveReference(context.Background(), reg.Host()+"/repo", "1.0.0")
		if err != nil || got != d.Digest.String() {
			t.Fatalf("resolve = %q, %v", got, err)
		}
		untrusted, err := oci.NewClient(oci.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := untrusted.ResolveReference(context.Background(), reg.Host()+"/repo", "1.0.0"); err == nil {
			t.Fatal("untrusted TLS registry accepted")
		}
		insecure := newClient(t, reg, oci.Options{InsecureSkipVerify: true})
		if _, err := insecure.ResolveReference(context.Background(), reg.Host()+"/repo", "1.0.0"); err != nil {
			t.Fatalf("insecure resolve: %v", err)
		}
	})
	t.Run("bad ca", func(t *testing.T) {
		t.Parallel()
		if _, err := oci.NewClient(oci.Options{CAData: []byte("not pem")}); !errors.Is(err, oci.ErrInvalidTLSConfig) {
			t.Fatalf("expected ErrInvalidTLSConfig, got %v", err)
		}
		if _, err := oci.NewClient(oci.Options{CertData: []byte("x"), KeyData: []byte("y")}); !errors.Is(err, oci.ErrInvalidTLSConfig) {
			t.Fatalf("expected ErrInvalidTLSConfig, got %v", err)
		}
	})
}

func TestFetchManifestAndMetadata(t *testing.T) {
	t.Parallel()
	reg := ocitest.New(t, ocitest.Options{})
	ann := map[string]string{
		ocispec.AnnotationCreated:       "2024-01-01T00:00:00Z",
		ocispec.AnnotationAuthors:       "Patrol",
		ocispec.AnnotationDocumentation: "https://docs.example.com",
		ocispec.AnnotationSource:        "https://git.example.com/app",
		ocispec.AnnotationVersion:       "1.2.3",
		ocispec.AnnotationDescription:   "demo",
		ocispec.AnnotationTitle:         "ignored",
		ocispec.AnnotationRevision:      "ignored",
		ocispec.AnnotationURL:           "ignored",
	}
	d := reg.PushArtifact("repo", "1.2.3", "application/vnd.oci.image.config.v1+json", nil,
		[]ocitest.Layer{{MediaType: oci.MediaTypeImageLayer, Data: buildTar(t, tarEntry{name: "a", body: "x"})}}, ann)
	c := newClient(t, reg, oci.Options{})
	m, desc, err := c.FetchManifest(context.Background(), reg.Host()+"/repo", d.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	if desc.Digest != d.Digest || len(m.Layers) != 1 {
		t.Fatalf("unexpected manifest %+v %+v", desc, m)
	}
	got := oci.MetadataFromManifest(m)
	want := oci.Metadata{CreatedAt: "2024-01-01T00:00:00Z", Authors: "Patrol", DocsURL: "https://docs.example.com",
		SourceURL: "https://git.example.com/app", Version: "1.2.3", Description: "demo"}
	if got != want {
		t.Fatalf("metadata = %+v, want %+v", got, want)
	}

	// An image index is not a manifest.
	reg.PushManifest("repo", "index", ocispec.MediaTypeImageIndex, []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`))
	if _, _, err := c.FetchManifest(context.Background(), reg.Host()+"/repo", "index"); !errors.Is(err, oci.ErrUnsupportedManifest) {
		t.Fatalf("expected ErrUnsupportedManifest, got %v", err)
	}
	// Oversized manifests are refused before download.
	big := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"x":"` + strings.Repeat("a", 2048) + `"}}`
	reg.PushManifest("repo", "big", ocispec.MediaTypeImageManifest, []byte(big))
	small := newClient(t, reg, oci.Options{MaxManifestBytes: 1024})
	if _, _, err := small.FetchManifest(context.Background(), reg.Host()+"/repo", "big"); !errors.Is(err, oci.ErrManifestTooLarge) {
		t.Fatalf("expected ErrManifestTooLarge, got %v", err)
	}
}
