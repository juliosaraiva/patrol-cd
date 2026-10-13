package scm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Defaults applied by Config.WithDefaults.
const (
	DefaultMaxPages         = 500
	DefaultMaxResponseBytes = 16 << 20
	DefaultRateLimitRPS     = 10
	DefaultRateLimitBurst   = 20
	DefaultRequestTimeout   = 60 * time.Second
)

// TokenFunc returns the current API token. It is called for every request, so it must be
// cheap (e.g. read from a Secret informer cache).
type TokenFunc func(ctx context.Context) (string, error)

// GitHubAppConfig holds GitHub App credentials. Its String and LogValue methods never
// reveal the private key.
type GitHubAppConfig struct {
	AppID int64
	// InstallationID may be 0, in which case the installation is discovered for Owner.
	InstallationID int64
	// PrivateKey is the PEM-encoded RSA key (PKCS#1 or PKCS#8).
	PrivateKey []byte
	// EnterpriseBaseURL is used when Config.APIURL is empty.
	EnterpriseBaseURL string
	// Owner is the organization or user login whose installation is discovered when
	// InstallationID is 0 (the SCM generator organization, the PR generator owner, or the
	// owner parsed from a repository URL).
	Owner string
}

func (g GitHubAppConfig) String() string {
	return fmt.Sprintf("GitHubAppConfig{AppID:%d, InstallationID:%d, EnterpriseBaseURL:%q, Owner:%q, PrivateKey:<redacted>}",
		g.AppID, g.InstallationID, g.EnterpriseBaseURL, g.Owner)
}

// GoString prevents %#v from printing the key.
func (g GitHubAppConfig) GoString() string { return g.String() }

// LogValue implements slog.LogValuer.
func (g GitHubAppConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("appID", g.AppID),
		slog.Int64("installationID", g.InstallationID),
		slog.String("enterpriseBaseURL", g.EnterpriseBaseURL),
		slog.String("owner", g.Owner),
		slog.String("privateKey", "<redacted>"),
	)
}

// RateLimit configures client-side request pacing. RPS < 0 disables pacing; zero values
// select the defaults.
type RateLimit struct {
	RPS   float64
	Burst int
}

// RetryPolicy configures bounded retries of idempotent requests.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first (default 4;
	// 1 disables retries).
	MaxAttempts int
	// BaseDelay is the first backoff delay (default 500ms), doubled per attempt with full
	// jitter.
	BaseDelay time.Duration
	// MaxDelay caps a computed backoff delay (default 10s).
	MaxDelay time.Duration
	// MaxWait caps how long a provider-advised wait (Retry-After, rate-limit reset) is
	// honored inside a call (default 60s). Longer waits are surfaced to the caller as a
	// RateLimited error carrying RetryAfter instead of blocking.
	MaxWait time.Duration
}

// WithDefaults fills zero fields.
func (p RetryPolicy) WithDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 4
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 10 * time.Second
	}
	if p.MaxWait <= 0 {
		p.MaxWait = 60 * time.Second
	}
	return p
}

// Config configures a provider instance. Callers adapt the SCMProvider CRD and generator
// specs into it; secrets are resolved by the caller (Token, GitHubApp.PrivateKey).
type Config struct {
	// Type selects the registered factory ("github").
	Type string
	// Name is an instance label used in Info, errors and logs.
	Name string
	// APIURL is the API base URL; empty means the provider's public SaaS endpoint.
	APIURL string
	// Insecure skips TLS verification (discouraged; mirrors upstream `insecure`).
	Insecure bool
	// CAData is a PEM bundle trusted in addition to the system pool.
	CAData []byte
	// ProxyURL routes API requests through an HTTP(S) proxy (upstream --scm-proxy-url).
	// Empty means the process environment (HTTPS_PROXY/NO_PROXY).
	ProxyURL string
	// NoProxy is a comma-separated list of hosts, domains, IPs or CIDRs that bypass
	// ProxyURL (upstream --scm-no-proxy). Ignored when ProxyURL is empty.
	NoProxy string
	// Token supplies a bearer token (personal access token). Nil means anonymous unless
	// GitHubApp is set.
	Token TokenFunc
	// GitHubApp selects GitHub App authentication (takes precedence over Token).
	GitHubApp *GitHubAppConfig
	// HTTPClient, when set, supplies the base transport (its Transport) and timeout.
	// TLS, proxy and CA settings above are then the caller's responsibility.
	HTTPClient *http.Client
	// RateLimit configures client-side pacing.
	RateLimit RateLimit
	// Retry configures bounded retries.
	Retry RetryPolicy
	// MaxPages caps pagination per listing (default DefaultMaxPages).
	MaxPages int
	// MaxResponseBytes caps every response body (default DefaultMaxResponseBytes).
	MaxResponseBytes int64
	// Timeout bounds a single HTTP attempt when HTTPClient is nil.
	Timeout time.Duration
	// UserAgent overrides the default user agent.
	UserAgent string
	// Logger receives debug request logs (redacted). Nil discards.
	Logger *slog.Logger
	// Clock overrides time.Now (tests).
	Clock func() time.Time
}

