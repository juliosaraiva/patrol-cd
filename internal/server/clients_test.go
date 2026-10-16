package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
)

// TestCommitServerClientTimeout: a commit-server that accepts the request and never answers
// must not hold a hydration worker forever (hydration contexts have no deadline).
func TestCommitServerClientTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) })

	c, err := newCommitServerClient(CommitServerClientOptions{Address: ts.URL, Timeout: 300 * time.Millisecond}, smokeToken)
	require.NoError(t, err)
	start := time.Now()
	_, err = c.CommitHydratedManifests(context.Background(), &commitserver.CommitHydratedManifestsRequest{})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the request is bounded by the client timeout")
}

// TestCommitServerClientTLS runs the real commit-server with TLS and mutual TLS and calls it
// with the hydrator's client. The empty request is refused by the commit service itself,
// which proves the call got through TLS and the token check.
func TestCommitServerClientTLS(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	_, srvCrt, srvKey := ca.issue(t, "commit-server")
	_, cliCrt, cliKey := ca.issue(t, "patrol-controller")
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, data, 0o600))
		return p
	}
	caFile := write("ca.crt", ca.pem)
	srvCrtFile, srvKeyFile := write("tls.crt", srvCrt), write("tls.key", srvKey)
	cliCrtFile, cliKeyFile := write("client.crt", cliCrt), write("client.key", cliKey)

	type addrs struct{ api, metrics net.Addr }
	ch := make(chan addrs, 1)
	stop := running(t, func(ctx context.Context) error {
		return RunCommitServer(ctx, CommitServerConfig{
			ListenAddress: "127.0.0.1:0", MetricsAddress: "127.0.0.1:0", Token: smokeToken,
			TLS:             TLSServerOptions{CertFile: srvCrtFile, KeyFile: srvKeyFile, ClientCAFile: caFile},
			Service:         commitserver.ServiceOptions{WorkDir: t.TempDir(), CredentialsDir: t.TempDir()},
			ShutdownTimeout: 5 * time.Second, Logger: testLogger(t),
			OnListening: func(a, m net.Addr) { ch <- addrs{a, m} },
		})
	})
	a := waitAddr(t, ch)
	url := "https://" + a.api.String()
	eventually(t, "http://"+a.metrics.String()+"/healthz", http.StatusOK)

	commit := func(o CommitServerClientOptions) error {
		c, err := newCommitServerClient(o, smokeToken)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = c.CommitHydratedManifests(ctx, &commitserver.CommitHydratedManifestsRequest{})
		return err
	}
	var svcErr *commitserver.Error

	// Verified server, client certificate presented: the commit service answers.
	err := commit(CommitServerClientOptions{Address: url, TLS: TLSClientOptions{CAFile: caFile, ClientCertFile: cliCrtFile, ClientKeyFile: cliKeyFile}})
	require.Error(t, err)
	require.True(t, errors.As(err, &svcErr), "an answer of the commit service, got %v", err)
	assert.Equal(t, http.StatusBadRequest, svcErr.Status)
	assert.NotEqual(t, commitserver.CodeInternal, svcErr.Code, "a commit service error, not a transport status: %v", svcErr)

	// No client certificate: the mutual TLS handshake fails.
	err = commit(CommitServerClientOptions{Address: url, TLS: TLSClientOptions{CAFile: caFile}})
	require.Error(t, err)
	assert.False(t, errors.As(err, &svcErr), "no answer without a client certificate: %v", err)

	// A CA that did not sign the server certificate: verification fails.
	otherCA := write("other-ca.crt", newTestCA(t).pem)
	err = commit(CommitServerClientOptions{Address: url, TLS: TLSClientOptions{CAFile: otherCA, ClientCertFile: cliCrtFile, ClientKeyFile: cliKeyFile}})
	require.Error(t, err)
	assert.False(t, errors.As(err, &svcErr))
	assert.Contains(t, err.Error(), "certificate")
	stop()
}
