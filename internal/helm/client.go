package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"

	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Defaults applied when the corresponding Client field is zero.
const (
	// DefaultIndexTTL is how long a fetched index (or OCI tag list) is served
	// without contacting the repository (upstream revision-cache expiration).
	DefaultIndexTTL = 3 * time.Minute
	// DefaultMaxIndexBytes bounds index.yaml.
	DefaultMaxIndexBytes int64 = 100 << 20
	// DefaultMaxChartBytes bounds a downloaded chart archive.
	DefaultMaxChartBytes int64 = 512 << 20
	// DefaultMaxValuesBytes bounds one values document read by GetParameters.
	DefaultMaxValuesBytes int64 = 16 << 20
	// DefaultIndexCacheEntries bounds the in-memory index cache.
	DefaultIndexCacheEntries = 128
	// DefaultHTTPTimeout bounds one index or chart HTTP request when the context
	// has no deadline.
	DefaultHTTPTimeout = 5 * time.Minute
)

// Creds are the credentials and transport settings of one Helm repository.
// Secrets are written only to 0600 files of a per-call directory (or kept in
// memory for native HTTP/OCI access); they never appear in helm arguments.
type Creds struct {
	Username, Password string
	// CAData is a PEM CA bundle that replaces the system roots for the repository.
	CAData []byte
	// CertData and KeyData are a PEM client certificate and key.
	CertData, KeyData  []byte
	InsecureSkipVerify bool
	// PassCredentials sends the credentials to chart URLs on other hosts than
	// the repository (helm --pass-credentials).
	PassCredentials bool
	// PlainHTTP talks to an OCI registry over http:// (insecureOCIForceHttp).
	PlainHTTP bool
}

func (c Creds) fingerprint() string {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(c.Username), []byte(c.Password), c.CAData, c.CertData, c.KeyData} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{0})
		h.Write(part)
	}
	_, _ = fmt.Fprintf(h, "%t%t%t", c.InsecureSkipVerify, c.PassCredentials, c.PlainHTTP) // hash writes never fail
	return hex.EncodeToString(h.Sum(nil))
}

func (c Creds) secrets() []string {
	var out []string
	if c.Password != "" {
		out = append(out, c.Password)
	}
	return out
}

func (c Creds) transportOptions(cl *Client) oci.TransportOptions {
	return oci.TransportOptions{
		CAData:             c.CAData,
		CertData:           c.CertData,
		KeyData:            c.KeyData,
		InsecureSkipVerify: c.InsecureSkipVerify,
		Proxy:              cl.Proxy,
		NoProxy:            cl.NoProxy,
	}
}

// Client runs Helm operations. The zero value is not usable: set at least
// WorkDir. A Client is safe for concurrent use and must not be copied after
// first use.
type Client struct {
	// HelmPath is the helm binary: absolute, or a bare name looked up in PATH.
	// Empty means "helm".
	HelmPath string
	// WorkDir is the per-repository cache root. Chart archives are cached under
	// WorkDir/charts, per-call helm homes and extracted charts live under
	// WorkDir/work. It is created on demand.
	WorkDir string
	// UserAgent is sent with index and chart requests. Empty means "patrol-cd".
	UserAgent string
	// Proxy and NoProxy override the process proxy environment for native HTTP
	// requests and for helm subprocesses.
	Proxy, NoProxy string
	// PluginsDir is exposed to helm as HELM_PLUGINS. Empty means an empty
	// per-call directory, so no plugin is ever loaded.
	PluginsDir string
	// IndexTTL, MaxIndexBytes, MaxChartBytes and MaxValuesBytes override the
	// package defaults when positive.
	IndexTTL       time.Duration
	MaxIndexBytes  int64
	MaxChartBytes  int64
	MaxValuesBytes int64
	// Archive bounds chart extraction (MaxTotalBytes is upstream
	// helm.manifest.max.extracted.size; negative disables it).
	Archive sandbox.ArchiveLimits
	// Exec bounds every helm subprocess (timeout, output caps, rlimits).
	Exec sandbox.Limits
	// IndexCacheEntries overrides DefaultIndexCacheEntries when positive.
	IndexCacheEntries int

	initOnce sync.Once
	initErr  error
	indexes  *lru.Cache[string, *indexEntry]
	flight   singleflight.Group
	locks    keyedMutex
}