// WithDefaults returns a copy with zero fields defaulted.
func (c Config) WithDefaults() Config {
	if c.MaxPages <= 0 {
		c.MaxPages = DefaultMaxPages
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if c.RateLimit.RPS == 0 {
		c.RateLimit.RPS = DefaultRateLimitRPS
	}
	if c.RateLimit.Burst <= 0 {
		c.RateLimit.Burst = DefaultRateLimitBurst
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultRequestTimeout
	}
	c.Retry = c.Retry.WithDefaults()
	if c.UserAgent == "" {
		c.UserAgent = "patrol-cd-scm/" + ContractVersion
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// LogValue implements slog.LogValuer without secrets.
func (c Config) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("type", c.Type),
		slog.String("name", c.Name),
		slog.String("apiURL", c.APIURL),
		slog.Bool("insecure", c.Insecure),
		slog.Bool("token", c.Token != nil),
		slog.Bool("caData", len(c.CAData) > 0),
		slog.Bool("proxy", c.ProxyURL != ""),
	}
	if c.GitHubApp != nil {
		attrs = append(attrs, slog.Any("githubApp", *c.GitHubApp))
	}
	return slog.GroupValue(attrs...)
}

// Factory builds a provider from a Config. Factories must not perform network I/O;
// authentication and discovery happen lazily on first use.
type Factory func(ctx context.Context, cfg Config) (Provider, error)

// Registry maps provider types to factories. It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{factories: map[string]Factory{}} }

// DefaultRegistry is the process-wide registry used by the package-level helpers.
var DefaultRegistry = NewRegistry()

func normalizeType(t string) string { return strings.ToLower(strings.TrimSpace(t)) }

// Register adds a factory. Registering the same type twice fails.
func (r *Registry) Register(typ string, f Factory) error {
	t := normalizeType(typ)
	if t == "" {
		return errors.New("scm: provider type must not be empty")
	}
	if f == nil {
		return fmt.Errorf("scm: nil factory for provider type %q", t)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[t]; dup {
		return fmt.Errorf("scm: provider type %q already registered", t)
	}
	r.factories[t] = f
	return nil
}

// Types lists registered types in sorted order.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for t := range r.factories {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// New builds a provider for cfg.Type and checks its declared contract version.
func (r *Registry) New(ctx context.Context, cfg Config) (Provider, error) {
	t := normalizeType(cfg.Type)
	r.mu.RLock()
	f, ok := r.factories[t]
	r.mu.RUnlock()
	if !ok {
		return nil, &Error{Kind: KindInvalid, Provider: t, Op: "New", Err: fmt.Errorf("%w %q", ErrUnknownProviderType, cfg.Type)}
	}
	p, err := f(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if v := p.Info().ContractVersion; v != ContractVersion {
		return nil, &Error{Kind: KindInvalid, Provider: t, Op: "New", Err: fmt.Errorf("provider implements contract %q, want %q", v, ContractVersion)}
	}
	return p, nil
}

// Register adds a factory to DefaultRegistry.
func Register(typ string, f Factory) error { return DefaultRegistry.Register(typ, f) }

// New builds a provider from DefaultRegistry.
func New(ctx context.Context, cfg Config) (Provider, error) { return DefaultRegistry.New(ctx, cfg) }
