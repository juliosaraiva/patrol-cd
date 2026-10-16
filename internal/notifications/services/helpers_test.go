package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func testDeps() Deps {
	return Deps{
		Network: Network{
			Retry:                       &RetryPolicy{MaxRetries: 2, WaitMin: time.Millisecond, WaitMax: 5 * time.Millisecond},
			Timeout:                     5 * time.Second,
			DisableProxyFromEnvironment: true,
		},
		Now: func() time.Time { return testNow },
	}
}

type captured struct {
	Method   string
	Path     string
	RawPath  string
	RawQuery string
	Header   http.Header
	Body     []byte
}

func (c captured) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("request body is not a JSON object: %v\n%s", err, c.Body)
	}
	return m
}

type recorder struct {
	mu   sync.Mutex
	reqs []captured
}

func (r *recorder) add(req *http.Request) captured {
	body, _ := io.ReadAll(req.Body)
	c := captured{Method: req.Method, Path: req.URL.Path, RawPath: req.URL.EscapedPath(), RawQuery: req.URL.RawQuery, Header: req.Header.Clone(), Body: body}
	r.mu.Lock()
	r.reqs = append(r.reqs, c)
	r.mu.Unlock()
	return c
}

func (r *recorder) all() []captured {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]captured(nil), r.reqs...)
}

// newServer starts an httptest server that records every request and delegates the
// response to h.
func newServer(t *testing.T, h func(w http.ResponseWriter, c captured)) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := rec.add(r)
		h(w, c)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newTLSRecordingServer is newServer over HTTPS (self-signed certificate).
func newTLSRecordingServer(t *testing.T, h func(w http.ResponseWriter, c captured)) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := rec.add(r)
		h(w, c)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newTLSServer starts a recording HTTPS server with a self-signed certificate.
func newTLSServer(t *testing.T) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func mustNew(t *testing.T, typ string, opts any, deps Deps) Service {
	t.Helper()
	raw, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(typ, raw, deps)
	if err != nil {
		t.Fatalf("New(%s): %v", typ, err)
	}
	return svc
}

func send(t *testing.T, svc Service, n Notification, d Destination) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return svc.Send(ctx, n, d)
}

func mustSend(t *testing.T, svc Service, n Notification, d Destination) {
	t.Helper()
	if err := send(t, svc, n, d); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("field %q missing or not a string in %v", key, m)
	}
	return v
}

// selfSignedCert returns a certificate for 127.0.0.1/localhost and a pool trusting it.
func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, pool
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
