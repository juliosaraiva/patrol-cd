package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Defaults for client options.
const (
	// DefaultKillGrace is the SIGTERM grace period before git is SIGKILLed on
	// timeout or cancellation (upstream sends SIGTERM to git and waits).
	DefaultKillGrace = 10 * time.Second
	// DefaultRetryBase and DefaultRetryMax are the backoff bounds used when
	// Retry is given attempts > 1 and zero durations (upstream
	// ARGOCD_GIT_RETRY_DURATION / ARGOCD_GIT_RETRY_MAX_DURATION defaults).
	DefaultRetryBase = 250 * time.Millisecond
	DefaultRetryMax  = 5 * time.Second
	// retryFactor is upstream's ARGOCD_GIT_RETRY_FACTOR default.
	retryFactor = 2
)

// Output caps for commands whose output is parsed. Exceeding a cap fails the
// command with sandbox.ErrOutputLimit.
const (
	maxSmallOutput    int64 = 1 << 20  // rev-parse, cat-file, config, tags
	maxMetadataOutput int64 = 16 << 20 // commit messages, trailers, diffs of names
	maxRefsOutput     int64 = 64 << 20 // ls-remote of very large repositories
)

// Option configures a Client.
type Option func(*options)

type options struct {
	insecure      bool
	enableLFS     bool
	proxy         string
	noProxy       string
	depth         int64
	timeout       time.Duration
	killGrace     time.Duration
	gitPath       string
	submodules    bool
	tagPrefix     string
	refCache      *RefCache
	readRefCache  bool
	gnupgHome     string
	gpgProgram    string
	attempts      int
	retryBase     time.Duration
	retryMax      time.Duration
	builtinConfig bool
	credsDir      string
	logger        *slog.Logger
	allowLocal    bool

	// Test hooks (unexported; set only from package tests).
	extraConfig []gitConfig
	runHook     func(sandbox.Cmd)
}

// Insecure disables TLS verification for HTTPS repositories (GIT_SSL_NO_VERIFY),
// in addition to any Insecure flag carried by the credentials.
func Insecure(v bool) Option { return func(o *options) { o.insecure = v } }

// EnableLFS enables Git LFS: large files are fetched after Fetch and populated by
// Checkout. Requires the git-lfs binary.
func EnableLFS(v bool) Option { return func(o *options) { o.enableLFS = v } }

// Proxy sets the HTTP(S) proxy URL for git (http_proxy / https_proxy). When empty
// the process proxy environment is passed through.
func Proxy(proxyURL string) Option { return func(o *options) { o.proxy = proxyURL } }

// NoProxy sets the proxy exclusion list (no_proxy). Effective only with Proxy.
func NoProxy(list string) Option { return func(o *options) { o.noProxy = list } }

// Depth enables shallow fetches of the given depth (0 = full history).
func Depth(n int64) Option { return func(o *options) { o.depth = n } }

// Timeout sets the wall-clock limit of each git command (default
// sandbox.DefaultTimeout).
func Timeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// KillGrace sets the SIGTERM grace period before SIGKILL on timeout or
// cancellation (default DefaultKillGrace; zero or negative kills immediately).
func KillGrace(d time.Duration) Option { return func(o *options) { o.killGrace = d } }

// GitPath sets the git executable: an absolute path, or a bare name resolved in
// the process PATH (default "git").
func GitPath(p string) Option { return func(o *options) { o.gitPath = p } }

// SubmodulesEnabled is the server-wide submodule switch (upstream
// ARGOCD_GIT_MODULES_ENABLED, default true). Checkout updates submodules only when
// this is true and its submodules argument is true.
func SubmodulesEnabled(v bool) Option { return func(o *options) { o.submodules = v } }

// TagPrefix restricts semver constraint resolution to tags with this prefix
// (Application spec.source.tagPrefix).
func TagPrefix(p string) Option { return func(o *options) { o.tagPrefix = p } }

