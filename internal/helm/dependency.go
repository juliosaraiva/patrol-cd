package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// DependencyMarkerFile is written into a chart directory after a successful
// EnsureDependencies; while it exists the build is skipped. Repository checkouts
// are cleaned on revision change, which removes it.
const DependencyMarkerFile = ".patrol-helm-dep-up"

// maxChartYAMLBytes bounds Chart.yaml reads.
const maxChartYAMLBytes = 4 << 20

// RepoCreds is a repository made available to `helm dependency build`.
type RepoCreds struct {
	// Name is the repository name referenced as "@name" or "alias:name" in
	// Chart.yaml. Empty means a name derived from URL ('/' replaced by '-').
	Name string
	// URL is the chart repository URL, or the OCI registry/repository (with or
	// without "oci://") when EnableOCI is set.
	URL string
	// EnableOCI marks an OCI registry: its credentials go to the helm registry
	// config (only when both username and password are set, as upstream).
	EnableOCI bool
	// Creds are the repository credentials. PassCredentials maps to the
	// repository's pass_credentials_all; PlainHTTP on any OCI repository adds
	// --plain-http to the build (helm cannot mix per repository).
	Creds Creds
}

// DependencyOpts tunes `helm dependency build`.
type DependencyOpts struct {
	// InsecureSkipVerify adds --insecure-skip-tls-verify (upstream: the source
	// repository is marked insecure).
	InsecureSkipVerify bool
}

// IsMissingDependencyErr reports a `helm template` failure caused by chart
// dependencies that are declared but not vendored, the only case in which the
// baseline runs a dependency build.
func IsMissingDependencyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "found in requirements.yaml, but missing in charts") ||
		strings.Contains(msg, "found in Chart.yaml, but missing in charts/ directory")
}

// DependencyBuild runs `helm dependency build` in chartDir with repos registered
// in a per-call repositories.yaml / registry config (0600, removed afterwards).
func (c *Client) DependencyBuild(ctx context.Context, chartDir string, repos []RepoCreds) error {
	return c.DependencyBuildWithOptions(ctx, chartDir, repos, DependencyOpts{})
}

// DependencyBuildWithOptions is DependencyBuild with options.
func (c *Client) DependencyBuildWithOptions(ctx context.Context, chartDir string, repos []RepoCreds, opts DependencyOpts) error {
	if err := c.init(); err != nil {
		return err
	}
	if err := checkChartDir(chartDir); err != nil {
		return err
	}
	h, err := c.newHome()
	if err != nil {
		return err
	}
	defer h.close()
	redact, plainHTTP, err := c.writeRepoConfig(ctx, h, repos)
	if err != nil {
		return err
	}
	args := []string{"dependency", "build"}
	if opts.InsecureSkipVerify {
		args = append(args, "--insecure-skip-tls-verify")
	}
	if plainHTTP {
		args = append(args, "--plain-http")
	}
	if _, _, err := c.run(ctx, h, chartDir, args, redact); err != nil {
		return fmt.Errorf("failed to build helm dependencies: %w", err)
	}
	return nil
}

// EnsureDependencies runs DependencyBuildWithOptions at most once per chart
// directory: concurrent callers for the same directory are serialized and
// DependencyMarkerFile records success.
func (c *Client) EnsureDependencies(ctx context.Context, chartDir string, repos []RepoCreds, opts DependencyOpts) error {
	if err := c.init(); err != nil {
		return err
	}
	unlock := c.locks.lock("deps|" + filepath.Clean(chartDir))
	defer unlock()
	marker := filepath.Join(chartDir, DependencyMarkerFile)
	if _, err := os.Lstat(marker); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("helm: check dependency marker: %w", err)
	}
	if err := c.DependencyBuildWithOptions(ctx, chartDir, repos, opts); err != nil {
		return fmt.Errorf("error building helm chart dependencies: %w", err)
	}
	if err := os.WriteFile(marker, []byte("marker"), 0o644); err != nil {
		return fmt.Errorf("helm: write dependency marker: %w", err)
	}
	return nil
}

