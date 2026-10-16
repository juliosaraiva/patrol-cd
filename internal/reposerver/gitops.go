package reposerver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// ResolveRevision resolves a symbolic revision of a source (SRC-131): OCI → digest,
// Helm → chart version (both uncached), git → commit SHA (tagPrefix honored, ref cache
// unless ForceRefresh).
func (s *Service) ResolveRevision(ctx context.Context, req *apiclient.ResolveRevisionRequest) (_ *apiclient.ResolveRevisionResponse, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	src := req.Source
	if src.RepoURL == "" {
		src.RepoURL = req.Repo.URL
	}
	kind := kindOf(req.Repo, src)
	var rev string
	switch kind {
	case kindGit:
		rev, err = s.resolveGit(ctx, req.Repo, req.Revision, src.TagPrefix, !req.ForceRefresh)
	default:
		rev, err = s.resolveSource(ctx, req.Repo, src, kind, req.Revision, true)
	}
	if err != nil {
		return nil, err
	}
	return &apiclient.ResolveRevisionResponse{Revision: rev, AmbiguousRevision: fmt.Sprintf("%s (%s)", req.Revision, rev)}, nil
}

// TestRepository checks that a repository is reachable with its credentials.
func (s *Service) TestRepository(ctx context.Context, req *apiclient.TestRepositoryRequest) (_ *apiclient.TestRepositoryResponse, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	repo := req.Repo
	var check func() error
	switch repo.Type {
	case "", v1alpha1.RepositoryTypeGit:
		check = func() error {
			gc, err := s.newGitClient(repo, gitClientOpts{readRefCache: false})
			if err != nil {
				return err
			}
			start := time.Now()
			_, err = gc.LsRemote(ctx)
			s.metrics.gitOp("ls-remote", start, err)
			return err
		}
	case v1alpha1.RepositoryTypeOCI:
		check = func() error {
			c, err := s.ociClient(repo)
			if err != nil {
				return err
			}
			return c.Ping(ctx, repo.URL)
		}
	case v1alpha1.RepositoryTypeHelm:
		check = func() error {
			if repo.EnableOCI {
				if !helm.IsHelmOCIRepo(repo.URL) {
					return withCode(apiclient.CodeInvalidArgument, errors.New("OCI Helm repository URL should include hostname and port only"))
				}
				c, err := s.ociClient(repo)
				if err != nil {
					return err
				}
				return c.Ping(ctx, repo.URL)
			}
			_, err := s.helm.GetIndex(ctx, repo.URL, s.helmCreds(repo, false), true)
			return err
		}
	default:
		return nil, errorf(apiclient.CodeInvalidArgument, "unsupported repository type %q", repo.Type)
	}
	if err := check(); err != nil {
		return nil, withCode(ErrorCode(err), fmt.Errorf("error testing repository connectivity: %w", err))
	}
	return &apiclient.TestRepositoryResponse{VerifiedRepository: true}, nil
}

// ListRefs lists branches and tags (SRC-126), through the ref cache.
func (s *Service) ListRefs(ctx context.Context, req *apiclient.ListRefsRequest) (_ *apiclient.Refs, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	gc, err := s.newGitClient(req.Repo, gitClientOpts{readRefCache: true})
	if err != nil {
		return nil, fmt.Errorf("error creating git client: %w", err)
	}
	start := time.Now()
	refs, err := gc.LsRemote(ctx)
	s.metrics.gitOp("ls-remote", start, err)
	if err != nil {
		return nil, err
	}
	return &apiclient.Refs{Branches: refs.Branches, Tags: refs.Tags}, nil
}

// ListOCITags lists the tags of an OCI repository (SRC-103), cached for the revision
// cache expiration.
func (s *Service) ListOCITags(ctx context.Context, req *apiclient.ListRefsRequest) (_ *apiclient.Refs, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	key := auxKey("ocitags", repoCacheURL(req.Repo.URL, kindOCI), s.fp.of(req.Repo))
	if r, ok := getAux[apiclient.Refs](s.aux, key); ok {
		s.metrics.cache("ocitags", true)
		return r, nil
	}
	s.metrics.cache("ocitags", false)
	c, err := s.ociClient(req.Repo)
	if err != nil {
		return nil, fmt.Errorf("error creating oci client: %w", err)
	}
	tags, err := c.ListTags(ctx, req.Repo.URL)
	if err != nil {
		return nil, err
	}
	out := &apiclient.Refs{Tags: tags}
	putAux(s.aux, key, out, s.cfg.RevisionCacheExpiration)
	return out, nil
}

