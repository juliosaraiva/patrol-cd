package cli_test

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

func TestRootPath(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{RootPath: "patrol"})
	r := e.Run("login", e.Addr, "--plaintext", "--grpc-web-root-path", "/patrol", "--username", "admin", "--password", clitest.Password).Succeeded(t)
	r.Contains(t, "Context '"+e.Addr+"/patrol' updated")
	e.Run("proj", "create", "rooted").Succeeded(t)
	e.Run("proj", "list", "-o", "name").Succeeded(t).Contains(t, "rooted\n")
	// Without the root path (and without the stored context) the API is not found.
	tok := e.LoginToken("admin")
	noCfg := clitest.RunOptions{Env: map[string]string{"PATROL_CONFIG": filepath.Join(t.TempDir(), "none")}}
	e.RunWith(noCfg, "--server", e.Addr, "--plaintext", "--auth-token", tok, "proj", "list").Failed(t, 20)
	e.RunWith(noCfg, "--server", e.Addr, "--plaintext", "--grpc-web-root-path", "patrol", "--auth-token", tok, "proj", "list").Succeeded(t).Contains(t, "rooted")
}

func TestHTTPRetriesAndHeaders(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	tok := e.LoginToken("admin")
	target, err := url.Parse(e.URL)
	if err != nil {
		t.Fatal(err)
	}
	var failures, seenHeader atomic.Int64
	failures.Store(2)
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Patrol-Test") == "yes" {
			seenHeader.Add(1)
		}
		if !strings.HasPrefix(r.UserAgent(), "patrol-cli/") {
			http.Error(w, "unexpected user agent "+r.UserAgent(), http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects") && failures.Add(-1) >= 0 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	addr := strings.TrimPrefix(front.URL, "http://")
	base := []string{"--server", addr, "--plaintext", "--auth-token", tok, "-H", "X-Patrol-Test: yes"}

	// Without retries the 503 is reported.
	e.Run(append(base, "proj", "list")...).Failed(t, 20, "503")
	// With retries the GET succeeds after the remaining failure.
	e.Run(append(base, "--http-retry-max", "3", "proj", "list")...).Succeeded(t)
	if seenHeader.Load() < 2 {
		t.Fatalf("custom header not sent: %d", seenHeader.Load())
	}
}

func TestVersionServer(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	e.Run("version").Succeeded(t).Contains(t, "patrol: ", "patrol-server: v9.9.9-clitest")
	e.Run("version", "--short").Succeeded(t).Contains(t, "patrol-server: v9.9.9-clitest").NotContains(t, "GoVersion")
	e.Run("version", "-o", "json").Succeeded(t).Contains(t, `"client"`, `"server"`, `"Version": "v9.9.9-clitest"`)
	e.Run("version", "-o", "yaml").Succeeded(t).Contains(t, "server:", "Version: v9.9.9-clitest")
	// The version route needs no authentication.
	e.RunWith(clitest.RunOptions{}, "--server", e.Addr, "--plaintext", "--patrol-context", e.Addr, "version", "--short").Succeeded(t)
	// --client never contacts the server.
	e.RunWith(clitest.RunOptions{}, "--server", "127.0.0.1:1", "version", "--client").Succeeded(t)
}
