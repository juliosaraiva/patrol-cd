package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// PullChart downloads chart at version (as returned by ResolveChartVersion) and
// extracts it into a fresh directory, which it returns.
// The caller owns that directory and removes it with os.RemoveAll when done.
//
// The chart archive is cached under WorkDir/charts, keyed by repository URL,
// chart, version, transport kind and a fingerprint of the credentials (so a
// cached private chart is never handed to a caller with other credentials).
// Concurrent pulls of the same key download once.
//
// HTTP(S) repositories: the index entry is chosen as `helm pull --version` does
// (an equal version string first, else the highest entry satisfying version as
// a semver constraint, so "v1.2.3" and "1.2" find 1.2.3; the index is refetched
// once when a cached copy has no exact match). The archive cache is keyed by the
// requested version string, as upstream's is, so a partial pin such as "1.2"
// keeps the chart it first found until CleanChartCache. Relative chart URLs
// resolve against the repository URL, and credentials are sent only to the
// repository's scheme and host unless Creds.PassCredentials is set (helm
// semantics). OCI repositories (enableOCI, or a repository URL without scheme):
// the Helm chart content layer of <repoURL>/<chart>:<version> is fetched and
// verified against its digest (no constraint fallback, as in helm).
//
// Either way the archive is bounded by Client.MaxChartBytes and extracted with
// internal/sandbox limits (Client.Archive); the chart's top-level directory
// (the last path segment of chart) becomes the returned directory. That
// directory is checked as a root of its own, so a symlink that leaves it fails
// the pull (sandbox.ErrPathEscape, *sandbox.OutOfBoundsSymlinkError, message
// "chart contains out-of-bounds symlinks. file: <path>"): every symlink in the
// returned tree resolves inside it.
func (c *Client) PullChart(ctx context.Context, repoURL, chart, version string, creds Creds, enableOCI bool) (string, error) {
	if err := c.init(); err != nil {
		return "", err
	}
	if chart == "" || version == "" {
		return "", fmt.Errorf("%w: chart and version are required", ErrInvalidArgument)
	}
	name := normalizeChartName(chart)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%w: chart name %q", ErrInvalidArgument, chart)
	}
	isOCI := enableOCI || IsHelmOCIRepo(repoURL)
	// The archive is opened under the cache-key lock so a concurrent
	// CleanChartCache cannot remove it in between; an archive removed before the
	// open is downloaded again once.
	var f *os.File
	for attempt := 0; f == nil; attempt++ {
		archive, err := c.cachedChart(ctx, repoURL, chart, version, creds, isOCI)
		if err != nil {
			return "", err
		}
		unlock := c.locks.lock(archive)
		f, err = os.Open(archive)
		unlock()
		if err != nil && (attempt > 0 || !errors.Is(err, fs.ErrNotExist)) {
			return "", fmt.Errorf("helm: open chart archive: %w", err)
		}
	}
	defer func() { _ = f.Close() }()
	dir, err := c.extractChart(ctx, f, name)
	if err != nil {
		return "", fmt.Errorf("error untarring chart: %w", err)
	}
	return dir, nil
}

// CleanChartCache removes the cached archive of chart at version (hard refresh).
func (c *Client) CleanChartCache(repoURL, chart, version string, creds Creds, enableOCI bool) error {
	if err := c.init(); err != nil {
		return err
	}
	p := c.chartCachePath(repoURL, chart, version, creds, enableOCI || IsHelmOCIRepo(repoURL))
	unlock := c.locks.lock(p)
	defer unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("error removing chart cache: %w", err)
	}
	return nil
}

func (c *Client) chartCachePath(repoURL, chart, version string, creds Creds, isOCI bool) string {
	key, _ := json.Marshal(map[string]any{
		"url": repoURL, "chart": chart, "version": version, "oci": isOCI, "creds": creds.fingerprint(),
	})
	sum := sha256.Sum256(key)
	return filepath.Join(c.chartCacheDir(), hex.EncodeToString(sum[:])+".tgz")
}