func (c *Client) init() error {
	c.initOnce.Do(func() {
		if c.WorkDir == "" {
			c.initErr = fmt.Errorf("%w: Client.WorkDir is required", ErrInvalidArgument)
			return
		}
		abs, err := filepath.Abs(c.WorkDir)
		if err != nil {
			c.initErr = fmt.Errorf("helm: resolve work dir: %w", err)
			return
		}
		c.WorkDir = abs
		for _, d := range []string{c.chartCacheDir(), c.workRoot()} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				c.initErr = fmt.Errorf("helm: create work dir: %w", err)
				return
			}
		}
		n := c.IndexCacheEntries
		if n <= 0 {
			n = DefaultIndexCacheEntries
		}
		c.indexes, c.initErr = lru.New[string, *indexEntry](n)
	})
	return c.initErr
}

func (c *Client) chartCacheDir() string { return filepath.Join(c.WorkDir, "charts") }
func (c *Client) workRoot() string      { return filepath.Join(c.WorkDir, "work") }

func (c *Client) helmPath() string {
	if c.HelmPath == "" {
		return "helm"
	}
	return c.HelmPath
}

func (c *Client) userAgent() string {
	if c.UserAgent == "" {
		return "patrol-cd"
	}
	return c.UserAgent
}

func (c *Client) indexTTL() time.Duration {
	if c.IndexTTL > 0 {
		return c.IndexTTL
	}
	return DefaultIndexTTL
}

func positiveOr(v, def int64) int64 {
	if v > 0 {
		return v
	}
	return def
}

// home is an isolated helm home for one call. Everything helm reads or writes
// (config, caches, registry and repository credentials, plugins) lives in it.
type home struct {
	dir string
}

func (c *Client) newHome() (*home, error) {
	dir, err := os.MkdirTemp(c.workRoot(), "home-")
	if err != nil {
		return nil, fmt.Errorf("helm: create helm home: %w", err)
	}
	h := &home{dir: dir}
	for _, d := range []string{h.configDir(), h.registryDir(), h.repoCacheDir(), h.contentCacheDir(), h.dataDir(), h.pluginsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			h.close()
			return nil, fmt.Errorf("helm: create helm home: %w", err)
		}
	}
	return h, nil
}

func (h *home) close()                  { _ = os.RemoveAll(h.dir) }
func (h *home) configDir() string       { return filepath.Join(h.dir, "config") }
func (h *home) cacheDir() string        { return filepath.Join(h.dir, "cache") }
func (h *home) dataDir() string         { return filepath.Join(h.dir, "data") }
func (h *home) registryDir() string     { return filepath.Join(h.configDir(), "registry") }
func (h *home) registryConfig() string  { return filepath.Join(h.registryDir(), "config.json") }
func (h *home) repoConfig() string      { return filepath.Join(h.configDir(), "repositories.yaml") }
func (h *home) repoCacheDir() string    { return filepath.Join(h.cacheDir(), "repository") }
func (h *home) contentCacheDir() string { return filepath.Join(h.cacheDir(), "content") }
func (h *home) pluginsDir() string      { return filepath.Join(h.dataDir(), "plugins") }

