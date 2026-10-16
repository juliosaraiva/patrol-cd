package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// SkipFileRenderingMarker excludes a Directory-source file that contains it anywhere
// (upstream "+argocd:skip-file-rendering"). The Patrol spelling is honored as well.
const (
	SkipFileRenderingMarker       = "+argocd:skip-file-rendering"
	PatrolSkipFileRenderingMarker = "+patrol:skip-file-rendering"
)

var manifestFileRe = regexp.MustCompile(`^.*\.(yaml|yml|json|jsonnet)$`)

// manifestFile is a Directory-source candidate.
type manifestFile struct {
	path    string // absolute path as walked (may be a symlink)
	name    string // base name of path
	jsonnet bool
	content []byte // decoded content (YAML/JSON only)
}

// renderDirectory renders a Directory source: YAML/JSON files and Jsonnet files
// (SRC-005..SRC-018).
func (r *Renderer) renderDirectory(ctx context.Context, c *call) ([]*unstructured.Unstructured, error) {
	dir := v1alpha1.ApplicationSourceDirectory{}
	if c.src.Directory != nil {
		dir = *c.src.Directory
	}
	files, err := c.potentiallyValidManifests(ctx, dir.Recurse, dir.Include, dir.Exclude)
	if err != nil {
		c.log.Debug("failed to get potentially valid manifests", "error", err)
		return nil, fmt.Errorf("failed to get potentially valid manifests: %w", err)
	}
	jsonnetEnabled := IsSourceTypeEnabled(v1alpha1.ApplicationSourceTypeDirectory, c.in.EnabledSourceTypes)

	var jsonnetPaths []string
	for _, f := range files {
		if f.jsonnet && jsonnetEnabled {
			jsonnetPaths = append(jsonnetPaths, f.path)
		}
	}
	var (
		jresp jsonnet.Response
		jerr  error // reported at the position of the first Jsonnet file
	)
	if len(jsonnetPaths) > 0 {
		jresp, jerr = r.evaluateJsonnet(ctx, c, dir.Jsonnet, jsonnetPaths)
	}

	var objs []*unstructured.Unstructured
	jsonnetIndex := 0
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !f.jsonnet {
			parsed, err := c.parseManifestFile(f)
			if err != nil {
				return nil, err
			}
			objs = append(objs, parsed...)
			continue
		}
		if !jsonnetEnabled {
			continue
		}
		idx := jsonnetIndex
		jsonnetIndex++
		if jerr != nil {
			return nil, jerr
		}
		if jresp.Error != nil && jresp.Error.Index == idx {
			return nil, codef(apiclient.CodeInvalidArgument, "Failed to evaluate jsonnet %q: %s", f.name, jresp.Error.Message)
		}
		out := jresp.Outputs[idx]
		arr, isArray, err := decodeJSONArray(out, c.budget)
		if err != nil {
			return nil, err
		}
		if isArray {
			objs = append(objs, arr...)
			continue
		}
		var obj unstructured.Unstructured
		if err := json.Unmarshal(out, &obj); err != nil {
			return nil, codef(apiclient.CodeInvalidArgument, "Failed to unmarshal generated json %q: %v", f.name, err)
		}
		if err := c.budget.take(&obj); err != nil {
			return nil, err
		}
		objs = append(objs, &obj)
	}
	return objs, nil
}

// evaluateJsonnet evaluates the Jsonnet files of a Directory source in the sandboxed
// child. Library paths resolve relative to the repository root and must stay inside
// it (SRC-017); external variables and top-level arguments are build-env substituted
// (SRC-015, SRC-016).
func (r *Renderer) evaluateJsonnet(ctx context.Context, c *call, opts v1alpha1.ApplicationSourceJsonnet, files []string) (jsonnet.Response, error) {
	req := jsonnet.Request{
		RepoRoot:       c.root,
		JPaths:         []string{c.appPath},
		Files:          files,
		MaxOutputBytes: c.limits.MaxOutputBytes,
	}
	for _, lib := range opts.Libs {
		p, err := helm.ResolveFileOrDirectoryPath(c.root, c.root, lib)
		if err != nil {
			return jsonnet.Response{}, err
		}
		req.JPaths = append(req.JPaths, p)
	}
	for _, v := range opts.ExtVars {
		req.ExtVars = append(req.ExtVars, jsonnet.Var{Name: v.Name, Value: c.env.Envsubst(v.Value), Code: v.Code})
	}
	for _, v := range opts.TLAs {
		req.TLAs = append(req.TLAs, jsonnet.Var{Name: v.Name, Value: c.env.Envsubst(v.Value), Code: v.Code})
	}
	runner := r.cfg.Jsonnet
	if runner.Limits.MaxStdout == 0 && c.limits.MaxOutputBytes > 0 {
		runner.Limits.MaxStdout = c.limits.MaxOutputBytes + 1<<20
	}
	resp, err := runner.Run(ctx, req)
	r.metrics.observeJsonnet(len(files), err == nil && resp.Error == nil)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return jsonnet.Response{}, ctxErr
		}
		return jsonnet.Response{}, codef(apiclient.CodeInvalidArgument, "Failed to evaluate jsonnet: %w", err)
	}
	return resp, nil
}