// ListPlugins lists the plugins served by sidecars (SRC-090).
func (s *Service) ListPlugins(context.Context) (_ *apiclient.PluginList, err error) {
	defer s.finish(&err)
	names, err := s.renderer.ListPlugins()
	if err != nil {
		return nil, err
	}
	out := &apiclient.PluginList{Items: []apiclient.PluginInfo{}}
	for _, n := range names {
		out.Items = append(out.Items, apiclient.PluginInfo{Name: n})
	}
	return out, nil
}

// ListApps discovers Helm and Kustomize applications in a repository (SRC-127).
func (s *Service) ListApps(ctx context.Context, req *apiclient.ListAppsRequest) (_ *apiclient.AppList, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	sha, err := s.resolveGit(ctx, req.Repo, req.Revision, "", true)
	if err != nil {
		return nil, fmt.Errorf("error setting up git client and resolving given revision: %w", err)
	}
	key := auxKey("ldir", repoCacheURL(req.Repo.URL, kindGit), s.fp.of(req.Repo), sha, digestJSON(req.EnabledSourceTypes), s.serverStamp)
	if l, ok := getAux[apiclient.AppList](s.aux, key); ok {
		s.metrics.cache("listapps", true)
		return l, nil
	}
	s.metrics.cache("listapps", false)
	set, err := s.holdAll(ctx, []holdSpec{{repo: req.Repo, revision: sha, readRefCache: true, submodules: !s.cfg.DisableSubmodules}})
	if err != nil {
		return nil, fmt.Errorf("error acquiring repository lock: %w", err)
	}
	defer set.release()
	apps, err := s.renderer.Discover(ctx, set.holds[0].root, set.holds[0].root, req.EnabledSourceTypes)
	if err != nil {
		return nil, fmt.Errorf("error discovering applications: %w", err)
	}
	if apps == nil {
		apps = map[string]string{}
	}
	out := &apiclient.AppList{Apps: apps}
	putAux(s.aux, key, out, s.cfg.RepoCacheExpiration)
	return out, nil
}

// GetRevisionMetadata returns commit metadata (SRC-128) and, when requested, source
// integrity and signature information. Cached results are reused only when no
// verification is requested and none was recorded.
func (s *Service) GetRevisionMetadata(ctx context.Context, req *apiclient.RevisionMetadataRequest) (_ *apiclient.RevisionMetadata, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	if !git.IsCommitSHA(req.Revision) && !git.IsTruncatedCommitSHA(req.Revision) {
		return nil, errorf(apiclient.CodeInvalidArgument, "revision %s must be resolved", req.Revision)
	}
	verify := req.SourceIntegrity != nil || req.CheckSignature
	key := auxKey("revisionmetadata", repoCacheURL(req.Repo.URL, kindGit), s.fp.of(req.Repo), req.Revision)
	if md, ok := getAux[apiclient.RevisionMetadata](s.aux, key); ok && !verify && md.SourceIntegrityResult == nil && md.SignatureInfo == "" {
		s.metrics.cache("revisionmetadata", true)
		return md, nil
	}
	s.metrics.cache("revisionmetadata", false)
	sha, err := s.resolveGit(ctx, req.Repo, req.Revision, "", true)
	if err != nil {
		return nil, err
	}
	set, err := s.holdAll(ctx, []holdSpec{{repo: req.Repo, revision: sha, readRefCache: true, submodules: !s.cfg.DisableSubmodules}})
	if err != nil {
		return nil, fmt.Errorf("error acquiring repo lock: %w", err)
	}
	defer set.release()
	gc := set.holds[0].gc
	integrity, legacy, err := s.verifyIntegrity(ctx, gc, req.SourceIntegrity, req.Repo, "", sha)
	if err != nil {
		return nil, err
	}
	if req.CheckSignature && legacy == "" && !s.cfg.DisableGPG {
		sig, err := s.signatureOf(ctx, gc, req.Repo.Depth, "", sha)
		if err != nil {
			return nil, err
		}
		legacy = legacySignatureDescription(sig)
	}
	m, err := gc.RevisionMetadata(ctx, sha)
	if err != nil {
		return nil, err
	}
	out := &apiclient.RevisionMetadata{
		Author:                m.Author,
		Date:                  m.Date,
		Tags:                  m.Tags,
		Message:               m.Message,
		SignatureInfo:         legacy,
		SourceIntegrityResult: integrity,
	}
	for _, r := range m.References {
		if r.Commit == nil {
			continue
		}
		ref := apiclient.RevisionReference{
			RepoURL: r.Commit.RepoURL, SHA: r.Commit.SHA, Author: r.Commit.Author,
			Subject: r.Commit.Subject, Body: r.Commit.Body,
		}
		if r.Commit.Date != "" {
			if t, err := time.Parse(time.RFC3339, r.Commit.Date); err == nil {
				ref.Date = t
			}
		}
		out.References = append(out.References, ref)
	}
	putAux(s.aux, key, out, s.cfg.RepoCacheExpiration)
	return out, nil
}

