package render

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// ErrHelmNotConfigured reports a Helm source rendered by a Renderer without a Helm
// client.
var ErrHelmNotConfigured = errors.New("render: helm client is not configured")

// pullChart downloads a Helm chart source (SRC-042, SRC-043) and returns its extracted
// directory, which becomes both the repository root and the application path.
func (r *Renderer) pullChart(ctx context.Context, c *call) (string, func(), error) {
	hc, err := r.helmClient(c)
	if err != nil {
		return "", nil, err
	}
	src := c.in.Source
	enableOCI := c.in.Repo.EnableOCI || c.in.Repo.Type == v1alpha1.RepositoryTypeOCI
	passCreds := src.Helm != nil && src.Helm.PassCredentials
	creds := r.helmCreds(c.in.Repo, passCreds)
	version := c.in.Revision
	if version == "" {
		v, err := hc.ResolveChartVersion(ctx, src.RepoURL, src.Chart, src.TargetRevision, creds, enableOCI)
		if err != nil {
			return "", nil, err
		}
		version = v
		c.in.Revision = v
	}
	dir, err := hc.PullChart(ctx, src.RepoURL, src.Chart, version, creds, enableOCI)
	if err != nil {
		return "", nil, fmt.Errorf("error fetching chart: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// helmCreds maps a repository to helm credentials.
func (r *Renderer) helmCreds(repo apiclient.Repository, passCredentials bool) helm.Creds {
	creds := helm.Creds{
		Username:           repo.Username,
		Password:           repo.Password,
		InsecureSkipVerify: repo.Insecure,
		PassCredentials:    passCredentials,
		PlainHTTP:          repo.InsecureOCIForceHTTP,
	}
	if creds.Password == "" && repo.BearerToken != "" {
		// Registries accepting tokens as basic-auth passwords (for example cloud
		// registries with short-lived tokens minted by the caller).
		creds.Password = repo.BearerToken
	}
	if repo.TLSClientCertData != "" && repo.TLSClientCertKey != "" {
		creds.CertData = []byte(repo.TLSClientCertData)
		creds.KeyData = []byte(repo.TLSClientCertKey)
	}
	if r.cfg.CAData != nil {
		if host := repoHost(repo.URL); host != "" {
			creds.CAData = r.cfg.CAData(host)
		}
	}
	return creds
}

func repoHost(raw string) string {
	s := raw
	if !strings.Contains(s, "://") {
		s = "//" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// renderHelm renders a Helm source (SRC-019..SRC-041, SRC-095).
func (r *Renderer) renderHelm(ctx context.Context, c *call) ([]*unstructured.Unstructured, string, error) {
	hc, err := r.helmClient(c)
	if err != nil {
		return nil, "", err
	}
	src := c.src
	appName, _ := tracking.ParseInstanceName(c.in.AppName, "")
	kubeVersion := kubeVersionOrDefault(src, c.in.KubeVersion)
	if _, err := helm.NormalizeKubeVersion(kubeVersion); err != nil {
		return nil, "", err
	}
	opts := helm.TemplateOpts{
		ReleaseName: appName,
		Namespace:   namespaceOrDefault(src, c.in.Namespace),
		KubeVersion: kubeVersion,
		APIVersions: apiVersionsOrDefault(src, c.in.APIVersions),
	}
	schemes := c.in.HelmOptions.ValuesFileSchemes
	passCredentials := false
	if h := src.Helm; h != nil {
		if err := helm.CheckVersion(h.Version); err != nil {
			return nil, "", fmt.Errorf("error initializing helm app object: failed to create new helm command: %w", err)
		}
		opts.Version = h.Version
		if h.ReleaseName != "" {
			opts.ReleaseName = h.ReleaseName
		}
		if err := c.validateRefs(h); err != nil {
			return nil, "", err
		}
		valueFiles, err := c.resolvedValueFiles(h.ValueFiles, schemes, h.IgnoreMissingValueFiles)
		if err != nil {
			var gnm *GlobNoMatchError
			if errors.As(err, &gnm) {
				return nil, "", withCode(apiclient.CodeNotFound, fmt.Errorf("error resolving helm value files: %w", err))
			}
			return nil, "", fmt.Errorf("error resolving helm value files: %w", err)
		}
		opts.ValueFiles = valueFiles
		values, err := helmValuesYAML(h)
		if err != nil {
			return nil, "", fmt.Errorf("error writing helm values file: %w", err)
		}
		opts.Values = values
		for _, p := range h.Parameters {
			kv := helm.KV{Name: p.Name, Value: c.env.Envsubst(p.Value)}
			if p.ForceString {
				opts.SetStringParams = append(opts.SetStringParams, kv)
			} else {
				opts.SetParams = append(opts.SetParams, kv)
			}
		}
		for _, p := range h.FileParameters {
			if refDir := c.refSourceDir(p.Path); refDir != "" {
				resolved, err := c.resolveRefValueFile(p.Path, refDir, schemes)
				if err != nil {
					return nil, "", fmt.Errorf("error resolving set-file path: %w", err)
				}
				opts.SetFileParams = append(opts.SetFileParams, helm.KV{Name: p.Name, Value: resolved})
				continue
			}
			resolved, _, err := helm.ResolveValueFilePathOrURL(c.appPath, c.root, c.env.Envsubst(p.Path), schemes)
			if err != nil {
				return nil, "", fmt.Errorf("error resolving helm value file path: %w", err)
			}
			opts.SetFileParams = append(opts.SetFileParams, helm.KV{Name: p.Name, Value: resolved})
		}
		passCredentials = h.PassCredentials
		opts.PassCredentials = h.PassCredentials
		opts.SkipCRDs = h.SkipCrds
		opts.SkipSchemaValidation = h.SkipSchemaValidation
		opts.SkipTests = h.SkipTests
	}
	if opts.ReleaseName == "" {
		opts.ReleaseName = c.in.AppName
	}

	out, cmd, err := hc.TemplateWithCommand(ctx, c.appPath, opts)
	if err != nil {
		if !helm.IsMissingDependencyErr(err) {
			return nil, "", err
		}
		repos, rerr := r.helmDependencyRepos(c, passCredentials)
		if rerr != nil {
			return nil, "", fmt.Errorf("error getting helm repos: %w", rerr)
		}
		if derr := hc.EnsureDependencies(ctx, c.appPath, repos, helm.DependencyOpts{InsecureSkipVerify: c.in.Repo.Insecure}); derr != nil {
			return nil, "", c.dependencyError(derr, repos)
		}
		out, cmd, err = hc.TemplateWithCommand(ctx, c.appPath, opts)
		if err != nil {
			return nil, "", err
		}
	}
	objs, err := splitYAML(out, c.budget)
	if err != nil {
		return nil, "", err
	}
	return objs, c.redactPaths(cmd), nil
}

// helmValuesYAML returns valuesObject as YAML when set, else values (SRC-026,
// SRC-027). Values are not env-substituted.
func helmValuesYAML(h *v1alpha1.ApplicationSourceHelm) ([]byte, error) {
	if h.ValuesObject != nil && h.ValuesObject.Raw != nil {
		y, err := yaml.JSONToYAML(h.ValuesObject.Raw)
		if err != nil {
			return nil, err
		}
		return y, nil
	}
	if h.Values == "" {
		return nil, nil
	}
	return []byte(h.Values), nil
}

// unreachableRepoMarkers are the dependency-build error fragments that mean a
// repository could not be used. The first two are upstream's (`helm repo add` and
// `helm dependency build` output); Patrol registers repositories by writing
// repositories.yaml instead of running `helm repo add`, so an unreachable index
// surfaces from `helm dependency build` as "no cached repository for ...".
var unreachableRepoMarkers = []string{
	"is not a valid chart repository or cannot be reached",
	"could not download",
	"no cached repository for",
}

// dependencyError explains a failed dependency build: when it looks like an
// unreachable repository and some dependency repositories are not permitted by the
// project, it reports them (SRC-041, PermissionDenied).
func (c *call) dependencyError(err error, repos []helm.RepoCreds) error {
	msg := err.Error()
	unreachable := false
	for _, m := range unreachableRepoMarkers {
		if strings.Contains(msg, m) {
			unreachable = true
			break
		}
	}
	if !unreachable {
		return err
	}
	proj := &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceRepos: c.in.ProjectSourceRepos}}
	var denied []string
	for _, repo := range repos {
		u := repo.URL
		if repo.EnableOCI && u != "" && !strings.Contains(u, "://") {
			// Projects list OCI repositories without scheme, as upstream.
			u = oci.TrimScheme(u)
		}
		if !project.IsSourcePermitted(proj, u) {
			denied = append(denied, u)
		}
	}
	if len(denied) == 0 {
		return err
	}
	return codef(apiclient.CodePermissionDenied, "helm repos %s are not permitted in project '%s'", strings.Join(denied, ", "), c.in.ProjectName)
}

// helmDependencyRepos matches the chart's dependency repositories with credentials
// (SRC-040): exact URL among the project's repositories, then repository name, then
// the first credential template whose URL prefix matches, then, for OCI
// dependencies, the first OCI repository whose URL is prefix-related.
func (r *Renderer) helmDependencyRepos(c *call, passCredentials bool) ([]helm.RepoCreds, error) {
	deps, err := helm.DependencyRepos(c.appPath)
	if err != nil {
		return nil, err
	}
	byURL := map[string]apiclient.Repository{}
	byName := map[string]apiclient.Repository{}
	for _, repo := range c.in.Repos {
		byURL[oci.TrimScheme(repo.URL)] = repo
		if repo.Name != "" {
			byName[repo.Name] = repo
		}
	}
	out := make([]helm.RepoCreds, 0, len(deps))
	for _, dep := range deps {
		repo, ok := byURL[dep.URL]
		if !ok || dep.URL == "" {
			repo, ok = byName[dep.Name]
			if !ok || dep.Name == "" {
				ok = false
			}
		}
		if !ok {
			repo = apiclient.Repository{URL: dep.URL, Name: dep.Name, EnableOCI: dep.EnableOCI}
			if tmpl := matchCredentialTemplate(c.in.HelmRepoCreds, dep.URL); tmpl != nil {
				access := tmpl.Access
				repo.EnableOCI = repo.EnableOCI || access.EnableOCI || access.Type == v1alpha1.RepositoryTypeOCI
				repo.Username, repo.Password, repo.BearerToken = access.Username, access.Password, access.BearerToken
				repo.TLSClientCertData, repo.TLSClientCertKey = access.TLSClientCertData, access.TLSClientCertKey
				repo.InsecureOCIForceHTTP = access.InsecureOCIForceHTTP
				repo.Insecure = access.Insecure
			} else if repo.EnableOCI {
				for _, cand := range c.in.Repos {
					if ociRepoMatches(cand, dep.URL) {
						repo.Username, repo.Password, repo.BearerToken = cand.Username, cand.Password, cand.BearerToken
						repo.InsecureOCIForceHTTP = cand.InsecureOCIForceHTTP
						repo.EnableOCI = true
						break
					}
				}
			}
		}
		enableOCI := repo.EnableOCI || repo.Type == v1alpha1.RepositoryTypeOCI
		u := repo.URL
		if enableOCI {
			u = oci.TrimScheme(u)
		}
		out = append(out, helm.RepoCreds{
			Name:      repo.Name,
			URL:       u,
			EnableOCI: enableOCI,
			Creds:     r.helmCreds(repo, passCredentials),
		})
	}
	return out, nil
}

// matchCredentialTemplate returns the first template whose URL prefix matches url
// (upstream getRepoCredential: non-OCI templates match the URL without "oci://", OCI
// templates match "oci://"+url).
func matchCredentialTemplate(tmpls []apiclient.RepositoryCredentialTemplate, u string) *apiclient.RepositoryCredentialTemplate {
	for i := range tmpls {
		t := &tmpls[i]
		if t.URLPrefix == "" {
			continue
		}
		if t.Access.Type != v1alpha1.RepositoryTypeOCI {
			if strings.HasPrefix(strings.TrimPrefix(u, "oci://"), t.URLPrefix) {
				return t
			}
		} else if strings.HasPrefix("oci://"+u, t.URLPrefix) {
			return t
		}
	}
	return nil
}

func ociRepoMatches(cand apiclient.Repository, dep string) bool {
	if cand.URL == "" || dep == "" {
		return false
	}
	if cand.EnableOCI && (strings.HasPrefix(dep, cand.URL) || strings.HasPrefix(cand.URL, dep)) {
		return true
	}
	return cand.Type == v1alpha1.RepositoryTypeOCI &&
		(strings.HasPrefix("oci://"+dep, cand.URL) || strings.HasPrefix(cand.URL, "oci://"+dep))
}

// refName returns "$name" for a "$name/..." path, or "".
func refName(p string) string {
	if !strings.HasPrefix(p, "$") {
		return ""
	}
	name, _, _ := strings.Cut(p, "/")
	return name
}

// refSourceDir returns the checkout directory of the source referenced by p, or ""
// when p does not reference a known source (it is then an ordinary,
// env-substituted path, as upstream).
func (c *call) refSourceDir(p string) string {
	name := refName(p)
	if name == "" || len(c.in.RefSources) == 0 {
		return ""
	}
	if dir, ok := c.in.RefSources[name]; ok {
		return dir
	}
	return c.in.RefSources[strings.TrimPrefix(name, "$")]
}

// validateRefs reproduces the multi-source `$ref` errors (SRC-096) for references
// that name no ref source.
func (c *call) validateRefs(h *v1alpha1.ApplicationSourceHelm) error {
	if !c.in.HasMultipleSources {
		return nil
	}
	candidates := slices.Clone(h.ValueFiles)
	for _, fp := range h.FileParameters {
		candidates = append(candidates, fp.Path)
	}
	for _, p := range candidates {
		name := refName(p)
		if name == "" || c.refSourceDir(p) != "" {
			continue
		}
		if len(c.in.RefSources) == 0 {
			return fmt.Errorf("source referenced %q, but no source has a 'ref' field defined", name)
		}
		keys := make([]string, 0, len(c.in.RefSources))
		for k := range c.in.RefSources {
			if !strings.HasPrefix(k, "$") {
				k = "$" + k
			}
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return fmt.Errorf("source referenced %q, which is not one of the available sources (%s)", name, strings.Join(keys, ", "))
	}
	return nil
}

// resolveRefValueFile resolves "$ref/rest" inside the referenced checkout root:
// the rest is env-substituted after the reference is expanded and must stay inside
// that root (SRC-095).
func (c *call) resolveRefValueFile(p, refDir string, schemes []string) (string, error) {
	_, rest, _ := strings.Cut(p, "/")
	resolved, _, err := helm.ResolveValueFilePathOrURL(refDir, refDir, c.env.Envsubst("/"+rest), schemes)
	if err != nil {
		return "", fmt.Errorf("error resolving value file path: %w", err)
	}
	return resolved, nil
}

func isGlobPath(p string) bool { return strings.ContainsAny(p, "*?[") }

// resolvedValueFiles resolves helm.valueFiles (SRC-022..SRC-025, SRC-095): local
// paths are confined to the repository (or referenced checkout) root, remote URLs
// need an allowed scheme, glob patterns are expanded in lexical order, files listed
// explicitly keep their explicit position, duplicates are passed once, and missing
// files (or empty globs) are skipped when ignoreMissing is set.
func (c *call) resolvedValueFiles(raw []string, schemes []string, ignoreMissing bool) ([]string, error) {
	type entry struct {
		resolved string
		remote   bool
		root     string
		err      error
	}
	entries := make([]entry, len(raw))
	explicit := map[string]bool{}
	for i, v := range raw {
		e := entry{root: c.root}
		if refDir := c.refSourceDir(v); refDir != "" {
			e.root = refDir
			e.resolved, e.err = c.resolveRefValueFile(v, refDir, schemes)
		} else {
			var err error
			e.resolved, e.remote, err = helm.ResolveValueFilePathOrURL(c.appPath, c.root, c.env.Envsubst(v), schemes)
			if err != nil {
				e.err = fmt.Errorf("error resolving value file path: %w", err)
			}
		}
		if e.err == nil && !e.remote && !isGlobPath(e.resolved) {
			explicit[e.resolved] = true
		}
		entries[i] = e
	}
	var out []string
	seen := map[string]bool{}
	appendUnique := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for i, e := range entries {
		if e.err != nil {
			return nil, e.err
		}
		if !e.remote && isGlobPath(e.resolved) {
			matches, err := doublestar.FilepathGlob(e.resolved)
			if err != nil {
				return nil, fmt.Errorf("error expanding glob pattern %q: %w", raw[i], err)
			}
			if len(matches) == 0 {
				if ignoreMissing {
					continue
				}
				return nil, &GlobNoMatchError{Pattern: raw[i]}
			}
			if err := verifyGlobMatchesWithinRoot(matches, e.root); err != nil {
				return nil, fmt.Errorf("glob pattern %q: %w", raw[i], err)
			}
			for _, m := range matches {
				if !explicit[m] {
					appendUnique(m)
				}
			}
			continue
		}
		if !e.remote {
			if _, err := os.Stat(e.resolved); errors.Is(err, fs.ErrNotExist) && ignoreMissing {
				continue
			}
		}
		appendUnique(e.resolved)
	}
	return out, nil
}

// verifyGlobMatchesWithinRoot rejects glob matches whose symlink-resolved path
// leaves root (doublestar returns the link itself).
func verifyGlobMatchesWithinRoot(matches []string, root string) error {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("error resolving symlinks in repo root: %w", err)
	}
	for _, m := range matches {
		real, err := filepath.EvalSymlinks(m)
		if err != nil {
			return fmt.Errorf("error resolving symlink for glob match %q: %w", filepath.Base(m), err)
		}
		rel, err := filepath.Rel(canonicalRoot, real)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			r, _ := filepath.Rel(root, m)
			return fmt.Errorf("glob match %q resolved to outside repository root", r)
		}
	}
	return nil
}

// redactPaths replaces the repository root and referenced checkout roots by "." in a
// command line (upstream redactPaths).
func (c *call) redactPaths(s string) string {
	roots := []string{c.root}
	for _, d := range c.in.RefSources {
		roots = append(roots, d)
	}
	// Longest first so nested roots are replaced before their parents.
	slices.SortFunc(roots, func(a, b string) int { return len(b) - len(a) })
	for _, p := range roots {
		if p != "" {
			s = strings.ReplaceAll(s, p, ".")
		}
	}
	return s
}
