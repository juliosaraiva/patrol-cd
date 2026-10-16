package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// Media types handled by the package.
const (
	// MediaTypeHelmConfig is the config media type that marks a Helm chart artifact.
	MediaTypeHelmConfig = "application/vnd.cncf.helm.config.v1+json"
	// MediaTypeHelmChartContent is the Helm chart content layer (a chart .tgz).
	MediaTypeHelmChartContent = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	// MediaTypeHelmChartContentLegacy is the pre-OCI-1.0 Helm chart layer type
	// still produced by old Helm 3 releases.
	MediaTypeHelmChartContentLegacy = "application/tar+gzip"
	// MediaTypeImageLayer is an uncompressed OCI tar layer.
	MediaTypeImageLayer = ocispec.MediaTypeImageLayer
	// MediaTypeImageLayerGzip is a gzip-compressed OCI tar layer.
	MediaTypeImageLayerGzip = ocispec.MediaTypeImageLayerGzip
	// mediaTypeDockerManifest is the Docker schema 2 manifest, layout-compatible
	// with the OCI image manifest.
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// DefaultAllowedMediaTypes is the default content-layer allow-list
// (upstream --oci-layer-media-types default).
var DefaultAllowedMediaTypes = []string{MediaTypeImageLayer, MediaTypeImageLayerGzip, MediaTypeHelmChartContent}

// Defaults applied when the corresponding field is zero.
const (
	// DefaultMaxManifestBytes bounds a manifest document (oras-go's metadata
	// default, as upstream).
	DefaultMaxManifestBytes int64 = 4 << 20
	// DefaultMaxLayers is the upstream guard against manifests with many layers.
	DefaultMaxLayers = 10
	// DefaultMaxLayerBytes bounds the compressed content layer.
	DefaultMaxLayerBytes int64 = 1 << 30
	// DefaultMaxTags bounds a tag listing.
	DefaultMaxTags = 100_000
	// DefaultTimeout bounds one registry operation when the context has no deadline.
	DefaultTimeout = 5 * time.Minute
)

// Options configures a Client. Credentials apply only to the registry host of the
// repository URL passed to each call.
type Options struct {
	// Username and Password are basic credentials (also used to obtain bearer
	// tokens from the registry's token service).
	Username, Password string
	// AccessToken, when set, is sent as a bearer token to registries that answer
	// with a Bearer challenge, instead of exchanging Username/Password.
	AccessToken string
	// TLS and proxy settings. CAData replaces the system roots when set.
	CAData, CertData, KeyData []byte
	InsecureSkipVerify        bool
	Proxy, NoProxy            string
	// PlainHTTP talks to the registry over http:// (upstream insecureOCIForceHttp).
	PlainHTTP bool
	// UserAgent is sent with every request. Empty means "patrol-cd".
	UserAgent string
	// MaxManifestBytes bounds manifests fetched by FetchManifest. Zero means
	// DefaultMaxManifestBytes.
	MaxManifestBytes int64
	// MaxTags bounds ListTags. Zero means DefaultMaxTags.
	MaxTags int
	// CacheDir, when set, keeps verified content layers on disk keyed by
	// repository and layer digest. A cached layer is only used after the manifest
	// that references it was fetched (and therefore authorized) again.
	CacheDir string
	// HTTPClient overrides the HTTP client (its transport must apply any TLS
	// settings itself). Intended for tests.
	HTTPClient *http.Client
}

// Client accesses OCI registries. It is safe for concurrent use.
type Client struct {
	opts       Options
	httpClient *http.Client
	authCache  auth.Cache
}

// NewClient validates opts and returns a client.
func NewClient(opts Options) (*Client, error) {
	hc := opts.HTTPClient
	if hc == nil {
		tr, err := NewTransport(TransportOptions{
			CAData:             opts.CAData,
			CertData:           opts.CertData,
			KeyData:            opts.KeyData,
			InsecureSkipVerify: opts.InsecureSkipVerify,
			Proxy:              opts.Proxy,
			NoProxy:            opts.NoProxy,
		})
		if err != nil {
			return nil, err
		}
		hc = &http.Client{Transport: tr}
	}
	if opts.MaxManifestBytes <= 0 {
		opts.MaxManifestBytes = DefaultMaxManifestBytes
	}
	if opts.MaxTags <= 0 {
		opts.MaxTags = DefaultMaxTags
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "patrol-cd"
	}
	return &Client{opts: opts, httpClient: hc, authCache: auth.NewCache()}, nil
}

// repository returns an oras repository for repoURL with credentials bound to its
// registry host.
func (c *Client) repository(repoURL string) (*remote.Repository, Repository, error) {
	parsed, err := ParseRepository(repoURL)
	if err != nil {
		return nil, Repository{}, err
	}
	repo, err := remote.NewRepository(parsed.String())
	if err != nil {
		return nil, Repository{}, tag(ErrInvalidReference, fmt.Errorf("failed to initialize repository: %w", err))
	}
	repo.PlainHTTP = c.opts.PlainHTTP
	repo.MaxMetadataBytes = c.opts.MaxManifestBytes
	repo.ManifestMediaTypes = []string{
		ocispec.MediaTypeImageManifest, mediaTypeDockerManifest,
		ocispec.MediaTypeImageIndex, mediaTypeDockerList,
	}
	// One page per 4 MiB response; the tag cap is enforced in ListTags.
	repo.TagListMaxPages = 10_000
	cred := auth.Credential{Username: c.opts.Username, Password: c.opts.Password, AccessToken: c.opts.AccessToken}
	ac := &auth.Client{
		Client:     c.httpClient,
		Cache:      c.authCache,
		Credential: auth.StaticCredential(parsed.Registry, cred),
	}
	ac.SetUserAgent(c.opts.UserAgent)
	repo.Client = ac
	return repo, parsed, nil
}

func withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, DefaultTimeout)
}

