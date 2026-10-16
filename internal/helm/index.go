package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
)

// ChartVersion is one entry of a repository index.
type ChartVersion struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	AppVersion  string   `json:"appVersion,omitempty"`
	Description string   `json:"description,omitempty"`
	URLs        []string `json:"urls,omitempty"`
	Digest      string   `json:"digest,omitempty"`
	Created     string   `json:"created,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`
}

// Index is a parsed repository index.yaml.
type Index struct {
	APIVersion string                    `json:"apiVersion,omitempty"`
	Entries    map[string][]ChartVersion `json:"entries"`
}

// GetEntries returns the versions of chart in index order.
func (i *Index) GetEntries(chart string) ([]ChartVersion, error) {
	entries, ok := i.Entries[chart]
	if !ok {
		return nil, tag(ErrChartNotFound, fmt.Errorf("chart '%s' not found in index", chart))
	}
	return entries, nil
}

// Versions returns the version strings of entries in order.
func Versions(entries []ChartVersion) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Version
	}
	return out
}

// ChartSummary lists the versions of one chart (GetHelmCharts).
type ChartSummary struct {
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
}

type indexEntry struct {
	index        *Index
	tags         []string // OCI tag lists share the cache
	etag         string
	lastModified string
	fetched      time.Time
}

// indexURL appends index.yaml to the repository URL path (upstream getIndexURL).
func indexURL(repoURL string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", fmt.Errorf("error parsing repository URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: repository URL %q is not http(s)", ErrInvalidArgument, redactURL(repoURL))
	}
	u.Path = path.Join(u.Path, "index.yaml")
	if u.RawPath != "" {
		u.RawPath = path.Join(u.RawPath, "index.yaml")
	}
	return u.String(), nil
}

func (c *Client) indexKey(kind, repoURL string, creds Creds) string {
	return kind + "|" + repoURL + "|" + creds.fingerprint()
}

// GetIndex returns the parsed index of an HTTP(S) chart repository.
//
// A fetched index is served from memory for Client.IndexTTL; after that it is
// revalidated with If-None-Match / If-Modified-Since when the server sent an
// ETag or Last-Modified header. noCache skips the cached copy (hard refresh) but
// still refreshes it. Cache entries are keyed by URL and credentials, so an index
// fetched with one set of credentials is never served to a caller with another.
// Concurrent fetches of the same index are collapsed into one request.
func (c *Client) GetIndex(ctx context.Context, repoURL string, creds Creds, noCache bool) (*Index, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	key := c.indexKey("index", repoURL, creds)
	prev, _ := c.indexes.Get(key)
	if !noCache && prev != nil && time.Since(prev.fetched) < c.indexTTL() {
		return prev.index, nil
	}
	flightKey := key
	if noCache {
		flightKey += "|refresh"
	}
	v, err := c.shared(ctx, flightKey, func(ctx context.Context) (any, error) {
		// Another caller may have refreshed the entry while we waited.
		if cur, ok := c.indexes.Get(key); ok && cur != prev && !noCache && time.Since(cur.fetched) < c.indexTTL() {
			return cur, nil
		}
		entry, err := c.fetchIndex(ctx, repoURL, creds, prev)
		if err != nil {
			return nil, err
		}
		c.indexes.Add(key, entry)
		return entry, nil
	})
	if err != nil {
		return nil, fmt.Errorf("error loading repo index: %w", err)
	}
	return v.(*indexEntry).index, nil
}

// InvalidateIndex drops every cached index and tag list of repoURL.
func (c *Client) InvalidateIndex(repoURL string) {
	if c.init() != nil {
		return
	}
	for _, k := range c.indexes.Keys() {
		if strings.HasPrefix(k, "index|"+repoURL+"|") || strings.HasPrefix(k, "tags|"+repoURL+"|") {
			c.indexes.Remove(k)
		}
	}
}

