package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Limits bounds PullArtifact. Zero fields take the defaults.
type Limits struct {
	// MaxLayers caps the number of layers in the manifest (DefaultMaxLayers).
	MaxLayers int
	// MaxLayerBytes caps the compressed size of the content layer
	// (DefaultMaxLayerBytes). Negative disables the cap.
	MaxLayerBytes int64
	// AllowedMediaTypes is the content-layer allow-list. Empty means
	// DefaultAllowedMediaTypes.
	AllowedMediaTypes []string
	// Archive bounds extraction. Archive.MaxTotalBytes is the upstream
	// reposerver.oci.manifest.max.extracted.size (negative = disabled).
	Archive sandbox.ArchiveLimits
}

func (l Limits) normalized() Limits {
	if l.MaxLayers <= 0 {
		l.MaxLayers = DefaultMaxLayers
	}
	if l.MaxLayerBytes == 0 {
		l.MaxLayerBytes = DefaultMaxLayerBytes
	}
	if len(l.AllowedMediaTypes) == 0 {
		l.AllowedMediaTypes = DefaultAllowedMediaTypes
	}
	return l
}

// Artifact describes a pulled artifact.
type Artifact struct {
	// Digest is the manifest digest.
	Digest string
	// ConfigMediaType is the manifest config media type.
	ConfigMediaType string
	// Layer is the extracted content layer.
	Layer ocispec.Descriptor
	// IsHelmChart reports a Helm chart artifact; its single top-level chart
	// directory was flattened into the destination.
	IsHelmChart bool
	// Annotations are the manifest annotations.
	Annotations map[string]string
}

// PullArtifact downloads the artifact at digest and extracts its single content
// layer into dest, which must not exist or be an empty directory.
//
// Upstream rules apply: at most Limits.MaxLayers layers; for Helm chart artifacts
// (config media type MediaTypeHelmConfig) only MediaTypeHelmChartContent layers
// count, otherwise every layer whose media type ends in "tar+gzip", "tar.gzip" or
// "tar" counts; counted layers must be in the allow-list; exactly one must exist.
// Other layers (attestations, provenance) are ignored. A Helm chart's single
// top-level directory becomes dest itself.
//
// The layer is downloaded completely and verified against its digest before any
// byte is extracted; extraction uses internal/sandbox (no absolute or '..' names,
// no escaping symlinks, no devices, size and entry limits) into a staging
// directory that is renamed to dest only on success. For a Helm chart the chart
// directory is checked as its own root before it becomes dest, so no symlink in
// dest resolves outside dest (sandbox.ErrPathEscape, message "oci image
// contains out-of-bounds symlinks. file: <path>"). Every symlink left in dest
// resolves inside dest.
func (c *Client) PullArtifact(ctx context.Context, repoURL, dgst, dest string, limits Limits) (Artifact, error) {
	if !IsDigest(dgst) {
		return Artifact{}, fmt.Errorf("%w: revision %q is not a digest", ErrInvalidReference, dgst)
	}
	limits = limits.normalized()
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return Artifact{}, fmt.Errorf("oci: resolve destination: %w", err)
	}
	if err := checkDestination(destAbs); err != nil {
		return Artifact{}, err
	}
	// DefaultTimeout bounds the registry operations only; extraction honors the
	// caller's context.
	extractCtx := ctx
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	repo, parsed, err := c.repository(repoURL)
	if err != nil {
		return Artifact{}, err
	}
	m, desc, err := c.fetchManifest(ctx, repo, dgst)
	if err != nil {
		return Artifact{}, err
	}
	layer, isHelm, err := selectContentLayer(m, limits.MaxLayers, limits.AllowedMediaTypes)
	if err != nil {
		return Artifact{}, err
	}
	if err := checkLayerSize(layer, limits.MaxLayerBytes); err != nil {
		return Artifact{}, err
	}
	blob, release, err := c.fetchLayer(ctx, repo, parsed, layer)
	if err != nil {
		return Artifact{}, fmt.Errorf("could not save oci digest %s: %w", dgst, err)
	}
	defer release()
	if err := extractLayer(extractCtx, blob, layer.MediaType, destAbs, isHelm, limits.Archive); err != nil {
		return Artifact{}, fmt.Errorf("cannot extract contents of oci image with revision %s: %w", dgst, err)
	}
	return Artifact{
		Digest:          desc.Digest.String(),
		ConfigMediaType: m.Config.MediaType,
		Layer:           layer,
		IsHelmChart:     isHelm,
		Annotations:     m.Annotations,
	}, nil
}

