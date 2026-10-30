package clusters

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
)

// Environment variables tuning Kubernetes clients for managed clusters (upstream ARGOCD_K8S_*
// with the PATROL_ prefix).
const (
	EnvK8sClientQPS                = "PATROL_K8S_CLIENT_QPS"
	EnvK8sClientBurst              = "PATROL_K8S_CLIENT_BURST"
	EnvK8sClientMaxIdleConnections = "PATROL_K8S_CLIENT_MAX_IDLE_CONNECTIONS"
	EnvK8sTCPTimeout               = "PATROL_K8S_TCP_TIMEOUT"
	EnvK8sTCPKeepAlive             = "PATROL_K8S_TCP_KEEPALIVE"
	EnvK8sTLSHandshakeTimeout      = "PATROL_K8S_TLS_HANDSHAKE_TIMEOUT"
	EnvK8sTCPIdleConnTimeout       = "PATROL_K8S_TCP_IDLE_TIMEOUT"
	EnvK8sServerSideTimeout        = "PATROL_K8S_SERVER_SIDE_TIMEOUT"
	EnvK8sClientRetryMax           = "PATROL_K8SCLIENT_RETRY_MAX"
	EnvK8sClientRetryBaseBackoff   = "PATROL_K8SCLIENT_RETRY_BASE_BACKOFF"
)

// EnvUpdateClusterInfoTimeout is how often the controller writes Cluster status (upstream
// ARGO_CD_UPDATE_CLUSTER_INFO_TIMEOUT).
const EnvUpdateClusterInfoTimeout = "PATROL_UPDATE_CLUSTER_INFO_TIMEOUT"

// DefaultClusterInfoInterval is the default of EnvUpdateClusterInfoTimeout and its minimum.
const DefaultClusterInfoInterval = 10 * time.Second

// maxClusterInfoInterval is the maximum of EnvUpdateClusterInfoTimeout.
const maxClusterInfoInterval = time.Minute

// Upstream defaults (pkg/apis/application/v1alpha1/cluster_constants.go, util/http).
const (
	DefaultQPS                 float32 = 50
	DefaultMaxIdleConns                = 500
	DefaultTCPTimeout                  = 30 * time.Second
	DefaultTCPKeepAlive                = 30 * time.Second
	DefaultTLSHandshakeTimeout         = 10 * time.Second
	DefaultIdleConnTimeout             = 5 * time.Minute
	DefaultRetryBaseBackoff            = 100 * time.Millisecond

	// retryWaitMax caps the exponential retry backoff.
	retryWaitMax = 10 * time.Second
	// retryDrainLimit bounds how much of a failed response body is read before retrying.
	retryDrainLimit = 512 * 1024
	// maxDurationEnv is the upper bound of duration variables (math.MaxInt32 seconds).
	maxDurationEnv = time.Duration(math.MaxInt32) * time.Second
)

// Tuning holds the client knobs applied to every managed-cluster rest.Config (REPO-105).
type Tuning struct {
	// QPS and Burst are the client-side rate limits. Burst defaults to 2×QPS.
	QPS   float32
	Burst int
	// MaxIdleConns is used for MaxIdleConns, MaxIdleConnsPerHost and MaxConnsPerHost.
	MaxIdleConns int
	// TCPTimeout and TCPKeepAlive configure the dialer.
	TCPTimeout   time.Duration
	TCPKeepAlive time.Duration
	// TLSHandshakeTimeout and IdleConnTimeout configure the transport.
	TLSHandshakeTimeout time.Duration
	IdleConnTimeout     time.Duration
	// ServerSideTimeout is sent as the request timeout (rest.Config.Timeout); 0 disables it.
	ServerSideTimeout time.Duration
	// RetryMax is the number of retries of failed requests; 0 disables retries.
	RetryMax int64
	// RetryBaseBackoff is the first retry delay; it doubles per attempt, capped at 10s.
	RetryBaseBackoff time.Duration
}

// DefaultTuning returns upstream's defaults.
func DefaultTuning() Tuning {
	return Tuning{
		QPS:                 DefaultQPS,
		Burst:               int(2 * DefaultQPS),
		MaxIdleConns:        DefaultMaxIdleConns,
		TCPTimeout:          DefaultTCPTimeout,
		TCPKeepAlive:        DefaultTCPKeepAlive,
		TLSHandshakeTimeout: DefaultTLSHandshakeTimeout,
		IdleConnTimeout:     DefaultIdleConnTimeout,
		RetryBaseBackoff:    DefaultRetryBaseBackoff,
	}
}

