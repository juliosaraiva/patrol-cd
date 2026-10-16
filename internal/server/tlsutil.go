package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
)

// maxPEMBytes bounds certificate, key and CA files.
const maxPEMBytes = 4 << 20

// readPEMFile reads a bounded PEM file.
func readPEMFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPEMBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPEMBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxPEMBytes)
	}
	return data, nil
}

// certPool reads a PEM CA bundle.
func certPool(path string) (*x509.CertPool, error) {
	data, err := readPEMFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("CA bundle %s contains no certificate", path)
	}
	return pool, nil
}

// fileCertificate serves a certificate/key pair from files, re-reading them when their
// modification time or size changes (mounted Secrets are updated in place), so a rotated
// certificate is used without a restart. A failed reload keeps the previous pair.
type fileCertificate struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   string
	checked time.Time
}

// recheckInterval bounds how often the files are stat'ed.
const recheckInterval = 10 * time.Second

func newFileCertificate(certFile, keyFile string) (*fileCertificate, error) {
	fc := &fileCertificate{certFile: certFile, keyFile: keyFile}
	if _, err := fc.load(); err != nil {
		return nil, err
	}
	return fc, nil
}

func fileStamp(paths ...string) (string, error) {
	var b strings.Builder
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s:%d:%d;", p, st.ModTime().UnixNano(), st.Size())
	}
	return b.String(), nil
}

