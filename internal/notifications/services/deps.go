package services

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net"
	"net/url"
	"time"
)

// Defaults for outbound delivery.
const (
	DefaultRequestTimeout   = 30 * time.Second
	DefaultMaxResponseBytes = 1 << 20 // 1 MiB
	DefaultRetryMax         = 3
	DefaultRetryWaitMin     = time.Second
	DefaultRetryWaitMax     = 5 * time.Second
	// maxRetryAfter bounds a server supplied Retry-After so a hostile or broken endpoint
	// cannot park a delivery worker for long.
	maxRetryAfter = 30 * time.Second
	// maxRetries is the hard upper bound for any configured retry count.
	maxRetries = 10
)

// RetryPolicy bounds retries of transient failures (network errors, HTTP 408/429/5xx).
type RetryPolicy struct {
	// MaxRetries is the number of retries after the first attempt (0 disables retries).
	MaxRetries int
	// WaitMin and WaitMax bound the exponential backoff between attempts.
	WaitMin time.Duration
	WaitMax time.Duration
}

// DefaultRetryPolicy returns the policy used when none is configured: 3 retries waiting
// between 1s and 5s, matching the webhook service defaults of the baseline.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: DefaultRetryMax, WaitMin: DefaultRetryWaitMin, WaitMax: DefaultRetryWaitMax}
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	}
	if p.MaxRetries > maxRetries {
		p.MaxRetries = maxRetries
	}
	if p.WaitMin < 0 {
		p.WaitMin = 0
	}
	if p.WaitMax < p.WaitMin {
		p.WaitMax = p.WaitMin
	}
	return p
}

// Network configures outbound connectivity shared by all services. The zero value is
// usable: direct dialing, system roots, every http/https URL allowed.
type Network struct {
	// DialContext replaces the default dialer for every outbound TCP connection (HTTP,
	// SMTP, NATS). Use NewGuardedDialer to block private, loopback and link-local targets.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// RootCAs returns the trust roots for a TLS server name. Returning (nil, nil) uses the
	// system roots. This is the hook for per-host custom certificates.
	RootCAs func(serverName string) (*x509.CertPool, error)
	// ValidateURL is called for every endpoint before it is contacted (after the built-in
	// scheme check). Non-HTTP transports pass smtp://host:port and nats://host:port URLs.
	ValidateURL func(u *url.URL) error
	// Timeout bounds a single HTTP attempt (default 30s).
	Timeout time.Duration
	// Retry is the default retry policy (DefaultRetryPolicy when zero).
	Retry *RetryPolicy
	// MaxResponseBytes caps how much of a response body is read (default 1 MiB).
	MaxResponseBytes int64
	// UserAgent is sent with every HTTP request.
	UserAgent string
	// DisableProxyFromEnvironment disables HTTP(S)_PROXY handling.
	DisableProxyFromEnvironment bool
}

// Deps carries everything a service needs from its environment.
type Deps struct {
	Network Network
	Logger  *slog.Logger
	// Now returns the current time (default time.Now).
	Now func() time.Time
	// SensitiveValues are redacted from every error message (typically every value
	// substituted from the notifications Secret).
	SensitiveValues []string
	// Product is used where the remote API wants a client/source name (default "Patrol CD").
	Product string
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) product() string {
	if d.Product != "" {
		return d.Product
	}
	return "Patrol CD"
}

func (d Deps) dialer() func(ctx context.Context, network, address string) (net.Conn, error) {
	if d.Network.DialContext != nil {
		return d.Network.DialContext
	}
	nd := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return nd.DialContext
}

func (d Deps) timeout() time.Duration {
	if d.Network.Timeout > 0 {
		return d.Network.Timeout
	}
	return DefaultRequestTimeout
}

func (d Deps) retry() RetryPolicy {
	if d.Network.Retry != nil {
		return d.Network.Retry.normalized()
	}
	return DefaultRetryPolicy()
}

func (d Deps) maxResponseBytes() int64 {
	if d.Network.MaxResponseBytes > 0 {
		return d.Network.MaxResponseBytes
	}
	return DefaultMaxResponseBytes
}

func (d Deps) userAgent() string {
	if d.Network.UserAgent != "" {
		return d.Network.UserAgent
	}
	return "patrol-notifications"
}
