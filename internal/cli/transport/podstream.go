package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ErrPodDialerClosed is returned by DialPod after Close.
var ErrPodDialerClosed = errors.New("pod dialer closed")

// PodDialer opens connections to pod ports through the Kubernetes API server
// (pods/portforward over WebSocket, the machinery of PortForward) without a local
// listener: every DialPod returns an in-memory net.Conn backed by its own stream, so
// nothing is reachable by other local processes. It authenticates with the given
// configuration (the caller's kubeconfig credentials) and needs get/create on
// pods/portforward of the target namespace. Core mode uses it to reach the repo-server and
// the controller tree API.
type PodDialer struct {
	hc  *http.Client
	cs  kubernetes.Interface
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// NewPodDialer returns a dialer using cfg; log (may be nil) receives stream failures.
func NewPodDialer(cfg *rest.Config, log *slog.Logger) (*PodDialer, error) {
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes HTTP client: %w", err)
	}
	cs, err := kubernetes.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PodDialer{hc: hc, cs: cs, log: log, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}, nil
}

// DialPod opens a stream to port of pod namespace/name. ctx bounds the stream setup only;
// the connection lives until it is closed, the remote side ends it, or the dialer is
// closed.
func (d *PodDialer) DialPod(ctx context.Context, namespace, pod string, port int) (net.Conn, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("port forward to pod %s/%s: invalid port %d", namespace, pod, port)
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, ErrPodDialerClosed
	}
	d.mu.Unlock()
	u := d.cs.CoreV1().RESTClient().Get().Namespace(namespace).Resource("pods").Name(pod).
		SubResource("portforward").Param("ports", strconv.Itoa(port)).URL()
	f := &forwarder{hc: d.hc, url: u.String(), port: port, log: d.log, pod: namespace + "/" + pod}
	// The stream context outlives ctx (the handshake request must not be canceled with the
	// caller's dial context once the stream is established); ctx only bounds the setup.
	sctx, cancel := context.WithCancel(d.ctx)
	unwatch := context.AfterFunc(ctx, cancel)
	ws, err := f.dial(sctx)
	if !unwatch() || err != nil {
		cancel()
		if err == nil {
			_ = ws.CloseNow()
			err = ctx.Err()
		}
		return nil, err
	}
	local, remote := net.Pipe()
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		cancel()
		_ = ws.CloseNow()
		_ = local.Close()
		_ = remote.Close()
		return nil, ErrPodDialerClosed
	}
	d.conns[remote] = struct{}{}
	d.wg.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.wg.Done()
		defer cancel()
		if err := f.pipe(sctx, remote, ws); err != nil && sctx.Err() == nil {
			d.log.Debug("port forward stream ended", slog.String("pod", f.pod), slog.Any("error", err))
		}
		_ = remote.Close()
		_ = ws.CloseNow()
		d.mu.Lock()
		delete(d.conns, remote)
		d.mu.Unlock()
	}()
	return local, nil
}

// Close ends every stream and waits for them; later dials fail with ErrPodDialerClosed.
func (d *PodDialer) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	for c := range d.conns {
		_ = c.Close()
	}
	d.mu.Unlock()
	d.cancel()
	d.wg.Wait()
	utilnet.CloseIdleConnectionsFor(d.hc.Transport)
	return nil
}
