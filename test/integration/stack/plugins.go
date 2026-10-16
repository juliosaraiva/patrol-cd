package stack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/cmp"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// StartPlugin serves a Config Management Plugin (internal/cmp, the cmp-server library) on the
// Stack's plugin socket directory until the end of the test, as a sidecar would. pluginYAML is
// the plugin.yaml document; commands run through the sandbox with PATH, HOME, TMPDIR and LANG
// of this process only. It returns the socket path. Requires Options.Plugins.
func (s *Stack) StartPlugin(t testing.TB, pluginYAML string) string {
	t.Helper()
	if s.pluginDir == "" {
		t.Fatal("StartPlugin requires Options.Plugins")
	}
	cfg, err := cmp.ParsePluginConfig([]byte(pluginYAML))
	if err != nil {
		t.Fatalf("plugin.yaml: %v", err)
	}
	work, err := os.MkdirTemp("", "pcmpw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	srv, err := cmp.New(cmp.Config{
		Plugin: cfg, SocketDir: s.pluginDir, WorkDir: work, Exec: cmpLimits(),
		ShutdownTimeout: 5 * time.Second, Logger: s.logs.logger("cmp-"+cfg.PluginName(), s.level()),
	})
	if err != nil {
		t.Fatalf("cmp server %s: %v", cfg.PluginName(), err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("cmp server %s: %v", cfg.PluginName(), err)
			}
		case <-time.After(Timeout(30 * time.Second)):
			t.Errorf("cmp server %s did not stop", cfg.PluginName())
		}
	})
	sock := srv.SocketPath()
	Eventually(t, 30*time.Second, "plugin socket "+filepath.Base(sock), func() (bool, string) {
		_, err := os.Stat(sock)
		return err == nil, ""
	})
	return sock
}

// cmpLimits bounds plugin commands like the sidecar defaults (90 s).
func cmpLimits() sandbox.Limits { return sandbox.Limits{Timeout: cmp.DefaultExecTimeout} }
