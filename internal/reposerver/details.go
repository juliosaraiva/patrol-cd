package reposerver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/render"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// maxValueFilesWalk bounds the directory walk listing Helm value files.
const maxValueFilesWalk = 100000

// GetAppDetails returns source-type-specific details (SRC-122..SRC-125): Helm value
// files, raw values and flattened parameters; Kustomize images; plugin parameter
// announcements. Results are cached per resolved revision unless NoCache.
func (s *Service) GetAppDetails(ctx context.Context, q *apiclient.AppDetailsQuery) (_ *apiclient.AppDetails, err error) {
	defer s.finish(&err)
	if q == nil || q.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "missing payload in request")
	}
	src := q.Source
	if src.RepoURL == "" {
		src.RepoURL = q.Repo.URL
	}
	kind := kindOf(q.Repo, src)
	noRevCache := q.NoCache || q.NoRevisionCache
	unresolved := src.TargetRevision
	revision, err := s.resolveSource(ctx, q.Repo, src, kind, unresolved, noRevCache)
	if err != nil {
		return nil, err
	}
	key := auxKey("appdetails", repoCacheURL(src.RepoURL, kind), s.fp.of(q.Repo), revision, digestJSON(struct {
		Source    v1alpha1.ApplicationSource
		Refs      map[string]apiclient.RefTarget
		Kustomize apiclient.KustomizeOptions
		Helm      apiclient.HelmOptions
		Enabled   map[string]bool
		AppName   string
		Server    string
	}{keySource(src, kind), stripRefCreds(q.RefSources), q.KustomizeOptions, q.HelmOptions, q.EnabledSourceTypes, q.AppName, s.serverStamp}))
	if !q.NoCache {
		if d, ok := getAux[apiclient.AppDetails](s.aux, key); ok {
			s.metrics.cache("appdetails", true)
			return d, nil
		}
	}
	s.metrics.cache("appdetails", false)

	// Referenced git sources used by the selected value files are resolved first, so
	// every working copy is locked together in path order (no lock-order deadlocks).
	refs, err := s.detailRefs(ctx, q, src, kind, unresolved, revision)
	if err != nil {
		return nil, err
	}
	specs := refHoldSpecs(refs, !s.cfg.DisableSubmodules)
	if kind == kindGit {
		specs = append([]holdSpec{{repo: q.Repo, revision: revision, tagPrefix: src.TagPrefix, readRefCache: !noRevCache, submodules: !s.cfg.DisableSubmodules}}, specs...)
	}
	set, err := s.holdAll(ctx, specs)
	if err != nil {
		return nil, err
	}
	defer set.release()
	refRoots := map[string]string{}
	if len(refs) > 0 {
		for k, v := range set.refRoots(refs, kind == kindGit) {
			refRoots[k] = v
			refRoots[strings.TrimPrefix(k, "$")] = v
		}
	}

	var root, appRel string
	switch kind {
	case kindHelm:
		pass := src.Helm != nil && src.Helm.PassCredentials
		if q.NoCache {
			_ = s.helm.CleanChartCache(q.Repo.URL, src.Chart, revision, s.helmCreds(q.Repo, pass), helmEnableOCI(q.Repo))
		}
		dir, err := s.helm.PullChart(ctx, q.Repo.URL, src.Chart, revision, s.helmCreds(q.Repo, pass), helmEnableOCI(q.Repo))
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		root, appRel = dir, "."
	case kindOCI:
		dir, cleanup, err := s.pullOCI(ctx, q.Repo, src.RepoURL, revision, q.NoCache)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		root, appRel = dir, src.Path
	default:
		root, appRel = set.holds[0].root, src.Path
		if err := s.checkSymlinks(ctx, root, revision, q.NoCache, "repository", ".git"); err != nil {
			return nil, err
		}
	}
	if appRel == "" {
		appRel = "."
	}
	dir, err := appDir(root, appRel)
	if err != nil {
		return nil, err
	}
	typ, err := s.detectType(ctx, root, dir, src, q.EnabledSourceTypes)
	if err != nil {
		return nil, err
	}
	res := &apiclient.AppDetails{Type: typ}
	switch v1alpha1.ApplicationSourceType(typ) {
	case v1alpha1.ApplicationSourceTypeHelm:
		if res.Helm, err = s.helmDetails(ctx, q, src, dir, root, refRoots); err != nil {
			return nil, err
		}
	case v1alpha1.ApplicationSourceTypeKustomize:
		images, err := s.kustomizeImages(ctx, q, src, kind, root, appRel, revision)
		if err != nil {
			return nil, err
		}
		res.Kustomize = &apiclient.KustomizeAppSpec{Images: images}
	case v1alpha1.ApplicationSourceTypePlugin:
		params, err := s.renderer.PluginParameters(ctx, render.Input{
			RootDir: root, AppPath: appRel, Source: src, Revision: revision, Repo: q.Repo,
			AppName: q.AppName, EnabledSourceTypes: q.EnabledSourceTypes,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to populate plugin app details: %w", err)
		}
		res.Plugin = &apiclient.PluginAppSpec{ParametersAnnouncement: params}
	}
	putAux(s.aux, key, res, s.cfg.RepoCacheExpiration)
	return res, nil
}

