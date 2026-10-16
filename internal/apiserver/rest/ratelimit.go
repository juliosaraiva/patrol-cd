package rest

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/time/rate"
)

// Login rate limiting defaults: per client IP, 5 requests per second sustained with a
// burst of 30. Per-account lockout and the global concurrency limit of internal/auth
// remain the primary brute-force defenses; this bounds request floods.
const (
	DefaultLoginRate      = rate.Limit(5)
	DefaultLoginBurst     = 30
	DefaultRateLimitCache = 10000
)

// RateLimiter is a per-client-IP token bucket with a bounded LRU of buckets.
type RateLimiter struct {
	limit   rate.Limit
	burst   int
	trusted []netip.Prefix
	now     func() time.Time

	mu      sync.Mutex
	buckets *lru.Cache[string, *rate.Limiter]
}

// RateLimiterConfig configures a RateLimiter.
type RateLimiterConfig struct {
	Limit rate.Limit
	Burst int
	// CacheSize bounds the number of tracked client IPs (LRU).
	CacheSize int
	// TrustedProxies are the networks of reverse proxies whose X-Forwarded-For header is
	// believed; the client IP is then the right-most untrusted address.
	TrustedProxies []netip.Prefix
	Now            func() time.Time
}

// NewRateLimiter returns a limiter; zero fields select the defaults.
func NewRateLimiter(cfg RateLimiterConfig) (*RateLimiter, error) {
	if cfg.Limit <= 0 {
		cfg.Limit = DefaultLoginRate
	}
	if cfg.Burst <= 0 {
		cfg.Burst = DefaultLoginBurst
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = DefaultRateLimitCache
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c, err := lru.New[string, *rate.Limiter](cfg.CacheSize)
	if err != nil {
		return nil, err
	}
	return &RateLimiter{limit: cfg.Limit, burst: cfg.Burst, trusted: cfg.TrustedProxies, now: cfg.Now, buckets: c}, nil
}

// Allow consumes one token for the request's client. When the bucket is empty it returns
// false and the time until a token is available.
func (l *RateLimiter) Allow(r *http.Request) (bool, time.Duration) {
	key := ClientIP(r, l.trusted)
	l.mu.Lock()
	b, ok := l.buckets.Get(key)
	if !ok {
		b = rate.NewLimiter(l.limit, l.burst)
		l.buckets.Add(key, b)
	}
	l.mu.Unlock()
	now := l.now()
	res := b.ReserveN(now, 1)
	if !res.OK() {
		return false, time.Second
	}
	if d := res.DelayFrom(now); d > 0 {
		res.CancelAt(now)
		return false, d
	}
	return true, 0
}

// ClientIP returns the client address of r: the host of RemoteAddr, or, when RemoteAddr
// belongs to a trusted proxy, the right-most X-Forwarded-For entry that is not a trusted
// proxy.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if !isTrusted(addr, trusted) {
		return addr.String()
	}
	hops := r.Header.Values("X-Forwarded-For")
	var all []string
	for _, h := range hops {
		all = append(all, strings.Split(h, ",")...)
	}
	for i := len(all) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(all[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !isTrusted(a, trusted) {
			return a.String()
		}
	}
	return addr.String()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ParseTrustedProxies parses CIDRs or single addresses.
func ParseTrustedProxies(values []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	var errs []error
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(v)
		if err != nil {
			errs = append(errs, errors.New("invalid trusted proxy "+v))
			continue
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, errors.Join(errs...)
}
