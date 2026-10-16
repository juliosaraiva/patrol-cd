package core

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/internal/cli/transport"
	"github.com/juliosaraiva/patrol-cd/internal/server"
)

func testPod(name string, labels map[string]string, ready bool, owner string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patrol", Labels: labels},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	if owner != "" {
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: owner, UID: "u", Controller: ptr.To(true)}}
	}
	return p
}

var (
	repoLabels = map[string]string{transport.LabelAppName: "patrol-repo-server"}
	ctrlLabels = map[string]string{transport.LabelAppName: "patrol-application-controller"}
)

// dialRecorder is a DialPodFunc that records the pods dialed and fails for broken ones.
type dialRecorder struct {
	mu     sync.Mutex
	calls  []string
	broken map[string]bool
}

func (d *dialRecorder) dial(_ context.Context, ns, pod string, port int) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, fmt.Sprintf("%s/%s:%d", ns, pod, port))
	if d.broken[pod] {
		return nil, errors.New("port forward failed")
	}
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}

func (d *dialRecorder) last() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		return ""
	}
	return d.calls[len(d.calls)-1]
}

func TestPodRouterRepoServer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	notReady := testPod("patrol-repo-server-0", repoLabels, false, "")
	deleting := testPod("patrol-repo-server-1", repoLabels, true, "")
	deleting.DeletionTimestamp = ptr.To(metav1.Now())
	deleting.Finalizers = []string{"x"}
	cs := fake.NewClientset(notReady, deleting,
		testPod("patrol-repo-server-b", repoLabels, true, ""), testPod("patrol-repo-server-a", repoLabels, true, ""),
		testPod("other", map[string]string{transport.LabelAppName: "other"}, true, ""))
	d := &dialRecorder{broken: map[string]bool{}}
	r := newPodRouter(cs, "patrol", "patrol-repo-server", "patrol-application-controller", "patrol-controller", d.dial)

	c, err := r.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentRepoServer, Port: 8081})
	require.NoError(t, err)
	_ = c.Close()
	assert.Equal(t, "patrol/patrol-repo-server-a:8081", d.last(), "first ready pod by name")

	// Cached: no new selection; a failed dial selects again (pod restarted).
	d.broken["patrol-repo-server-a"] = true
	c, err = r.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentRepoServer, Port: 8081})
	require.NoError(t, err)
	_ = c.Close()
	assert.Equal(t, "patrol/patrol-repo-server-b:8081", d.last())
	c, err = r.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentRepoServer, Port: 8081})
	require.NoError(t, err)
	_ = c.Close()
	assert.Equal(t, "patrol/patrol-repo-server-b:8081", d.last(), "the working pod stays cached")

	// No ready pod: the message names the flags selecting the repo-server.
	r2 := newPodRouter(fake.NewClientset(notReady), "patrol", "patrol-repo-server", "", "patrol-controller", d.dial)
	_, err = r2.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentRepoServer, Port: 8081})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot find ready pod with selector: [app.kubernetes.io/name=patrol-repo-server] in namespace patrol")
	assert.Contains(t, err.Error(), "--repo-server-name")

	_, err = r.Dial(ctx, server.CoreDialTarget{Component: "nope"})
	require.Error(t, err)
}

