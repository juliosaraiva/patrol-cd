package core_test

import (
	"context"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/core"
	"github.com/juliosaraiva/patrol-cd/internal/cli/transport"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// TestCoreConnectionInProcessAndCleanShutdown connects through the provider (as the CLI
// does), checks that the API client is served in-process (no address, no listener: the
// HTTP client's transport is the in-memory HandlerTransport), uses the lazily started
// parts (repository informers, the Application watch informer, an open event stream) and
// checks that Close leaves no goroutine behind.
func TestCoreConnectionInProcessAndCleanShutdown(t *testing.T) {
	e := newCoreEnv(t)
	require.NoError(t, e.admin.Create(e.ctx, &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: "watched"},
		Spec: v1alpha1.ApplicationSpec{
			Project: "default", Source: &v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "guestbook"},
			Destination: v1alpha1.ApplicationDestination{Server: inCluster, Namespace: e.dest},
		},
	}))
	kc, user := e.user("operator", operatorRules()...)
	p := core.NewProvider(e.opts)
	req := transport.Request{Kube: transport.KubeOptions{KubeconfigPaths: []string{kc}}}

	cycle := func() {
		conn, err := p.Connect(e.ctx, req)
		require.NoError(t, err)
		assert.True(t, core.IsInProcess(conn.HTTPClient), "the API client must be served in-process")
		assert.Equal(t, cmdutil.CoreServer, conn.ServerAddr)
		assert.True(t, conn.AuthDisabled)
		c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: conn.ServerAddr, PlainText: conn.PlainText, HTTPClient: conn.HTTPClient})
		require.NoError(t, err)
		v, err := c.Version(e.ctx)
		require.NoError(t, err)
		assert.Contains(t, v.ExtraBuildInfo["mode"], "core")
		ui, err := c.UserInfo(e.ctx)
		require.NoError(t, err)
		assert.True(t, ui.LoggedIn)
		assert.Equal(t, user, ui.Username)
		_, err = c.ListRepositories(e.ctx, &apiclient.RepositoryListRequest{})
		require.NoError(t, err)
		// An event stream left open is ended by Close.
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		s, err := c.WatchApplications(ctx, &apiclient.ApplicationQuery{}, nil)
		require.NoError(t, err)
		ev, err := s.Next()
		require.NoError(t, err)
		assert.Contains(t, string(ev.Data), `"watched"`)
		require.NoError(t, conn.Close())
		_, err = c.Version(e.ctx)
		require.Error(t, err, "requests after Close fail")
		_ = s.Close()
	}
	cycle() // warm up process-wide state (client-go caches, discovery)
	base := settledGoroutines()
	cycle()
	cycle()
	deadline := time.Now().Add(15 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			var b strings.Builder
			_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
			t.Fatalf("goroutines did not return to %d after Close (now %d):\n%s", base, runtime.NumGoroutine(), b.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// settledGoroutines returns the goroutine count once it stopped decreasing.
func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for range 40 {
		time.Sleep(100 * time.Millisecond)
		m := runtime.NumGoroutine()
		if m >= n {
			return max(m, n)
		}
		n = m
	}
	return n
}
