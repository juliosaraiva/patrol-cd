package commitserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// Retry bounds for pushes rejected because another writer updated the branch.
const (
	DefaultMaxAttempts = 5
	retryInitialDelay  = 100 * time.Millisecond
	retryMaxDelay      = 2 * time.Second
)

// ServiceOptions configure a Service. The zero value is usable.
type ServiceOptions struct {
	// WorkDir is where per-request clones are created and removed (default os.TempDir()).
	// Point it at an ephemeral volume.
	WorkDir string
	// CredentialsDir is where internal/git creates the per-command credential files (default
	// os.TempDir()). Point it at a memory-backed file system.
	CredentialsDir string
	// GitPath is the git executable (default "git" from PATH).
	GitPath string
	// GitTimeout bounds each git command (default: internal/git default).
	GitTimeout time.Duration
	// SSHKnownHosts is the ssh_known_hosts data used for SSH repositories.
	SSHKnownHosts []byte
	// TLSCertificates returns the PEM CA bundle trusted for an HTTPS server name, or nil
	// for the system roots.
	TLSCertificates func(serverName string) []byte
	// MaxAttempts bounds the attempts of one request when pushes are rejected because the
	// branch or the notes ref moved (default DefaultMaxAttempts).
	MaxAttempts int
	// MaxConcurrentRequests bounds the requests processed at once (0: unlimited).
	MaxConcurrentRequests int
	// AllowLocalRepositories permits file:// and local-path repositories (tests and
	// development only; see internal/git).
	AllowLocalRepositories bool
	// Metrics records request and git metrics (nil: none).
	Metrics *Metrics
	// Logger is the service logger (default slog.Default()).
	Logger *slog.Logger
}

// Service implements Client by writing to git.
type Service struct {
	opts  ServiceOptions
	log   *slog.Logger
	locks keyedMutex
	sem   chan struct{}

	// beforePush is a test hook run before every branch push.
	beforePush func(ctx context.Context, attempt int)
}

var _ Client = (*Service)(nil)

// NewService returns a commit service.
func NewService(opts ServiceOptions) *Service {
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	s := &Service{opts: opts, log: opts.Logger}
	if s.log == nil {
		s.log = slog.Default()
	}
	if opts.MaxConcurrentRequests > 0 {
		s.sem = make(chan struct{}, opts.MaxConcurrentRequests)
	}
	return s
}

// commitNote is the git note attached to hydrated commits. drySha is upstream's field;
// manifestsDigest (Patrol) identifies the written manifest content, so a request with the
// same dry SHA but different manifests (changed spec, added path) is not skipped.
type commitNote struct {
	DrySHA          string `json:"drySha"`
	ManifestsDigest string `json:"manifestsDigest,omitempty"`
}

// CommitHydratedManifests writes the request to git and returns the hydrated commit.
func (s *Service) CommitHydratedManifests(ctx context.Context, req *CommitHydratedManifestsRequest) (*CommitHydratedManifestsResponse, error) {
	start := time.Now()
	repoLabel := ""
	if req != nil {
		repoLabel = git.SanitizeURL(req.Repo.URL)
	}
	s.opts.Metrics.incPending(repoLabel)
	defer s.opts.Metrics.decPending(repoLabel)

	log := s.log.With(slog.Any("request", req))
	sha, err := s.handle(ctx, log, req)
	if err != nil {
		s.opts.Metrics.observeCommit(repoLabel, ResponseTypeFailure, time.Since(start))
		log.Error("commit-server: failed to handle commit request", slog.Any("error", err))
		return nil, err
	}
	s.opts.Metrics.observeCommit(repoLabel, ResponseTypeSuccess, time.Since(start))
	log.Info("commit-server: handled commit request", slog.String("hydratedSha", sha), slog.Duration("duration", time.Since(start)))
	return &CommitHydratedManifestsResponse{HydratedSHA: sha}, nil
}