func (c *Client) cachedChart(ctx context.Context, repoURL, chart, version string, creds Creds, isOCI bool) (string, error) {
	p := c.chartCachePath(repoURL, chart, version, creds, isOCI)
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p, nil
	}
	_, err := c.shared(ctx, "chart|"+p, func(ctx context.Context) (any, error) {
		unlock := c.locks.lock(p)
		defer unlock()
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return nil, nil
		}
		return nil, c.downloadChart(ctx, p, repoURL, chart, version, creds, isOCI)
	})
	if err != nil {
		if isOCI {
			return "", fmt.Errorf("error pulling OCI chart: %w", err)
		}
		return "", fmt.Errorf("error fetching chart: %w", err)
	}
	return p, nil
}

// downloadChart writes the archive to a temp file next to dst and renames it.
func (c *Client) downloadChart(ctx context.Context, dst, repoURL, chart, version string, creds Creds, isOCI bool) error {
	f, err := os.CreateTemp(filepath.Dir(dst), ".download-*")
	if err != nil {
		return fmt.Errorf("helm: create download file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	maxBytes := positiveOr(c.MaxChartBytes, DefaultMaxChartBytes)
	start := time.Now()
	if isOCI {
		err = c.fetchOCIChart(ctx, f, repoURL, chart, version, creds, maxBytes)
	} else {
		err = c.fetchHTTPChart(ctx, f, repoURL, chart, version, creds, maxBytes)
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("helm: write download file: %w", cerr)
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("helm: store chart in cache: %w", err)
	}
	slog.DebugContext(ctx, "helm: pulled chart", "repo", redactURL(repoURL), "chart", chart, "version", version, "oci", isOCI, "duration", time.Since(start))
	return nil
}

func (c *Client) fetchOCIChart(ctx context.Context, w io.Writer, repoURL, chart, version string, creds Creds, maxBytes int64) error {
	oc, err := c.ociClient(creds)
	if err != nil {
		return err
	}
	_, err = oc.FetchHelmChart(ctx, ociChartRepo(repoURL, chart), version, w, maxBytes)
	if errors.Is(err, oci.ErrLayerTooLarge) {
		return tag(ErrChartTooLarge, err)
	}
	return err
}

func (c *Client) fetchHTTPChart(ctx context.Context, w io.Writer, repoURL, chart, version string, creds Creds, maxBytes int64) error {
	entry, err := c.findIndexEntry(ctx, repoURL, chart, version, creds)
	if err != nil {
		return err
	}
	if len(entry.URLs) == 0 {
		return tag(ErrVersionNotFound, fmt.Errorf("chart %q version %q has no downloadable URL", chart, version))
	}
	chartURL, err := resolveReferenceURL(repoURL, entry.URLs[0])
	if err != nil {
		return fmt.Errorf("helm: invalid chart URL: %w", err)
	}
	u, err := url.Parse(chartURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%w: chart URL %q is not http(s)", ErrInvalidArgument, redactURL(chartURL))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chartURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("error creating HTTP request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent())
	if (creds.Username != "" || creds.Password != "") && (creds.PassCredentials || sameOrigin(repoURL, chartURL)) {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	hc, err := c.httpClient(creds)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("error making HTTP request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return tag(ErrDownload, fmt.Errorf("failed to fetch %s : %s", redactURL(chartURL), resp.Status))
	}
	if resp.ContentLength > maxBytes {
		return tag(ErrChartTooLarge, fmt.Errorf("chart archive is %d bytes, limit is %d", resp.ContentLength, maxBytes))
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("helm: download chart: %w", err)
	}
	if n > maxBytes {
		return tag(ErrChartTooLarge, fmt.Errorf("chart archive exceeds %d bytes", maxBytes))
	}
	return nil
}

// findIndexEntry looks version up in the index with the semantics of
// `helm pull --repo <url> --version <version>` (Helm's IndexFile.Get): an entry
// whose version string equals version wins; otherwise the highest entry that
// satisfies version read as a semver constraint is used, so pins that
// ResolveChartVersion returns verbatim ("v1.2.3", "1.2") still find 1.2.3.
//
// An exact match may come from the cached index. Without one the index is
// refetched once (a cached copy may not know a new version yet) and the
// constraint fallback is applied to the fresh copy only, as helm always reads a
// fresh index.
func (c *Client) findIndexEntry(ctx context.Context, repoURL, chart, version string, creds Creds) (ChartVersion, error) {
	for _, noCache := range []bool{false, true} {
		idx, err := c.GetIndex(ctx, repoURL, creds, noCache)
		if err != nil {
			return ChartVersion{}, err
		}
		entries, err := idx.GetEntries(chart)
		if err != nil && noCache {
			return ChartVersion{}, err
		}
		if i := slices.IndexFunc(entries, func(e ChartVersion) bool { return e.Version == version }); i >= 0 {
			return entries[i], nil
		}
		if !noCache {
			continue
		}
		if e, ok := closestIndexEntry(entries, version); ok {
			slog.InfoContext(ctx, "helm: no exact chart version match, using closest available version",
				"repo", redactURL(repoURL), "chart", chart, "requested", version, "selected", e.Version)
			return e, nil
		}
	}
	return ChartVersion{}, tag(ErrVersionNotFound, fmt.Errorf("chart %q version %q not found in %s repository", chart, version, redactURL(repoURL)))
}

// closestIndexEntry returns the entry with the highest semantic version that
// satisfies version parsed as a semver constraint (Helm sorts index entries by
// descending version and takes the first match). Entries that are not semantic
// versions are skipped; on equal versions the first entry in index order wins.
// It reports false when version is not a constraint or nothing matches.
func closestIndexEntry(entries []ChartVersion, version string) (ChartVersion, bool) {
	constraint, err := semver.NewConstraint(version)
	if err != nil {
		return ChartVersion{}, false
	}
	var (
		best    ChartVersion
		bestVer *semver.Version
	)
	for _, e := range entries {
		v, err := semver.NewVersion(e.Version)
		if err != nil || !constraint.Check(v) {
			continue
		}
		if bestVer == nil || v.GreaterThan(bestVer) {
			best, bestVer = e, v
		}
	}
	return best, bestVer != nil
}

// resolveReferenceURL resolves a chart URL from an index against the repository
// URL with Helm's rules (the base path is treated as a directory and the base
// query string is kept).
func resolveReferenceURL(baseURL, refURL string) (string, error) {
	ref, err := url.Parse(refURL)
	if err != nil {
		return "", err
	}
	if ref.IsAbs() {
		return refURL, nil
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	base.RawPath = strings.TrimSuffix(base.RawPath, "/") + "/"
	if base.RawPath == "/" {
		base.RawPath = ""
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/"
	resolved := base.ResolveReference(ref)
	resolved.RawQuery = base.RawQuery
	return resolved.String(), nil
}

func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// normalizeChartName returns the last path segment of chart (upstream: charts
// "foo/bar" are extracted to "bar"). Degenerate names are returned unchanged and
// rejected by the caller.
func normalizeChartName(chart string) string {
	_, nc := path.Split(chart)
	if nc == "" || nc == "." || nc == ".." {
		return chart
	}
	return nc
}

// extractChart extracts archive into a staging directory and moves the chart
// directory name to a fresh directory under WorkDir/work.
//
// Extraction checks symlinks against the staging root, but only the chart
// directory is kept, and it lands next to other callers' helm homes and charts.
// The chart directory is therefore checked again as a root of its own before
// the move, so a link such as "up -> .." (inside the archive, outside the chart)
// is rejected (upstream: checkOutOfBoundsSymlinks on the chart path).
func (c *Client) extractChart(ctx context.Context, archive io.Reader, name string) (string, error) {
	staging, err := os.MkdirTemp(c.workRoot(), "extract-")
	if err != nil {
		return "", fmt.Errorf("helm: create extraction dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := sandbox.ExtractTarGzContext(ctx, archive, staging, c.Archive); err != nil {
		return "", err
	}
	src := filepath.Join(staging, name)
	fi, err := os.Lstat(src)
	if err != nil || !fi.IsDir() {
		return "", tag(ErrInvalidChart, fmt.Errorf("chart directory %q not found in archive", name))
	}
	if err := sandbox.CheckSymlinksContext(ctx, src); err != nil {
		var oob *sandbox.OutOfBoundsSymlinkError
		if errors.As(err, &oob) {
			return "", tag(err, fmt.Errorf("chart contains out-of-bounds symlinks. file: %s", oob.Path))
		}
		return "", fmt.Errorf("helm: check chart symlinks: %w", err)
	}
	dst := staging + ".chart"
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("helm: move extracted chart: %w", err)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("helm: set chart dir mode: %w", err)
	}
	return dst, nil
}