// globMatcher caches compiled include/exclude patterns. Patterns are compiled without
// separators, so "*" also matches "/" (upstream glob.Match). An invalid pattern is
// logged and matches nothing.
type globMatcher struct {
	c     *call
	cache map[string]*dirGlob
	bad   map[string]bool
}

func (g *globMatcher) match(pattern, text string) bool {
	if pattern == "" {
		return false
	}
	if g.bad[pattern] {
		return false
	}
	compiled, ok := g.cache[pattern]
	if !ok {
		var err error
		compiled, err = compileDirGlob(pattern)
		if err != nil {
			g.c.log.Warn("failed to compile directory glob pattern", "pattern", pattern, "error", err)
			g.bad[pattern] = true
			return false
		}
		g.cache[pattern] = compiled
	}
	return compiled.match(text)
}

// potentiallyValidManifests walks the application directory and returns the
// candidate manifest files in walk (lexical) order, enforcing the combined size
// limit on YAML/JSON files (SRC-013).
func (c *call) potentiallyValidManifests(ctx context.Context, recurse bool, include, exclude string) ([]manifestFile, error) {
	maxCombined := c.limits.MaxCombinedDirectoryManifestsSize
	var combined, read int64
	gm := &globMatcher{c: c, cache: map[string]*dirGlob{}, bad: map[string]bool{}}
	var out []manifestFile
	err := filepath.WalkDir(c.appPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if p != c.appPath && !recurse {
				return filepath.SkipDir
			}
			return nil
		}
		f, size, warning, err := c.potentiallyValidManifest(p, d, include, exclude, gm)
		if err != nil {
			return fmt.Errorf("invalid manifest file %q: %w", p, err)
		}
		if f == nil {
			if warning != "" {
				c.log.Warn("skipping manifest file", "file", d.Name(), "reason", warning)
			}
			return nil
		}
		if !f.jsonnet {
			combined += size
			if maxCombined > 0 && combined > maxCombined {
				return ErrExceededMaxCombinedManifestFileSize
			}
			read += int64(len(f.content))
			if c.limits.MaxOutputBytes > 0 && read > c.limits.MaxOutputBytes {
				return fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, c.limits.MaxOutputBytes)
			}
		}
		out = append(out, *f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// potentiallyValidManifest applies the baseline candidate rules to one walked entry:
// name pattern, symlink resolution inside the repository root (SRC-012), regular
// files only, exclude then include globs, and the skip marker (SRC-010). It returns
// nil without error for skipped files (with a warning worth logging, if any) and the
// size that counts toward the combined limit.
func (c *call) potentiallyValidManifest(p string, d fs.DirEntry, include, exclude string, gm *globMatcher) (*manifestFile, int64, string, error) {
	relPath, err := filepath.Rel(c.appPath, p)
	if err != nil {
		return nil, 0, "", fmt.Errorf("failed to get relative path of %q: %w", p, err)
	}
	name := d.Name()
	if !manifestFileRe.MatchString(name) {
		return nil, 0, "", nil
	}
	realPath := p
	info, err := d.Info()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, "", nil
		}
		return nil, 0, "", err
	}
	relRealPath := relPath
	if info.Mode()&fs.ModeSymlink != 0 {
		rootRel, err := relToRoot(c.root, p)
		if err != nil {
			return nil, 0, "", fmt.Errorf("illegal filepath in symlink at %q", relPath)
		}
		resolved, err := sandbox.SecureJoin(c.root, rootRel)
		if err != nil {
			if errors.Is(err, sandbox.ErrPathEscape) {
				return nil, 0, "", fmt.Errorf("illegal filepath in symlink at %q", relPath)
			}
			return nil, 0, "", fmt.Errorf("failed to evaluate symlink at %q: %w", relPath, err)
		}
		realInfo, err := os.Stat(resolved)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, 0, fmt.Sprintf("destination of symlink %q is missing", relPath), nil
			}
			return nil, 0, "", fmt.Errorf("failed to get file info for symlink at %q: %w", relPath, err)
		}
		info = realInfo
		realPath = resolved
		if rr, err := relToRoot(c.root, resolved); err == nil {
			relRealPath = rr
		}
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Sprintf("ignoring symlink at %q to non-regular file %q", relPath, relRealPath), nil
	}
	if exclude != "" && gm.match(exclude, relPath) {
		return nil, 0, "", nil
	}
	if include != "" && !gm.match(include, relPath) {
		return nil, 0, "", nil
	}
	isJsonnet := strings.HasSuffix(name, ".jsonnet")
	// Upstream reads every candidate whole to look for the skip marker; Patrol bounds
	// that read: YAML/JSON files by the output limit, Jsonnet files by the import cap.
	limit := c.limits.MaxOutputBytes
	if isJsonnet {
		limit = jsonnet.DefaultMaxImportBytes
	}
	if limit > 0 && info.Size() > limit {
		if isJsonnet {
			return nil, 0, "", fmt.Errorf("jsonnet file %q is larger than %d bytes", relPath, limit)
		}
		return nil, 0, "", fmt.Errorf("%w: %q is larger than %d bytes", ErrOutputTooLarge, relPath, limit)
	}
	raw, err := readFileBounded(realPath, limit)
	if err != nil {
		return nil, 0, "", fmt.Errorf("failed to read %q: %w", relPath, err)
	}
	content, err := decodeText(raw)
	if err != nil {
		return nil, 0, "", fmt.Errorf("failed to read %q: %w", relPath, err)
	}
	if bytes.Contains(content, []byte(SkipFileRenderingMarker)) || bytes.Contains(content, []byte(PatrolSkipFileRenderingMarker)) {
		return nil, 0, "", nil
	}
	f := &manifestFile{path: p, name: name, jsonnet: isJsonnet}
	if !isJsonnet {
		f.content = content
	}
	return f, info.Size(), "", nil
}

