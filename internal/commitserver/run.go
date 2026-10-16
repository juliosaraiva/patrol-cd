package commitserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Defaults of the patrolcd commit-server command (upstream ports).
const (
	DefaultListenAddress   = "0.0.0.0:8086"
	DefaultMetricsAddress  = "0.0.0.0:8087"
	defaultShutdownTimeout = 30 * time.Second
	readHeaderTimeout      = 10 * time.Second
)

// RunOptions configure Run.
type RunOptions struct {
	// ListenAddress serves the API (default DefaultListenAddress).
	ListenAddress string
	// MetricsAddress serves /metrics from Registry (default DefaultMetricsAddress; "-"
	// disables the metrics listener).
	MetricsAddress string
	// Token is the shared internal token (required).
	Token string
	// TLSConfig enables TLS on the API listener (set ClientAuth/ClientCAs for mTLS).
	TLSConfig *tls.Config
	// Service configures the commit service. Its Metrics default to metrics registered
	// with Registry.
	Service ServiceOptions
	// MaxRequestBytes caps request bodies (default DefaultMaxRequestBytes).
	MaxRequestBytes int64
	// Registry receives the commit-server metrics and is served on MetricsAddress (default:
	// a new registry with the Go and process collectors).
	Registry *prometheus.Registry
	// ShutdownTimeout bounds graceful shutdown (default 30s); in-flight commits are
	// cancelled when it expires.
	ShutdownTimeout time.Duration
	// Logger is the server logger (default slog.Default()).
	Logger *slog.Logger
	// Listener and MetricsListener replace the addresses (tests, socket activation).
	Listener        net.Listener
	MetricsListener net.Listener
}

// Run serves the commit-server API (and metrics) until ctx is cancelled, then shuts down
// gracefully. It returns nil after a shutdown requested through ctx.
func Run(ctx context.Context, opts RunOptions) error {
	if opts.Token == "" {
		return errors.New("commit-server: an internal token is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultShutdownTimeout
	}
	if opts.Registry == nil {
		opts.Registry = prometheus.NewRegistry()
		opts.Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	if opts.Service.Metrics == nil {
		m, err := NewMetrics(opts.Registry)
		if err != nil {
			return err
		}
		opts.Service.Metrics = m
	}
	if opts.Service.Logger == nil {
		opts.Service.Logger = opts.Logger
	}
	handler, err := NewHandler(NewService(opts.Service), HandlerOptions{Token: opts.Token, MaxRequestBytes: opts.MaxRequestBytes, Logger: opts.Logger})
	if err != nil {
		return err
	}

	ln, err := listen(opts.Listener, opts.ListenAddress, DefaultListenAddress)
	if err != nil {
		return err
	}
	if opts.TLSConfig != nil {
		ln = tls.NewListener(ln, opts.TLSConfig)
	}
	// Request contexts derive from baseCtx, cancelled only when graceful shutdown times out,
	// so in-flight commits can finish.
	baseCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	servers := []*http.Server{{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}}
	listeners := []net.Listener{ln}
	if opts.MetricsAddress != "-" || opts.MetricsListener != nil {
		mln, err := listen(opts.MetricsListener, opts.MetricsAddress, DefaultMetricsAddress)
		if err != nil {
			_ = ln.Close()
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", promhttp.HandlerFor(opts.Registry, promhttp.HandlerOpts{}))
		servers = append(servers, &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout})
		listeners = append(listeners, mln)
	}

	errc := make(chan error, len(servers))
	for i, srv := range servers {
		opts.Logger.Info("commit-server: listening", slog.String("address", listeners[i].Addr().String()), slog.Bool("tls", i == 0 && opts.TLSConfig != nil))
		go func() {
			if err := srv.Serve(listeners[i]); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("commit-server: serve %s: %w", listeners[i].Addr(), err)
				return
			}
			errc <- nil
		}()
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opts.ShutdownTimeout)
	defer cancel()
	var errs []error
	if serveErr != nil {
		errs = append(errs, serveErr)
	}
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			cancelRequests()
			_ = srv.Close()
			errs = append(errs, fmt.Errorf("commit-server: shutdown: %w", err))
		}
	}
	return errors.Join(errs...)
}

func listen(ln net.Listener, addr, def string) (net.Listener, error) {
	if ln != nil {
		return ln, nil
	}
	if addr == "" {
		addr = def
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("commit-server: listen on %s: %w", addr, err)
	}
	return l, nil
}