func (fc *fileCertificate) load() (*tls.Certificate, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	now := time.Now()
	if fc.cert != nil && now.Sub(fc.checked) < recheckInterval {
		return fc.cert, nil
	}
	fc.checked = now
	stamp, err := fileStamp(fc.certFile, fc.keyFile)
	if err != nil {
		if fc.cert != nil {
			return fc.cert, nil
		}
		return nil, fmt.Errorf("TLS certificate: %w", err)
	}
	if fc.cert != nil && stamp == fc.stamp {
		return fc.cert, nil
	}
	certPEM, err := readPEMFile(fc.certFile)
	if err != nil {
		return fc.keep(err)
	}
	keyPEM, err := readPEMFile(fc.keyFile)
	if err != nil {
		return fc.keep(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fc.keep(fmt.Errorf("parse key pair: %w", err))
	}
	fc.cert, fc.stamp = &pair, stamp
	return fc.cert, nil
}

func (fc *fileCertificate) keep(err error) (*tls.Certificate, error) {
	if fc.cert != nil {
		return fc.cert, nil
	}
	return nil, fmt.Errorf("TLS certificate: %w", err)
}

// get returns the current pair (nil when it never loaded).
func (fc *fileCertificate) get() *tls.Certificate {
	c, err := fc.load()
	if err != nil {
		return nil
	}
	return c
}

// TLSServerOptions configure an internal TLS listener.
type TLSServerOptions struct {
	// CertFile and KeyFile are the serving pair (both or neither).
	CertFile, KeyFile string
	// ClientCAFile enables mutual TLS: client certificates are required and verified.
	ClientCAFile string
	// MinVersion, MaxVersion ("1.2", "1.3") and colon-separated TLS 1.2 Ciphers.
	MinVersion, MaxVersion, Ciphers string
}

// Enabled reports whether a certificate pair is configured.
func (o TLSServerOptions) Enabled() bool { return o.CertFile != "" || o.KeyFile != "" }

// serverTLSConfig builds a listener configuration: versions and ciphers, the certificate
// pair (reloaded on change) and mutual TLS with ClientCAFile. A missing ClientCAFile file
// disables mutual TLS (upstream: an absent mounted CA means no mTLS).
func serverTLSConfig(o TLSServerOptions) (*tls.Config, error) {
	if (o.CertFile == "") != (o.KeyFile == "") {
		return nil, errors.New("--tls-cert and --tls-key must be set together")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if err := applyTLSVersions(cfg, o.MinVersion, o.MaxVersion, o.Ciphers); err != nil {
		return nil, err
	}
	if o.CertFile != "" {
		fc, err := newFileCertificate(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, err
		}
		cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return fc.load() }
	}
	if o.ClientCAFile != "" {
		pool, err := certPool(o.ClientCAFile)
		switch {
		case err == nil:
			cfg.ClientCAs = pool
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		case errors.Is(err, fs.ErrNotExist):
		default:
			return nil, err
		}
	}
	return cfg, nil
}

// applyTLSVersions sets the version range and TLS 1.2 cipher suites.
func applyTLSVersions(cfg *tls.Config, minV, maxV, ciphers string) error {
	if minV != "" {
		v, err := apiserver.ParseTLSVersion(minV)
		if err != nil {
			return err
		}
		cfg.MinVersion = v
	}
	if maxV != "" {
		v, err := apiserver.ParseTLSVersion(maxV)
		if err != nil {
			return err
		}
		cfg.MaxVersion = v
	}
	if cfg.MaxVersion != 0 && cfg.MaxVersion < cfg.MinVersion {
		return errors.New("TLS max version is lower than the min version")
	}
	if c := splitCiphers(ciphers); len(c) > 0 {
		ids, err := apiserver.ParseCipherSuites(c)
		if err != nil {
			return err
		}
		cfg.CipherSuites = ids
	}
	return nil
}

// DefaultTLSCiphers is the compiled default of server.tls.ciphers and reposerver.tls.ciphers
// (upstream DefaultTLSCipherSuite).
const DefaultTLSCiphers = "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"

// http2CipherSuites are the TLS 1.2 suites HTTP/2 requires (RFC 9113 section 9.2.2). net/http
// refuses to serve a listener that offers h2 with an explicit TLS 1.2 list containing neither.
var http2CipherSuites = []string{"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"}

// apiCipherSuites returns the TLS 1.2 cipher suites of patrol-server's HTTPS listener, which
// offers HTTP/2:
//   - minimum version 1.3: none (upstream: TLS 1.3 suites are not configurable); ignored
//     reports a configured list other than the default, which upstream warns about;
//   - a list without an HTTP/2-required suite (the upstream default is one): both
//     http2CipherSuites are appended and reported in added. Otherwise the listener cannot
//     start, and TLS 1.2 clients could not use the generated ECDSA certificate with the
//     RSA-only default;
//   - an empty list: none (Go's defaults).
func apiCipherSuites(minVersion, ciphers string) (suites []string, ignored bool, added []string, err error) {
	minV, err := apiserver.ParseTLSVersion(orDefault(minVersion, "1.2"))
	if err != nil {
		return nil, false, nil, err
	}
	names := splitCiphers(ciphers)
	if minV >= tls.VersionTLS13 {
		return nil, len(names) > 0 && ciphers != DefaultTLSCiphers, nil, nil
	}
	if len(names) == 0 {
		return nil, false, nil, nil
	}
	if _, err := apiserver.ParseCipherSuites(names); err != nil {
		return nil, false, nil, err
	}
	for _, n := range names {
		if slices.Contains(http2CipherSuites, n) {
			return names, false, nil, nil
		}
	}
	return append(names, http2CipherSuites...), false, http2CipherSuites, nil
}

func splitCiphers(s string) []string {
	var out []string
	for c := range strings.SplitSeq(s, ":") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// TLSClientOptions configure a client of an internal service (upstream
// --repo-server-plaintext / --repo-server-strict-tls / --repo-server-ca-cert-path /
// --repo-server-client-cert-path / --repo-server-client-cert-key-path).
type TLSClientOptions struct {
	// Plaintext connects without TLS.
	Plaintext bool
	// StrictTLS verifies the server certificate against the system roots when CAFile is
	// empty (deprecated upstream; CAFile implies verification).
	StrictTLS bool
	// CAFile verifies the server certificate against this bundle.
	CAFile string
	// ClientCertFile and ClientKeyFile present a client certificate (mTLS) when both files
	// exist.
	ClientCertFile, ClientKeyFile string
	// ServerName overrides the verified host name.
	ServerName string
}

// clientTLSConfig returns nil for plaintext. Without CAFile or StrictTLS the server
// certificate is not verified (upstream default: internal services use self-signed
// certificates); the shared token still authenticates the server's peer.
func clientTLSConfig(o TLSClientOptions) (*tls.Config, error) {
	if o.Plaintext {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: o.ServerName}
	switch {
	case o.CAFile != "":
		pool, err := certPool(o.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	case o.StrictTLS:
	default:
		cfg.InsecureSkipVerify = true //nolint:gosec // upstream default for internal services without a configured CA; see TLSClientOptions
	}
	if o.ClientCertFile != "" && o.ClientKeyFile != "" && fileExists(o.ClientCertFile) && fileExists(o.ClientKeyFile) {
		fc, err := newFileCertificate(o.ClientCertFile, o.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return fc.load() }
	}
	return cfg, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