// WithRefCache shares remote ref listings through cache. When read is false the
// cached listing is ignored but a fresh listing is still stored (upstream
// noRevisionCache semantics).
func WithRefCache(cache *RefCache, read bool) Option {
	return func(o *options) { o.refCache, o.readRefCache = cache, read }
}

// GnuPGHome makes RevisionMetadata verify signatures against the keyring in dir.
func GnuPGHome(dir string) Option { return func(o *options) { o.gnupgHome = dir } }

// GPGProgram sets git's gpg.program (default: gpg from the process PATH).
func GPGProgram(path string) Option { return func(o *options) { o.gpgProgram = path } }

// Retry retries ls-remote and fetch up to attempts times on transient transport
// errors with exponential backoff (base * 2^n, capped at maxDelay). attempts < 2
// disables retries (upstream ARGOCD_GIT_ATTEMPTS_COUNT default 1).
func Retry(attempts int, base, maxDelay time.Duration) Option {
	return func(o *options) { o.attempts, o.retryBase, o.retryMax = attempts, base, maxDelay }
}

// BuiltinGitConfig toggles the built-in configuration (maintenance.autoDetach and
// gc.autoDetach set to false), default true.
func BuiltinGitConfig(enable bool) Option { return func(o *options) { o.builtinConfig = enable } }

// CredentialsDir sets where per-command credential directories are created
// (default os.TempDir()). Point it at a memory-backed file system in production.
func CredentialsDir(dir string) Option { return func(o *options) { o.credsDir = dir } }

// AllowLocalRepositories permits file:// URLs and local paths (default false).
// Local repositories are refused by default because a tenant-controlled URL could
// otherwise read other working copies or arbitrary repositories on the server's
// file system. Enable only for development and tests.
func AllowLocalRepositories(v bool) Option { return func(o *options) { o.allowLocal = v } }

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// withGitConfig adds configuration entries to every command (tests only, for
// example protocol.file.allow=always for local submodules).
func withGitConfig(key, value string) Option {
	return func(o *options) { o.extraConfig = append(o.extraConfig, gitConfig{key, value}) }
}

// withRunHook observes every command before it runs (tests only).
func withRunHook(f func(sandbox.Cmd)) Option { return func(o *options) { o.runHook = f } }

// Client drives git for one repository URL and one working copy (root).
//
// Methods that touch the working copy are not safe for concurrent use on the
// same root; callers serialize them with RepoLock. LsRemote and ResolveRevision of
// non-truncated revisions do not touch the working copy.
type Client struct {
	repoURL   string // as given, trimmed (may contain credentials; never logged)
	remoteURL string // repoURL without password, used for git
	root      string
	creds     Creds
	opts      options
	origin    string // http(s) origin, "" for other schemes
	redact    []string
	log       *slog.Logger

	authorMu    sync.RWMutex
	authorName  string
	authorEmail string
}

