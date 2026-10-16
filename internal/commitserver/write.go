package commitserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"go.yaml.in/yaml/v3"
	utiljson "k8s.io/apimachinery/pkg/util/json"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
)

// Request bounds (the HTTP layer additionally caps the body size).
const (
	MaxPaths               = 10000
	MaxPathLength          = 1024
	MaxCommitMessageBytes  = 1 << 20
	MaxReadmeTemplateBytes = 1 << 20
	maxAuthorFieldLength   = 256
	maxCommandsPerPath     = 1000
	maxCommandLength       = 64 << 10
)

// gitAttributesContent is written to the repository root (upstream content, no trailing
// newline).
const gitAttributesContent = "**/README.md linguist-generated=true\n**/hydrator.metadata linguist-generated=true"

// plan is a validated request with the manifests already encoded.
type plan struct {
	paths  []plannedPath
	readme *template.Template
	author Author
	// digest identifies the manifest content of the request (paths and encoded manifests);
	// it is recorded in the git note next to the dry SHA.
	digest string
}

type plannedPath struct {
	dir      string
	manifest []byte
	commands []string
}

// prepare validates req and encodes its manifests. Every error wraps ErrInvalidRequest.
func prepare(req *CommitHydratedManifestsRequest) (*plan, error) {
	if req == nil {
		return nil, invalidf("request is required")
	}
	switch {
	case req.Repo.URL == "":
		return nil, invalidf("repo URL is required")
	case req.Repo.Type != "" && req.Repo.Type != v1alpha1.RepositoryTypeGit:
		return nil, invalidf("repository type %q cannot be written, only git", req.Repo.Type)
	case req.TargetBranch == "":
		return nil, invalidf("target branch is required")
	case req.SyncBranch == "":
		return nil, invalidf("sync branch is required")
	case !git.IsCommitSHA(req.DrySHA):
		return nil, invalidf("dry SHA %q is not a commit SHA", truncate(req.DrySHA, 80))
	case len(req.Paths) == 0:
		return nil, invalidf("at least one path is required")
	case len(req.Paths) > MaxPaths:
		return nil, invalidf("too many paths (%d > %d)", len(req.Paths), MaxPaths)
	case len(req.CommitMessage) > MaxCommitMessageBytes:
		return nil, invalidf("commit message exceeds %d bytes", MaxCommitMessageBytes)
	case len(req.ReadmeTemplate) > MaxReadmeTemplateBytes:
		return nil, invalidf("readme template exceeds %d bytes", MaxReadmeTemplateBytes)
	}
	author := req.Author
	if author.Name == "" {
		author.Name = DefaultAuthorName
	}
	if author.Email == "" {
		author.Email = DefaultAuthorEmail
	}
	for _, v := range []string{author.Name, author.Email} {
		if len(v) > maxAuthorFieldLength || strings.ContainsFunc(v, unicode.IsControl) || strings.ContainsAny(v, "<>") {
			return nil, invalidf("invalid commit author identity")
		}
	}
	readme, err := ParseTemplate("readme", req.ReadmeTemplate)
	if err != nil {
		return nil, invalidf("failed to parse readme template: %v", err)
	}

	p := &plan{readme: readme, author: author, paths: make([]plannedPath, 0, len(req.Paths))}
	seen := make(map[string]bool, len(req.Paths))
	// folded holds the case-folded paths: on a case-insensitive file system "A/manifest.yaml"
	// and "a/manifest.yaml" are the same entry.
	folded := make(map[string]bool, len(req.Paths))
	for i, pd := range req.Paths {
		dir, err := cleanPath(pd.Path)
		if err != nil {
			return nil, err
		}
		if seen[dir] {
			return nil, invalidf("path %q is listed more than once", dir)
		}
		seen[dir] = true
		folded[strings.ToLower(dir)] = true
		if len(pd.Commands) > maxCommandsPerPath {
			return nil, invalidf("path %q: too many commands", dir)
		}
		for _, c := range pd.Commands {
			if len(c) > maxCommandLength {
				return nil, invalidf("path %q: command exceeds %d bytes", dir, maxCommandLength)
			}
		}
		manifest, err := encodeManifests(pd.Manifests)
		if err != nil {
			return nil, invalidf("path %q (entry %d): %v", dir, i, err)
		}
		p.paths = append(p.paths, plannedPath{dir: dir, manifest: manifest, commands: pd.Commands})
	}
	for _, pp := range p.paths {
		if err := checkOwnedFileOverlap(pp.dir, folded); err != nil {
			return nil, err
		}
	}
	p.digest = digestOf(p.paths)
	return p, nil
}