func TestPodRouterController(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cs := fake.NewClientset(
		testPod("patrol-controller-0", ctrlLabels, true, ""),
		testPod("patrol-controller-1", nil, true, "patrol-controller"),
		testPod("patrol-controller-2", ctrlLabels, false, ""),
		testPod("impostor", map[string]string{transport.LabelAppName: "patrol-repo-server"}, true, ""),
		testPod("orphan", nil, true, "someone-else"),
	)
	d := &dialRecorder{broken: map[string]bool{}}
	r := newPodRouter(cs, "patrol", "patrol-repo-server", "patrol-application-controller", "patrol-controller", d.dial)
	for _, ok := range []string{"patrol-controller-0", "patrol-controller-1"} {
		c, err := r.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentController, Pod: ok, Port: 8090})
		require.NoError(t, err, ok)
		_ = c.Close()
		assert.Equal(t, "patrol/"+ok+":8090", d.last())
	}
	for pod, msg := range map[string]string{
		"patrol-controller-2": "is not ready",
		"impostor":            "is not a controller pod",
		"orphan":              "is not a controller pod",
		"missing":             "not found",
	} {
		before := len(d.calls)
		_, err := r.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentController, Pod: pod, Port: 8090})
		require.Error(t, err, pod)
		assert.Contains(t, err.Error(), msg, pod)
		assert.Len(t, d.calls, before, "%s: the internal token must not reach an unverified pod", pod)
	}

	// Verification is cached briefly; a failed dial drops it.
	now := time.Now()
	r.now = func() time.Time { return now }
	require.NoError(t, cs.CoreV1().Pods("patrol").Delete(ctx, "patrol-controller-0", metav1.DeleteOptions{}))
	c, err := r.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentController, Pod: "patrol-controller-0", Port: 8090})
	require.NoError(t, err, "verified moments ago")
	_ = c.Close()
	d.broken["patrol-controller-0"] = true
	_, err = r.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentController, Pod: "patrol-controller-0", Port: 8090})
	require.Error(t, err)
	_, err = r.Dial(ctx, server.CoreDialTarget{Component: server.CoreComponentController, Pod: "patrol-controller-0", Port: 8090})
	require.ErrorContains(t, err, "not found")
}

// fakeKubeAPI serves pods (list, get) and pods/portforward over WebSocket
// (v4.channel.k8s.io) without a kubelet: each stream is bridged to the TCP address of the
// pod's port in targets. Requests need the bearer token.
type fakeKubeAPI struct {
	srv     *httptest.Server
	token   string
	pods    []*corev1.Pod
	targets map[string]string // "<pod>:<port>" → address
	streams atomic.Int64
	refuse  map[string]bool // pods whose port-forward answers 403
}

