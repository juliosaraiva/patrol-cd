package webhook

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/time/rate"
)

// RateLimit configures the per-source token bucket applied to every webhook request
// before its body is read. The global in-flight body budget (byteBudget) complements it:
// the bucket bounds requests per source, the budget bounds memory across sources.
type RateLimit struct {
	// Disabled turns per-source rate limiting off.
	Disabled bool
	// RequestsPerSecond is the sustained rate per source (default 50).
	RequestsPerSecond float64
	// Burst is the bucket size per source (default 200).
	Burst int
	// MaxSources bounds the number of tracked sources; the least recently seen source
	// is forgotten first (default 10000).
	MaxSources int
}

// Rate limit defaults: generous enough for an SCM that delivers a whole organization's
// pushes from a handful of addresses.
const (
	DefaultRateLimitRPS        = 50
	DefaultRateLimitBurst      = 200
	DefaultRateLimitMaxSources = 10000
)

func (r RateLimit) withDefaults() RateLimit {
	if r.RequestsPerSecond <= 0 {
		r.RequestsPerSecond = DefaultRateLimitRPS
	}
	if r.Burst <= 0 {
		r.Burst = DefaultRateLimitBurst
	}
	if r.MaxSources <= 0 {
		r.MaxSources = DefaultRateLimitMaxSources
	}
	return r
}

// sourceLimiter keeps one token bucket per source in a bounded LRU.
type sourceLimiter struct {
	cfg   RateLimit
	cache *lru.Cache[string, *rate.Limiter]
}

func newSourceLimiter(cfg RateLimit) (*sourceLimiter, error) {
	if cfg.Disabled {
		return nil, nil
	}
	cfg = cfg.withDefaults()
	c, err := lru.New[string, *rate.Limiter](cfg.MaxSources)
	if err != nil {
		return nil, err
	}
	return &sourceLimiter{cfg: cfg, cache: c}, nil
}

// allow consumes a token of source; a nil limiter allows everything.
func (l *sourceLimiter) allow(source string) bool {
	if l == nil {
		return true
	}
	lim, ok := l.cache.Get(source)
	if !ok {
		fresh := rate.NewLimiter(rate.Limit(l.cfg.RequestsPerSecond), l.cfg.Burst)
		prev, found, _ := l.cache.PeekOrAdd(source, fresh)
		if found {
			lim = prev
		} else {
			lim = fresh
		}
	}
	return lim.Allow()
}

// RemoteAddrSource is the default source key: the client IP of the connection. Behind a
// reverse proxy every request shares the proxy's address; supply Config.SourceKey to key
// on a header set by a trusted proxy instead.
func RemoteAddrSource(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// byteBudget is a non-blocking byte semaphore: requests that would exceed the limit are
// refused instead of waiting.
type byteBudget struct {
	limit int64
	used  atomic.Int64
}

// tryAcquire charges n bytes, or reports false (charging nothing) when the budget would
// be exceeded.
func (b *byteBudget) tryAcquire(n int64) bool {
	if n <= 0 {
		return true
	}
	if b.used.Add(n) > b.limit {
		b.used.Add(-n)
		return false
	}
	return true
}

// release returns n charged bytes.
func (b *byteBudget) release(n int64) {
	if n > 0 {
		b.used.Add(-n)
	}
}

// errBodyBudget is returned by budgetReader when the in-flight body budget is exhausted.
var errBodyBudget = errors.New("webhook: in-flight request body budget exhausted")

// budgetReader charges the bytes it reads to a byteBudget; the caller releases charged.
type budgetReader struct {
	r       io.Reader
	budget  *byteBudget
	charged int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		if !b.budget.tryAcquire(int64(n)) {
			return 0, errBodyBudget
		}
		b.charged += int64(n)
	}
	return n, err
}