// ownedFileNames are the files the commit-server writes in every application path.
var ownedFileNames = []string{ManifestFileName, MetadataFileName, ReadmeFileName}

func isOwnedFileName(name string) bool {
	return slices.ContainsFunc(ownedFileNames, func(o string) bool { return strings.EqualFold(o, name) })
}

// checkOwnedFileOverlap rejects dir when it passes through a file another path of the
// request owns ("x" and "x/manifest.yaml/..."): the two paths cannot both be written, and
// writing one would replace the other's output. paths holds the case-folded request paths
// (the comparison ignores case, for case-insensitive file systems).
func checkOwnedFileOverlap(dir string, paths map[string]bool) error {
	parts := strings.Split(dir, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		if paths[strings.ToLower(parent)] && isOwnedFileName(parts[i]) {
			return invalidf("path %q overlaps the file %s written for path %q", dir, parts[i], parent)
		}
	}
	return nil
}

// cleanPath validates an application path and returns its cleaned slash form. The path
// must be relative, must not be the repository root, must stay inside the repository and
// must not enter .git; its first component cannot be one of the root files the
// commit-server writes.
func cleanPath(p string) (string, error) {
	switch {
	case p == "":
		return "", invalidf("path is required")
	case len(p) > MaxPathLength:
		return "", invalidf("path exceeds %d bytes", MaxPathLength)
	case strings.ContainsFunc(p, unicode.IsControl):
		return "", invalidf("path %q contains control characters", truncate(p, 80))
	case strings.Contains(p, `\`):
		return "", invalidf("path %q contains a backslash", p)
	case path.IsAbs(p):
		return "", invalidf("path %q must be relative to the repository root", p)
	}
	c := path.Clean(p)
	switch {
	case c == ".":
		return "", invalidf("path %q is the repository root, which is not allowed", p)
	case c == ".." || strings.HasPrefix(c, "../"):
		return "", invalidf("path %q is outside the repository", p)
	}
	parts := strings.Split(c, "/")
	for _, part := range parts {
		if strings.EqualFold(part, ".git") {
			return "", invalidf("path %q enters the .git directory", p)
		}
	}
	if parts[0] == MetadataFileName || parts[0] == GitAttributesFileName {
		return "", invalidf("path %q collides with the root file %s", p, parts[0])
	}
	return c, nil
}

// encodeManifests renders JSON objects as a YAML stream (2-space indentation, keys sorted,
// render order kept; upstream encoder settings).
func encodeManifests(manifests []json.RawMessage) ([]byte, error) {
	if len(manifests) == 0 {
		return []byte{}, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for i, m := range manifests {
		var obj map[string]any
		if err := utiljson.Unmarshal(m, &obj); err != nil {
			return nil, fmt.Errorf("manifest %d is not valid JSON: %w", i, err)
		}
		if obj == nil {
			return nil, fmt.Errorf("manifest %d is not a JSON object", i)
		}
		if err := enc.Encode(obj); err != nil {
			return nil, fmt.Errorf("failed to encode manifest %d: %w", i, err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("failed to encode manifests: %w", err)
	}
	return buf.Bytes(), nil
}

func digestOf(paths []plannedPath) string {
	sorted := slices.Clone(paths)
	slices.SortFunc(sorted, func(a, b plannedPath) int { return strings.Compare(a.dir, b.dir) })
	h := sha256.New()
	for _, p := range sorted {
		h.Write([]byte(strconv.Itoa(len(p.dir)) + ":" + p.dir + ":" + strconv.Itoa(len(p.manifest)) + ":"))
		h.Write(p.manifest)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// writeTree writes the plan into the checked-out working tree under root. A path is
// rewritten only when its manifest.yaml differs from the committed one (a missing, non-regular
// or different file counts as changed); its other files are never touched, and an owned
// file name occupied by a directory fails the request (see replaceFile). When anything
// changed the root hydrator.metadata and .gitattributes are rewritten too. It reports
// whether anything changed.
func writeTree(root *os.Root, logger *slog.Logger, p *plan, rootMD HydratorCommitMetadata) (bool, error) {
	changed := false
	for _, pp := range p.paths {
		if err := ensureDir(root, pp.dir); err != nil {
			return false, err
		}
		manifestPath := pp.dir + "/" + ManifestFileName
		old, ok, err := readRegular(root, manifestPath, int64(len(pp.manifest)))
		if err != nil {
			return false, err
		}
		if ok && bytes.Equal(old, pp.manifest) {
			continue
		}
		changed = true
		logger.Debug("commit-server: manifests changed", slog.String("path", pp.dir))
		md := HydratorCommitMetadata{RepoURL: rootMD.RepoURL, DrySHA: rootMD.DrySHA, Commands: pp.commands}
		meta, err := marshalMetadata(md)
		if err != nil {
			return false, err
		}
		readme, err := renderReadme(p.readme, md)
		if err != nil {
			return false, fmt.Errorf("failed to write readme: %w", err)
		}
		for _, f := range []struct {
			name string
			data []byte
		}{
			{ManifestFileName, pp.manifest},
			{MetadataFileName, meta},
			{ReadmeFileName, readme},
		} {
			if err := replaceFile(root, pp.dir+"/"+f.name, f.data); err != nil {
				return false, err
			}
		}
	}
	if !changed {
		return false, nil
	}
	meta, err := marshalMetadata(rootMD)
	if err != nil {
		return false, err
	}
	if err := replaceFile(root, MetadataFileName, meta); err != nil {
		return false, fmt.Errorf("failed to write top-level hydrator metadata: %w", err)
	}
	if err := replaceFile(root, GitAttributesFileName, []byte(gitAttributesContent)); err != nil {
		return false, fmt.Errorf("failed to write git attributes: %w", err)
	}
	return true, nil
}

// ensureDir creates dir (0755) after checking that none of its existing components is a
// symbolic link or a non-directory: repository content is untrusted, and os.Root alone
// would follow in-repository links.
func ensureDir(root *os.Root, dir string) error {
	parts := strings.Split(dir, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		fi, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to inspect path %q: %w", prefix, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write path %q: %q is a symbolic link", dir, prefix)
		}
		if !fi.IsDir() {
			return fmt.Errorf("refusing to write path %q: %q is not a directory", dir, prefix)
		}
	}
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create path %q: %w", dir, err)
	}
	return nil
}

// readRegular returns the content of name when it is a regular file of at most limit bytes
// (a larger file cannot equal the new content).
func readRegular(root *os.Root, name string, limit int64) ([]byte, bool, error) {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to inspect %q: %w", name, err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > limit {
		return nil, false, nil
	}
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read %q: %w", name, err)
	}
	return data, true, nil
}

// replaceFile replaces the file or symbolic link at name (the link itself, never its
// target) with a new regular file (0644) holding data. A directory at name is refused and
// left untouched: it is not the hydrator's (it may hold another path's output or user
// files), so it is never deleted.
func replaceFile(root *os.Root, name string, data []byte) error {
	fi, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("failed to inspect %q: %w", name, err)
	case fi.IsDir():
		return fmt.Errorf("refusing to replace %q: it is a directory", name)
	default:
		if err := root.Remove(name); err != nil {
			return fmt.Errorf("failed to remove %q: %w", name, err)
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("failed to create %q: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to write %q: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close %q: %w", name, err)
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
