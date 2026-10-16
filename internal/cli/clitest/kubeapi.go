package clitest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// FakeKubeAPI is a minimal Kubernetes API server for --port-forward tests: it lists one
// Running and Ready pod labelled app.kubernetes.io/name=<ServerName> in Namespace and
// serves the pods/portforward subresource over WebSocket (v4.channel.k8s.io), bridging
// each stream to Target (normally Env.Addr). Requests must carry Token.
type FakeKubeAPI struct {
	URL, Namespace, ServerName, PodName, Token string
	CAPEM                                      []byte
	// Target is the address the forwarded port reaches.
	Target string
	// Port is the container port the forward must request (8080).
	Port int
	// Streams counts the port-forward streams opened.
	Streams atomic.Int64
}

// NewFakeKubeAPI starts a FakeKubeAPI forwarding to target.
func NewFakeKubeAPI(t testing.TB, target string) *FakeKubeAPI {
	t.Helper()
	k := &FakeKubeAPI{Namespace: "patrol", ServerName: "patrol-server", PodName: "patrol-server-7d9f-abcde", Token: "kube-" + randomToken(), Target: target, Port: 8080}
	srv := httptest.NewTLSServer(http.HandlerFunc(k.serve))
	t.Cleanup(srv.Close)
	k.URL = srv.URL
	k.CAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return k
}

// WriteKubeconfig writes a kubeconfig whose current context targets the API with the
// given default namespace and returns its path.
func (k *FakeKubeAPI) WriteKubeconfig(t testing.TB, dir, namespace string) string {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["fake"] = &clientcmdapi.Cluster{Server: k.URL, CertificateAuthorityData: k.CAPEM}
	cfg.AuthInfos["user"] = &clientcmdapi.AuthInfo{Token: k.Token}
	cfg.Contexts["fake"] = &clientcmdapi.Context{Cluster: "fake", AuthInfo: "user", Namespace: namespace}
	cfg.CurrentContext = "fake"
	p := filepath.Join(dir, "kubeconfig-portforward")
	if err := clientcmd.WriteToFile(*cfg, p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (k *FakeKubeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+k.Token {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	podsPath := "/api/v1/namespaces/" + k.Namespace + "/pods"
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/") && strings.HasSuffix(r.URL.Path, "/pods") && r.Method == http.MethodGet:
		// Other namespaces exist but hold no matching pod.
		list := corev1.PodList{TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"}}
		if r.URL.Path == podsPath && r.URL.Query().Get("labelSelector") == "app.kubernetes.io/name="+k.ServerName {
			list.Items = []corev1.Pod{{
				TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: k.PodName, Namespace: k.Namespace, Labels: map[string]string{"app.kubernetes.io/name": k.ServerName}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				}},
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	case r.URL.Path == podsPath+"/"+k.PodName+"/portforward":
		k.portForward(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	default:
		http.NotFound(w, r)
	}
}

func (k *FakeKubeAPI) portForward(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("ports") != strconv.Itoa(k.Port) {
		http.Error(w, "unexpected port", http.StatusBadRequest)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v4.channel.k8s.io"}})
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	k.Streams.Add(1)
	header := make([]byte, 3)
	binary.LittleEndian.PutUint16(header[1:], uint16(k.Port)) //nolint:gosec // small port number
	for _, ch := range []byte{0, 1} {
		header[0] = ch
		if err := ws.Write(ctx, websocket.MessageBinary, header); err != nil {
			return
		}
	}
	var conn net.Conn
	dial := func() bool {
		if conn != nil {
			return true
		}
		c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", k.Target)
		if err != nil {
			_ = ws.Write(ctx, websocket.MessageBinary, append([]byte{1}, []byte("dial target: "+err.Error())...))
			return false
		}
		conn = c
		go func() {
			defer cancel()
			buf := make([]byte, 32<<10)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					if werr := ws.Write(ctx, websocket.MessageBinary, append([]byte{0}, buf[:n]...)); werr != nil {
						return
					}
				}
				if err != nil {
					// The client side is gone or the target closed: no close handshake.
					_ = ws.CloseNow()
					return
				}
			}
		}()
		return true
	}
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if len(msg) < 2 || msg[0] != 0 {
			continue
		}
		if !dial() {
			return
		}
		if _, err := conn.Write(msg[1:]); err != nil {
			return
		}
	}
}