// New returns a client for repoURL whose working copy lives in root (made
// absolute; it may be empty for clients that only list or resolve refs). A nil
// creds means anonymous access. A password embedded in an http(s) URL is removed
// from the URL and, when creds is NopCreds, delivered as HTTPSCreds.
func New(repoURL, root string, creds Creds, opts ...Option) (*Client, error) {
	o := options{
		submodules:    true,
		builtinConfig: true,
		killGrace:     DefaultKillGrace,
		gitPath:       "git",
	}
	for _, opt := range opts {
		opt(&o)
	}
	repoURL = strings.TrimSpace(repoURL)
	if err := validateRepoURL(repoURL); err != nil {
		return nil, err
	}
	if !o.allowLocal && isLocalURL(repoURL) {
		return nil, fmt.Errorf("%w: local repositories are not allowed", ErrInvalidURL)
	}
	if o.gitPath == "" {
		o.gitPath = "git"
	}
	if strings.ContainsRune(o.gitPath, filepath.Separator) && !filepath.IsAbs(o.gitPath) {
		return nil, fmt.Errorf("git: git path %q must be absolute or a bare name", o.gitPath)
	}
	if o.depth < 0 {
		o.depth = 0
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	if strings.ContainsFunc(o.proxy+o.noProxy, isControl) {
		return nil, fmt.Errorf("%w: proxy settings contain control characters", ErrInvalidCredentials)
	}
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("git: resolve root: %w", err)
		}
		root = abs
	}
	if creds == nil {
		creds = NopCreds{}
	}
	clean, user, password := stripURLPassword(repoURL)
	if password != "" {
		if _, anonymous := creds.(NopCreds); anonymous {
			creds = HTTPSCreds{Username: user, Password: password}
		}
	}
	c := &Client{
		repoURL:   repoURL,
		remoteURL: clean,
		root:      root,
		creds:     creds,
		opts:      o,
		origin:    httpOrigin(clean),
		log:       o.logger,
	}
	c.redact = credsRedactions(creds)
	if password != "" {
		c.redact = append(c.redact, password, url.QueryEscape(password), url.PathEscape(password))
	}
	// Longest first, so a secret containing another one is masked entirely.
	slices.SortFunc(c.redact, func(a, b string) int { return len(b) - len(a) })
	c.redact = slices.Compact(c.redact)
	return c, nil
}

// Root returns the working copy directory.
func (c *Client) Root() string { return c.root }

// RepoURL returns the repository URL with any password masked.
func (c *Client) RepoURL() string { return SanitizeURL(c.repoURL) }

// IsLFSEnabled reports whether LFS support is enabled.
func (c *Client) IsLFSEnabled() bool { return c.opts.enableLFS }

// cmdSpec describes one git invocation.
type cmdSpec struct {
	args []string
	// noRepo runs the command in a fresh private directory instead of root (for
	// commands that need no local repository, such as ls-remote).
	noRepo       bool
	stdin        io.Reader
	credentialed bool
	env          []string
	maxStdout    int64
}

// op returns the git subcommand used in errors and logs.
func (s cmdSpec) op() string {
	for _, a := range s.args {
		if !strings.HasPrefix(a, "-") {
			if a == "lfs" || a == "submodule" || a == "notes" {
				continue
			}
			return a
		}
	}
	return "git"
}

// git runs a command and returns trimmed-right stdout.
func (c *Client) git(ctx context.Context, s cmdSpec) (string, error) {
	res, err := c.run(ctx, s)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(res.Stdout), "\r\n"), nil
}

// run executes one git command in the sandbox.
func (c *Client) run(ctx context.Context, s cmdSpec) (sandbox.Result, error) {
	if !s.noRepo && c.root == "" {
		return sandbox.Result{}, fmt.Errorf("%w: client has no working copy root", ErrNotInitialized)
	}
	var credEnv []string
	var credCfg []gitConfig
	var scratch string
	if s.credentialed || s.noRepo {
		var err error
		scratch, err = os.MkdirTemp(c.opts.credsDir, "patrol-git-")
		if err != nil {
			return sandbox.Result{}, fmt.Errorf("git: create credentials directory: %w", err)
		}
		defer func() {
			if err := os.RemoveAll(scratch); err != nil {
				c.log.Warn("git: could not remove credentials directory", slog.Any("error", err))
			}
		}()
	}
	if s.credentialed {
		env, cleanup, err := c.creds.Environ(scratch)
		if err != nil {
			return sandbox.Result{}, fmt.Errorf("git: prepare credentials: %w", err)
		}
		defer cleanup()
		credEnv, credCfg = splitGitConfig(env)
		credCfg = scopeHeaders(credCfg, c.origin)
		if c.origin != "" {
			credEnv = append(credEnv, askpassOriginEnv+"="+c.origin)
		}
	}
	dir := c.root
	if s.noRepo {
		dir = scratch
	}
	env := c.baseEnv(dir)
	env = append(env, s.env...)
	env = append(env, credEnv...)
	env = appendGitConfig(env, append(c.baseConfig(), credCfg...)...)

	maxOut := s.maxStdout
	if maxOut == 0 {
		maxOut = sandbox.DefaultMaxStdout
	}
	cmd := sandbox.Cmd{
		Path:  c.opts.gitPath,
		Args:  s.args,
		Dir:   dir,
		Env:   env,
		Stdin: s.stdin,
		Limits: sandbox.Limits{
			Timeout:   c.opts.timeout,
			KillGrace: c.opts.killGrace,
			MaxStdout: maxOut,
		},
		Redact: c.redact,
	}
	if c.opts.runHook != nil {
		c.opts.runHook(cmd)
	}
	res, err := sandbox.Run(ctx, cmd)
	if err != nil {
		return res, c.commandError(s.op(), res, err)
	}
	return res, nil
}

