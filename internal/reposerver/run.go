package reposerver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// shutdownGrace bounds graceful shutdown.
const shutdownGrace = 30 * time.Second

// Run serves the repo-server until ctx ends: the API on ListenAddress (TLS unless
// DisableTLS) and, when MetricsAddress is set, the plain-HTTP probe and metrics
// listener. It is the entry point used by `patrolcd repo-server`.
func Run(ctx context.Context, cfg Config) error {
	addr := cfg.ListenAddress
	if addr == "" {
		addr = DefaultListenAddress
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("reposerver: listen: %w", err)
	}
	return Serve(ctx, cfg, ln)
}

// Serve is Run on an existing API listener (closed on return).
func Serve(ctx context.Context, cfg Config, ln net.Listener) error {
	var probeLn net.Listener
	if cfg.MetricsAddress != "" {
		var err error
		if probeLn, err = net.Listen("tcp", cfg.MetricsAddress); err != nil {
			_ = ln.Close()
			return fmt.Errorf("reposerver: listen metrics: %w", err)
		}
	}
	return serve(ctx, cfg, ln, probeLn)
}

// serve runs the API on ln and, when probeLn is set, the probe and metrics handler on
// probeLn. Both listeners are closed on return.
func serve(ctx context.Context, cfg Config, ln, probeLn net.Listener) error {
	defer func() { _ = ln.Close() }()
	if probeLn != nil {
		defer func() { _ = probeLn.Close() }()
	}
	svc, err := New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	handler, err := svc.Handler()
	if err != nil {
		return err
	}
	tlsCfg, healthCert, err := serverTLSConfig(svc.cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig:         tlsCfg,
		ErrorLog:          slog.NewLogLogger(svc.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	apiAddr := ln.Addr()
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}
	errCh := make(chan error, 2)
	go func() { errCh <- srv.Serve(ln) }()
	var psrv *http.Server
	if probeLn != nil {
		psrv = &http.Server{
			Handler:           svc.probeHandler(newSelfCheck(apiAddr, tlsCfg != nil, healthCert)),
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(svc.log.Handler(), slog.LevelWarn),
		}
		go func() { errCh <- psrv.Serve(probeLn) }()
		svc.log.Info("reposerver: serving probes and metrics", slog.String("address", probeLn.Addr().String()))
	}
	svc.log.Info("reposerver: serving", slog.String("address", apiAddr.String()), slog.Bool("tls", tlsCfg != nil),
		slog.Bool("mtls", tlsCfg != nil && tlsCfg.ClientAuth == tls.RequireAndVerifyClientCert))

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}
	svc.shuttingDown.Store(true)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	shutdownErr := srv.Shutdown(sctx)
	if psrv != nil {
		_ = psrv.Shutdown(sctx)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("reposerver: serve: %w", serveErr)
	}
	if shutdownErr != nil {
		return fmt.Errorf("reposerver: shutdown: %w", shutdownErr)
	}
	return nil
}

// probeHandler serves the probe listener: GET /healthz (with ?full=true, check runs
// too, as upstream's liveness probe), GET /readyz and GET /metrics (with a Gatherer).
// It needs no token and no client certificate.
func (s *Service) probeHandler(check func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+apiclient.PathHealthz, func(w http.ResponseWriter, r *http.Request) {
		if check != nil && r.URL.Query().Get("full") == "true" {
			if err := check(r.Context()); err != nil {
				s.log.Warn("reposerver: self health check failed", slog.Any("error", err))
				writeProblem(w, http.StatusServiceUnavailable, apiclient.CodeUnavailable, "self health check failed")
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET "+apiclient.PathReadyz, func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready() {
			writeProblem(w, http.StatusServiceUnavailable, apiclient.CodeUnavailable, "not ready")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	if s.cfg.Gatherer != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.cfg.Gatherer, promhttp.HandlerOpts{}))
	}
	return mux
}

// selfCheckTimeout bounds one self health check.
const selfCheckTimeout = 10 * time.Second