// FetchHelmChart downloads the chart content layer (.tgz) of the Helm chart
// tagged version (semver '+' is mapped to '_') in repoURL and writes it to w,
// returning the manifest digest. The layer must be the only Helm chart content
// layer (current or legacy media type), at most maxBytes long (negative = no
// cap) and is verified against its digest; on a verification error w has
// received unverified bytes and must be discarded by the caller.
func (c *Client) FetchHelmChart(ctx context.Context, repoURL, version string, w io.Writer, maxBytes int64) (string, error) {
	if version == "" {
		return "", fmt.Errorf("%w: empty chart version", ErrInvalidReference)
	}
	ref := version
	if !IsDigest(version) {
		ref = TagFromVersion(version)
	}
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	repo, _, err := c.repository(repoURL)
	if err != nil {
		return "", err
	}
	m, desc, err := c.fetchManifest(ctx, repo, ref)
	if err != nil {
		return "", err
	}
	var layers []ocispec.Descriptor
	for _, l := range m.Layers {
		if l.MediaType == MediaTypeHelmChartContent || l.MediaType == MediaTypeHelmChartContentLegacy {
			layers = append(layers, l)
		}
	}
	if len(layers) != 1 {
		return "", tag(ErrContentLayerCount, fmt.Errorf("expected only a single helm chart content layer, got %d", len(layers)))
	}
	if err := checkLayerSize(layers[0], maxBytes); err != nil {
		return "", err
	}
	if err := copyVerified(ctx, repo, layers[0], w); err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// CleanCache removes every cached layer of repoURL (hard refresh).
func (c *Client) CleanCache(repoURL string) error {
	if c.opts.CacheDir == "" {
		return nil
	}
	parsed, err := ParseRepository(repoURL)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(c.repoCacheDir(parsed)); err != nil {
		return fmt.Errorf("oci: clean cache: %w", err)
	}
	return nil
}

func (c *Client) repoCacheDir(r Repository) string {
	sum := sha256.Sum256([]byte(r.String()))
	return filepath.Join(c.opts.CacheDir, hex.EncodeToString(sum[:16]))
}

func checkLayerSize(layer ocispec.Descriptor, maxBytes int64) error {
	if layer.Size < 0 {
		return fmt.Errorf("%w: negative layer size", ErrLayerTooLarge)
	}
	if maxBytes >= 0 && layer.Size > maxBytes {
		return tag(ErrLayerTooLarge, fmt.Errorf("oci layer %s is %d bytes, limit is %d", layer.Digest, layer.Size, maxBytes))
	}
	return nil
}

// checkDestination accepts a missing path or an empty directory.
func checkDestination(dest string) error {
	fi, err := os.Lstat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("oci: inspect destination: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrDestinationExists, filepath.Base(dest))
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return fmt.Errorf("oci: inspect destination: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s is not empty", ErrDestinationExists, filepath.Base(dest))
	}
	return nil
}

// fetchLayer returns the path of a verified local copy of layer and a release
// function that removes it unless it lives in the cache.
func (c *Client) fetchLayer(ctx context.Context, repo *remote.Repository, parsed Repository, layer ocispec.Descriptor) (string, func(), error) {
	noop := func() {}
	dir := os.TempDir()
	var cached string
	if c.opts.CacheDir != "" {
		dir = filepath.Join(c.repoCacheDir(parsed), layer.Digest.Algorithm().String())
		cached = filepath.Join(dir, layer.Digest.Encoded())
		if fi, err := os.Stat(cached); err == nil && fi.Mode().IsRegular() && fi.Size() == layer.Size {
			return cached, noop, nil
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", noop, fmt.Errorf("oci: create cache dir: %w", err)
		}
	}
	f, err := os.CreateTemp(dir, ".layer-*")
	if err != nil {
		return "", noop, fmt.Errorf("oci: create layer file: %w", err)
	}
	tmp := f.Name()
	remove := func() { _ = os.Remove(tmp) }
	if err := copyVerified(ctx, repo, layer, f); err != nil {
		_ = f.Close()
		remove()
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		remove()
		return "", noop, fmt.Errorf("oci: write layer file: %w", err)
	}
	if cached == "" {
		return tmp, remove, nil
	}
	if err := os.Rename(tmp, cached); err != nil {
		remove()
		return "", noop, fmt.Errorf("oci: store layer in cache: %w", err)
	}
	return cached, noop, nil
}

// copyVerified streams the blob described by desc into w, enforcing its size and
// digest.
func copyVerified(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor, w io.Writer) error {
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("oci: fetch layer %s: %w", desc.Digest, classify(err))
	}
	defer func() { _ = rc.Close() }()
	vr := content.NewVerifyReader(rc, desc)
	if _, err := io.Copy(w, vr); err != nil {
		return fmt.Errorf("oci: read layer %s: %w", desc.Digest, err)
	}
	if err := vr.Verify(); err != nil {
		return fmt.Errorf("oci: verify layer %s: %w", desc.Digest, err)
	}
	return nil
}

