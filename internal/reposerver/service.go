package reposerver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/render"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Service implements the repo-server API (apiclient.Client) in process. It is safe
// for concurrent use. Handler serves it over HTTP; Run wires both into a process.
type Service struct {
	cfg  Config
	log  *slog.Logger
	root string // resolved RootDir

	renderer    *render.Renderer
	helm        *helm.Client
	refCache    *git.RefCache
	workdirs    *workdirs
	manifests   *manifestCache
	aux         *auxCache
	symlinks    *lru[symlinkResult]
	sem         *semaphore.Weighted
	metrics     *metrics
	trust       *trust
	keyring     *keyring
	fp          *credFingerprinter
	sanitizer   *pathSanitizer
	serverStamp string
	ociCacheDir string

	closeOnce    sync.Once
	shuttingDown atomic.Bool
}

var _ apiclient.Client = (*Service)(nil)

type symlinkResult struct{ err error }

// privateRootName is the subdirectory the service works in when RootDir is a mount root whose
// mode it cannot change (see prepareRoot).
const privateRootName = "_patrol-repo"

// rootFS is the part of the file system prepareRoot depends on; tests replace it to stand in for a
// volume root owned by another user, which a non-root test process cannot create.
type rootFS struct {
	chmod func(string, fs.FileMode) error
	owner func(fs.FileInfo) (uid uint32, known bool)
	euid  uint32
}

// prepareRoot creates rootDir and makes it private (0700), returning the directory the service
// works in. rootDir is normally a path the process creates itself ($TMPDIR/_patrol-repo). When it
// is the root of a volume, Kubernetes has created it owned by root and world-writable (emptyDir is
// 0777, setgid with an fsGroup), so a non-root process may use it but cannot change its mode. The
// service then works in a private subdirectory it creates (privateRootName) and leaves the mount
// root as it is.
//
// The fallback is taken only for a root dir owned by root or by this process. A directory owned by
// any other user is refused, as is a privateRootName entry that is not a real directory owned by
// this process (a symlink or a directory planted by someone else is never adopted). Anyone who can
// write the mount root can still rename the private directory, so a work volume must be mounted
// only into the repo-server container.
func prepareRoot(rootDir string, fsys rootFS, log *slog.Logger) (string, error) {
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return "", fmt.Errorf("reposerver: create root dir: %w", err)
	}
	err := fsys.chmod(rootDir, 0o700)
	if err == nil {
		return rootDir, nil
	}
	if !errors.Is(err, fs.ErrPermission) {
		return "", fmt.Errorf("reposerver: root dir permissions: %w", err)
	}
	fi, statErr := os.Stat(rootDir)
	if statErr != nil {
		return "", fmt.Errorf("reposerver: root dir permissions: %w", err)
	}
	uid, known := fsys.owner(fi)
	if !known || (uid != 0 && uid != fsys.euid) {
		return "", fmt.Errorf("reposerver: root dir %s is owned by another user and its mode cannot be changed; use a directory owned by this process: %w", rootDir, err)
	}
	sub := filepath.Join(rootDir, privateRootName)
	if err := mkdirPrivate(sub, fsys); err != nil {
		return "", fmt.Errorf("reposerver: private work dir in %s: %w", rootDir, err)
	}
	log.Info("cannot change the mode of the root dir; working in a private subdirectory",
		slog.String("rootDir", rootDir), slog.String("workDir", sub))
	return sub, nil
}

// mkdirPrivate makes dir a real directory owned by this process with mode 0700. An existing entry
// is accepted only if it already is one (the previous process of a restarted container leaves it
// locked, 0300); a symlink or a directory of another owner is refused.
func mkdirPrivate(dir string, fsys rootFS) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory (%s)", dir, fi.Mode().Type())
	}
	if uid, known := fsys.owner(fi); !known || uid != fsys.euid {
		return fmt.Errorf("%s exists and is not owned by this process", dir)
	}
	return fsys.chmod(dir, 0o700)
}