func (s *Service) handle(ctx context.Context, log *slog.Logger, req *CommitHydratedManifestsRequest) (string, error) {
	p, err := prepare(req)
	if err != nil {
		return "", err
	}
	unlock, err := s.locks.lock(ctx, git.RepoKey(req.Repo.URL))
	if err != nil {
		return "", fmt.Errorf("waiting for the repository lock: %w", err)
	}
	defer unlock()
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for a request slot: %w", ctx.Err())
		}
	}

	dryRepoURL := req.DryRepoURL
	if dryRepoURL == "" {
		dryRepoURL = req.Repo.URL
	}
	rootMD := NewCommitMetadata(log, stripCredentials(dryRepoURL), req.DrySHA, req.DryCommitMetadata)
	dir, err := os.MkdirTemp(s.opts.WorkDir, "patrol-commit-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Error("commit-server: failed to remove temp dir", slog.Any("error", err))
		}
	}()

	delay := retryInitialDelay
	for attempt := 1; ; attempt++ {
		workDir := filepath.Join(dir, strconv.Itoa(attempt))
		sha, err := s.attempt(ctx, log, workDir, req, p, rootMD, attempt)
		if err == nil {
			return sha, nil
		}
		if !errors.Is(err, ErrConcurrentUpdate) || ctx.Err() != nil {
			return "", err
		}
		if attempt >= s.opts.MaxAttempts {
			return "", fmt.Errorf("giving up after %d attempts: %w", attempt, err)
		}
		_ = os.RemoveAll(workDir)
		wait := delay/2 + rand.N(delay/2+1)
		log.Info("commit-server: branch updated concurrently, retrying on a fresh clone",
			slog.Int("attempt", attempt), slog.Duration("backoff", wait), slog.Any("error", err))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", errors.Join(err, ctx.Err())
		case <-t.C:
		}
		delay = min(2*delay, retryMaxDelay)
	}
}