// TuningFromEnv reads the PATROL_K8S* variables from the process environment.
func TuningFromEnv(logger *slog.Logger) Tuning {
	return TuningFromLookup(os.LookupEnv, logger)
}

// TuningFromLookup reads the PATROL_K8S* variables through lookup. As upstream's env parsers,
// an unparsable value or one outside the allowed range keeps the default and logs a warning.
// Durations accept Go syntax ("90s") and a whole number of days ("2d").
func TuningFromLookup(lookup func(string) (string, bool), logger *slog.Logger) Tuning {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p := envParser{lookup: lookup, logger: logger}
	t := DefaultTuning()
	t.QPS = p.float32(EnvK8sClientQPS, DefaultQPS, 0, math.MaxFloat32)
	t.Burst = int(p.int64(EnvK8sClientBurst, int64(2*t.QPS), 0, math.MaxInt32))
	t.MaxIdleConns = int(p.int64(EnvK8sClientMaxIdleConnections, DefaultMaxIdleConns, 0, math.MaxInt32))
	t.TLSHandshakeTimeout = p.duration(EnvK8sTLSHandshakeTimeout, DefaultTLSHandshakeTimeout, 0, maxDurationEnv)
	t.TCPTimeout = p.duration(EnvK8sTCPTimeout, DefaultTCPTimeout, 0, maxDurationEnv)
	t.TCPKeepAlive = p.duration(EnvK8sTCPKeepAlive, DefaultTCPKeepAlive, 0, maxDurationEnv)
	t.IdleConnTimeout = p.duration(EnvK8sTCPIdleConnTimeout, DefaultIdleConnTimeout, 0, maxDurationEnv)
	t.ServerSideTimeout = p.duration(EnvK8sServerSideTimeout, 0, 0, maxDurationEnv)
	t.RetryMax = p.int64(EnvK8sClientRetryMax, 0, 1, math.MaxInt64)
	t.RetryBaseBackoff = time.Duration(p.int64(EnvK8sClientRetryBaseBackoff, int64(DefaultRetryBaseBackoff/time.Millisecond), 1, math.MaxInt64/int64(time.Millisecond))) * time.Millisecond
	return t
}

// ClusterInfoIntervalFromLookup reads EnvUpdateClusterInfoTimeout through lookup. As upstream, an
// unparsable value or one outside 10s to 1m keeps the default and logs a warning.
func ClusterInfoIntervalFromLookup(lookup func(string) (string, bool), logger *slog.Logger) time.Duration {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p := envParser{lookup: lookup, logger: logger}
	return p.duration(EnvUpdateClusterInfoTimeout, DefaultClusterInfoInterval, DefaultClusterInfoInterval, maxClusterInfoInterval)
}

type envParser struct {
	lookup func(string) (string, bool)
	logger *slog.Logger
}

func (p envParser) raw(name string) (string, bool) {
	v, ok := p.lookup(name)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func (p envParser) int64(name string, def, lo, hi int64) int64 {
	s, ok := p.raw(name)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		p.logger.Warn("cannot parse environment variable as a number, using default", "name", name, "default", def)
		return def
	}
	if n < lo || n > hi {
		p.logger.Warn("environment variable out of range, using default", "name", name, "value", n, "min", lo, "max", hi, "default", def)
		return def
	}
	return n
}

func (p envParser) float32(name string, def, lo, hi float32) float32 {
	s, ok := p.raw(name)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(s, 32)
	if err != nil || math.IsNaN(f) {
		p.logger.Warn("cannot parse environment variable as a float, using default", "name", name, "default", def)
		return def
	}
	if float32(f) < lo || float32(f) > hi {
		p.logger.Warn("environment variable out of range, using default", "name", name, "value", f, "min", lo, "max", hi, "default", def)
		return def
	}
	return float32(f)
}

func (p envParser) duration(name string, def, lo, hi time.Duration) time.Duration {
	s, ok := p.raw(name)
	if !ok {
		return def
	}
	d, err := parseDuration(s)
	if err != nil {
		p.logger.Warn("cannot parse environment variable as a duration, using default", "name", name, "default", def)
		return def
	}
	if d < lo || d > hi {
		p.logger.Warn("environment variable out of range, using default", "name", name, "value", d, "min", lo, "max", hi, "default", def)
		return def
	}
	return d
}