// gitFilesResult is the cached form of GetGitFiles.
type gitFilesResult struct {
	Map map[string][]byte `json:"map"`
}

// GetGitFiles returns the files matching a pattern (SRC-132): legacy mode lists
// tracked files with git pathspec semantics (`git ls-files`), new globbing mode uses
// doublestar over the working tree, ignoring matches whose resolved path leaves the
// repository. File reads never follow symlinks out of the repository.
func (s *Service) GetGitFiles(ctx context.Context, req *apiclient.GitFilesRequest) (_ *apiclient.GitFilesResponse, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	pattern := req.Path
	if pattern == "" {
		pattern = "."
	}
	sha, err := s.resolveGit(ctx, req.Repo, req.Revision, "", !req.NoRevisionCache)
	if err != nil {
		return nil, withCode(ErrorCode(err), fmt.Errorf("unable to resolve git revision %s: %w", req.Revision, err))
	}
	key := auxKey("gitfiles", repoCacheURL(req.Repo.URL, kindGit), s.fp.of(req.Repo), sha, pattern,
		fmt.Sprint(req.NewGitFileGlobbingEnabled, req.SubmoduleEnabled), digestJSON(req.SourceIntegrity))
	if r, ok := getAux[gitFilesResult](s.aux, key); ok {
		s.metrics.cache("gitfiles", true)
		return &apiclient.GitFilesResponse{Map: r.Map}, nil
	}
	s.metrics.cache("gitfiles", false)
	set, err := s.holdAll(ctx, []holdSpec{{repo: req.Repo, revision: sha, readRefCache: !req.NoRevisionCache, submodules: req.SubmoduleEnabled}})
	if err != nil {
		return nil, fmt.Errorf("unable to checkout git repo %s with revision %s pattern %s: %w", git.SanitizeURL(req.Repo.URL), sha, pattern, err)
	}
	defer set.release()
	h := set.holds[0]
	if err := s.requireIntegrity(ctx, h.gc, req.SourceIntegrity, req.Repo, req.Revision, sha, req.VerifyCommit); err != nil {
		return nil, err
	}
	var files []string
	if req.NewGitFileGlobbingEnabled {
		files, err = globFiles(h.root, pattern)
	} else {
		files, err = s.lsFiles(ctx, h.root, pattern)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to list files. repo %s with revision %s pattern %s: %w", git.SanitizeURL(req.Repo.URL), sha, pattern, err)
	}
	out, err := s.readRepoFiles(h.root, files)
	if err != nil {
		return nil, fmt.Errorf("unable to read files. repo %s with revision %s pattern %s: %w", git.SanitizeURL(req.Repo.URL), sha, pattern, err)
	}
	putAux(s.aux, key, gitFilesResult{Map: out}, s.cfg.RepoCacheExpiration)
	return &apiclient.GitFilesResponse{Map: out}, nil
}

// requireIntegrity fails when source integrity verification reports problems; the
// deprecated VerifyCommit additionally requires a signature when integrity applies.
func (s *Service) requireIntegrity(ctx context.Context, gc *git.Client, si *v1alpha1.SourceIntegrity, repo apiclient.Repository, unresolved, sha string, verifyCommit bool) error {
	res, _, err := s.verifyIntegrity(ctx, gc, si, repo, unresolved, sha)
	if err != nil {
		return err
	}
	if err := integrityError(res); err != nil {
		return withCode(apiclient.CodePermissionDenied, err)
	}
	if res != nil && verifyCommit && !s.cfg.DisableGPG {
		sig, err := s.signatureOf(ctx, gc, repo.Depth, "", sha)
		if err != nil {
			return err
		}
		if sig.info.Result == git.SignatureUnsigned {
			return errorf(apiclient.CodePermissionDenied, "revision %s is not signed", sha)
		}
	}
	return nil
}

// globFiles is the new globbing mode: doublestar over the real repository root;
// matches resolving outside it are ignored, directories are skipped.
func globFiles(root, pattern string) ([]string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	matches, err := doublestar.FilepathGlob(filepath.Join(realRoot, pattern))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		rel, err := filepath.Rel(realRoot, m)
		if err != nil || rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			continue
		}
		if !within(realRoot, m) {
			slog.Default().Debug("reposerver: glob match outside of the repository ignored", slog.String("file", rel))
			continue
		}
		if st, err := os.Stat(m); err != nil || !st.Mode().IsRegular() {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out, nil
}