// extractLayer extracts blob into a staging directory next to dest and renames
// the result (or, for Helm charts, the single chart directory) to dest.
//
// Extraction checks symlinks against the staging root. A Helm chart directory
// is checked again as a root of its own before it becomes dest: a link such as
// "up -> .." stays inside the layer but would point at dest's parent (sibling
// destinations) after the move.
func extractLayer(ctx context.Context, blob, mediaType, dest string, isHelm bool, lim sandbox.ArchiveLimits) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("oci: create destination parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".oci-extract-*")
	if err != nil {
		return fmt.Errorf("oci: create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	f, err := os.Open(blob)
	if err != nil {
		return fmt.Errorf("oci: open layer: %w", err)
	}
	defer func() { _ = f.Close() }()
	if isGzipLayer(mediaType) {
		err = sandbox.ExtractTarGzContext(ctx, f, staging, lim)
	} else {
		err = sandbox.ExtractTarContext(ctx, f, staging, lim)
	}
	if err != nil {
		return fmt.Errorf("could not decompress layer: %w", err)
	}

	src := staging
	if isHelm {
		entries, err := os.ReadDir(staging)
		if err != nil {
			return fmt.Errorf("oci: read extracted chart: %w", err)
		}
		if len(entries) != 1 || !entries[0].IsDir() {
			return tag(ErrInvalidHelmChart, fmt.Errorf("expected 1 directory, found %v", len(entries)))
		}
		src = filepath.Join(staging, entries[0].Name())
		if err := sandbox.CheckSymlinksContext(ctx, src); err != nil {
			var oob *sandbox.OutOfBoundsSymlinkError
			if errors.As(err, &oob) {
				return tag(err, fmt.Errorf("oci image contains out-of-bounds symlinks. file: %s", oob.Path))
			}
			return fmt.Errorf("oci: check chart symlinks: %w", err)
		}
	}
	if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("oci: prepare destination: %w", err)
	}
	if err := os.Rename(src, dest); err != nil {
		return fmt.Errorf("oci: move extracted content: %w", err)
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		return fmt.Errorf("oci: set destination mode: %w", err)
	}
	return nil
}