// ResolveReference resolves ref (a tag, a digest, or a semver constraint over the
// repository tags) to a manifest digest ("sha256:..."), with the baseline order:
// the reference is first resolved literally; only when that fails and ref is a
// constraint (not an exact version) are the tags listed and the highest
// satisfying version resolved.
func (c *Client) ResolveReference(ctx context.Context, repoURL, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("%w: empty revision", ErrInvalidReference)
	}
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	repo, _, err := c.repository(repoURL)
	if err != nil {
		return "", err
	}
	d, err := resolveDigest(ctx, repo, ref)
	if err == nil {
		return d, nil
	}
	if !IsConstraint(ref) {
		return "", err
	}
	tags, terr := c.ListTags(ctx, repoURL)
	if terr != nil {
		return "", fmt.Errorf("error fetching tags: %w", terr)
	}
	version, verr := MaxVersion(ref, tags)
	if verr != nil {
		return "", fmt.Errorf("no version for constraints: %w", verr)
	}
	// Tags were converted '_' -> '+'; the registry only knows the '_' form.
	return resolveDigest(ctx, repo, TagFromVersion(version))
}

func resolveDigest(ctx context.Context, repo *remote.Repository, ref string) (string, error) {
	desc, err := repo.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("cannot get digest for revision %s: %w", ref, classify(err))
	}
	return desc.Digest.String(), nil
}

// ListTags returns every tag of the repository in registry order, with '_'
// converted back to '+' (Helm's convention for semver build metadata).
func (c *Client) ListTags(ctx context.Context, repoURL string) ([]string, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	repo, _, err := c.repository(repoURL)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	var tags []string
	err = repo.Tags(ctx, "", func(page []string) error {
		if len(tags)+len(page) > c.opts.MaxTags {
			return fmt.Errorf("%w: more than %d tags", ErrTooManyTags, c.opts.MaxTags)
		}
		for _, t := range page {
			tags = append(tags, VersionFromTag(t))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get tags: %w", classify(err))
	}
	slog.DebugContext(ctx, "oci: listed tags", "tags", len(tags), "duration", time.Since(start))
	return tags, nil
}

// FetchManifest fetches and decodes the image manifest identified by ref (tag or
// digest). The document is bounded by Options.MaxManifestBytes and verified
// against its digest. Image indexes and other documents fail with
// ErrUnsupportedManifest.
func (c *Client) FetchManifest(ctx context.Context, repoURL, ref string) (ocispec.Manifest, ocispec.Descriptor, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	repo, _, err := c.repository(repoURL)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Descriptor{}, err
	}
	return c.fetchManifest(ctx, repo, ref)
}

func (c *Client) fetchManifest(ctx context.Context, repo *remote.Repository, ref string) (ocispec.Manifest, ocispec.Descriptor, error) {
	desc, err := repo.Resolve(ctx, ref)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("error resolving oci manifest for digest %s: %w", ref, classify(err))
	}
	if desc.Size > c.opts.MaxManifestBytes || desc.Size < 0 {
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("%w: %d bytes (limit %d)", ErrManifestTooLarge, desc.Size, c.opts.MaxManifestBytes)
	}
	if desc.MediaType != ocispec.MediaTypeImageManifest && desc.MediaType != mediaTypeDockerManifest {
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("%w: media type %q", ErrUnsupportedManifest, desc.MediaType)
	}
	raw, err := content.FetchAll(ctx, repo, desc)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("error fetching oci manifest for digest %s: %w", ref, classify(err))
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("error decoding oci manifest for digest %s: %w", ref, err)
	}
	if m.MediaType != "" && m.MediaType != desc.MediaType {
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("%w: manifest declares %q but was served as %q", ErrUnsupportedManifest, m.MediaType, desc.MediaType)
	}
	return m, desc, nil
}