// parseManifestFile decodes a YAML or JSON candidate (SRC-005, SRC-006).
func (c *call) parseManifestFile(f manifestFile) ([]*unstructured.Unstructured, error) {
	if strings.HasSuffix(f.name, ".json") {
		return c.parseJSONFile(f)
	}
	objs, err := splitYAMLOrJSON(bytes.NewReader(f.content), c.budget)
	if err != nil {
		if errors.Is(err, ErrTooManyManifests) {
			return nil, err
		}
		if len(objs) > 0 || looksLikeManifest(f.content, false) {
			return nil, codef(apiclient.CodeInvalidArgument, "Failed to unmarshal %q: %v", f.name, err)
		}
		// Unrelated YAML (for example a values file) is ignored.
		c.log.Debug("ignoring non-manifest YAML file", "file", f.name)
		return nil, nil
	}
	return objs, nil
}

// parseJSONFile decodes a JSON candidate: exactly one JSON value. A JSON object without
// kind is skipped unless it looks like a manifest (Patrol: upstream's OCI-manifest
// fallback, whose intent is to tolerate non-Kubernetes JSON files, is applied as a
// skip rather than emitting an object without kind).
func (c *call) parseJSONFile(f manifestFile) ([]*unstructured.Unstructured, error) {
	dec := json.NewDecoder(bytes.NewReader(f.content))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, codef(apiclient.CodeInvalidArgument, "Failed to unmarshal %q: %v", f.name, err)
	}
	if dec.More() {
		return nil, codef(apiclient.CodeInvalidArgument, "Found multiple objects in %q. Only single objects are allowed in JSON files.", f.name)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(raw); err != nil {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && trimmed[0] == '{' && !looksLikeManifest(f.content, true) {
			c.log.Debug("ignoring non-manifest JSON file", "file", f.name)
			return nil, nil
		}
		return nil, codef(apiclient.CodeInvalidArgument, "Failed to unmarshal %q: %v", f.name, err)
	}
	if err := c.budget.take(u); err != nil {
		return nil, err
	}
	return []*unstructured.Unstructured{u}, nil
}