// detectType returns the source type (upstream GetAppSourceType): the explicit block
// (Directory when that tool is disabled), else plugin discovery, Chart.yaml or
// kustomization in the application directory, else Directory.
func (s *Service) detectType(ctx context.Context, root, appDir string, src v1alpha1.ApplicationSource, enabled map[string]bool) (string, error) {
	t, err := render.ExplicitType(&src)
	if err != nil {
		return "", withCode(apiclient.CodeInvalidArgument, fmt.Errorf("error getting app source type: %w", err))
	}
	if t != nil {
		if !render.IsSourceTypeEnabled(*t, enabled) {
			return string(v1alpha1.ApplicationSourceTypeDirectory), nil
		}
		return string(*t), nil
	}
	apps, err := s.renderer.Discover(ctx, root, appDir, enabled)
	if err != nil {
		return "", fmt.Errorf("error getting app source type: %w", err)
	}
	if typ, ok := apps["."]; ok {
		return typ, nil
	}
	return string(v1alpha1.ApplicationSourceTypeDirectory), nil
}

// helmDetails builds the Helm details: value files under the chart, raw values.yaml
// and the flattened parameters of the chart merged with the selected value files
// (resolved without environment substitution; `$ref` files from git ref sources).
func (s *Service) helmDetails(ctx context.Context, q *apiclient.AppDetailsQuery, src v1alpha1.ApplicationSource, chartDir, repoRoot string, refRoots map[string]string) (*apiclient.HelmAppSpec, error) {
	out := &apiclient.HelmAppSpec{}
	files, err := listValueFiles(chartDir)
	if err != nil {
		return nil, err
	}
	out.ValueFiles = files
	if data, ok, err := s.helm.ReadChartValues(chartDir, repoRoot); err != nil {
		return nil, err
	} else if ok {
		out.Values = string(data)
	}
	var selected []string
	ignoreMissing := false
	if src.Helm != nil {
		selected = src.Helm.ValueFiles
		ignoreMissing = src.Helm.IgnoreMissingValueFiles
		out.FileParameters = slices.Clone(src.Helm.FileParameters)
	}
	resolved, err := resolveDetailValueFiles(chartDir, repoRoot, selected, refRoots, q.HelmOptions.ValuesFileSchemes, ignoreMissing)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve value files: %w", err)
	}
	params, err := s.helm.GetParameters(ctx, chartDir, repoRoot, resolved)
	if err != nil {
		return nil, err
	}
	for _, name := range sortedKeys(params) {
		out.Parameters = append(out.Parameters, v1alpha1.HelmParameter{Name: name, Value: params[name]})
	}
	return out, nil
}

// detailRefs resolves the git ref sources used by the selected value files of a
// multi-source Helm source (GetAppDetails checks out only git-type refs, as upstream).
func (s *Service) detailRefs(ctx context.Context, q *apiclient.AppDetailsQuery, src v1alpha1.ApplicationSource, kind sourceKind, unresolved, revision string) ([]resolvedRef, error) {
	if len(q.RefSources) == 0 || src.Helm == nil {
		return nil, nil
	}
	gitRefs := map[string]apiclient.RefTarget{}
	for k, v := range q.RefSources {
		if v.Repo.Type == "" || v.Repo.Type == v1alpha1.RepositoryTypeGit {
			gitRefs[k] = v
		}
	}
	var used []string
	for _, f := range src.Helm.ValueFiles {
		if strings.HasPrefix(f, "$") {
			if _, ok := lookupRef(gitRefs, strings.SplitN(f, "/", 2)[0]); ok {
				used = append(used, f)
			}
		}
	}
	if len(used) == 0 {
		return nil, nil
	}
	mainURL := ""
	if kind == kindGit {
		mainURL = src.RepoURL
	}
	return s.resolveRefs(ctx, true, &v1alpha1.ApplicationSourceHelm{ValueFiles: used}, gitRefs, kind, mainURL, unresolved, revision, q.NoCache || q.NoRevisionCache)
}