// attempt performs one clone-write-commit-push cycle in dir.
func (s *Service) attempt(ctx context.Context, log *slog.Logger, dir string, req *CommitHydratedManifestsRequest, p *plan, rootMD HydratorCommitMetadata, attempt int) (string, error) {
	repoLabel := git.SanitizeURL(req.Repo.URL)
	gc, err := s.newGitClient(req.Repo, dir)
	if err != nil {
		return "", fmt.Errorf("failed to create git client: %w", err)
	}
	if err := gc.SetAuthor(p.author.Name, p.author.Email); err != nil {
		return "", invalidf("failed to set author: %v", err)
	}
	if err := gc.Init(ctx); err != nil {
		return "", fmt.Errorf("failed to init git client: %w", err)
	}
	start := time.Now()
	err = gc.Fetch(ctx, "")
	s.opts.Metrics.observeGit(repoLabel, GitRequestTypeFetch, time.Since(start))
	if err != nil {
		return "", fmt.Errorf("failed to clone repo: %w", err)
	}

	targetExists := req.TargetBranch == req.SyncBranch ||
		gc.IsRevisionPresent(ctx, "refs/remotes/origin/"+req.TargetBranch)
	if err := gc.CheckoutOrOrphan(ctx, req.SyncBranch); err != nil {
		return "", classify(fmt.Errorf("failed to checkout sync branch: %w", err))
	}
	if req.TargetBranch != req.SyncBranch {
		if err := gc.CheckoutOrNew(ctx, req.TargetBranch, req.SyncBranch); err != nil {
			return "", fmt.Errorf("failed to checkout target branch: %w", err)
		}
	}
	head, err := gc.CommitSHA(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get commit SHA: %w", err)
	}

	push := func() error {
		if s.beforePush != nil {
			s.beforePush(ctx, attempt)
		}
		start := time.Now()
		err := gc.Push(ctx, req.TargetBranch, false)
		s.opts.Metrics.observeGit(repoLabel, GitRequestTypePush, time.Since(start))
		if err != nil {
			return classify(fmt.Errorf("failed to push: %w", err))
		}
		return nil
	}

	hydrated, err := s.isHydrated(ctx, log, gc, head, req.DrySHA, p.digest)
	if err != nil {
		return "", err
	}
	if hydrated {
		log.Debug("commit-server: dry SHA already hydrated", slog.String("head", head))
		if !targetExists {
			if err := push(); err != nil {
				return "", err
			}
		}
		return head, nil
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("failed to open root dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	changed, err := writeTree(root, log, p, rootMD)
	if err != nil {
		return "", fmt.Errorf("failed to write manifests: %w", err)
	}

	sha := head
	if changed {
		sha, err = gc.CommitAll(ctx, req.CommitMessage, false)
		switch {
		case errors.Is(err, git.ErrNothingToCommit):
			changed, sha = false, head
		case err != nil:
			return "", fmt.Errorf("failed to commit: %w", err)
		}
	}
	if changed || !targetExists {
		if err := push(); err != nil {
			return "", err
		}
	}
	note, err := json.Marshal(commitNote{DrySHA: req.DrySHA, ManifestsDigest: p.digest})
	if err != nil {
		return "", fmt.Errorf("failed to marshal commit note: %w", err)
	}
	start = time.Now()
	err = gc.AddNote(ctx, sha, NoteNamespace, string(note))
	s.opts.Metrics.observeGit(repoLabel, GitRequestTypePush, time.Since(start))
	if err != nil {
		return "", classify(fmt.Errorf("failed to add commit note: %w", err))
	}
	if !changed {
		log.Info("commit-server: manifests unchanged, no commit created", slog.String("head", sha))
	}
	return sha, nil
}

// isHydrated reports whether head's note records the request's dry SHA and manifests digest.
// A missing or malformed note means "not hydrated" (a malformed note is overwritten).
func (s *Service) isHydrated(ctx context.Context, log *slog.Logger, gc *git.Client, head, drySHA, digest string) (bool, error) {
	raw, err := gc.GetNote(ctx, head, NoteNamespace)
	if errors.Is(err, git.ErrNoteNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get notes from git: %w", err)
	}
	var n commitNote
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		log.Warn("commit-server: ignoring malformed hydrator note", slog.String("commit", head), slog.Any("error", err))
		return false, nil
	}
	return n.DrySHA == drySHA && n.ManifestsDigest == digest, nil
}

func (s *Service) newGitClient(repo apiclient.Repository, dir string) (*git.Client, error) {
	isSSH, _ := git.IsSSHURL(repo.URL)
	opts := []git.Option{
		git.WithLogger(s.log),
		git.Insecure(repo.Insecure),
		git.SubmodulesEnabled(false),
		git.AllowLocalRepositories(s.opts.AllowLocalRepositories),
	}
	if s.opts.GitTimeout > 0 {
		opts = append(opts, git.Timeout(s.opts.GitTimeout))
	}
	if s.opts.CredentialsDir != "" {
		opts = append(opts, git.CredentialsDir(s.opts.CredentialsDir))
	}
	if s.opts.GitPath != "" {
		opts = append(opts, git.GitPath(s.opts.GitPath))
	}
	if !isSSH && repo.Proxy != "" {
		opts = append(opts, git.Proxy(repo.Proxy), git.NoProxy(repo.NoProxy))
	}
	return git.New(repo.URL, dir, s.creds(repo, isSSH), opts...)
}

// creds maps resolved write access to git credentials. Secrets stay in memory; internal/git
// writes them to per-command temporary files only.
func (s *Service) creds(r apiclient.Repository, isSSH bool) git.Creds {
	if isSSH {
		if r.SSHPrivateKey == "" {
			return git.NopCreds{}
		}
		return git.SSHCreds{
			PrivateKey:            r.SSHPrivateKey,
			InsecureIgnoreHostKey: r.Insecure || r.InsecureIgnoreHostKey,
			KnownHosts:            s.opts.SSHKnownHosts,
			Proxy:                 r.Proxy,
		}
	}
	var ca []byte
	if s.opts.TLSCertificates != nil {
		if u, err := url.Parse(r.URL); err == nil && u.Hostname() != "" {
			ca = s.opts.TLSCertificates(u.Hostname())
		}
	}
	if !r.HasCredentials() && len(ca) == 0 {
		return git.NopCreds{}
	}
	return git.HTTPSCreds{
		Username:       r.Username,
		Password:       r.Password,
		BearerToken:    r.BearerToken,
		ClientCertData: r.TLSClientCertData,
		ClientCertKey:  r.TLSClientCertKey,
		CAData:         ca,
		Insecure:       r.Insecure,
		ForceBasicAuth: r.ForceHTTPBasicAuth,
	}
}

// classify marks errors caused by another writer updating the branch or the notes ref.
func classify(err error) error {
	if err == nil || !isConcurrentUpdate(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrConcurrentUpdate, err)
}

func isConcurrentUpdate(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, m := range []string{
		"fetch first", "non-fast-forward", "cannot lock ref", "failed to update ref",
		"incorrect old value", "reference already exists", "stale info",
	} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// stripCredentials removes a password from an URL written into git.
func stripCredentials(raw string) string {
	if ok, _ := git.IsSSHURL(raw); ok && !strings.HasPrefix(strings.ToLower(raw), "ssh://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, has := u.User.Password(); !has {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

// keyedMutex serializes work per key; waiting honors the context.
type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	slot chan struct{}
	refs int
}

func (k *keyedMutex) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = map[string]*lockEntry{}
	}
	e := k.entries[key]
	if e == nil {
		e = &lockEntry{slot: make(chan struct{}, 1)}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	select {
	case e.slot <- struct{}{}:
	case <-ctx.Done():
		k.release(key, e)
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-e.slot
			k.release(key, e)
		})
	}, nil
}

func (k *keyedMutex) release(key string, e *lockEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(k.entries, key)
	}
}