// New prepares the working directory root and returns a Service. Directories left by a
// previous process under the root are removed (caches are disposable).
func New(cfg Config) (*Service, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	log := cfg.Logger.With(slog.String("component", "reposerver"))
	fsys := defaultRootFS()
	if cfg.rootFS != nil {
		fsys = *cfg.rootFS
	}
	workDir, err := prepareRoot(cfg.RootDir, fsys, log)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, fmt.Errorf("reposerver: resolve root dir: %w", err)
	}
	cleanStale(root, log)
	helmDir := filepath.Join(root, "helm")
	renderDir := filepath.Join(root, "render")
	ociCache := filepath.Join(root, "oci-cache")
	for _, d := range []string{helmDir, renderDir, ociCache} {
		if err := os.RemoveAll(d); err != nil {
			return nil, fmt.Errorf("reposerver: reset %s: %w", filepath.Base(d), err)
		}
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, fmt.Errorf("reposerver: create %s: %w", filepath.Base(d), err)
		}
	}
	// The root may be traversed but not listed (upstream 0300): entries are only
	// reachable through their random names.
	if err := os.Chmod(root, 0o300); err != nil {
		return nil, fmt.Errorf("reposerver: root dir permissions: %w", err)
	}

	fp, err := newCredFingerprinter()
	if err != nil {
		return nil, fmt.Errorf("reposerver: %w", err)
	}
	s := &Service{
		cfg:         cfg,
		log:         log,
		root:        root,
		refCache:    git.NewRefCache(cfg.RevisionCacheExpiration, 1024),
		workdirs:    newWorkdirs(root, cfg.MaxWorkingCopies, cfg.Now, log),
		manifests:   newManifestCache(cfg.ManifestCacheEntries, cfg.ManifestCacheBytes, cfg.RepoCacheExpiration, cfg.Now),
		aux:         newAuxCache(cfg.AuxCacheEntries, cfg.AuxCacheBytes, cfg.Now),
		symlinks:    newLRU[symlinkResult](8192, 0, cfg.Now),
		fp:          fp,
		sanitizer:   newPathSanitizer(cfg.RootDir, root),
		ociCacheDir: ociCache,
		trust:       &trust{knownHostsPath: cfg.SSHKnownHostsPath, tlsCertsDir: cfg.TLSCertsDir, log: log},
	}
	gnupgParent := cfg.GnuPGHomeParent
	if gnupgParent == "" {
		gnupgParent = os.TempDir()
	}
	s.keyring = &keyring{srcDir: cfg.GPGKeysDir, parent: gnupgParent, gpgPath: cfg.GPGPath, path: cfg.ToolPATH, timeout: cfg.ExecTimeout, log: log}
	if cfg.ParallelismLimit > 0 {
		s.sem = semaphore.NewWeighted(int64(cfg.ParallelismLimit))
	}
	s.helm = &helm.Client{
		HelmPath:   cfg.HelmPath,
		WorkDir:    helmDir,
		UserAgent:  cfg.HelmUserAgent,
		PluginsDir: cfg.HelmPluginsDir,
		IndexTTL:   cfg.RevisionCacheExpiration,
		Archive:    cfg.helmExtractLimits(),
		Exec:       sandbox.Limits{Timeout: cfg.ExecTimeout},
	}
	limits := render.Limits{MaxCombinedDirectoryManifestsSize: cfg.MaxCombinedDirectoryManifestsSize}
	s.renderer, err = render.New(render.Config{
		Helm:                           s.helm,
		KustomizePath:                  cfg.KustomizePath,
		HelmPath:                       cfg.HelmPath,
		GitPath:                        cfg.GitPath,
		ToolPATH:                       cfg.ToolPATH,
		WorkDir:                        renderDir,
		Exec:                           sandbox.Limits{Timeout: cfg.ExecTimeout},
		Jsonnet:                        cfg.Jsonnet,
		PluginSocketDir:                cfg.PluginSocketDir,
		PluginTarExclusions:            cfg.PluginTarExclusions,
		PluginUseManifestGeneratePaths: cfg.PluginUseManifestGeneratePaths,
		CAData:                         s.trust.CAData,
		Limits:                         limits,
		Registerer:                     cfg.Registerer,
		Logger:                         cfg.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("reposerver: %w", err)
	}
	s.serverStamp = digestJSON(struct {
		Tools          map[string]string
		Exclusions     []string
		UseGenPaths    bool
		MaxDirManifest int64
		AllowOOB       bool
		OCIMediaTypes  []string
	}{
		Tools:          toolStamps(cfg),
		Exclusions:     cfg.PluginTarExclusions,
		UseGenPaths:    cfg.PluginUseManifestGeneratePaths,
		MaxDirManifest: cfg.MaxCombinedDirectoryManifestsSize,
		AllowOOB:       cfg.AllowOutOfBoundsSymlinks,
		OCIMediaTypes:  cfg.OCILayerMediaTypes,
	})
	if s.metrics, err = newMetrics(cfg.Registerer, s); err != nil {
		return nil, fmt.Errorf("reposerver: register metrics: %w", err)
	}
	return s, nil
}

