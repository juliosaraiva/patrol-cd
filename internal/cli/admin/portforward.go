package admin

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Port forwarding uses the WebSocket channel protocol of the pods/portforward subresource
// ("v4.channel.k8s.io", served by the kubelet/CRI streaming server through the API server):
// each WebSocket connection carries one TCP connection to one pod port. Binary messages are
// prefixed with a channel byte: 0 is data, 1 is errors; the server first sends the port
// number (uint16, little endian) on both channels. client-go's tools/portforward is not used
// because its SPDY and tunneling dependencies are not part of this module.
const (
	portForwardProtocol = "v4.channel.k8s.io"
	pfDataChannel       = 0
	pfErrorChannel      = 1
	// pfReadLimit bounds one WebSocket message received (the kubelet writes up to 32 KiB of
	// data plus the channel byte per message).
	pfReadLimit = 1 << 20
)

// readyPod returns a running, ready pod matching selector in namespace.
func readyPod(ctx context.Context, kube kubernetes.Interface, namespace, selector string) (*corev1.Pod, error) {
	pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return p, nil
			}
		}
	}
	return nil, fmt.Errorf("cannot find a running and ready pod with selector %q in namespace %s", selector, namespace)
}

// podDialer opens TCP connections to a pod port through the Kubernetes API.
type podDialer struct {
	endpoint string
	client   *websocket.DialOptions
}

// newPodDialer returns a dialer for port of pod using cfg's credentials.
func newPodDialer(cfg *rest.Config, namespace, pod string, port int) (*podDialer, error) {
	c := rest.CopyConfig(cfg)
	// WebSocket upgrades need HTTP/1.1.
	c.NextProtos = []string{"http/1.1"}
	httpClient, err := rest.HTTPClientFor(c)
	if err != nil {
		return nil, err
	}
	httpClient.Timeout = 0
	base, err := url.Parse(c.Host)
	if err != nil {
		return nil, fmt.Errorf("invalid API server URL: %w", err)
	}
	switch base.Scheme {
	case "https", "":
		base.Scheme = "https"
	case "http":
	default:
		return nil, fmt.Errorf("unsupported API server scheme %q", base.Scheme)
	}
	base.Path = fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s/portforward", c.APIPath, url.PathEscape(namespace), url.PathEscape(pod))
	base.RawQuery = url.Values{"ports": {strconv.Itoa(port)}}.Encode()
	return &podDialer{endpoint: base.String(), client: &websocket.DialOptions{HTTPClient: httpClient, Subprotocols: []string{portForwardProtocol}}}, nil
}

// DialContext opens one tunneled TCP connection (net.Dialer signature; network and address
// are ignored).
func (d *podDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return dialPortForward(ctx, d.endpoint, d.client)
}

// dialPortForward opens a WebSocket port-forward stream and waits for the port headers.
func dialPortForward(ctx context.Context, endpoint string, opts *websocket.DialOptions) (net.Conn, error) {
	ws, resp, err := websocket.Dial(ctx, endpoint, opts)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("port forwarding: %s: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("port forwarding: %w", err)
	}
	ws.SetReadLimit(pfReadLimit)
	c := &pfConn{ws: ws, ctx: context.Background()}
	c.ctx, c.cancel = context.WithCancel(c.ctx)
	// The server announces the port on the data and error channels.
	for seen := 0; seen < 2; {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			c.cancel()
			_ = ws.CloseNow()
			return nil, fmt.Errorf("port forwarding: %w", err)
		}
		if len(msg) != 3 || (msg[0] != pfDataChannel && msg[0] != pfErrorChannel) {
			c.cancel()
			_ = ws.CloseNow()
			return nil, errors.New("port forwarding: unexpected stream header")
		}
		_ = binary.LittleEndian.Uint16(msg[1:])
		seen++
	}
	return c, nil
}

// pfConn is a net.Conn over one port-forward WebSocket.
type pfConn struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	rmu     sync.Mutex
	pending []byte
	wmu     sync.Mutex
	once    sync.Once
}

func (c *pfConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.pending) == 0 {
		_, msg, err := c.ws.Read(c.ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) || errors.Is(err, context.Canceled) {
				return 0, io.EOF
			}
			return 0, err
		}
		if len(msg) == 0 {
			continue
		}
		switch msg[0] {
		case pfDataChannel:
			c.pending = msg[1:]
		case pfErrorChannel:
			if len(msg) > 1 {
				return 0, fmt.Errorf("port forwarding: %s", msg[1:])
			}
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *pfConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// One message carries at most 32 KiB including the channel byte, within the default
	// message limit of common WebSocket servers.
	const chunk = 32<<10 - 1
	written := 0
	for len(p) > 0 {
		n := min(len(p), chunk)
		msg := make([]byte, n+1)
		msg[0] = pfDataChannel
		copy(msg[1:], p[:n])
		if err := c.ws.Write(c.ctx, websocket.MessageBinary, msg); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *pfConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.ws.Close(websocket.StatusNormalClosure, "")
		c.cancel()
	})
	return err
}

type pfAddr struct{}

func (pfAddr) Network() string { return "portforward" }
func (pfAddr) String() string  { return "portforward" }

func (c *pfConn) LocalAddr() net.Addr              { return pfAddr{} }
func (c *pfConn) RemoteAddr() net.Addr             { return pfAddr{} }
func (c *pfConn) SetDeadline(time.Time) error      { return nil }
func (c *pfConn) SetReadDeadline(time.Time) error  { return nil }
func (c *pfConn) SetWriteDeadline(time.Time) error { return nil }
