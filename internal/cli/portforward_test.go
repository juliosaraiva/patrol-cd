package cli_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
)

func TestPortForward(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{TLS: true})
	k := clitest.NewFakeKubeAPI(t, e.Addr)
	kc := k.WriteKubeconfig(t, t.TempDir(), "patrol")
	e.SetEnv("KUBECONFIG", kc)

	// login through a forwarded port stores a "port-forward" context.
	e.Run("login", "--port-forward", "--username", "admin", "--password", clitest.Password).Succeeded(t).
		Contains(t, "'admin:login' logged in successfully", "Context 'port-forward' updated")
	if s, err := e.ReadConfig().GetServer("port-forward"); err != nil || s.Server != "port-forward" {
		t.Fatalf("server entry: %+v %v", s, err)
	}
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Username: admin")
	e.Run("proj", "create", "pf").Succeeded(t)
	e.Run("proj", "list", "-o", "name").Succeeded(t).Contains(t, "pf\n")
	if k.Streams.Load() < 3 {
		t.Fatalf("expected forwarded streams, got %d", k.Streams.Load())
	}

	// Pod selection errors.
	e.Run("--port-forward-namespace", "elsewhere", "proj", "list").Failed(t, 20, "cannot find ready pod with selector: [app.kubernetes.io/name=patrol-server] in namespace elsewhere")
	e.Run("--server-name", "custom-server", "proj", "list").Failed(t, 20, "cannot find ready pod with selector: [app.kubernetes.io/name=custom-server] in namespace patrol")
	e.RunWith(clitest.RunOptions{Env: map[string]string{"PATROL_SERVER_NAME": "custom-server"}}, "proj", "list").Failed(t, 20, "app.kubernetes.io/name=custom-server")
	// Without Kubernetes access.
	e.RunWith(clitest.RunOptions{Unset: []string{"KUBECONFIG"}}, "proj", "list").Failed(t, 20)
}

// TestPortForwardDoesNotReuseRemoteContext: --port-forward with the current context of a
// remote server must not send that context's token (or IdP refresh token) to the pod the
// kubeconfig selects, nor store a token renewed by that pod in the remote context.
func TestPortForwardDoesNotReuseRemoteContext(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{TLS: true})
	tok := e.LoginAs("admin")

	var (
		mu    sync.Mutex
		auths []string
		paths []string
	)
	capture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Patrol-Renew-Token", "aaa.bbb.ccc")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(capture.Close)
	k := clitest.NewFakeKubeAPI(t, strings.TrimPrefix(capture.URL, "https://"))
	e.SetEnv("KUBECONFIG", k.WriteKubeconfig(t, t.TempDir(), "patrol"))

	check := func(t *testing.T, wantToken string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(paths) == 0 {
			t.Fatal("the forwarded pod received no request")
		}
		for i, a := range auths {
			if a != "" {
				t.Fatalf("request %s to the forwarded pod carried credentials", paths[i])
			}
			if paths[i] != "/api/v1/projects" {
				t.Fatalf("unexpected request to the forwarded pod: %s", paths[i])
			}
		}
		auths, paths = nil, nil
		if got := e.ReadConfig().GetToken(e.Addr); got != wantToken {
			t.Fatal("the stored token of the remote context changed")
		}
	}

	r := e.Run("--port-forward", "proj", "list").Succeeded(t)
	r.NotContains(t, tok)
	check(t, tok)
	e.RunWith(clitest.RunOptions{Env: map[string]string{"PATROL_OPTS": "--port-forward-namespace patrol"}}, "proj", "list").Succeeded(t)
	check(t, tok)

	// An expired SSO session of the remote context is not refreshed through the pod.
	cfg := e.ReadConfig()
	cfg.UpsertUser(config.User{Name: e.Addr, AuthToken: "expired", RefreshToken: "idp-refresh"})
	if err := config.Write(cfg, e.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	e.Run("--port-forward", "proj", "list").Succeeded(t).NotContains(t, "idp-refresh")
	check(t, "expired")
}
