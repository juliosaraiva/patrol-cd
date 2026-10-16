package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Port-forward protocol constants (kubelet streaming server, "v4.channel.k8s.io"): every
// forwarded port has a data channel (2i) and an error channel (2i+1); each binary message
// starts with its channel number, and the first message of each channel carries the
// port number (2 bytes, little endian).
const (
	portForwardProtocol = "v4.channel.k8s.io"
	dataChannel         = 0
	errorChannel        = 1
	// DefaultServerPort is the patrol-server container port (TLS unless the server runs
	// with --insecure).
	DefaultServerPort = 8080
	// LabelAppName selects component pods.
	LabelAppName = "app.kubernetes.io/name"
	// maxFrameBytes bounds one websocket message from the API server.
	maxFrameBytes = 1 << 20
	// maxErrorBytes bounds the error text read from the error channel.
	maxErrorBytes = 4 << 10
)

// PortForward forwards a random local port to a ready patrol-server pod through the
// Kubernetes API server (pods/portforward over WebSocket, channel protocol v4). Each local
// TCP connection gets its own WebSocket stream. It needs get/list on pods and get/create
// on pods/portforward in the Patrol namespace.
type PortForward struct {
	// TargetPort is the pod port (default DefaultServerPort).
	TargetPort int
	// Timeout bounds pod selection and the initial stream check (default 30 s).
	Timeout time.Duration
}

// Connect implements Provider. The returned connection always skips certificate
// verification (the forwarded server presents its own, usually self-signed, certificate
// for the cluster-internal name), as upstream --port-forward does.
func (p *PortForward) Connect(ctx context.Context, req Request) (*Connection, error) {
	log := req.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	port := p.TargetPort
	if port <= 0 {
		port = DefaultServerPort
	}
	cc := req.Kube.ClientConfig()
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	ns := req.Kube.Namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil {
			return nil, fmt.Errorf("determine the namespace from the kubeconfig: %w", err)
		}
	}
	name := req.Kube.ServerName
	if name == "" {
		name = "patrol-server"
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	selector := LabelAppName + "=" + name
	pod, err := selectReadyPod(sctx, cs, ns, selector)
	if err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes HTTP client: %w", err)
	}
	u := cs.CoreV1().RESTClient().Get().Namespace(pod.Namespace).Resource("pods").Name(pod.Name).
		SubResource("portforward").Param("ports", strconv.Itoa(port)).URL()
	f := &forwarder{hc: hc, url: u.String(), port: port, log: log, pod: pod.Namespace + "/" + pod.Name}
	// Fail fast (permissions, unsupported protocol) before handing out the address.
	ws, err := f.dial(sctx)
	if err != nil {
		return nil, err
	}
	_ = ws.CloseNow()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on a local port: %w", err)
	}
	f.ln = ln
	f.done = make(chan struct{})
	go f.serve()
	log.LogAttrs(ctx, slog.LevelDebug, "port forwarding", slog.String("local", ln.Addr().String()), slog.String("pod", f.pod), slog.Int("port", port))
	return NewConnection(Connection{ServerAddr: ln.Addr().String(), PlainText: req.PlainText, Insecure: true}, f.close), nil
}

func selectReadyPod(ctx context.Context, cs kubernetes.Interface, ns, selector string) (*corev1.Pod, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods with selector %s in namespace %s: %w", selector, ns, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && podReady(p) && p.DeletionTimestamp == nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("cannot find ready pod with selector: [%s] in namespace %s - use --server-name (or PATROL_SERVER_NAME) and --port-forward-namespace to select the patrol-server pods", selector, ns)
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

type forwarder struct {
	hc   *http.Client
	url  string
	port int
	pod  string
	log  *slog.Logger
	ln   net.Listener
	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (f *forwarder) dial(ctx context.Context) (*websocket.Conn, error) {
	ws, resp, err := websocket.Dial(ctx, f.url, &websocket.DialOptions{HTTPClient: f.hc, Subprotocols: []string{portForwardProtocol}})
	if resp != nil && resp.Body != nil && err != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		status := ""
		if resp != nil {
			status = " (HTTP " + strconv.Itoa(resp.StatusCode) + ")"
		}
		return nil, fmt.Errorf("port forward to pod %s failed%s: %w", f.pod, status, err)
	}
	if ws.Subprotocol() != portForwardProtocol {
		_ = ws.CloseNow()
		return nil, fmt.Errorf("port forward to pod %s: unsupported stream protocol %q", f.pod, ws.Subprotocol())
	}
	ws.SetReadLimit(maxFrameBytes)
	return ws, nil
}

func (f *forwarder) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			select {
			case <-f.done:
			default:
				f.log.Warn("port forward listener stopped", slog.Any("error", err))
			}
			return
		}
		f.track(c, true)
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer f.track(c, false)
			if err := f.handle(c); err != nil {
				f.log.Warn("port forward stream failed", slog.String("pod", f.pod), slog.Any("error", err))
			}
		}()
	}
}

func (f *forwarder) track(c net.Conn, add bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns == nil {
		f.conns = map[net.Conn]struct{}{}
	}
	if add {
		f.conns[c] = struct{}{}
		return
	}
	delete(f.conns, c)
}

// handle pipes one local connection through a new stream.
func (f *forwarder) handle(c net.Conn) error {
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	ws, err := f.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = ws.CloseNow() }()
	return f.pipe(ctx, c, ws)
}

// pipe copies data between c and the stream ws until either side ends (or ctx is
// canceled), then closes the stream normally. The caller closes c and ws.
func (f *forwarder) pipe(ctx context.Context, c net.Conn, ws *websocket.Conn) error {
	errc := make(chan error, 2)
	// Remote → local.
	go func() {
		seen := [2]bool{}
		for {
			_, msg, err := ws.Read(ctx)
			if err != nil {
				if websocket.CloseStatus(err) == websocket.StatusNormalClosure || errors.Is(err, io.EOF) || ctx.Err() != nil {
					errc <- nil
					return
				}
				errc <- err
				return
			}
			if len(msg) == 0 {
				continue
			}
			ch, payload := int(msg[0]), msg[1:]
			if ch != dataChannel && ch != errorChannel {
				continue
			}
			if !seen[ch] {
				seen[ch] = true
				if len(payload) < 2 || int(binary.LittleEndian.Uint16(payload[:2])) != f.port {
					errc <- fmt.Errorf("port forward to pod %s: unexpected stream header", f.pod)
					return
				}
				payload = payload[2:]
			}
			if len(payload) == 0 {
				continue
			}
			if ch == errorChannel {
				if len(payload) > maxErrorBytes {
					payload = payload[:maxErrorBytes]
				}
				errc <- fmt.Errorf("port forward to pod %s: %s", f.pod, string(payload))
				return
			}
			if _, err := c.Write(payload); err != nil {
				errc <- nil
				return
			}
		}
	}()
	// Local → remote.
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				frame := make([]byte, n+1)
				frame[0] = dataChannel
				copy(frame[1:], buf[:n])
				if werr := ws.Write(ctx, websocket.MessageBinary, frame); werr != nil {
					errc <- nil
					return
				}
			}
			if err != nil {
				errc <- nil
				return
			}
		}
	}()
	err := <-errc
	_ = ws.Close(websocket.StatusNormalClosure, "")
	return err
}

func (f *forwarder) close() error {
	var err error
	f.once.Do(func() {
		close(f.done)
		err = f.ln.Close()
		f.mu.Lock()
		for c := range f.conns {
			_ = c.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