// listValueFiles lists files under dir whose name contains "values" with a .yaml/.yml
// extension (case-insensitive), relative to dir (upstream walkHelmValueFilesInPath).
// Symlinks are not followed.
func listValueFiles(dir string) ([]string, error) {
	var out []string
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error reading helm values file from %s: %w", p, err)
		}
		n++
		if n > maxValueFilesWalk {
			return errors.New("too many files in the chart directory")
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if strings.Contains(d.Name(), "values") && (ext == ".yaml" || ext == ".yml") {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out, err
}

// resolveDetailValueFiles resolves selected value files like manifest generation but
// without environment substitution: URLs (allowed schemes) verbatim, local paths
// confined to their repository root, globs expanded, `$ref/...` relative to the ref root.
func resolveDetailValueFiles(chartDir, repoRoot string, files []string, refRoots map[string]string, schemes []string, ignoreMissing bool) ([]string, error) {
	var out []string
	for _, raw := range files {
		base, root, rel := chartDir, repoRoot, raw
		if strings.HasPrefix(raw, "$") {
			refVar, rest, _ := strings.Cut(raw, "/")
			r, ok := refRoots[refVar]
			if !ok {
				continue
			}
			base, root, rel = r, r, rest
		}
		p, remote, err := helm.ResolveValueFilePathOrURL(base, root, rel, schemes)
		if err != nil {
			return nil, err
		}
		if remote {
			out = append(out, p)
			continue
		}
		if strings.ContainsAny(rel, "*?[{") {
			matches, err := doublestar.FilepathGlob(p, doublestar.WithFilesOnly())
			if err != nil {
				return nil, fmt.Errorf("values file glob %q: %w", raw, err)
			}
			sort.Strings(matches)
			var inside []string
			for _, m := range matches {
				if within(root, m) {
					inside = append(inside, m)
				}
			}
			if len(inside) == 0 && !ignoreMissing {
				return nil, withCode(apiclient.CodeNotFound, fmt.Errorf("values file glob %q matched no files", raw))
			}
			for _, m := range inside {
				if !slices.Contains(out, m) {
					out = append(out, m)
				}
			}
			continue
		}
		if _, err := os.Stat(p); err != nil && ignoreMissing {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// within reports whether p (after resolving symlinks) is strictly inside root.
func within(root, p string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(realRoot, real)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// kustomizeImages builds the kustomization (with the source's edits, empty namespace)
// and returns every container and init container image, sorted (SRC-123).
func (s *Service) kustomizeImages(ctx context.Context, q *apiclient.AppDetailsQuery, src v1alpha1.ApplicationSource, kind sourceKind, root, appRel, revision string) ([]string, error) {
	res, err := s.renderTimed(ctx, render.Input{
		RootDir: root, AppPath: appRel, Source: src, Revision: revision, Repo: q.Repo,
		AppName: q.AppName, KustomizeOptions: q.KustomizeOptions, EnabledSourceTypes: q.EnabledSourceTypes,
		GitCreds: s.remoteBaseCreds(kind, q.Repo),
	})
	if err != nil {
		return nil, err
	}
	var images []string
	for _, o := range res.Manifests {
		images = append(images, imagesOf(o.Object)...)
	}
	sort.Strings(images)
	return images, nil
}

// imagesOf collects "image" of every containers/initContainers entry at any depth
// (upstream getImages).
func imagesOf(obj map[string]any) []string {
	var out []string
	for k, v := range obj {
		switch v := v.(type) {
		case []any:
			if k == "containers" || k == "initContainers" {
				for _, c := range v {
					if m, ok := c.(map[string]any); ok {
						if img, ok := m["image"]; ok {
							out = append(out, fmt.Sprintf("%v", img))
						}
					}
				}
				continue
			}
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					out = append(out, imagesOf(m)...)
				}
			}
		case map[string]any:
			out = append(out, imagesOf(v)...)
		}
	}
	return out
}

// stripRefCreds removes credentials from ref targets (cache keys).
func stripRefCreds(refs map[string]apiclient.RefTarget) map[string]apiclient.RefTarget {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]apiclient.RefTarget, len(refs))
	for k, v := range refs {
		out[k] = apiclient.RefTarget{Repo: apiclient.Repository{URL: v.Repo.URL, Type: v.Repo.Type}, TargetRevision: v.TargetRevision, Chart: v.Chart}
	}
	return out
}
