package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// RepoServerClientOptions configure a component's repo-server client (OPS-020, OPS-101,
// OPS-103).
type RepoServerClientOptions struct {
	// Address is host:port (or a URL).
	Address string
	// Timeout bounds calls without a deadline (0: the client default).
	Timeout time.Duration
	// TLS selects plaintext, verification and the mTLS client certificate.
	TLS TLSClientOptions
}

// repoServerOptionsFromParams reads the repo-server client parameters of a component:
// repo.server (when the component has it, else address), <prefix>.repo.server.timeout.seconds,
// .plaintext, .strict.tls, .ca.cert.path, .client.cert.path, .client.cert.key.path.
func repoServerOptionsFromParams(p *settings.Params, prefix, address string) RepoServerClientOptions {
	o := RepoServerClientOptions{Address: address}
	if p == nil {
		return o
	}
	if _, ok := p.Lookup("repo.server"); ok && address == "" {
		o.Address = p.String("repo.server")
	}
	k := prefix + ".repo.server."
	if _, ok := p.Lookup(k + "timeout.seconds"); ok {
		o.Timeout = time.Duration(p.Int(k+"timeout.seconds")) * time.Second
	}
	o.TLS = TLSClientOptions{
		Plaintext:      p.Bool(k + "plaintext"),
		StrictTLS:      p.Bool(k + "strict.tls"),
		CAFile:         p.String(k + "ca.cert.path"),
		ClientCertFile: p.String(k + "client.cert.path"),
		ClientKeyFile:  p.String(k + "client.cert.key.path"),
	}
	return o
}

// internalTransport is the HTTP transport of internal service clients: no environment
// proxy, bounded dials, optional TLS, trace context propagation.
func internalTransport(name string, tlsCfg *tls.Config) http.RoundTripper {
	t := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       tlsCfg,
	}
	return &tracingTransport{name: name, next: t}
}

// newRepoServerClient builds the repo-server HTTP client authenticated with the internal
// token.
func newRepoServerClient(o RepoServerClientOptions, token string) (*apiclient.HTTPClient, error) {
	if o.Address == "" {
		return nil, fmt.Errorf("repo-server address is empty")
	}
	tlsCfg, err := clientTLSConfig(o.TLS)
	if err != nil {
		return nil, fmt.Errorf("repo-server client TLS: %w", err)
	}
	opts := []apiclient.HTTPClientOption{apiclient.WithHTTPClient(&http.Client{Transport: internalTransport("repo-server", tlsCfg)})}
	if o.Timeout > 0 {
		opts = append(opts, apiclient.WithRequestTimeout(o.Timeout))
	}
	c, err := apiclient.NewHTTPClient(o.Address, token, tlsCfg, opts...)
	if err != nil {
		return nil, fmt.Errorf("repo-server client: %w", err)
	}
	return c, nil
}

// CommitServerClientOptions configure the source hydrator's commit-server client.
type CommitServerClientOptions struct {
	// Address is commit.server: host:port or http://host:port (plain HTTP) or
	// https://host:port (TLS).
	Address string
	// Timeout bounds one commit request end to end, including clone and push (0:
	// commitserver.DefaultClientTimeout). Hydration runs without a deadline, so this bound
	// is what frees a worker when the commit-server stops answering.
	Timeout time.Duration
	// TLS applies to an https address: CAFile verifies the commit-server certificate
	// (without it the certificate is not verified, like the repo-server client; the
	// internal token authenticates), ClientCertFile/ClientKeyFile present a client
	// certificate for a commit-server started with --tls-client-ca. Plaintext is derived
	// from the address.
	TLS TLSClientOptions
}

// newCommitServerClient builds the commit-server client authenticated with the internal
// token.
func newCommitServerClient(o CommitServerClientOptions, token string) (*commitserver.HTTPClient, error) {
	tlsOpts := o.TLS
	tlsOpts.Plaintext = !strings.HasPrefix(strings.ToLower(o.Address), "https://")
	tlsCfg, err := clientTLSConfig(tlsOpts)
	if err != nil {
		return nil, fmt.Errorf("commit-server client TLS: %w", err)
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = commitserver.DefaultClientTimeout
	}
	return commitserver.NewClient(o.Address, token, commitserver.WithHTTPClient(&http.Client{
		Transport: internalTransport("commit-server", tlsCfg), Timeout: timeout,
	}))
}