func newFakeKubeAPI(t *testing.T, pods ...*corev1.Pod) *fakeKubeAPI {
	k := &fakeKubeAPI{token: "fake-kube-token", pods: pods, targets: map[string]string{}, refuse: map[string]bool{}}
	k.srv = httptest.NewTLSServer(http.HandlerFunc(k.serve))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *fakeKubeAPI) config() *rest.Config {
	return &rest.Config{Host: k.srv.URL, BearerToken: k.token, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
}

func (k *fakeKubeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+k.token {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// api/v1/namespaces/<ns>/pods[/<name>[/portforward]]
	if len(parts) < 5 || parts[0] != "api" || parts[2] != "namespaces" || parts[4] != "pods" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch len(parts) {
	case 5:
		sel := r.URL.Query().Get("labelSelector")
		list := corev1.PodList{TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"}}
		for _, p := range k.pods {
			k, v, _ := strings.Cut(sel, "=")
			if p.Namespace == parts[3] && p.Labels[k] == v {
				list.Items = append(list.Items, *p)
			}
		}
		_ = json.NewEncoder(w).Encode(list)
	case 6:
		for _, p := range k.pods {
			if p.Namespace == parts[3] && p.Name == parts[5] {
				_ = json.NewEncoder(w).Encode(p)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"pods \"`+parts[5]+`\" not found"}`)
	case 7:
		k.portForward(w, r, parts[5])
	}
}

func (k *fakeKubeAPI) portForward(w http.ResponseWriter, r *http.Request, pod string) {
	if k.refuse[pod] {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"pods \"`+pod+`\" is forbidden: User \"tester\" cannot create resource \"pods/portforward\""}`)
		return
	}
	port, _ := strconv.Atoi(r.URL.Query().Get("ports"))
	target, ok := k.targets[pod+":"+strconv.Itoa(port)]
	if !ok {
		http.Error(w, "no such port", http.StatusBadRequest)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v4.channel.k8s.io"}})
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	k.streams.Add(1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	header := make([]byte, 3)
	binary.LittleEndian.PutUint16(header[1:], uint16(port)) //nolint:gosec // test port
	for _, ch := range []byte{0, 1} {
		header[0] = ch
		if ws.Write(ctx, websocket.MessageBinary, header) != nil {
			return
		}
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", target)
	if err != nil {
		_ = ws.Write(ctx, websocket.MessageBinary, append([]byte{1}, []byte(err.Error())...))
		return
	}
	defer func() { _ = conn.Close() }()
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 && ws.Write(ctx, websocket.MessageBinary, append([]byte{0}, buf[:n]...)) != nil {
				return
			}
			if err != nil {
				_ = ws.CloseNow()
				return
			}
		}
	}()
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if len(msg) < 1 || msg[0] != 0 {
			continue
		}
		if _, err := conn.Write(msg[1:]); err != nil {
			return
		}
	}
}

// TestPortForwardRouting exercises the real port-forward path of core mode (PodDialer:
// WebSocket streams through the Kubernetes API, in-memory connections, no listener) with
// the pod selection and verification of podRouter against a fake API server, as far as
// envtest cannot go (no kubelet). Live verification on a kind cluster is pending.
func TestPortForwardRouting(t *testing.T) {
	t.Parallel()
	echo := func(name string) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "%s %s %s", name, r.URL.Path, r.Header.Get("Authorization"))
		}))
		t.Cleanup(s.Close)
		return s.Listener.Addr().String()
	}
	k := newFakeKubeAPI(t,
		testPod("patrol-repo-server-x", repoLabels, true, ""),
		testPod("patrol-controller-3", ctrlLabels, true, ""),
		testPod("patrol-controller-4", ctrlLabels, true, ""),
	)
	k.targets["patrol-repo-server-x:8081"] = echo("repo-server")
	k.targets["patrol-controller-3:8090"] = echo("controller-3")
	k.refuse["patrol-controller-4"] = true

	pd, err := transport.NewPodDialer(k.config(), nil)
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(k.config())
	require.NoError(t, err)
	r := newPodRouter(kube, "patrol", "patrol-repo-server", "patrol-application-controller", "patrol-controller", pd.DialPod)

	call := func(target server.CoreDialTarget) (string, error) {
		hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return r.Dial(ctx, target)
		}}}
		defer hc.CloseIdleConnections()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://internal/healthz", nil)
		req.Header.Set("Authorization", "Bearer internal")
		resp, err := hc.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	got, err := call(server.CoreDialTarget{Component: server.CoreComponentRepoServer, Port: 8081})
	require.NoError(t, err)
	assert.Equal(t, "repo-server /healthz Bearer internal", got)
	got, err = call(server.CoreDialTarget{Component: server.CoreComponentController, Pod: "patrol-controller-3", Port: 8090})
	require.NoError(t, err)
	assert.Equal(t, "controller-3 /healthz Bearer internal", got)
	assert.EqualValues(t, 2, k.streams.Load())

	// Kubernetes refuses the port-forward: the error names the pod and the status.
	_, err = call(server.CoreDialTarget{Component: server.CoreComponentController, Pod: "patrol-controller-4", Port: 8090})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port forward to pod patrol/patrol-controller-4 failed (HTTP 403)")

	// Close ends the streams; later dials fail.
	conn, err := pd.DialPod(t.Context(), "patrol", "patrol-repo-server-x", 8081)
	require.NoError(t, err)
	require.NoError(t, pd.Close())
	_, err = bufio.NewReader(conn).ReadByte()
	require.Error(t, err)
	_, err = pd.DialPod(t.Context(), "patrol", "patrol-repo-server-x", 8081)
	require.ErrorIs(t, err, transport.ErrPodDialerClosed)
	assert.True(t, slices.Contains([]int64{3}, k.streams.Load()))
}
