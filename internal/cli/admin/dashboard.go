package admin

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Defaults of "admin dashboard".
const (
	defaultDashboardPort    = 8080
	defaultDashboardAddress = "localhost"
	defaultServerName       = "patrol-server"
	defaultServerPort       = 8080
	// upstreamHost is the host name of the tunneled patrol-server (matches the self-signed
	// certificate's default SAN).
	upstreamHost = "localhost"
)

type dashboardOptions struct {
	port       int
	address    string
	serverName string
	serverPort int
	plaintext  bool
	insecure   bool
	serverCrt  string
	redis      redisFlags
}

func newDashboardCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	o := &dashboardOptions{}
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Starts Patrol CD Web UI locally",
		Long: `Serves the Patrol CD web UI and API on a local port by tunneling to a running patrol-server
pod through the Kubernetes API (port forwarding with the kubeconfig credentials). Users log
in as on the server: unlike upstream, no unauthenticated in-process server is started.

The connection to patrol-server is verified: its certificate is pinned from patrol-secret
(tls.crt) when configured there, or verified with --server-crt; --insecure skips the
verification and --plaintext connects to a server started with --insecure.`,
		Example: `# Start the Patrol CD Web UI locally on the default port and address
$ patrol admin dashboard

# Start the Patrol CD Web UI locally on a custom port and address
$ patrol admin dashboard --port 8085 --address 127.0.0.1`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			warnIgnoredFlags(c, f.errOut(), redisFlagNames, "Patrol CD has no Redis; the dashboard proxies to patrol-server")
			return runDashboard(c.Context(), f, kube, o)
		},
	}
	fl := cmd.Flags()
	fl.IntVar(&o.port, "port", defaultDashboardPort, "Listen on given port")
	fl.StringVar(&o.address, "address", defaultDashboardAddress, "Listen on given address")
	fl.StringVar(&o.serverName, "server-name", defaultServerName, "Value of the app.kubernetes.io/name label of the patrol-server pods")
	fl.IntVar(&o.serverPort, "server-port", defaultServerPort, "Port of the patrol-server container")
	fl.BoolVar(&o.plaintext, "plaintext", false, "Connect to patrol-server without TLS (server started with --insecure)")
	fl.BoolVar(&o.insecure, "insecure", false, "Skip verification of the patrol-server certificate")
	fl.StringVar(&o.serverCrt, "server-crt", "", "CA certificate(s) verifying the patrol-server certificate")
	bindIgnoredRedisFlags(fl, &o.redis, "Ignored: Patrol CD has no Redis", true)
	return cmd
}

// dashboardUpstream describes how to reach patrol-server.
type dashboardUpstream struct {
	dial   func(ctx context.Context, network, address string) (net.Conn, error)
	scheme string
	tls    *tls.Config
}

// upstreamTLS returns the TLS configuration verifying patrol-server.
func upstreamTLS(ctx context.Context, kc *kubeClients, o *dashboardOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: upstreamHost}
	switch {
	case o.insecure:
		cfg.InsecureSkipVerify = true //nolint:gosec // explicit --insecure
		return cfg, nil
	case o.serverCrt != "":
		data, err := readFileLimited(o.serverCrt)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("%s contains no PEM certificate", o.serverCrt)
		}
		cfg.RootCAs = pool
		return cfg, nil
	}
	if kc != nil {
		sec, err := kc.kube.CoreV1().Secrets(kc.namespace).Get(ctx, settings.DefaultSecretName, metav1.GetOptions{})
		if err == nil && len(sec.Data["tls.crt"]) > 0 {
			return pinnedTLS(sec.Data["tls.crt"])
		}
	}
	return cfg, nil
}

