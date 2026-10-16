package clitest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// FakeCluster is a destination cluster for `cluster add/rm/rotate-auth` tests: an HTTPS
// reverse proxy in front of the envtest API server that authenticates bearer tokens itself
// (envtest cannot validate legacy service-account tokens) and forwards authenticated
// requests with the envtest administrator's credentials. Accepted tokens are AdminToken
// and the tokens of kubernetes.io/service-account-token Secrets in the token namespace,
// which a token-controller emulation fills with legacy-shaped JWTs (iss
// kubernetes/serviceaccount, namespace, secret and service-account claims).
type FakeCluster struct {
	// URL is the cluster's API server URL (https://127.0.0.1:port); CAPEM its CA.
	URL   string
	CAPEM []byte
	// AdminToken authenticates as cluster administrator (the kubeconfig credential).
	AdminToken string
	// Requests counts proxied and rejected requests.
	Requests atomic.Int64

	e       *Env
	tokenNS string
	key     []byte
}

// NewFakeCluster starts a FakeCluster whose token controller serves tokenNS (the
// patrol-manager system namespace, kube-system by default).
func (e *Env) NewFakeCluster(tokenNS string) *FakeCluster {
	e.T.Helper()
	target, err := url.Parse(e.RestConfig.Host)
	e.must(err)
	rt, err := rest.TransportFor(e.RestConfig)
	e.must(err)
	fc := &FakeCluster{e: e, tokenNS: tokenNS, AdminToken: "admin-" + randomToken(), key: []byte(randomToken())}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Header.Del("Authorization")
		},
		Transport: rt,
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.Requests.Add(1)
		if !fc.authenticated(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	e.T.Cleanup(srv.Close)
	fc.URL = srv.URL
	fc.CAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	fc.startTokenController()
	return fc
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (fc *FakeCluster) authenticated(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return false
	}
	if tok == fc.AdminToken {
		return true
	}
	list, err := fc.e.Clientset.CoreV1().Secrets(fc.tokenNS).List(r.Context(), metav1.ListOptions{FieldSelector: "type=" + string(corev1.SecretTypeServiceAccountToken)})
	if err != nil {
		return false
	}
	for _, s := range list.Items {
		if string(s.Data[corev1.ServiceAccountTokenKey]) == tok {
			return true
		}
	}
	return false
}

// startTokenController fills service-account token Secrets of the token namespace.
func (fc *FakeCluster) startTokenController() {
	ctx, cancel := context.WithCancel(fc.e.ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		secrets := fc.e.Clientset.CoreV1().Secrets(fc.tokenNS)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			list, err := secrets.List(ctx, metav1.ListOptions{FieldSelector: "type=" + string(corev1.SecretTypeServiceAccountToken)})
			if err != nil {
				continue
			}
			for i := range list.Items {
				s := &list.Items[i]
				if len(s.Data[corev1.ServiceAccountTokenKey]) > 0 {
					continue
				}
				sa := s.Annotations[corev1.ServiceAccountNameKey]
				tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
					"iss":                                    "kubernetes/serviceaccount",
					"sub":                                    clusterlib.ServiceAccountUsername(fc.tokenNS, sa),
					"kubernetes.io/serviceaccount/namespace": fc.tokenNS,
					"kubernetes.io/serviceaccount/secret.name":          s.Name,
					"kubernetes.io/serviceaccount/service-account.name": sa,
					"kubernetes.io/serviceaccount/service-account.uid":  "uid-" + sa,
				}).SignedString(fc.key)
				if err != nil {
					continue
				}
				if s.Data == nil {
					s.Data = map[string][]byte{}
				}
				s.Data[corev1.ServiceAccountTokenKey] = []byte(tok)
				s.Data["namespace"] = []byte(fc.tokenNS)
				_, _ = secrets.Update(ctx, s, metav1.UpdateOptions{})
			}
		}
	})
	fc.e.T.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// WriteKubeconfig writes a kubeconfig with one context per name, all pointing at the
// FakeCluster with the administrator token, and returns its path. The first name is the
// current context.
func (fc *FakeCluster) WriteKubeconfig(dir string, contexts ...string) string {
	fc.e.T.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["fake"] = &clientcmdapi.Cluster{Server: fc.URL, CertificateAuthorityData: fc.CAPEM}
	cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: fc.AdminToken}
	for i, name := range contexts {
		cfg.Contexts[name] = &clientcmdapi.Context{Cluster: "fake", AuthInfo: "admin"}
		if i == 0 {
			cfg.CurrentContext = name
		}
	}
	p := filepath.Join(dir, "kubeconfig")
	fc.e.must(clientcmd.WriteToFile(*cfg, p))
	fc.e.must(os.Chmod(p, 0o600))
	return p
}
