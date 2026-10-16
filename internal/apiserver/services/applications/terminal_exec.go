package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// Kubernetes remote command over WebSocket (the channel protocol of
// k8s.io/apimachinery/pkg/util/httpstream/wsstream): binary frames whose first byte is the
// channel (0 stdin, 1 stdout, 2 stderr, 3 error, 4 resize); v4 and v5 report the command
// status on the error channel as a metav1.Status. A TTY session never half-closes stdin, so
// v5's close channel is not used.
const (
	execProtocolV5 = "v5.channel.k8s.io"
	execProtocolV4 = "v4.channel.k8s.io"

	execChannelStdin  byte = 0
	execChannelStdout byte = 1
	execChannelStderr byte = 2
	execChannelError  byte = 3
	execChannelResize byte = 4

	// execReadLimit bounds one frame from the destination cluster.
	execReadLimit = 1 << 20
	// statusReasonNonZeroExitCode is the reason of a command that exited with a code.
	statusReasonNonZeroExitCode = "NonZeroExitCode"
)

// execRequest is a command to run in a container with a TTY.
type execRequest struct {
	namespace, pod, container string
	command                   []string
}

// execURL is the pods/exec URL of req on the API server of cfg.
func execURL(cfg *rest.Config, req execRequest) (string, error) {
	host := cfg.Host
	if host == "" {
		return "", errors.New("cluster configuration has no host")
	}
	if !strings.Contains(host, "://") {
		scheme := "http"
		if tlsOn, err := rest.TLSConfigFor(cfg); err == nil && tlsOn != nil {
			scheme = "https"
		}
		host = scheme + "://" + host
	}
	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("invalid cluster host: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/namespaces/" + url.PathEscape(req.namespace) + "/pods/" + url.PathEscape(req.pod) + "/exec"
	q := url.Values{}
	for _, c := range req.command {
		q.Add("command", c)
	}
	q.Set("container", req.container)
	q.Set("stdin", "true")
	q.Set("stdout", "true")
	q.Set("tty", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// execHandshakeError is a refused exec handshake (the destination answered with a status).
type execHandshakeError struct {
	Status  int
	Message string
}

func (e *execHandshakeError) Error() string {
	return fmt.Sprintf("exec refused by the destination cluster (%d): %s", e.Status, e.Message)
}

// execStatusError is a command that ended with a failure status.
type execStatusError struct {
	Status metav1.Status
}

func (e *execStatusError) Error() string {
	if e.Status.Message != "" {
		return e.Status.Message
	}
	return "command failed: " + string(e.Status.Reason)
}

// startFailure reports whether the command could not be started (as opposed to a command
// that ran and exited with a non-zero code), so another shell may be tried.
func (e *execStatusError) startFailure() bool {
	return e.Status.Reason != statusReasonNonZeroExitCode
}

// execStream is an exec session with a container.
type execStream struct {
	conn     *websocket.Conn
	protocol string
	tr       *http.Transport
}

// dialExec starts req on the cluster of cfg over WebSocket, authenticating with cfg's
// credentials (TLS, bearer token or exec plugin, impersonation headers when set).
func dialExec(ctx context.Context, cfg *rest.Config, req execRequest, handshakeTimeout time.Duration) (*execStream, error) {
	u, err := execURL(cfg, req)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := rest.TLSConfigFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster TLS configuration: %w", err)
	}
	proxy := cfg.Proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	dialer := &net.Dialer{Timeout: handshakeTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 proxy,
		TLSClientConfig:       tlsCfg,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   handshakeTimeout,
		ResponseHeaderTimeout: handshakeTimeout,
		MaxIdleConns:          1,
		IdleConnTimeout:       time.Second,
	}
	if cfg.Dial != nil {
		tr.DialContext = cfg.Dial
	}
	rt, err := rest.HTTPWrappersForConfig(cfg, tr)
	if err != nil {
		return nil, fmt.Errorf("cluster credentials: %w", err)
	}
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: rt},
		Subprotocols:    []string{execProtocolV5, execProtocolV4},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		tr.CloseIdleConnections()
		if resp != nil && resp.StatusCode >= 300 {
			return nil, &execHandshakeError{Status: resp.StatusCode, Message: handshakeMessage(resp)}
		}
		return nil, err
	}
	p := conn.Subprotocol()
	if p != execProtocolV5 && p != execProtocolV4 {
		_ = conn.Close(websocket.StatusProtocolError, "unsupported protocol")
		tr.CloseIdleConnections()
		return nil, fmt.Errorf("the destination cluster does not support the exec protocols %s and %s", execProtocolV5, execProtocolV4)
	}
	conn.SetReadLimit(execReadLimit)
	return &execStream{conn: conn, protocol: p, tr: tr}, nil
}

// handshakeMessage extracts the Status message of a refused handshake (bounded read).
func handshakeMessage(resp *http.Response) string {
	if resp.Body == nil {
		return http.StatusText(resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var st metav1.Status
	if json.Unmarshal(b, &st) == nil && st.Message != "" {
		return st.Message
	}
	return http.StatusText(resp.StatusCode)
}

func (e *execStream) write(ctx context.Context, ch byte, payload []byte) error {
	frame := make([]byte, 0, len(payload)+1)
	frame = append(frame, ch)
	frame = append(frame, payload...)
	return e.conn.Write(ctx, websocket.MessageBinary, frame)
}

// stdin sends input to the command.
func (e *execStream) stdin(ctx context.Context, data []byte) error {
	return e.write(ctx, execChannelStdin, data)
}

// resize sends the terminal size.
func (e *execStream) resize(ctx context.Context, cols, rows uint16) error {
	b, err := json.Marshal(struct {
		Width  uint16 `json:"Width"`
		Height uint16 `json:"Height"`
	}{cols, rows})
	if err != nil {
		return err
	}
	return e.write(ctx, execChannelResize, b)
}

// run reads the command's output until it ends, passing stdout and stderr to output. It
// returns nil for a successful command, *execStatusError for a failure status, and the
// connection error otherwise.
func (e *execStream) run(ctx context.Context, output func([]byte) error) error {
	for {
		typ, data, err := e.conn.Read(ctx)
		if err != nil {
			if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			continue
		}
		ch, payload := data[0], data[1:]
		switch ch {
		case execChannelStdout, execChannelStderr:
			if len(payload) == 0 {
				continue
			}
			if err := output(payload); err != nil {
				return err
			}
		case execChannelError:
			if len(payload) == 0 {
				continue
			}
			var st metav1.Status
			if err := json.Unmarshal(payload, &st); err != nil {
				return &execStatusError{Status: metav1.Status{Status: metav1.StatusFailure, Message: string(payload)}}
			}
			if st.Status == metav1.StatusSuccess {
				return nil
			}
			return &execStatusError{Status: st}
		}
	}
}

// close ends the exec session.
func (e *execStream) close() {
	_ = e.conn.Close(websocket.StatusNormalClosure, "")
	e.tr.CloseIdleConnections()
}