// pinnedTLS accepts exactly the leaf certificate of certPEM (the certificate configured
// in patrol-secret), whatever its host names: the tunnel itself is authenticated by the
// Kubernetes API.
func pinnedTLS(certPEM []byte) (*tls.Config, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("patrol-secret tls.crt is not a PEM certificate")
	}
	pinned := block.Bytes
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // verification is done by VerifyPeerCertificate (pinning)
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 || !bytes.Equal(rawCerts[0], pinned) {
				return errors.New("patrol-server certificate does not match tls.crt of patrol-secret")
			}
			return nil
		},
	}, nil
}

func runDashboard(ctx context.Context, f *Factory, kube *kubeOptions, o *dashboardOptions) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	kc, err := kube.get(nil)
	if err != nil {
		return err
	}
	pod, err := readyPod(ctx, kc.kube, kc.namespace, v1alpha1.LabelKeyAppName+"="+o.serverName)
	if err != nil {
		return fmt.Errorf("could not start dashboard: %w", err)
	}
	dialer, err := newPodDialer(kc.config, kc.namespace, pod.Name, o.serverPort)
	if err != nil {
		return fmt.Errorf("could not start dashboard: %w", err)
	}
	up := dashboardUpstream{dial: dialer.DialContext, scheme: "http"}
	if !o.plaintext {
		up.scheme = "https"
		if up.tls, err = upstreamTLS(ctx, kc, o); err != nil {
			return fmt.Errorf("could not start dashboard: %w", err)
		}
	}
	return serveDashboard(ctx, f, o, up)
}

// newDashboardProxy returns the reverse proxy to patrol-server and its transport (whose
// idle tunnels are closed on shutdown). The browser's Host header is kept so the server's
// CSRF origin check sees the dashboard origin.
func newDashboardProxy(up dashboardUpstream) (*httputil.ReverseProxy, *http.Transport) {
	target := &url.URL{Scheme: up.scheme, Host: upstreamHost}
	transport := &http.Transport{
		DialContext:           up.dial,
		TLSClientConfig:       up.tls,
		ForceAttemptHTTP2:     false,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 0,
		Proxy:                 nil,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		Transport:     transport,
		FlushInterval: -1, // stream Server-Sent Events
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "patrol-server is not reachable through the port forward: "+err.Error(), http.StatusBadGateway)
		},
	}, transport
}

// checkUpstream verifies that patrol-server answers (and its certificate verifies) before
// the dashboard is announced.
func checkUpstream(ctx context.Context, up dashboardUpstream) error {
	// One request: no idle tunnel is kept open.
	client := &http.Client{Transport: &http.Transport{DialContext: up.dial, TLSClientConfig: up.tls, Proxy: nil, DisableKeepAlives: true}, Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, up.scheme+"://"+upstreamHost+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		hint := ""
		if up.scheme == "https" && strings.Contains(err.Error(), "certificate") {
			hint = " (the patrol-server certificate is not trusted: pass --server-crt, or --insecure to skip verification)"
		}
		return fmt.Errorf("patrol-server is not reachable: %w%s", err, hint)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return nil
}

func serveDashboard(ctx context.Context, f *Factory, o *dashboardOptions, up dashboardUpstream) error {
	if err := checkUpstream(ctx, up); err != nil {
		return fmt.Errorf("could not start dashboard: %w", err)
	}
	addr := net.JoinHostPort(o.address, strconv.Itoa(o.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("could not start dashboard: %w", err)
	}
	if host := o.address; host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			_, _ = fmt.Fprintf(f.errOut(), "warning: the dashboard listens on %s, which is reachable from other hosts\n", host)
		}
	}
	proxy, transport := newDashboardProxy(up)
	defer transport.CloseIdleConnections()
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	_, _ = fmt.Fprintf(f.out(), "Patrol CD UI is available at http://%s\n", net.JoinHostPort(o.address, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)))
	select {
	case err := <-served:
		return fmt.Errorf("dashboard: %w", err)
	case <-ctx.Done():
	}
	_, _ = fmt.Fprintln(f.errOut(), "signal received, shutting down dashboard")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	_, _ = fmt.Fprintln(f.errOut(), "clean shutdown")
	return nil
}