// Close releases process resources (GnuPG home). Working copies stay on disk and are
// removed by the next New on the same root.
func (s *Service) Close() error {
	s.closeOnce.Do(func() { s.keyring.close() })
	return nil
}

// toolStamps identifies the tool binaries (path, size, modification time) so that a
// tool upgrade invalidates cached manifests.
func toolStamps(cfg Config) map[string]string {
	out := map[string]string{}
	for name, p := range map[string]string{"helm": cfg.HelmPath, "kustomize": cfg.KustomizePath, "git": cfg.GitPath} {
		resolved := lookPath(p, cfg.ToolPATH)
		stamp := resolved
		if st, err := os.Stat(resolved); err == nil {
			stamp += "|" + st.ModTime().UTC().Format(time.RFC3339Nano) + "|" + fmt.Sprint(st.Size())
		}
		out[name] = stamp
	}
	return out
}

// lookPath resolves a bare tool name in pathList (or the process PATH).
func lookPath(p, pathList string) string {
	if p == "" || filepath.IsAbs(p) || strings.ContainsRune(p, os.PathSeparator) {
		return p
	}
	if pathList == "" {
		pathList = os.Getenv("PATH")
	}
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		c := filepath.Join(dir, p)
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return c
		}
	}
	return p
}

// gitClientOpts are per-client options.
type gitClientOpts struct {
	root         string
	tagPrefix    string
	readRefCache bool
}

// newGitClient returns a git client for repo (list-only when root is empty).
func (s *Service) newGitClient(repo apiclient.Repository, o gitClientOpts) (*git.Client, error) {
	isSSH, _ := git.IsSSHURL(repo.URL)
	opts := []git.Option{
		git.WithLogger(s.log),
		git.Insecure(repo.Insecure),
		git.EnableLFS(repo.EnableLFS),
		git.Depth(repo.Depth),
		git.Timeout(s.cfg.ExecTimeout),
		git.KillGrace(s.cfg.ExecKillGrace),
		git.GitPath(s.cfg.GitPath),
		git.BuiltinGitConfig(!s.cfg.DisableBuiltinGitConfig),
		git.AllowLocalRepositories(s.cfg.AllowLocalRepositories),
		git.SubmodulesEnabled(true),
		git.WithRefCache(s.refCache, o.readRefCache),
	}
	if o.tagPrefix != "" {
		opts = append(opts, git.TagPrefix(o.tagPrefix))
	}
	if s.cfg.CredentialsDir != "" {
		opts = append(opts, git.CredentialsDir(s.cfg.CredentialsDir))
	}
	if filepath.IsAbs(s.cfg.GPGPath) {
		opts = append(opts, git.GPGProgram(s.cfg.GPGPath))
	}
	if s.cfg.GitRetryAttempts > 1 {
		opts = append(opts, git.Retry(s.cfg.GitRetryAttempts, s.cfg.GitRetryBase, s.cfg.GitRetryMax))
	}
	if !isSSH && repo.Proxy != "" {
		opts = append(opts, git.Proxy(repo.Proxy), git.NoProxy(repo.NoProxy))
	}
	return git.New(repo.URL, o.root, s.gitCreds(repo), opts...)
}

