package apiserver

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TLSOptions configures the HTTPS listener.
type TLSOptions struct {
	// MinVersion and MaxVersion are "1.2" or "1.3" (default 1.2 and 1.3).
	MinVersion string
	MaxVersion string
	// Ciphers are IANA TLS 1.2 cipher suite names joined by ":" in upstream flags; empty
	// selects Go's secure defaults. TLS 1.3 suites are not configurable.
	Ciphers []string
	// Certificate returns the serving certificate for each handshake (e.g. from
	// patrol-secret tls.crt/tls.key, so rotation needs no restart); nil or a nil result
	// falls back to a self-signed certificate generated at startup.
	Certificate func() *tls.Certificate
	// Hosts are the DNS names and IPs of the self-signed certificate (default localhost).
	Hosts []string
}

// ParseTLSVersion maps "1.0".."1.3" to the crypto/tls constant.
func ParseTLSVersion(v string) (uint16, error) {
	switch strings.TrimSpace(v) {
	case "1.0":
		return tls.VersionTLS10, nil
	case "1.1":
		return tls.VersionTLS11, nil
	case "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	}
	return 0, fmt.Errorf("unsupported TLS version %q (use 1.0, 1.1, 1.2 or 1.3)", v)
}

// ParseCipherSuites maps IANA cipher suite names to IDs; insecure suites are rejected.
func ParseCipherSuites(names []string) ([]uint16, error) {
	known := map[string]uint16{}
	for _, c := range tls.CipherSuites() {
		known[c.Name] = c.ID
	}
	insecure := map[string]bool{}
	for _, c := range tls.InsecureCipherSuites() {
		insecure[c.Name] = true
	}
	var out []uint16
	var errs []error
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if insecure[n] {
			errs = append(errs, fmt.Errorf("cipher suite %s is insecure", n))
			continue
		}
		id, ok := known[n]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown cipher suite %s", n))
			continue
		}
		out = append(out, id)
	}
	return out, errors.Join(errs...)
}

// buildTLSConfig returns the server TLS configuration.
func buildTLSConfig(o TLSOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	if o.MinVersion != "" {
		v, err := ParseTLSVersion(o.MinVersion)
		if err != nil {
			return nil, err
		}
		cfg.MinVersion = v
	}
	if o.MaxVersion != "" {
		v, err := ParseTLSVersion(o.MaxVersion)
		if err != nil {
			return nil, err
		}
		cfg.MaxVersion = v
	}
	if cfg.MaxVersion != 0 && cfg.MaxVersion < cfg.MinVersion {
		return nil, errors.New("TLS max version is lower than the min version")
	}
	if len(o.Ciphers) > 0 {
		ids, err := ParseCipherSuites(o.Ciphers)
		if err != nil {
			return nil, err
		}
		cfg.CipherSuites = ids
	}
	hosts := o.Hosts
	if len(hosts) == 0 {
		hosts = []string{"localhost"}
	}
	var (
		once     sync.Once
		selfCert *tls.Certificate
		selfErr  error
	)
	selfSigned := func() (*tls.Certificate, error) {
		once.Do(func() { selfCert, selfErr = GenerateSelfSigned(hosts, 365*24*time.Hour) })
		return selfCert, selfErr
	}
	cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		if o.Certificate != nil {
			if c := o.Certificate(); c != nil {
				return c, nil
			}
		}
		return selfSigned()
	}
	return cfg, nil
}

// GenerateSelfSigned returns an ECDSA P-256 self-signed certificate for hosts (DNS names
// or IP addresses) valid for validity.
func GenerateSelfSigned(hosts []string, validity time.Duration) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Patrol CD"}, CommonName: "patrol-server"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// sniffTimeout bounds the wait for the first byte of a connection.
const sniffTimeout = 10 * time.Second

// splitListener classifies accepted connections by their first byte: TLS handshakes
// (0x16) go to the TLS listener, anything else (plain HTTP on the TLS port) to the plain
// listener, which serves a redirect to https.
type splitListener struct {
	inner     net.Listener
	tlsCh     chan net.Conn
	plainCh   chan net.Conn
	done      chan struct{}
	once      sync.Once
	closeOnce sync.Once
	err       error
	errMu     sync.Mutex
}

func newSplitListener(l net.Listener) *splitListener {
	s := &splitListener{inner: l, tlsCh: make(chan net.Conn), plainCh: make(chan net.Conn), done: make(chan struct{})}
	go s.run()
	return s
}

func (s *splitListener) run() {
	for {
		c, err := s.inner.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			s.errMu.Lock()
			s.err = err
			s.errMu.Unlock()
			s.close()
			return
		}
		go s.classify(c)
	}
}

func (s *splitListener) classify(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(sniffTimeout))
	br := bufio.NewReaderSize(c, 1)
	b, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		_ = c.Close()
		return
	}
	pc := &peekedConn{Conn: c, r: br}
	ch := s.plainCh
	if b[0] == 0x16 {
		ch = s.tlsCh
	}
	select {
	case ch <- pc:
	case <-s.done:
		_ = c.Close()
	}
}

func (s *splitListener) close() {
	s.once.Do(func() { close(s.done) })
}

func (s *splitListener) acceptFrom(ch chan net.Conn) (net.Conn, error) {
	select {
	case c := <-ch:
		return c, nil
	case <-s.done:
		s.errMu.Lock()
		defer s.errMu.Unlock()
		if s.err != nil {
			return nil, s.err
		}
		return nil, net.ErrClosed
	}
}

// Close closes the underlying listener. Both sub-listeners share it, and each owning
// http.Server closes its listener on Shutdown, so Close is idempotent: only the first
// call closes the socket and reports its error; later calls return nil instead of
// "use of closed network connection".
func (s *splitListener) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.close()
		err = s.inner.Close()
	})
	return err
}

type subListener struct {
	parent *splitListener
	ch     chan net.Conn
}

func (l subListener) Accept() (net.Conn, error) { return l.parent.acceptFrom(l.ch) }
func (l subListener) Close() error              { return l.parent.Close() }
func (l subListener) Addr() net.Addr            { return l.parent.inner.Addr() }

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// httpsRedirect answers plain HTTP requests on the TLS port with 307 to the same URL
// over https; the root path is part of the request path already.
func httpsRedirect() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	})
}
