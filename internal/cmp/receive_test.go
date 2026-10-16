package cmp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// stalledPost writes a POST to endpoint on a raw connection to the server socket,
// announcing contentLength body bytes but sending only sent, and then keeps the
// connection open without sending more. It returns the response (nil when none
// arrived within wait) and the time it took to arrive.
func stalledPost(t *testing.T, s *testServer, endpoint string, header http.Header, contentLength int, sent []byte, wait time.Duration) (*http.Response, []byte, time.Duration) {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "unix", s.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\nHost: cmp\r\nContent-Type: %s\r\nContent-Length: %d\r\n", endpoint, cmpwire.ContentTypeStream, contentLength)
	for k, vs := range header {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	start := time.Now()
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(sent); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Logf("no response after %s: %v (work dir %v)", time.Since(start), err, workDirEntries(t, s.Server))
		return nil, nil, time.Since(start)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data, time.Since(start)
}

// TestStalledStream checks that a client that stops sending its body cannot hold a
// request (and its directory) past the receive deadline: the client deadline minus
// DeadlineBuffer, or Config.ReceiveTimeout when it is earlier or no deadline was sent.
func TestStalledStream(t *testing.T) {
	t.Parallel()
	archive := tarGzTree(t, map[string]string{"app/a": "a"})
	frame := streamBody(t, archive, "app")[:len(streamBody(t, archive, "app"))-len(archive)]
	full := len(frame) + len(archive)
	// notGzip is a payload whose first bytes already fail extraction; the server then
	// drains the rest of the declared payload, which never arrives.
	notGzip := append([]byte("this is not a gzip stream"), make([]byte, 64<<10)...)
	notGzipFrame, err := cmpwire.EncodeMetadata(cmpwire.Metadata{AppName: "app", AppRelPath: "app", Checksum: sha(notGzip), Size: int64(len(notGzip))})
	if err != nil {
		t.Fatal(err)
	}
	deadlineIn := func(d time.Duration) http.Header {
		return http.Header{cmpwire.HeaderDeadline: {time.Now().Add(d).UTC().Format(time.RFC3339Nano)}}
	}

	tests := []struct {
		name           string
		receiveTimeout time.Duration
		header         http.Header
		contentLength  int
		sent           []byte
		status         int
		code           string
		msg            string
		// within bounds the time until the response.
		within time.Duration
	}{
		{
			name: "client deadline, payload stalls", receiveTimeout: -1, header: deadlineIn(time.Second),
			contentLength: full, sent: append(append([]byte{}, frame...), archive[:5]...),
			status: http.StatusGatewayTimeout, code: apiclient.CodeDeadlineExceeded,
			msg:    "generate manifest error receiving stream: timed out receiving the request stream",
			within: 4 * time.Second,
		},
		{
			name: "client deadline, metadata stalls", receiveTimeout: -1, header: deadlineIn(time.Second),
			contentLength: full, sent: frame[:2],
			status: http.StatusGatewayTimeout, code: apiclient.CodeDeadlineExceeded,
			msg:    "generate manifest error receiving stream: timed out receiving the request stream",
			within: 4 * time.Second,
		},
		{
			name: "receive timeout without client deadline", receiveTimeout: time.Second,
			contentLength: full, sent: append(append([]byte{}, frame...), archive[:5]...),
			status: http.StatusGatewayTimeout, code: apiclient.CodeDeadlineExceeded,
			msg:    "generate manifest error receiving stream: timed out receiving the request stream",
			within: 4 * time.Second,
		},
		{
			name: "receive timeout earlier than client deadline", receiveTimeout: time.Second, header: deadlineIn(time.Hour),
			contentLength: full, sent: frame[:2],
			status: http.StatusGatewayTimeout, code: apiclient.CodeDeadlineExceeded,
			within: 4 * time.Second,
		},
		{
			// Extraction fails at once; the drain of the rest of the payload (to tell
			// tampering apart) must stop at the deadline and report the archive error.
			name: "drain after archive error stops at deadline", receiveTimeout: time.Second,
			contentLength: len(notGzipFrame) + len(notGzip), sent: append(append([]byte{}, notGzipFrame...), notGzip[:32]...),
			status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument,
			msg:    "generate manifest error receiving stream: error decompressing tgz file:",
			within: 4 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := startServer(t, "", "kind: ConfigManagementPlugin\nmetadata: {name: stall}\nspec:\n  generate: {command: [sh, -c, 'echo should-not-run >&2']}",
				func(c *Config) { c.ReceiveTimeout = tt.receiveTimeout })
			resp, data, took := stalledPost(t, srv, cmpwire.PathGenerate, tt.header, tt.contentLength, tt.sent, 15*time.Second)
			if resp == nil {
				t.Fatalf("no response within %s", took)
			}
			if took > tt.within {
				t.Errorf("response took %s, want at most %s", took, tt.within)
			}
			var e cmpwire.Error
			if err := json.Unmarshal(data, &e); err != nil {
				t.Fatalf("decode %q: %v", data, err)
			}
			if resp.StatusCode != tt.status || e.Code != tt.code {
				t.Fatalf("status %d code %q (%s), want %d %q", resp.StatusCode, e.Code, e.Message, tt.status, tt.code)
			}
			if tt.msg != "" {
				assertContains(t, e.Message, tt.msg)
			}
			if resp.Header.Get("Connection") != "close" && !resp.Close {
				t.Errorf("connection with an unread body kept alive")
			}
			waitNoEntries(t, srv.Server)
		})
	}
}

// TestReceiveDeadlineDoesNotBoundCommands checks that the receive deadline ends with
// the receive phase: commands may run past it (bounded by the exec timeout and the
// client deadline instead), on a connection that is reused afterwards.
func TestReceiveDeadlineDoesNotBoundCommands(t *testing.T) {
	t.Parallel()
	srv := startServer(t, "", `
kind: ConfigManagementPlugin
metadata: {name: slowgen}
spec:
  generate:
    command: [sh, -c, 'sleep 1; echo "{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"x\"}}"']
`, func(c *Config) { c.ReceiveTimeout = 300 * time.Millisecond })
	client := srv.client()
	body := streamBody(t, tarGzTree(t, map[string]string{"app/a": "a"}), "app")
	for i := range 2 {
		req, err := http.NewRequest(http.MethodPost, "http://cmp"+cmpwire.PathGenerate, strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", cmpwire.ContentTypeStream)
		req.Header.Set(cmpwire.HeaderDeadline, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d: %s", i, resp.StatusCode, data)
		}
		var mr cmpwire.ManifestResponse
		if err := json.Unmarshal(data, &mr); err != nil || len(mr.Manifests) != 1 {
			t.Fatalf("request %d: response %s: %v", i, data, err)
		}
	}
	waitNoEntries(t, srv.Server)
}
