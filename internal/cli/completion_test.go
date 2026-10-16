package cli_test

import (
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

func TestDynamicCompletion(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	e.Run("proj", "create", "alpha").Succeeded(t)
	e.Run("proj", "create", "beta").Succeeded(t)
	e.Run("repo", "add", "https://git.example.com/comp.git").Succeeded(t)

	r := e.Run("__complete", "proj", "get", "").Succeeded(t)
	r.Contains(t, "alpha\n", "beta\n", ":4")
	e.Run("__complete", "proj", "get", "al").Succeeded(t).Contains(t, "alpha").NotContains(t, "beta")
	// Only as many positional arguments as the command takes are completed.
	if out := e.Run("__complete", "proj", "get", "alpha", "").Succeeded(t).Stdout; strings.Contains(out, "beta") {
		t.Fatalf("completed a second project: %q", out)
	}
	e.Run("__complete", "repo", "rm", "").Succeeded(t).Contains(t, "https://git.example.com/comp.git")
	e.Run("__complete", "context", "").Succeeded(t).Contains(t, e.Addr)
	e.Run("__complete", "logout", "").Succeeded(t).Contains(t, e.Addr)
	// Commands and flags complete statically.
	e.Run("__complete", "pr").Succeeded(t).Contains(t, "proj")
	e.Run("__complete", "proj", "list", "--o").Succeeded(t).Contains(t, "--output")

	// Without access the completion is empty, never an error.
	clitest.NewRunner(t).Run("__complete", "proj", "get", "").Succeeded(t).NotContains(t, "alpha")
}
