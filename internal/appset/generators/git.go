package generators

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/appset/template"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// GitGenerator produces parameter sets from the directories or files of a Git repository.
type GitGenerator struct {
	cfg Config
}

// RequeueAfter implements Generator: requeueAfterSeconds, else the controller default (3m).
func (g *GitGenerator) RequeueAfter(gen *v1alpha1.ApplicationSetGenerator) time.Duration {
	return requeueSeconds(gen.Git.RequeueAfterSeconds, g.cfg.defaultRequeueAfter())
}

// Template implements Generator.
func (g *GitGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.Git.Template
}

// GenerateParams implements Generator.
//
// The scoping project is the project of the generator entry's effective template (the
// generator-level template's spec.project when set, else spec.template.spec.project; see
// ScopeProject), so credentials and source integrity follow the project the generated
// Applications land in. When it is not templated, its AppProject must exist (control-plane
// namespace) and must permit the ApplicationSet's namespace (spec.sourceNamespaces, for
// ApplicationSets outside the control-plane namespace); its source integrity policy decides
// whether commits are verified and it scopes repository credentials. A templated project skips
// verification and uses only global credentials. The refresh annotation bypasses the
// repo-server revision cache.
func (g *GitGenerator) GenerateParams(ctx context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.Git == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	if g.cfg.Repos == nil {
		return nil, errors.New("error generating params from git: no repository service configured")
	}
	req := RepoRequest{
		RepoURL:         gen.Git.RepoURL,
		Revision:        gen.Git.Revision,
		NoRevisionCache: RefreshRequired(appSet),
	}
	projName := ScopeProject(ctx, appSet)
	if !strings.Contains(projName, "{{") {
		ns := g.cfg.Namespace
		if ns == "" {
			ns = appSet.Namespace
		}
		proj := &v1alpha1.AppProject{}
		if err := g.cfg.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: projName}, proj); err != nil {
			return nil, fmt.Errorf("error getting project %s: %w", projName, err)
		}
		// The project scopes repository credentials: a tenant ApplicationSet may only borrow
		// the credentials (and integrity policy) of a project that trusts its namespace.
		if !project.IsAppNamespacePermitted(proj, appSet.Namespace, ns) {
			return nil, fmt.Errorf("error getting project %s: %w", projName,
				&ProjectNotPermittedError{Namespace: appSet.Namespace, Project: projName})
		}
		si, _ := project.EffectiveSourceIntegrity(proj)
		req.VerifyCommit = si != nil && project.HasSourceIntegrityCriteria(si, true, v1alpha1.ApplicationSource{RepoURL: gen.Git.RepoURL})
		req.Project = projName
	} else {
		g.cfg.logger().InfoContext(ctx, "cannot enforce source integrity: the project is templated", appSetLogAttrs(appSet))
	}

	opts := TemplateOptions(appSet)
	var (
		res []map[string]any
		err error
	)
	switch {
	case len(gen.Git.Directories) != 0:
		res, err = g.directories(ctx, gen.Git, req, opts)
	case len(gen.Git.Files) != 0:
		res, err = g.files(ctx, gen.Git, req, opts)
	default:
		return nil, ErrEmptyAppSetGenerator
	}
	if err != nil {
		return nil, fmt.Errorf("error generating params from git: %w", err)
	}
	return res, nil
}

func (g *GitGenerator) directories(ctx context.Context, git *v1alpha1.GitGenerator, req RepoRequest, opts template.Options) ([]map[string]any, error) {
	all, err := g.cfg.Repos.GetDirectories(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("error getting directories from repo: %w", err)
	}
	g.cfg.logger().DebugContext(ctx, "directories from the repository",
		slog.String("repoURL", git.RepoURL), slog.String("revision", git.Revision), slog.Int("total", len(all)))
	selected := filterDirectories(g.cfg.logger(), git.Directories, all)
	res := make([]map[string]any, 0, len(selected))
	for _, dir := range selected {
		params := map[string]any{}
		if opts.GoTemplate {
			p := map[string]any{
				"path":               dir,
				"basename":           path.Base(dir),
				"basenameNormalized": template.SanitizeName(path.Base(dir)),
				"segments":           strings.Split(dir, "/"),
			}
			if git.PathParamPrefix != "" {
				params[git.PathParamPrefix] = map[string]any{"path": p}
			} else {
				params["path"] = p
			}
		} else {
			name := "path"
			if git.PathParamPrefix != "" {
				name = git.PathParamPrefix + "." + name
			}
			params[name] = dir
			params[name+".basename"] = path.Base(dir)
			params[name+".basenameNormalized"] = template.SanitizeName(path.Base(dir))
			addSegments(params, name, dir)
		}
		if err := appendTemplatedValues(git.Values, params, opts); err != nil {
			return nil, fmt.Errorf("error generating params from apps: failed to append templated values: %w", err)
		}
		res = append(res, params)
	}
	return res, nil
}