func (c *Client) fetchIndex(ctx context.Context, repoURL string, creds Creds, prev *indexEntry) (*indexEntry, error) {
	target, err := indexURL(repoURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("error creating HTTP request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent())
	if creds.Username != "" || creds.Password != "" {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	if prev != nil {
		if prev.etag != "" {
			req.Header.Set("If-None-Match", prev.etag)
		}
		if prev.lastModified != "" {
			req.Header.Set("If-Modified-Since", prev.lastModified)
		}
	}
	hc, err := c.httpClient(creds)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error making HTTP request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotModified && prev != nil:
		slog.DebugContext(ctx, "helm: index not modified", "repo", redactURL(repoURL))
		return &indexEntry{index: prev.index, etag: prev.etag, lastModified: prev.lastModified, fetched: time.Now()}, nil
	case resp.StatusCode != http.StatusOK:
		return nil, tag(ErrIndexFetch, errors.New("failed to get index: "+resp.Status))
	}
	maxBytes := positiveOr(c.MaxIndexBytes, DefaultMaxIndexBytes)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("error reading index: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, tag(ErrIndexTooLarge, fmt.Errorf("index of %s exceeds %d bytes", redactURL(repoURL), maxBytes))
	}
	idx, err := parseIndex(data)
	if err != nil {
		return nil, err
	}
	slog.DebugContext(ctx, "helm: fetched index", "repo", redactURL(repoURL), "bytes", len(data), "duration", time.Since(start))
	return &indexEntry{
		index:        idx,
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
		fetched:      time.Now(),
	}, nil
}

func parseIndex(data []byte) (*Index, error) {
	idx := &Index{}
	if err := yaml.Unmarshal(data, idx); err != nil {
		return nil, fmt.Errorf("error decoding index: %w", err)
	}
	if idx.Entries == nil {
		idx.Entries = map[string][]ChartVersion{}
	}
	return idx, nil
}

func (c *Client) httpClient(creds Creds) (*http.Client, error) {
	tr, err := oci.NewTransport(creds.transportOptions(c))
	if err != nil {
		return nil, fmt.Errorf("error creating TLS config: %w", err)
	}
	return &http.Client{Transport: tr}, nil
}

// ListCharts returns every chart of an HTTP(S) repository with its versions in
// index order, sorted by chart name (GetHelmCharts). The cached index is
// bypassed and refreshed.
func (c *Client) ListCharts(ctx context.Context, repoURL string, creds Creds) ([]ChartSummary, error) {
	idx, err := c.GetIndex(ctx, repoURL, creds, true)
	if err != nil {
		return nil, err
	}
	out := make([]ChartSummary, 0, len(idx.Entries))
	for name, entries := range idx.Entries {
		out = append(out, ChartSummary{Name: name, Versions: Versions(entries)})
	}
	slices.SortFunc(out, func(a, b ChartSummary) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ociChartRepo returns the OCI repository of chart in a Helm OCI repository URL
// (upstream: "<repoURL>/<chart>" with an "https://" prefix removed).
func ociChartRepo(repoURL, chart string) string {
	r := strings.TrimSuffix(oci.TrimScheme(strings.Replace(repoURL, "https://", "", 1)), "/")
	return r + "/" + chart
}

func (c *Client) ociClient(creds Creds) (*oci.Client, error) {
	return oci.NewClient(oci.Options{
		Username:           creds.Username,
		Password:           creds.Password,
		CAData:             creds.CAData,
		CertData:           creds.CertData,
		KeyData:            creds.KeyData,
		InsecureSkipVerify: creds.InsecureSkipVerify,
		PlainHTTP:          creds.PlainHTTP,
		Proxy:              c.Proxy,
		NoProxy:            c.NoProxy,
		UserAgent:          c.userAgent(),
	})
}

// GetTags lists the tags of chart in a Helm OCI repository ('_' converted to
// '+'), cached like the index for Client.IndexTTL unless noCache.
func (c *Client) GetTags(ctx context.Context, repoURL, chart string, creds Creds, noCache bool) ([]string, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	repo := ociChartRepo(repoURL, chart)
	key := c.indexKey("tags", repoURL, creds) + "|" + chart
	if cur, ok := c.indexes.Get(key); ok && !noCache && time.Since(cur.fetched) < c.indexTTL() {
		return slices.Clone(cur.tags), nil
	}
	v, err := c.shared(ctx, key, func(ctx context.Context) (any, error) {
		oc, err := c.ociClient(creds)
		if err != nil {
			return nil, err
		}
		tags, err := oc.ListTags(ctx, repo)
		if err != nil {
			return nil, err
		}
		entry := &indexEntry{tags: tags, fetched: time.Now()}
		c.indexes.Add(key, entry)
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Clone(v.(*indexEntry).tags), nil
}

// ResolveChartVersion resolves constraint to a chart version with the baseline
// semantics: an exact version is returned verbatim without contacting the
// repository; otherwise the candidate versions are the index entries of chart
// (HTTP repositories) or the registry tags of <repoURL>/<chart> (OCI, when
// enableOCI is set or repoURL has no scheme), and the highest version satisfying
// the semver constraint wins ('*', ranges, '^', '~', '*-0' for prereleases).
// A string that is neither a version nor a constraint must equal a candidate.
func (c *Client) ResolveChartVersion(ctx context.Context, repoURL, chart, constraint string, creds Creds, enableOCI bool) (string, error) {
	if chart == "" {
		return "", fmt.Errorf("%w: chart name is required", ErrInvalidArgument)
	}
	if oci.IsVersion(constraint) {
		return constraint, nil
	}
	var candidates []string
	if enableOCI || IsHelmOCIRepo(repoURL) {
		tags, err := c.GetTags(ctx, repoURL, chart, creds, false)
		if err != nil {
			return "", fmt.Errorf("unable to get tags: %w", err)
		}
		candidates = tags
	} else {
		idx, err := c.GetIndex(ctx, repoURL, creds, false)
		if err != nil {
			return "", err
		}
		entries, err := idx.GetEntries(chart)
		if err != nil {
			return "", err
		}
		candidates = Versions(entries)
	}
	v, err := oci.MaxVersion(constraint, candidates)
	if err != nil {
		return "", fmt.Errorf("invalid revision: %w", err)
	}
	return v, nil
}