// parseDuration accepts time.ParseDuration syntax and "<n>d" (days), the backwards-compatible
// form upstream's env parser accepts.
func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 64)
		if err == nil && n >= 0 && n <= int64(maxDurationEnv/(24*time.Hour)) {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("invalid duration %q", s)
}

// applyLimits sets the fields RawRestConfig sets from the tuning (upstream RawRestConfig sets
// timeout, QPS and burst even without the custom transport).
func (t Tuning) applyLimits(cfg *rest.Config) {
	cfg.Timeout = t.ServerSideTimeout
	cfg.QPS = t.QPS
	cfg.Burst = t.Burst
}

// ApplyTuning bakes TLS, authentication and proxy settings of cfg into a custom transport built
// from t, mirroring upstream SetK8SConfigDefaults: the TLS client config and the exec/auth
// providers are cleared afterwards because the transport already carries them. WrapTransport
// and Impersonate are also cleared once baked, so applying impersonation to the returned
// config (ImpersonatingConfig) does not duplicate headers. When t.RetryMax > 0 a retrying
// wrapper is installed as WrapTransport. cfg is modified in place.
func ApplyTuning(cfg *rest.Config, t Tuning) error {
	cfg.QPS = t.QPS
	cfg.Burst = t.Burst
	tlsConfig, err := rest.TLSConfigFor(cfg)
	if err != nil {
		return fmt.Errorf("build TLS config: %w", err)
	}
	dialer := &net.Dialer{Timeout: t.TCPTimeout, KeepAlive: t.TCPKeepAlive}
	transport := utilnet.SetTransportDefaults(&http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: t.TLSHandshakeTimeout,
		TLSClientConfig:     tlsConfig,
		MaxIdleConns:        t.MaxIdleConns,
		MaxIdleConnsPerHost: t.MaxIdleConns,
		MaxConnsPerHost:     t.MaxIdleConns,
		DialContext:         dialer.DialContext,
		DisableCompression:  cfg.DisableCompression,
		IdleConnTimeout:     t.IdleConnTimeout,
	})
	if cfg.Proxy != nil {
		transport.Proxy = cfg.Proxy
	}
	rt, err := rest.HTTPWrappersForConfig(cfg, transport)
	if err != nil {
		return fmt.Errorf("build transport: %w", err)
	}
	cfg.TLSClientConfig = rest.TLSClientConfig{}
	cfg.AuthProvider = nil
	cfg.ExecProvider = nil
	cfg.WrapTransport = nil
	cfg.Impersonate = rest.ImpersonationConfig{}
	cfg.Timeout = t.ServerSideTimeout
	cfg.Transport = rt
	if t.RetryMax > 0 {
		retries, base := t.RetryMax, t.RetryBaseBackoff
		cfg.WrapTransport = func(inner http.RoundTripper) http.RoundTripper {
			return &retryTransport{inner: inner, maxRetries: retries, backoff: base}
		}
	}
	return nil
}

// retryTransport retries requests that failed with a transport error, 429 or a 5xx other than
// 501, with exponential backoff (upstream util/http WithRetry).
type retryTransport struct {
	inner      http.RoundTripper
	maxRetries int64
	backoff    time.Duration
}

func isRetriable(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return true
	case resp.StatusCode == 0:
		return true
	case resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented:
		return true
	}
	return false
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("buffer request body for retries: %w", err)
		}
		body = b
	}
	backoff := t.backoff
	for attempt := int64(0); ; attempt++ {
		r := req
		if attempt > 0 || body != nil {
			r = req.Clone(req.Context())
			switch {
			case body != nil:
				r.Body = io.NopCloser(bytes.NewReader(body))
			case req.GetBody != nil:
				b, err := req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("rewind request body: %w", err)
				}
				r.Body = b
			}
		}
		resp, err := t.inner.RoundTrip(r)
		if attempt >= t.maxRetries || (err == nil && !isRetriable(resp)) {
			return resp, err
		}
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, retryDrainLimit))
			_ = resp.Body.Close()
		}
		backoff = min(backoff, retryWaitMax)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		}
		backoff *= 2
	}
}
