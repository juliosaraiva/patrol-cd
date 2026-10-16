package oci

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// TransportOptions configures the HTTP transport used to reach registries and Helm
// chart repositories.
type TransportOptions struct {
	// CAData is a PEM bundle. When set it replaces the system roots (upstream
	// semantics: the repository CA is the only trusted issuer for that repository).
	CAData []byte
	// CertData and KeyData are a PEM client certificate and key. Both must be set
	// for the certificate to be presented; one without the other is ignored, as
	// upstream does.
	CertData, KeyData []byte
	// InsecureSkipVerify disables server certificate verification.
	InsecureSkipVerify bool
	// Proxy is an explicit proxy URL for every request. Empty means the process
	// environment (HTTPS_PROXY, HTTP_PROXY, NO_PROXY) applies.
	Proxy string
	// NoProxy lists hosts that bypass Proxy (NO_PROXY syntax). Only used when Proxy
	// is set.
	NoProxy string
}

// NewTransport returns an HTTP transport with the TLS and proxy settings of opts.
// Keep-alives are disabled, matching the upstream repository clients, so a
// transport per credential set does not leak idle connections.
func NewTransport(opts TransportOptions) (*http.Transport, error) {
	tlsConf, err := newTLSConfig(opts)
	if err != nil {
		return nil, err
	}
	proxy := http.ProxyFromEnvironment
	if opts.Proxy != "" {
		cfg := &httpproxy.Config{HTTPProxy: opts.Proxy, HTTPSProxy: opts.Proxy, NoProxy: opts.NoProxy}
		fn := cfg.ProxyFunc()
		proxy = func(r *http.Request) (*url.URL, error) { return fn(r.URL) }
	}
	return &http.Transport{
		Proxy:                 proxy,
		TLSClientConfig:       tlsConf,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		ExpectContinueTimeout: time.Second,
	}, nil
}

func newTLSConfig(opts TransportOptions) (*tls.Config, error) {
	conf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: opts.InsecureSkipVerify, //nolint:gosec // explicit per-repository opt-in
	}
	if len(opts.CAData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(opts.CAData) {
			return nil, fmt.Errorf("%w: no PEM certificate found in CA data", ErrInvalidTLSConfig)
		}
		conf.RootCAs = pool
	}
	if len(opts.CertData) > 0 && len(opts.KeyData) > 0 {
		cert, err := tls.X509KeyPair(opts.CertData, opts.KeyData)
		if err != nil {
			// The parse error never contains key material.
			return nil, fmt.Errorf("%w: client certificate: %w", ErrInvalidTLSConfig, err)
		}
		conf.Certificates = []tls.Certificate{cert}
	}
	return conf, nil
}
