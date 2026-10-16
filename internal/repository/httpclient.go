package repository

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// transportOptions are the per-repository settings applied to token endpoint requests.
type transportOptions struct {
	insecure       bool
	caData         []byte
	clientCertData string
	clientCertKey  string
	proxy          string
	noProxy        string
	// what names the endpoint in policy errors ("GitHub API").
	what string
	// allow restricts the host names requests may reach (nil: any host).
	allow hostPatterns
}

// safeProxy returns proxy when it passes upstream's ValidateHTTPProxyURL rules and drops it
// otherwise (upstream rejects unsafe proxies of GitHub App and Azure service principal
// credentials with a high-severity security log instead of failing).
func (r *Resolver) safeProxy(proxy, method string) string {
	if proxy == "" {
		return ""
	}
	if err := git.ValidateHTTPProxyURL(proxy); err != nil {
		r.logger.Warn("rejecting unsafe proxy URL for token requests",
			"security", 3, "CWE", 78, "method", method, "error", err)
		return ""
	}
	return proxy
}

// httpClient returns a client over the resolver's base transport with the given TLS and
// proxy settings and the endpoint policy: https only, o.allow when set, and the egress guard
// on every connection (the base transport's dialers are replaced). A base transport that is
// not an *http.Transport is used unchanged under the https and host checks.
func (r *Resolver) httpClient(o transportOptions, timeout time.Duration) (*http.Client, error) {
	what := o.what
	if what == "" {
		what = "token endpoint"
	}
	base, ok := r.transport.(*http.Transport)
	if !ok {
		return &http.Client{Transport: &policyTransport{base: r.transport, allow: o.allow, what: what}, Timeout: timeout}, nil
	}
	tr := base.Clone()
	tr.DialContext = r.guard.dialer().DialContext
	tr.DialTLSContext = nil
	var cfg *tls.Config
	if tr.TLSClientConfig != nil {
		cfg = tr.TLSClientConfig.Clone()
	} else {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if o.insecure {
		cfg.InsecureSkipVerify = true //nolint:gosec // explicit operator opt-in (spec.insecure), as upstream
	}
	if len(o.caData) > 0 {
		pool := cfg.RootCAs
		if pool == nil {
			sys, err := x509.SystemCertPool()
			if err != nil || sys == nil {
				sys = x509.NewCertPool()
			}
			pool = sys
		} else {
			pool = pool.Clone()
		}
		if !pool.AppendCertsFromPEM(o.caData) {
			return nil, configError(ErrInvalidConfiguration, "CA bundle contains no valid PEM certificate")
		}
		cfg.RootCAs = pool
	}
	if o.clientCertData != "" && o.clientCertKey != "" {
		pair, err := tls.X509KeyPair([]byte(o.clientCertData), []byte(o.clientCertKey))
		if err != nil {
			// The x509 error does not contain key material.
			return nil, fmt.Errorf("invalid TLS client certificate: %w: %w", ErrInvalidCredentials, err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	tr.TLSClientConfig = cfg
	if o.proxy != "" {
		pf, err := scm.ProxyFunc(o.proxy, o.noProxy)
		if err != nil {
			return nil, configError(ErrInvalidConfiguration, "invalid proxy: %s", redactString(err.Error(), []string{o.proxy}))
		}
		tr.Proxy = pf
	}
	return &http.Client{Transport: &policyTransport{base: tr, allow: o.allow, what: what}, Timeout: timeout}, nil
}

// logRepo returns the log attributes of a repository URL (credentials in the URL removed).
func logRepo(repoURL string) slog.Attr { return slog.String("repo", git.SanitizeURL(repoURL)) }