// Ping checks that the registry of repoURL answers the /v2/ endpoint with the
// configured credentials (upstream TestRepo).
func (c *Client) Ping(ctx context.Context, repoURL string) error {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	repo, parsed, err := c.repository(repoURL)
	if err != nil {
		return err
	}
	reg, err := remote.NewRegistry(parsed.Registry)
	if err != nil {
		return tag(ErrInvalidReference, err)
	}
	reg.PlainHTTP = repo.PlainHTTP
	reg.Client = repo.Client
	return classify(reg.Ping(ctx))
}

// Metadata is the revision metadata derived from manifest annotations
// (upstream GetOCIMetadata).
type Metadata struct {
	CreatedAt   string `json:"createdAt,omitempty"`
	Authors     string `json:"authors,omitempty"`
	DocsURL     string `json:"docsUrl,omitempty"`
	SourceURL   string `json:"sourceUrl,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// MetadataFromManifest maps the standard OCI annotations of m. Title, revision
// and URL annotations are not part of the result, as upstream.
func MetadataFromManifest(m ocispec.Manifest) Metadata {
	a := m.Annotations
	return Metadata{
		CreatedAt:   a[ocispec.AnnotationCreated],
		Authors:     a[ocispec.AnnotationAuthors],
		DocsURL:     a[ocispec.AnnotationDocumentation],
		SourceURL:   a[ocispec.AnnotationSource],
		Version:     a[ocispec.AnnotationVersion],
		Description: a[ocispec.AnnotationDescription],
	}
}

// IsDigest reports whether s is a valid content digest ("sha256:<64 hex>").
func IsDigest(s string) bool {
	_, err := digest.Parse(s)
	return err == nil
}

// isContentLayer reports whether a layer is a (possibly compressed) tar archive,
// using the upstream suffix rule.
func isContentLayer(mediaType string) bool {
	return isGzipLayer(mediaType) || strings.HasSuffix(mediaType, "tar")
}

func isGzipLayer(mediaType string) bool {
	return strings.HasSuffix(mediaType, "tar+gzip") || strings.HasSuffix(mediaType, "tar.gzip")
}

// selectContentLayer applies the upstream layer rules: at most maxLayers layers;
// for Helm chart artifacts only the chart content layer counts, otherwise every
// tar layer counts; counted layers must be allowed; exactly one must remain.
func selectContentLayer(m ocispec.Manifest, maxLayers int, allowed []string) (ocispec.Descriptor, bool, error) {
	if len(m.Layers) > maxLayers {
		return ocispec.Descriptor{}, false, tag(ErrTooManyLayers, fmt.Errorf("expected no more than %d oci layers, got %d", maxLayers, len(m.Layers)))
	}
	isHelm := m.Config.MediaType == MediaTypeHelmConfig
	var found []ocispec.Descriptor
	for _, l := range m.Layers {
		counted := isContentLayer(l.MediaType)
		if isHelm {
			counted = l.MediaType == MediaTypeHelmChartContent
		}
		if !counted {
			continue
		}
		if !slices.Contains(allowed, l.MediaType) {
			return ocispec.Descriptor{}, false, tag(ErrMediaTypeNotAllowed, fmt.Errorf("oci layer media type %s is not in the list of allowed media types", l.MediaType))
		}
		found = append(found, l)
	}
	if len(found) != 1 {
		return ocispec.Descriptor{}, false, tag(ErrContentLayerCount, fmt.Errorf("expected only a single oci content layer, got %d", len(found)))
	}
	return found[0], isHelm, nil
}