// lsFiles runs `git ls-files --full-name -z -- <pattern>` in the working copy
// (legacy GetGitFiles mode: tracked files, git pathspec matching).
func (s *Service) lsFiles(ctx context.Context, root, pattern string) ([]string, error) {
	if strings.ContainsFunc(pattern, func(r rune) bool { return r == 0 || r == '\n' }) {
		return nil, withCode(apiclient.CodeInvalidArgument, errors.New("invalid path pattern"))
	}
	env := []string{
		"HOME=/dev/null", "LANG=C", "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(root), "GIT_DIR=" + filepath.Join(root, ".git"),
		"GIT_WORK_TREE=" + root, "GIT_OPTIONAL_LOCKS=0",
	}
	if s.cfg.ToolPATH != "" {
		env = append(env, "PATH="+s.cfg.ToolPATH)
	} else {
		env = append(env, "PATH="+os.Getenv("PATH"))
	}
	res, err := sandbox.Run(ctx, sandbox.Cmd{
		Path: s.cfg.GitPath,
		Args: []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "ls-files", "--full-name", "-z", "--", pattern},
		Dir:  root,
		Env:  env,
		Limits: sandbox.Limits{
			Timeout: s.cfg.ExecTimeout, KillGrace: s.cfg.ExecKillGrace, MaxStdout: 64 << 20, MaxStderr: 1 << 20,
		},
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range bytes.Split(res.Stdout, []byte{0}) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out, nil
}

// readRepoFiles reads repository files through an os.Root (symlinks leaving the
// repository are skipped), bounded by MaxGitFilesBytes.
func (s *Service) readRepoFiles(root string, files []string) (map[string][]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	out := make(map[string][]byte, len(files))
	var total int64
	for _, f := range files {
		st, err := r.Stat(f)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			s.log.Warn("reposerver: file outside of the repository ignored", slog.String("file", f), slog.Any("error", err))
			continue
		}
		if !st.Mode().IsRegular() {
			continue
		}
		total += st.Size()
		if total > s.cfg.MaxGitFilesBytes {
			return nil, withCode(apiclient.CodeResourceExhausted, fmt.Errorf("files exceed %d bytes", s.cfg.MaxGitFilesBytes))
		}
		data, err := r.ReadFile(f)
		if err != nil {
			return nil, err
		}
		out[f] = data
	}
	return out, nil
}

// gitDirsResult is the cached form of GetGitDirectories.
type gitDirsResult struct {
	Paths []string `json:"paths"`
}

// GetGitDirectories lists the directories of a revision (SRC-133); dot-directories are
// skipped unless IncludeHiddenDirectories (".git" always is).
func (s *Service) GetGitDirectories(ctx context.Context, req *apiclient.GitDirectoriesRequest) (_ *apiclient.GitDirectoriesResponse, err error) {
	defer s.finish(&err)
	if req == nil || req.Repo.URL == "" {
		return nil, errorf(apiclient.CodeInvalidArgument, "must pass a valid repo")
	}
	sha, err := s.resolveGit(ctx, req.Repo, req.Revision, "", !req.NoRevisionCache)
	if err != nil {
		return nil, withCode(ErrorCode(err), fmt.Errorf("unable to resolve git revision %s: %w", req.Revision, err))
	}
	key := auxKey("gitdirs", repoCacheURL(req.Repo.URL, kindGit), s.fp.of(req.Repo), sha,
		fmt.Sprint(s.cfg.IncludeHiddenDirectories, req.SubmoduleEnabled), digestJSON(req.SourceIntegrity))
	if r, ok := getAux[gitDirsResult](s.aux, key); ok {
		s.metrics.cache("gitdirs", true)
		return &apiclient.GitDirectoriesResponse{Paths: r.Paths}, nil
	}
	s.metrics.cache("gitdirs", false)
	set, err := s.holdAll(ctx, []holdSpec{{repo: req.Repo, revision: sha, readRefCache: !req.NoRevisionCache, submodules: req.SubmoduleEnabled}})
	if err != nil {
		return nil, fmt.Errorf("unable to checkout git repo %s with revision %s: %w", git.SanitizeURL(req.Repo.URL), sha, err)
	}
	defer set.release()
	h := set.holds[0]
	if err := s.requireIntegrity(ctx, h.gc, req.SourceIntegrity, req.Repo, req.Revision, sha, req.VerifyCommit); err != nil {
		return nil, err
	}
	var paths []string
	err = filepath.WalkDir(h.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error walking the file tree: %w", err)
		}
		if !d.IsDir() {
			return nil
		}
		if p != h.root && (d.Name() == ".git" || (!s.cfg.IncludeHiddenDirectories && strings.HasPrefix(d.Name(), "."))) {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(h.root, p)
		if err != nil {
			return fmt.Errorf("error constructing relative repo path: %w", err)
		}
		if rel != "." {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	putAux(s.aux, key, gitDirsResult{Paths: paths}, s.cfg.RepoCacheExpiration)
	return &apiclient.GitDirectoriesResponse{Paths: paths}, nil
}