// writeSecret writes data to a new 0600 file in the home and returns its path.
func (h *home) writeSecret(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(h.dir, pattern)
	if err != nil {
		return "", fmt.Errorf("helm: create temp file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("helm: chmod temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("helm: write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("helm: write temp file: %w", err)
	}
	return f.Name(), nil
}

var proxyEnvKeys = []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"}

func (c *Client) env(h *home) []string {
	plugins := c.PluginsDir
	if plugins == "" {
		plugins = h.pluginsDir()
	}
	extra := []string{
		"HOME=" + h.dir,
		"XDG_CACHE_HOME=" + h.cacheDir(),
		"XDG_CONFIG_HOME=" + h.configDir(),
		"XDG_DATA_HOME=" + h.dataDir(),
		"HELM_CACHE_HOME=" + h.cacheDir(),
		"HELM_CONFIG_HOME=" + h.configDir(),
		"HELM_DATA_HOME=" + h.dataDir(),
		"HELM_REGISTRY_CONFIG=" + h.registryConfig(),
		"HELM_REPOSITORY_CONFIG=" + h.repoConfig(),
		"HELM_REPOSITORY_CACHE=" + h.repoCacheDir(),
		"HELM_CONTENT_CACHE=" + h.contentCacheDir(),
		"HELM_PLUGINS=" + plugins,
		"KUBECONFIG=" + filepath.Join(h.dir, "kubeconfig"),
	}
	if c.Proxy != "" {
		extra = append(extra, "HTTPS_PROXY="+c.Proxy, "HTTP_PROXY="+c.Proxy, "NO_PROXY="+c.NoProxy)
	} else {
		for _, k := range proxyEnvKeys {
			if v, ok := os.LookupEnv(k); ok {
				extra = append(extra, k+"="+v)
			}
		}
	}
	return sandbox.BaseEnv(extra...)
}

// run executes helm with args in dir using the isolated home h. It returns
// stdout and the display command line ("helm ..."). Errors use the upstream form
// "failed running helm: `<command>` failed exit status N: <stderr>".
func (c *Client) run(ctx context.Context, h *home, dir string, args []string, redact []string) ([]byte, string, error) {
	cmdline := commandLine(args)
	res, err := sandbox.Run(ctx, sandbox.Cmd{
		Path:   c.helmPath(),
		Args:   args,
		Dir:    dir,
		Env:    c.env(h),
		Limits: c.Exec,
		Redact: redact,
	})
	if err != nil {
		var ee *sandbox.ExitError
		if errors.As(err, &ee) {
			trimmed := *ee
			trimmed.StderrTail = strings.TrimSpace(strings.ReplaceAll(ee.StderrTail, h.dir, "<helm-home>"))
			return nil, cmdline, fmt.Errorf("failed running helm: `%s` failed %w", cmdline, &trimmed)
		}
		return nil, cmdline, fmt.Errorf("failed running helm: `%s`: %w", cmdline, err)
	}
	return res.Stdout, cmdline, nil
}

// commandLine formats argv the way upstream logs commands: arguments containing
// whitespace are quoted, empty ones become "".
func commandLine(args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "helm")
	for _, a := range args {
		switch {
		case a == "":
			parts = append(parts, `""`)
		case strings.IndexFunc(a, unicode.IsSpace) >= 0:
			parts = append(parts, strconv.Quote(a))
		default:
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}

var credentialArgs = regexp.MustCompile(`(--username|--password) [^ ]*`)

// RedactCommand masks --username/--password values in a helm command line
// (upstream redactor). Patrol never puts credentials on argv; this is kept for
// command strings received from elsewhere.
func RedactCommand(s string) string {
	return credentialArgs.ReplaceAllString(s, "$1 ******")
}

// keyedMutex serializes work per key.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// IsHelmOCIRepo reports whether a Helm repository URL has no scheme/host and is
// therefore an OCI registry reference (upstream IsHelmOciRepo).
func IsHelmOCIRepo(repoURL string) bool {
	if repoURL == "" {
		return false
	}
	u, err := url.Parse(repoURL)
	return err == nil && u.Host == ""
}

// redactURL removes userinfo from a URL for logs and errors.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	return u.Redacted()
}

// shared runs fn once for concurrent callers with the same key. fn runs with a
// context detached from any single caller's cancellation (bounded by
// DefaultHTTPTimeout); each caller still returns as soon as its own ctx ends.
func (c *Client) shared(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	ch := c.flight.DoChan(key, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultHTTPTimeout)
		defer cancel()
		return fn(fctx)
	})
	select {
	case r := <-ch:
		return r.Val, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
