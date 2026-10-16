package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Maintainer is a Chart.yaml maintainer.
type Maintainer struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// ChartMetadata is the subset of Chart.yaml exposed for application details.
type ChartMetadata struct {
	APIVersion   string       `json:"apiVersion,omitempty"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	AppVersion   string       `json:"appVersion,omitempty"`
	KubeVersion  string       `json:"kubeVersion,omitempty"`
	Description  string       `json:"description,omitempty"`
	Type         string       `json:"type,omitempty"`
	Home         string       `json:"home,omitempty"`
	Icon         string       `json:"icon,omitempty"`
	Keywords     []string     `json:"keywords,omitempty"`
	Sources      []string     `json:"sources,omitempty"`
	Maintainers  []Maintainer `json:"maintainers,omitempty"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
	Deprecated   bool         `json:"deprecated,omitempty"`
}

// ChartDetails is the GetRevisionChartDetails view of a chart.
type ChartDetails struct {
	Description string   `json:"description,omitempty"`
	Home        string   `json:"home,omitempty"`
	Maintainers []string `json:"maintainers,omitempty"`
}

// Details formats maintainers as "Name <email>" (or "Name" without email), as
// upstream does.
func (m ChartMetadata) Details() ChartDetails {
	var maintainers []string
	for _, mt := range m.Maintainers {
		if mt.Email != "" {
			maintainers = append(maintainers, strings.TrimSpace(fmt.Sprintf("%s <%s>", mt.Name, mt.Email)))
		} else {
			maintainers = append(maintainers, mt.Name)
		}
	}
	return ChartDetails{Description: m.Description, Home: m.Home, Maintainers: maintainers}
}

// ShowChart runs `helm show chart .` in chartDir and parses the result.
func (c *Client) ShowChart(ctx context.Context, chartDir string) (ChartMetadata, error) {
	out, err := c.show(ctx, chartDir, "chart")
	if err != nil {
		return ChartMetadata{}, fmt.Errorf("failed to inspect chart: %w", err)
	}
	var m ChartMetadata
	if err := yaml.Unmarshal(out, &m); err != nil {
		return ChartMetadata{}, fmt.Errorf("failed to unmarshal chart: %w", err)
	}
	return m, nil
}

// ShowValues runs `helm show values .` in chartDir and returns the chart's
// values.yaml as helm prints it.
func (c *Client) ShowValues(ctx context.Context, chartDir string) ([]byte, error) {
	out, err := c.show(ctx, chartDir, "values")
	if err != nil {
		return nil, fmt.Errorf("failed to inspect values: %w", err)
	}
	return out, nil
}

func (c *Client) show(ctx context.Context, chartDir, what string) ([]byte, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	if err := checkChartDir(chartDir); err != nil {
		return nil, err
	}
	h, err := c.newHome()
	if err != nil {
		return nil, err
	}
	defer h.close()
	out, _, err := c.run(ctx, h, chartDir, []string{"show", what, "."}, nil)
	return out, err
}

// ReadChartValues returns the raw values.yaml of chartDir for application
// details. ok is false when the file does not exist, is not readable, or
// resolves outside repoRoot (upstream only shows in-bounds values files). The
// content is bounded by Client.MaxValuesBytes.
func (c *Client) ReadChartValues(chartDir, repoRoot string) (data []byte, ok bool, err error) {
	p, _, err := ResolveValueFilePathOrURL(chartDir, repoRoot, "values.yaml", nil)
	if err != nil {
		return nil, false, nil //nolint:nilerr // out-of-bounds values are hidden, not an error
	}
	return c.readValues(context.Background(), p, positiveOr(c.MaxValuesBytes, DefaultMaxValuesBytes))
}

// GetParameters returns the flattened parameters of a chart for application
// details: the chart's values (`helm show values .`, skipped when values.yaml
// would resolve outside repoRoot) merged with valueFiles in order, later files
// overriding earlier keys. valueFiles are already resolved: absolute local
// paths (missing or unreadable files are skipped, as upstream) or URLs; http and
// https URLs are fetched without credentials, other schemes are skipped.
// Every document is bounded by Client.MaxValuesBytes.
//
// Keys are flattened as "a.b" for maps and "list[0].c" for lists; scalars are
// formatted with %v (null becomes "<nil>"), as upstream.
func (c *Client) GetParameters(ctx context.Context, chartDir, repoRoot string, valueFiles []string) (map[string]string, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	var docs [][]byte
	if _, _, err := ResolveValueFilePathOrURL(chartDir, repoRoot, "values.yaml", nil); err == nil {
		out, err := c.ShowValues(ctx, chartDir)
		if err != nil {
			return nil, fmt.Errorf("failed to execute helm inspect values command: %w", err)
		}
		docs = append(docs, out)
	} else {
		slog.WarnContext(ctx, "helm: values.yaml not allowed", "error", err)
	}
	maxBytes := positiveOr(c.MaxValuesBytes, DefaultMaxValuesBytes)
	for _, f := range valueFiles {
		data, ok, err := c.readValues(ctx, f, maxBytes)
		if err != nil {
			return nil, err
		}
		if ok {
			docs = append(docs, data)
		}
	}
	out := map[string]string{}
	for _, d := range docs {
		values := map[string]any{}
		if err := yaml.Unmarshal(d, &values); err != nil {
			return nil, fmt.Errorf("failed to parse values: %w", err)
		}
		flattenValues(values, out)
	}
	return out, nil
}

func (c *Client) readValues(ctx context.Context, f string, maxBytes int64) ([]byte, bool, error) {
	if u, err := url.ParseRequestURI(f); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		data, err := c.fetchRemoteValues(ctx, f, maxBytes)
		if err != nil {
			return nil, false, fmt.Errorf("failed to read value file %s: %w", redactURL(f), err)
		}
		return data, true, nil
	}
	file, err := os.Open(f)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to read value file: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := readCapped(file, maxBytes)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read value file: %w", err)
	}
	return data, true, nil
}

func (c *Client) fetchRemoteValues(ctx context.Context, f string, maxBytes int64) ([]byte, error) {
	hc, err := c.httpClient(Creds{})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return readCapped(resp.Body, maxBytes)
}

func readCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: limit %d bytes", ErrValuesTooLarge, maxBytes)
	}
	return data, nil
}

// flattenValues writes every leaf of values into out with its dotted path.
func flattenValues(values map[string]any, out map[string]string) {
	for _, k := range slices.Sorted(maps.Keys(values)) {
		flattenInto(values[k], k, out)
	}
}

func flattenInto(v any, key string, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			flattenInto(t[k], key+"."+k, out)
		}
	case []any:
		for i, e := range t {
			flattenInto(e, fmt.Sprintf("%s[%d]", key, i), out)
		}
	default:
		out[key] = fmt.Sprintf("%v", t)
	}
}