// filterDirectories keeps the directories that match at least one include pattern and no
// exclude pattern (path.Match; '*' does not cross '/'). Invalid patterns are logged and ignored.
func filterDirectories(log *slog.Logger, items []v1alpha1.GitDirectoryGeneratorItem, all []string) []string {
	var res []string
	for _, dir := range all {
		include, exclude := false, false
		for _, item := range items {
			ok, err := path.Match(item.Path, dir)
			if err != nil {
				log.Warn("invalid git directory pattern", slog.String("pattern", item.Path), slog.Any("error", err))
				continue
			}
			if ok && item.Exclude {
				exclude = true
			} else if ok {
				include = true
			}
		}
		if include && !exclude {
			res = append(res, dir)
		}
	}
	return res
}

func (g *GitGenerator) files(ctx context.Context, git *v1alpha1.GitGenerator, req RepoRequest, opts template.Options) ([]map[string]any, error) {
	contents := map[string][]byte{}
	var excludes []string
	for _, item := range git.Files {
		if item.Exclude {
			excludes = append(excludes, item.Path)
			continue
		}
		files, err := g.cfg.Repos.GetFiles(ctx, req, item.Path)
		if err != nil {
			return nil, err
		}
		maps.Copy(contents, files)
	}
	for _, pattern := range excludes {
		files, err := g.cfg.Repos.GetFiles(ctx, req, pattern)
		if err != nil {
			return nil, err
		}
		for p := range files {
			delete(contents, p)
		}
	}
	paths := make([]string, 0, len(contents))
	for p := range contents {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var res []map[string]any
	for _, p := range paths {
		params, err := paramsFromFile(p, contents[p], git, opts)
		if err != nil {
			return nil, fmt.Errorf("unable to process file '%s' from repository '%s': %w", p, git.RepoURL, err)
		}
		res = append(res, params...)
	}
	return res, nil
}

// paramsFromFile parses a file as one YAML/JSON object (an empty file is one empty object) or,
// failing that, as a list of objects.
func paramsFromFile(filePath string, content []byte, git *v1alpha1.GitGenerator, opts template.Options) ([]map[string]any, error) {
	var objects []map[string]any
	single := map[string]any{}
	if err := yaml.Unmarshal(content, &single); err == nil {
		objects = append(objects, single)
	} else if err := yaml.Unmarshal(content, &objects); err != nil {
		return nil, fmt.Errorf("unable to parse file: %w", err)
	}
	dir := path.Dir(filePath)
	res := make([]map[string]any, 0, len(objects))
	for _, obj := range objects {
		params := map[string]any{}
		if opts.GoTemplate {
			maps.Copy(params, obj)
			p := map[string]any{
				"path":               dir,
				"basename":           path.Base(dir),
				"filename":           path.Base(filePath),
				"basenameNormalized": template.SanitizeName(path.Base(dir)),
				"filenameNormalized": template.SanitizeName(path.Base(filePath)),
				"segments":           strings.Split(dir, "/"),
			}
			if git.PathParamPrefix != "" {
				params[git.PathParamPrefix] = map[string]any{"path": p}
			} else {
				params["path"] = p
			}
		} else {
			maps.Copy(params, flattenObject(obj))
			name := "path"
			if git.PathParamPrefix != "" {
				name = git.PathParamPrefix + "." + name
			}
			params[name] = dir
			params[name+".basename"] = path.Base(dir)
			params[name+".filename"] = path.Base(filePath)
			params[name+".basenameNormalized"] = template.SanitizeName(path.Base(dir))
			params[name+".filenameNormalized"] = template.SanitizeName(path.Base(filePath))
			addSegments(params, name, dir)
		}
		if err := appendTemplatedValues(git.Values, params, opts); err != nil {
			return nil, fmt.Errorf("failed to append templated values: %w", err)
		}
		res = append(res, params)
	}
	return res, nil
}

func addSegments(params map[string]any, name, dir string) {
	for i, seg := range strings.Split(dir, "/") {
		if seg != "" {
			params[name+"["+strconv.Itoa(i)+"]"] = seg
		}
	}
}

// RefreshRequired reports whether the ApplicationSet carries the refresh annotation.
func RefreshRequired(appSet *v1alpha1.ApplicationSet) bool {
	if appSet == nil {
		return false
	}
	_, ok := appSet.Annotations[v1alpha1.AnnotationKeyApplicationSetRefresh]
	return ok
}