// baseEnv is the exact environment of every git command.
func (c *Client) baseEnv(dir string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = sandbox.DefaultPath
	}
	env := []string{
		"PATH=" + path,
		"HOME=/dev/null",
		"TMPDIR=" + os.TempDir(),
		"LANG=C",
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(filepath.Clean(dir)),
	}
	if c.opts.insecure && (IsHTTPSURL(c.remoteURL) || IsHTTPURL(c.remoteURL)) {
		env = append(env, "GIT_SSL_NO_VERIFY=true")
	}
	if c.opts.proxy != "" {
		env = append(env, "http_proxy="+c.opts.proxy, "https_proxy="+c.opts.proxy, "no_proxy="+c.opts.noProxy)
	} else {
		for _, k := range []string{"http_proxy", "https_proxy", "no_proxy", "all_proxy", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	for _, k := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// baseConfig is the configuration passed to every command through
// GIT_CONFIG_COUNT/KEY/VALUE. It overrides repository and system configuration.
func (c *Client) baseConfig() []gitConfig {
	cfg := make([]gitConfig, 0, 8+len(c.opts.extraConfig))
	if c.opts.builtinConfig {
		cfg = append(cfg, gitConfig{"maintenance.autoDetach", "false"}, gitConfig{"gc.autoDetach", "false"})
	}
	cfg = append(cfg,
		gitConfig{"credential.helper", ""},
		gitConfig{"core.hooksPath", "/dev/null"},
		gitConfig{"core.fsmonitor", "false"},
		gitConfig{"protocol.ext.allow", "never"},
		gitConfig{"safe.bareRepository", "explicit"},
	)
	if c.opts.gpgProgram != "" {
		cfg = append(cfg, gitConfig{"gpg.program", c.opts.gpgProgram})
	}
	return append(cfg, c.opts.extraConfig...)
}

// splitGitConfig separates GIT_CONFIG_COUNT/KEY/VALUE entries from other
// environment entries.
func splitGitConfig(env []string) ([]string, []gitConfig) {
	keys := map[int]string{}
	values := map[int]string{}
	count := 0
	var rest []string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "GIT_CONFIG_COUNT":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				count = n
			}
		case strings.HasPrefix(k, "GIT_CONFIG_KEY_"):
			if n, err := strconv.Atoi(strings.TrimPrefix(k, "GIT_CONFIG_KEY_")); err == nil {
				keys[n] = v
			}
		case strings.HasPrefix(k, "GIT_CONFIG_VALUE_"):
			if n, err := strconv.Atoi(strings.TrimPrefix(k, "GIT_CONFIG_VALUE_")); err == nil {
				values[n] = v
			}
		default:
			rest = append(rest, kv)
		}
	}
	cfg := make([]gitConfig, 0, count)
	for i := range count {
		if k, ok := keys[i]; ok {
			cfg = append(cfg, gitConfig{k, values[i]})
		}
	}
	return rest, cfg
}

