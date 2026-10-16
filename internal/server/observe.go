package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Profiler (OPS-056): pprof endpoints on the metrics listener, off by default.
const (
	// EnvProfilerFile names the file whose content "true" enables the profiler per request
	// (the install manifests project <component>.profile.enabled into it, so toggling needs no
	// restart).
	EnvProfilerFile = "PATROL_ENABLE_PROFILER_FILE_PATH"
	// DefaultProfilerFile is the default of EnvProfilerFile.
	DefaultProfilerFile = "/home/patrol/params/profiler.enabled"
	// profilerDisabledMessage is the 401 body (upstream wording, Patrol object name).
	profilerDisabledMessage = "Profiler endpoint is not enabled in 'patrol-cmd-params-cm' ConfigMap"
	// observabilityReadHeaderTimeout bounds request headers on the probe listeners.
	observabilityReadHeaderTimeout = 10 * time.Second
	// readinessTimeout bounds one readiness evaluation.
	readinessTimeout = 5 * time.Second
)

// newRegistry returns a metrics registry with the Go and process collectors.
func newRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// ObservabilityOptions configure the metrics, health and profiling listeners of a component.
type ObservabilityOptions struct {
	// MetricsAddr serves /metrics, the probes and (when enabled) /debug/pprof/. "" or "-"
	// disables it.
	MetricsAddr string
	// HealthzAddr, when set (not "-"), serves the probes on their own listener as well.
	HealthzAddr string
	// Gatherer serves /metrics.
	Gatherer prometheus.Gatherer
	// Ready reports readiness (/healthz, /readyz); nil is always ready. /livez only reports
	// that the process serves HTTP.
	Ready func(ctx context.Context) error
	// PprofEnabled serves the profiler unconditionally; otherwise PprofFile must contain
	// "true" at request time.
	PprofEnabled bool
	PprofFile    string
	Logger       *slog.Logger
	// ShutdownTimeout bounds the drain of the listeners.
	ShutdownTimeout time.Duration
}

// observability serves the probe and metrics listeners.
type observability struct {
	o            ObservabilityOptions
	shuttingDown atomic.Bool
}

func (ob *observability) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if ob.shuttingDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "shutting down\n")
		return
	}
	if ob.o.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := ob.o.Ready(ctx); err != nil {
			ob.o.Logger.LogAttrs(ctx, slog.LevelWarn, "health check failed", slog.String("component", "healthcheck"), slog.Any("error", err))
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready\n")
			return
		}
	}
	_, _ = io.WriteString(w, "ok\n")
}

func livez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (ob *observability) pprofEnabled() bool {
	if ob.o.PprofEnabled {
		return true
	}
	if ob.o.PprofFile == "" {
		return false
	}
	data, err := os.ReadFile(ob.o.PprofFile)
	return err == nil && strings.TrimSpace(string(data)) == "true"
}

