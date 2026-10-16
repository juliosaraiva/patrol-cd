package commitserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// APP-135: the commit-server process serves health, the authenticated API and metrics,
// and shuts down when its context ends.
func TestRun(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	mln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Token:           testToken,
			Listener:        ln,
			MetricsListener: mln,
			Service:         ServiceOptions{AllowLocalRepositories: true, WorkDir: t.TempDir()},
			Logger:          discard,
		})
	}()

	base := "http://" + ln.Addr().String()
	get := func(url string) (int, string) {
		resp, err := http.Get(url) //nolint:noctx // test
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + PathHealthz) //nolint:noctx // test
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 20*time.Millisecond)

	resp, err := http.Post(base+PathCommitHydratedManifests, "application/json", strings.NewReader("{}")) //nolint:noctx // test
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	c, err := NewClient(ln.Addr().String(), testToken)
	require.NoError(t, err)
	_, err = c.CommitHydratedManifests(context.Background(), &CommitHydratedManifestsRequest{})
	assert.ErrorIs(t, err, ErrInvalidRequest)

	code, body := get("http://" + mln.Addr().String() + "/metrics")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `patrol_commitserver_commit_request_total{repo="",response_type="failure"} 1`)
	assert.Contains(t, body, "go_goroutines")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestRunRequiresToken(t *testing.T) {
	t.Parallel()
	assert.ErrorContains(t, Run(context.Background(), RunOptions{}), "token is required")
}