type repoFile struct {
	APIVersion   string      `json:"apiVersion"`
	Generated    string      `json:"generated"`
	Repositories []repoEntry `json:"repositories"`
}

type repoEntry struct {
	Name                  string `json:"name"`
	URL                   string `json:"url"`
	Username              string `json:"username"`
	Password              string `json:"password"`
	CertFile              string `json:"certFile"`
	KeyFile               string `json:"keyFile"`
	CAFile                string `json:"caFile"`
	InsecureSkipTLSVerify bool   `json:"insecure_skip_tls_verify"`
	PassCredentialsAll    bool   `json:"pass_credentials_all"`
}

// writeRepoConfig writes the repositories.yaml and registry config of h. It
// returns the secrets to redact and whether any OCI repository needs plain HTTP.
func (c *Client) writeRepoConfig(ctx context.Context, h *home, repos []RepoCreds) ([]string, bool, error) {
	var (
		redact    []string
		plainHTTP bool
		file      = repoFile{APIVersion: "v1", Generated: "0001-01-01T00:00:00Z", Repositories: []repoEntry{}}
		names     = map[string]string{}
		store     *credentials.FileStore
	)
	for _, r := range repos {
		redact = append(redact, r.Creds.secrets()...)
		if r.EnableOCI {
			if r.Creds.PlainHTTP {
				plainHTTP = true
			}
			if r.Creds.Username == "" || r.Creds.Password == "" {
				continue
			}
			host, err := registryHost(r.URL)
			if err != nil {
				return nil, false, err
			}
			if store == nil {
				s, err := credentials.NewFileStore(h.registryConfig())
				if err != nil {
					return nil, false, fmt.Errorf("helm: create registry config: %w", err)
				}
				store = s
			}
			cred := auth.Credential{Username: r.Creds.Username, Password: r.Creds.Password}
			if err := store.Put(ctx, credentials.ServerAddressFromRegistry(host), cred); err != nil {
				return nil, false, fmt.Errorf("failed to login to registry %s: %w", host, err)
			}
			continue
		}
		name, err := repoName(r)
		if err != nil {
			return nil, false, err
		}
		if prev, dup := names[name]; dup {
			if prev == r.URL {
				continue
			}
			for i := 2; ; i++ {
				candidate := name + "-" + strconv.Itoa(i)
				if _, taken := names[candidate]; !taken {
					name = candidate
					break
				}
			}
		}
		names[name] = r.URL
		e := repoEntry{
			Name:                  name,
			URL:                   r.URL,
			Username:              r.Creds.Username,
			Password:              r.Creds.Password,
			InsecureSkipTLSVerify: r.Creds.InsecureSkipVerify,
			PassCredentialsAll:    r.Creds.PassCredentials,
		}
		if len(r.Creds.CAData) > 0 {
			if e.CAFile, err = h.writeSecret("ca-*.pem", r.Creds.CAData); err != nil {
				return nil, false, err
			}
		}
		if len(r.Creds.CertData) > 0 && len(r.Creds.KeyData) > 0 {
			if e.CertFile, err = h.writeSecret("cert-*.pem", r.Creds.CertData); err != nil {
				return nil, false, err
			}
			if e.KeyFile, err = h.writeSecret("key-*.pem", r.Creds.KeyData); err != nil {
				return nil, false, err
			}
		}
		file.Repositories = append(file.Repositories, e)
	}
	data, err := yaml.Marshal(file)
	if err != nil {
		return nil, false, fmt.Errorf("helm: encode repositories.yaml: %w", err)
	}
	if err := writeFileExclusive(h.repoConfig(), data); err != nil {
		return nil, false, err
	}
	return redact, plainHTTP, nil
}

