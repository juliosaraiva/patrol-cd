package cli_test

import (
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

func TestHarnessSmoke(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.Login("admin")
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Logged In: true", "admin")
	e.Run("proj", "create", "demo", "-d", "https://kubernetes.default.svc,default").Succeeded(t)
	e.Run("proj", "list").Succeeded(t).Contains(t, "demo")
	if p := e.GetProject("demo"); len(p.Spec.Destinations) != 1 {
		t.Fatalf("destinations: %+v", p.Spec.Destinations)
	}
}