// newSelfCheck returns a check that the API listener at addr answers GET /healthz over
// loopback, presenting clientCert (the ephemeral health-check certificate under mTLS).
func newSelfCheck(addr net.Addr, useTLS bool, clientCert *tls.Certificate) func(context.Context) error {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return func(context.Context) error { return fmt.Errorf("invalid API address %q: %w", addr.String(), err) }
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		// Wildcard listeners accept loopback IPv4 (Go listens dual-stack on "::").
		host = "127.0.0.1"
	}
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	scheme := "http"
	if useTLS {
		scheme = "https"
		// The connection goes to this process's own listener over loopback: the server
		// certificate (often issued for the service name) is not verified, as upstream's
		// buildHealthCheckTLSConfig.
		tc := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // loopback self-check of this process's listener
		if clientCert != nil {
			tc.Certificates = []tls.Certificate{*clientCert}
		}
		tr.TLSClientConfig = tc
	}
	client := &http.Client{Transport: tr, Timeout: selfCheckTimeout}
	target := scheme + "://" + net.JoinHostPort(host, port) + apiclient.PathHealthz
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("API health check answered %d", resp.StatusCode)
		}
		return nil
	}
}

// serverTLSConfig builds the server TLS configuration: nil with DisableTLS; the given
// TLSConfig; the certificate files; otherwise a self-signed certificate (upstream
// behavior). ClientCAFile enables mTLS. When client certificates are verified, an
// ephemeral health-check client certificate is generated and trusted (upstream
// GenerateHealthCheckClientCert) and returned for the self health check; its key
// never leaves the process.
func serverTLSConfig(cfg Config) (*tls.Config, *tls.Certificate, error) {
	if cfg.DisableTLS {
		return nil, nil, nil
	}
	var out *tls.Config
	switch {
	case cfg.TLSConfig != nil:
		out = cfg.TLSConfig.Clone()
	case cfg.TLSCertFile != "" && cfg.TLSKeyFile != "":
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("reposerver: load TLS certificate: %w", err)
		}
		out = &tls.Config{Certificates: []tls.Certificate{cert}}
	default:
		cert, err := selfSignedCertificate()
		if err != nil {
			return nil, nil, err
		}
		out = &tls.Config{Certificates: []tls.Certificate{cert}}
	}
	if out.MinVersion == 0 {
		out.MinVersion = tls.VersionTLS12
	}
	if cfg.ClientCAFile != "" {
		pem, err := readFileLimited(cfg.ClientCAFile, 4<<20)
		switch {
		case err == nil:
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, nil, errors.New("reposerver: client CA file contains no certificate")
			}
			out.ClientCAs = pool
			out.ClientAuth = tls.RequireAndVerifyClientCert
		case os.IsNotExist(err):
			// No client CA file: mTLS is not enabled (upstream).
		default:
			return nil, nil, fmt.Errorf("reposerver: read client CA: %w", err)
		}
	}
	if out.ClientCAs == nil || (out.ClientAuth != tls.RequireAndVerifyClientCert && out.ClientAuth != tls.VerifyClientCertIfGiven) {
		return out, nil, nil
	}
	hc, err := healthCheckClientCertificate()
	if err != nil {
		return nil, nil, err
	}
	pool := out.ClientCAs.Clone()
	pool.AddCert(hc.Leaf)
	out.ClientCAs = pool
	return out, &hc, nil
}

// healthCheckClientCertificate creates the ephemeral self-signed client certificate of
// the self health check (its own CA, so it can be trusted directly).
func healthCheckClientCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reposerver: generate health-check key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Patrol CD Health Check"}, CommonName: "patrol-repo-server-healthcheck"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reposerver: create health-check certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reposerver: parse health-check certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// selfSignedCertificate creates an in-memory ECDSA certificate for the repo-server.
func selfSignedCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reposerver: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Patrol CD"}, CommonName: "patrol-repo-server"},
		DNSNames:     []string{"localhost", "patrol-repo-server"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reposerver: create certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