func (ob *observability) gatePprof(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ob.pprofEnabled() {
			http.Error(w, profilerDisabledMessage, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (ob *observability) probeMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", ob.healthz)
	mux.HandleFunc("GET /readyz", ob.healthz)
	mux.HandleFunc("GET /livez", livez)
	return mux
}

func (ob *observability) metricsMux() *http.ServeMux {
	mux := ob.probeMux()
	if ob.o.Gatherer != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(ob.o.Gatherer, promhttp.HandlerOpts{ErrorLog: slog.NewLogLogger(ob.o.Logger.Handler(), slog.LevelError)}))
	}
	mux.HandleFunc("/debug/pprof/", ob.gatePprof(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", ob.gatePprof(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", ob.gatePprof(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", ob.gatePprof(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", ob.gatePprof(pprof.Trace))
	return mux
}

func listenerEnabled(addr string) bool { return addr != "" && addr != "-" }

// serveObservability serves the listeners until ctx ends. Readiness turns 503 as soon as ctx
// ends (before the other servers drain), so load balancers stop routing first. started, when
// not nil, receives the bound metrics address (tests use port 0).
func serveObservability(ctx context.Context, o ObservabilityOptions, started func(metrics, healthz net.Addr)) error {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 5 * time.Second
	}
	ob := &observability{o: o}
	type srv struct {
		s *http.Server
		l net.Listener
	}
	var servers []srv
	var maddr, haddr net.Addr
	errLog := slog.NewLogLogger(o.Logger.Handler(), slog.LevelWarn)
	if listenerEnabled(o.MetricsAddr) {
		l, err := net.Listen("tcp", o.MetricsAddr)
		if err != nil {
			return fmt.Errorf("listen on metrics address %s: %w", o.MetricsAddr, err)
		}
		maddr = l.Addr()
		servers = append(servers, srv{&http.Server{Handler: ob.metricsMux(), ReadHeaderTimeout: observabilityReadHeaderTimeout, ErrorLog: errLog}, l})
	}
	if listenerEnabled(o.HealthzAddr) {
		l, err := net.Listen("tcp", o.HealthzAddr)
		if err != nil {
			for _, s := range servers {
				_ = s.l.Close()
			}
			return fmt.Errorf("listen on health address %s: %w", o.HealthzAddr, err)
		}
		haddr = l.Addr()
		servers = append(servers, srv{&http.Server{Handler: ob.probeMux(), ReadHeaderTimeout: observabilityReadHeaderTimeout, ErrorLog: errLog}, l})
	}
	if started != nil {
		started(maddr, haddr)
	}
	if len(servers) == 0 {
		<-ctx.Done()
		return nil
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		o.Logger.LogAttrs(ctx, slog.LevelInfo, "serving metrics and probes", slog.String("address", s.l.Addr().String()))
		go func() {
			if err := s.s.Serve(s.l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}
	ob.shuttingDown.Store(true)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ShutdownTimeout)
	defer cancel()
	var errs []error
	if serveErr != nil {
		errs = append(errs, fmt.Errorf("observability listener: %w", serveErr))
	}
	for _, s := range servers {
		if err := s.s.Shutdown(sctx); err != nil {
			_ = s.s.Close()
		}
	}
	return errors.Join(errs...)
}

// TracingOptions configure OpenTelemetry tracing (OPS-085).
type TracingOptions struct {
	// Address is the OTLP/gRPC collector ("" disables tracing).
	Address string
	// Insecure uses a plaintext connection (default true upstream); otherwise TLS with the
	// system roots.
	Insecure bool
	// Headers are sent with every export (they may carry credentials and are never logged).
	Headers map[string]string
	// Attrs are "key:value" resource attributes.
	Attrs []string
	// ServiceName is the service.name resource attribute.
	ServiceName string
}

// setupTracing installs the global tracer provider and W3C propagator when an address is
// configured and returns the provider's shutdown function (a no-op otherwise). Exporter
// failures are fatal at startup, as upstream ("failed to initialize tracing").
func setupTracing(ctx context.Context, o TracingOptions, log *slog.Logger) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if strings.TrimSpace(o.Address) == "" {
		return noop, nil
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(o.Address)}
	if o.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(o.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(o.Headers))
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return noop, fmt.Errorf("failed to initialize tracing: %w", err)
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", o.ServiceName)}
	for _, a := range o.Attrs {
		k, v, ok := strings.Cut(a, ":")
		if !ok || strings.Contains(v, ":") || strings.TrimSpace(k) == "" {
			log.Warn(fmt.Sprintf("OTLP attr '%s' split with ':' length not 2", a))
			continue
		}
		attrs = append(attrs, attribute.String(strings.TrimSpace(k), strings.TrimSpace(v)))
	}
	res, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(attrs...))
	if err != nil {
		return noop, fmt.Errorf("failed to initialize tracing: resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	log.Info("tracing enabled", "collector", o.Address, "insecure", o.Insecure, "service", o.ServiceName)
	return tp.Shutdown, nil
}

// tracingTransport starts a client span per request and injects the W3C trace context, so
// calls between components (repo-server, controller tree API, commit-server) are linked.
// With no tracer provider installed the spans are no-ops.
type tracingTransport struct {
	name string
	next http.RoundTripper
}

func (t *tracingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, span := otel.Tracer("patrolcd").Start(r.Context(), t.name+" "+r.Method+" "+r.URL.Path)
	defer span.End()
	span.SetAttributes(attribute.String("http.request.method", r.Method), attribute.String("server.address", r.URL.Host),
		attribute.String("url.path", r.URL.Path))
	r = r.Clone(ctx)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(r.Header))
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	return resp, nil
}

// tracingHandler starts a server span per request on listeners owned by this package,
// continuing the caller's trace.
func tracingHandler(name string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := otel.Tracer("patrolcd").Start(ctx, name+" "+r.Method+" "+r.URL.Path)
		defer span.End()
		span.SetAttributes(attribute.String("http.request.method", r.Method), attribute.String("url.path", r.URL.Path))
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}