func writeFileExclusive(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("helm: create %s: %w", filepath.Base(p), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("helm: write %s: %w", filepath.Base(p), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("helm: write %s: %w", filepath.Base(p), err)
	}
	return nil
}

// registryHost extracts host[:port] from an OCI repository reference (upstream
// getHelmRegistry: "//" is prepended when the URL has none).
func registryHost(repo string) (string, error) {
	r := oci.TrimScheme(repo)
	if !strings.Contains(r, "//") {
		r = "//" + r
	}
	u, err := url.Parse(r)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: failed to parse registry URL %q", ErrInvalidArgument, redactURL(repo))
	}
	return u.Host, nil
}

// repoName returns a safe repository name: the given one, or the URL with '/'
// replaced by '-' (upstream sanitizeRepoName). Characters that could change the
// meaning of the name as a cache file name are replaced by '-'.
func repoName(r RepoCreds) (string, error) {
	name := r.Name
	if name == "" {
		name = strings.ReplaceAll(r.URL, "/", "-")
	}
	name = strings.Map(func(c rune) rune {
		if c == '/' || c == '\\' || c == 0 || c < 0x20 {
			return '-'
		}
		return c
	}, name)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q", ErrInvalidRepoName, r.Name)
	}
	return name, nil
}

// Dependency is a Chart.yaml dependency.
type Dependency struct {
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Repository string `json:"repository,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Condition  string `json:"condition,omitempty"`
}

// DependencyRepo is a repository referenced by a chart dependency.
type DependencyRepo struct {
	// Name is set for "@name" / "alias:name" references and for URL references
	// (URL with '/' replaced by '-').
	Name string
	// URL is set for URL references; "oci://" is stripped.
	URL string
	// EnableOCI is set for "oci://" references.
	EnableOCI bool
}

// DependencyRepos lists the repositories referenced by the dependencies in
// chartDir/Chart.yaml with the baseline rules: "@name" and "alias:name" are
// named references; "https://" and "oci://" URLs are URL references; other
// schemes (http://, file://) and empty repositories are not listed. Callers use
// the result to pick credentials for DependencyBuild (exact URL, then name, then
// credential-template prefix).
func DependencyRepos(chartDir string) ([]DependencyRepo, error) {
	deps, err := ChartDependencies(chartDir)
	if err != nil {
		return nil, err
	}
	var out []DependencyRepo
	for _, d := range deps {
		switch {
		case strings.HasPrefix(d.Repository, "@"):
			out = append(out, DependencyRepo{Name: d.Repository[1:]})
		case strings.HasPrefix(d.Repository, "alias:"):
			out = append(out, DependencyRepo{Name: strings.TrimPrefix(d.Repository, "alias:")})
		default:
			u, err := url.Parse(d.Repository)
			if err != nil || (u.Scheme != "https" && u.Scheme != "oci") {
				continue
			}
			out = append(out, DependencyRepo{
				Name:      strings.ReplaceAll(d.Repository, "/", "-"),
				URL:       oci.TrimScheme(d.Repository),
				EnableOCI: u.Scheme == "oci",
			})
		}
	}
	return out, nil
}

// ChartDependencies parses the dependencies of chartDir/Chart.yaml. Chart.yaml
// must resolve inside chartDir and is read with a size cap.
func ChartDependencies(chartDir string) ([]Dependency, error) {
	p, err := sandbox.SecureJoin(chartDir, "Chart.yaml")
	if err != nil {
		return nil, tag(ErrPathOutsideRoot, fmt.Errorf("error reading helm chart: %w", err))
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("error reading helm chart Chart.yaml: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxChartYAMLBytes+1))
	if err != nil {
		return nil, fmt.Errorf("error reading helm chart Chart.yaml: %w", err)
	}
	if len(data) > maxChartYAMLBytes {
		return nil, fmt.Errorf("%w: Chart.yaml exceeds %d bytes", ErrInvalidChart, maxChartYAMLBytes)
	}
	var meta struct {
		Dependencies []Dependency `json:"dependencies"`
	}
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("error unmarshalling the helm chart while getting helm dependency repos: %w", err)
	}
	return meta.Dependencies, nil
}