// resolveGit resolves a git revision to a full commit SHA (remote listing through the
// ref cache, truncated SHAs expanded in the working copy under an exclusive lock).
func (s *Service) resolveGit(ctx context.Context, repo apiclient.Repository, revision, tagPrefix string, readCache bool) (string, error) {
	gc, err := s.newGitClient(repo, gitClientOpts{tagPrefix: tagPrefix, readRefCache: readCache})
	if err != nil {
		return "", err
	}
	if git.IsCommitSHA(revision) {
		return revision, nil
	}
	start := time.Now()
	sha, err := gc.ResolveRevision(ctx, revision)
	s.metrics.gitOp("ls-remote", start, err)
	if err == nil || !errors.Is(err, git.ErrNotInitialized) {
		return sha, err
	}
	// A truncated SHA must be expanded locally (initialize and fetch when needed).
	hold, err := s.holdWorkingCopy(ctx, repo, gitClientOpts{tagPrefix: tagPrefix, readRefCache: readCache}, "\x00resolve:"+revision, false, nil)
	if err != nil {
		return "", err
	}
	defer hold.release()
	return hold.gc.ResolveRevision(ctx, revision)
}

// wcHold is a working copy held under the repository lock.
type wcHold struct {
	gc      *git.Client
	root    string
	release func()
}

// prepareFunc prepares a working copy once the repository lock is held (clean reports
// that the previous holder was exclusive or used another revision).
type prepareFunc func(ctx context.Context, gc *git.Client, clean bool) error

// holdWorkingCopy acquires the working copy of repo for revision under the repository
// lock, running prepare (nil: no checkout) when the lock is initialized.
func (s *Service) holdWorkingCopy(ctx context.Context, repo apiclient.Repository, o gitClientOpts, revision string, allowConcurrent bool, prepare prepareFunc) (*wcHold, error) {
	dir, repoLock, releaseDir, err := s.workdirs.acquire(repo, s.fp.of(repo))
	if err != nil {
		return nil, err
	}
	o.root = dir
	gc, err := s.newGitClient(repo, o)
	if err != nil {
		releaseDir()
		return nil, err
	}
	closer, err := repoLock.Lock(ctx, dir, revision, allowConcurrent, func(clean bool) (io.Closer, error) {
		pc := s.workdirs.permissionInit(dir)
		if prepare != nil {
			if err := prepare(ctx, gc, clean); err != nil {
				_ = pc.Close()
				return nil, err
			}
		}
		return pc, nil
	})
	if err != nil {
		releaseDir()
		return nil, err
	}
	var once sync.Once
	return &wcHold{gc: gc, root: dir, release: func() {
		once.Do(func() {
			if err := closer.Close(); err != nil {
				s.log.Warn("reposerver: releasing repository lock failed", slog.Any("error", err))
			}
			releaseDir()
		})
	}}, nil
}

// checkoutPrepare returns the upstream checkout strategy for sha: skip when the
// working copy already has it checked out cleanly, fetch only when the commit is
// missing (the revision itself for shallow repositories, the default refspec
// otherwise), check out, and on failure fetch the revision explicitly and check out
// FETCH_HEAD.
func (s *Service) checkoutPrepare(depth int64, sha string, submodules bool) prepareFunc {
	return func(ctx context.Context, gc *git.Client, clean bool) error {
		start := time.Now()
		err := s.checkout(ctx, gc, depth, sha, submodules, clean)
		s.metrics.gitOp("checkout", start, err)
		return err
	}
}

func (s *Service) checkout(ctx context.Context, gc *git.Client, depth int64, sha string, submodules, clean bool) error {
	if err := gc.Init(ctx); err != nil {
		return fmt.Errorf("Failed to initialize git repo: %w", err) //nolint:staticcheck // upstream message
	}
	if !clean {
		if head, err := gc.CommitSHA(ctx); err == nil && head == sha {
			return nil
		}
	}
	if !gc.IsRevisionPresent(ctx, sha) {
		rev := ""
		if depth > 0 {
			rev = sha
		}
		if err := s.fetch(ctx, gc, rev); err != nil {
			return fmt.Errorf("Failed to fetch default: %w", err) //nolint:staticcheck // upstream message
		}
	}
	if err := gc.Checkout(ctx, sha, submodules); err != nil {
		s.log.Info("reposerver: checkout failed; fetching the revision explicitly", slog.String("revision", sha), slog.Any("error", err))
		if err := s.fetch(ctx, gc, sha); err != nil {
			return fmt.Errorf("Failed to checkout revision %s: %w", sha, err) //nolint:staticcheck // upstream message
		}
		if err := gc.Checkout(ctx, "FETCH_HEAD", submodules); err != nil {
			return fmt.Errorf("Failed to checkout FETCH_HEAD: %w", err) //nolint:staticcheck // upstream message
		}
	}
	return nil
}