// commandError converts a sandbox error into a redacted *CommandError.
func (c *Client) commandError(op string, res sandbox.Result, err error) error {
	var msg string
	var exitErr *sandbox.ExitError
	if errors.As(err, &exitErr) {
		tail := strings.TrimSpace(exitErr.StderrTail)
		status := fmt.Sprintf("exit status %d", exitErr.Code)
		if exitErr.Signal != 0 {
			status = "terminated by signal " + exitErr.Signal.String()
		}
		if tail == "" {
			msg = status
		} else {
			msg = tail + " (" + status + ")"
		}
	} else {
		msg = err.Error()
	}
	msg = redactAll(msg, c.redact)
	stderr := strings.ToLower(redactAll(string(res.Stderr), c.redact))
	return &CommandError{Op: op, Message: msg, Kind: classifyStderr(stderr), Err: err}
}

func redactAll(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "******")
		}
	}
	return s
}

var authMarkers = []string{
	"authentication failed",
	"could not read username",
	"could not read password",
	"terminal prompts disabled",
	"permission denied (publickey",
	"host key verification failed",
	"http basic: access denied",
	"invalid username or password",
	"the requested url returned error: 401",
	"the requested url returned error: 403",
}

var notFoundMarkers = []string{
	"couldn't find remote ref",
	"invalid reference",
	"unknown revision",
	"not a valid object name",
	"bad revision",
	"not a valid commit name",
	"needed a single revision",
	"did not match any file(s) known to git",
}

func classifyStderr(lower string) error {
	for _, m := range authMarkers {
		if strings.Contains(lower, m) {
			return ErrAuthentication
		}
	}
	for _, m := range notFoundMarkers {
		if strings.Contains(lower, m) {
			return ErrRevisionNotFound
		}
	}
	return nil
}

var transientMarkers = []string{
	"connection reset",
	"early eof",
	"unexpected disconnect",
	"the remote end hung up unexpectedly",
	"the requested url returned error: 5",
	"the requested url returned error: 429",
	"operation timed out",
	"connection timed out",
	"rpc failed",
	"empty reply from server",
}

// isTransient reports whether a failed command is worth retrying.
func isTransient(err error) bool {
	var ce *CommandError
	if !errors.As(err, &ce) || errors.Is(err, ErrAuthentication) {
		return false
	}
	lower := strings.ToLower(ce.Message)
	for _, m := range transientMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// withRetry runs f up to the configured attempts while it fails transiently.
func (c *Client) withRetry(ctx context.Context, f func() error) error {
	attempts := max(c.opts.attempts, 1)
	base, maxDelay := c.opts.retryBase, c.opts.retryMax
	if base <= 0 {
		base = DefaultRetryBase
	}
	if maxDelay <= 0 {
		maxDelay = DefaultRetryMax
	}
	var err error
	for attempt := range attempts {
		if err = f(); err == nil || !isTransient(err) || attempt == attempts-1 {
			return err
		}
		delay := time.Duration(math.Min(float64(maxDelay), float64(base)*math.Pow(retryFactor, float64(attempt))))
		c.log.Debug("git: retrying after transient error", slog.Int("attempt", attempt+1), slog.Duration("delay", delay))
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
	}
	return err
}

// isRepo reports whether root holds an initialized repository.
func (c *Client) isRepo() bool {
	if c.root == "" {
		return false
	}
	fi, err := os.Lstat(filepath.Join(c.root, ".git", "HEAD"))
	return err == nil && fi.Mode().IsRegular()
}

func (c *Client) requireRepo() error {
	if !c.isRepo() {
		return fmt.Errorf("%w: %s", ErrNotInitialized, filepath.Base(c.root))
	}
	return nil
}

// lines splits command output into non-empty lines.
func lines(out string) []string {
	var res []string
	for l := range strings.SplitSeq(out, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			res = append(res, l)
		}
	}
	return res
}

// splitNUL splits NUL-terminated output.
func splitNUL(out []byte) []string {
	res := []string{}
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) > 0 {
			res = append(res, string(p))
		}
	}
	return res
}
