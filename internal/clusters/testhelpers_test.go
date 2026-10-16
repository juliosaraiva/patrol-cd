package clusters

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const (
	waitTimeout  = 20 * time.Second
	pollInterval = 50 * time.Millisecond
)

// apiServer is a fake Kubernetes API server answering /version and recording request headers.
type apiServer struct {
	*httptest.Server
	mu       sync.Mutex
	headers  []http.Header
	statuses []int // statuses to return before succeeding (consumed in order)
	version  string
}

func newAPIHandler(s *apiServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		status := http.StatusOK
		if len(s.statuses) > 0 {
			status, s.statuses = s.statuses[0], s.statuses[1:]
		}
		ver := s.version
		s.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, http.StatusText(status), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"36","gitVersion":"` + ver + `","platform":"linux/arm64"}`))
	})
}

// newTLSAPIServer starts a TLS fake API server (certificate valid for 127.0.0.1 and
// example.com).
func newTLSAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{version: "v1.36.1-eks-1234"}
	s.Server = httptest.NewUnstartedServer(newAPIHandler(s))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// newPlainAPIServer starts an HTTP fake API server.
func newPlainAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{version: "v1.36.0"}
	s.Server = httptest.NewServer(newAPIHandler(s))
	t.Cleanup(s.Close)
	return s
}

func (s *apiServer) lastHeader(t *testing.T) http.Header {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.headers, "no request reached the server")
	return s.headers[len(s.headers)-1]
}

func (s *apiServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.headers)
}

func (s *apiServer) caPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

// testPKI is a CA with helpers to issue server and client certificates.
type testPKI struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	caPEM []byte
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "patrol-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testPKI{ca: ca, caKey: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns PEM certificate and key signed by the CA.
func (p *testPKI) issue(t *testing.T, cn string, client bool, dns []string, ips []net.IP) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	require.NoError(t, err)
	kb, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// newMTLSAPIServer starts a fake API server that requires a client certificate issued by pki.
func newMTLSAPIServer(t *testing.T, pki *testPKI) *apiServer {
	t.Helper()
	certPEM, keyPEM := pki.issue(t, "server", false, []string{"cluster.test"}, []net.IP{net.ParseIP("127.0.0.1")})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(pki.ca)
	s := &apiServer{version: "v1.36.0"}
	s.Server = httptest.NewUnstartedServer(newAPIHandler(s))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func cluster(name, server string, mutate ...func(*v1alpha1.Cluster)) *v1alpha1.Cluster {
	c := &v1alpha1.Cluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol"},
		Spec:       v1alpha1.ClusterSpec{Name: name, Server: server},
	}
	for _, m := range mutate {
		m(c)
	}
	return c
}