func (s *Service) fetch(ctx context.Context, gc *git.Client, rev string) error {
	start := time.Now()
	err := gc.Fetch(ctx, rev)
	s.metrics.gitOp("fetch", start, err)
	return err
}

// ensureFetched makes the given commits present (upstream fetch helper used by
// UpdateRevisionForPaths).
func (s *Service) ensureFetched(ctx context.Context, gc *git.Client, depth int64, shas ...string) error {
	missing := slices.DeleteFunc(slices.Clone(shas), func(sha string) bool { return gc.IsRevisionPresent(ctx, sha) })
	if len(missing) == 0 {
		return nil
	}
	if depth > 0 {
		for _, sha := range missing {
			if err := s.fetch(ctx, gc, sha); err != nil {
				return fmt.Errorf("Failed to fetch revision %s: %w", sha, err) //nolint:staticcheck // upstream message
			}
		}
		return nil
	}
	if err := s.fetch(ctx, gc, ""); err != nil {
		return err
	}
	for _, sha := range missing {
		if !gc.IsRevisionPresent(ctx, sha) {
			if err := s.fetch(ctx, gc, sha); err != nil {
				return fmt.Errorf("Failed to fetch revision %s: %w", sha, err) //nolint:staticcheck // upstream message
			}
		}
	}
	return nil
}

// checkSymlinks rejects trees with out-of-bounds symlinks (SRC-117), memoized per root
// and revision for 12 hours unless noCache.
func (s *Service) checkSymlinks(ctx context.Context, root, revision string, noCache bool, what string, skip ...string) error {
	if s.cfg.AllowOutOfBoundsSymlinks {
		return nil
	}
	key := root + "\x00" + revision + "\x00" + strings.Join(skip, ",")
	if !noCache {
		if r, ok := s.symlinks.Get(key); ok {
			return r.err
		}
	}
	err := sandbox.CheckSymlinksContext(ctx, root, skip...)
	if ctx.Err() != nil {
		return err
	}
	var oob *sandbox.OutOfBoundsSymlinkError
	if errors.As(err, &oob) {
		s.log.Warn("reposerver: out-of-bounds symlink rejected", slog.String("security", "high"),
			slog.String("tree", what), slog.String("revision", revision), slog.String("file", oob.Path))
		err = withCode(apiclient.CodeInvalidArgument, fmt.Errorf("%s contains out-of-bounds symlinks. file: %s", what, oob.Path))
	}
	s.symlinks.Add(key, symlinkResult{err: err}, 0, symlinkMemoTTL)
	return err
}

// appDir resolves an application path inside root with upstream messages.
func appDir(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", errorf(apiclient.CodeInvalidArgument, "%s: app path is absolute", rel)
	}
	p, err := sandbox.SecureJoin(root, rel)
	if err != nil {
		return "", errorf(apiclient.CodeInvalidArgument, "%s: app path outside root", rel)
	}
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", errorf(apiclient.CodeInvalidArgument, "%s: %s", rel, render.ErrMessageAppPathDoesNotExist)
	}
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errorf(apiclient.CodeInvalidArgument, "%s: app path is not a directory", rel)
	}
	return p, nil
}

// repoCacheURL is the repository part of cache keys: RepoKey for git (never merges
// locations a server may distinguish), the trimmed URL otherwise.
func repoCacheURL(url string, kind sourceKind) string {
	if kind == kindGit {
		return git.RepoKey(url)
	}
	return strings.TrimSuffix(strings.TrimSpace(url), "/")
}

// helmEnableOCI reports whether a Helm repository is an OCI registry.
func helmEnableOCI(repo apiclient.Repository) bool {
	return repo.EnableOCI || repo.Type == v1alpha1.RepositoryTypeOCI || helm.IsHelmOCIRepo(repo.URL)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// finish sanitizes a returned error.
func (s *Service) finish(err *error) {
	if *err != nil {
		*err = s.sanitizer.sanitize(*err)
	}
}
