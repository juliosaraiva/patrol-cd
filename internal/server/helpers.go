package server

import (
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Helper is a hidden mode of the patrolcd binary. Libraries re-execute the running binary
// for work that must happen in a separate process (rlimits before exec, the git askpass
// prompt answerer, the sandboxed Jsonnet evaluator); the binary must recognize those
// invocations before it parses any argument.
type Helper struct {
	// Name identifies the helper in logs and tests.
	Name string
	// Main returns when the process was not started in this mode; otherwise it never
	// returns (it exits the process).
	Main func()
}

// Helpers returns the helper modes in dispatch order. The sandbox helper comes first: it
// applies rlimits and execs the target in place, and the targets it starts may be this
// binary again in another helper mode (the Jsonnet evaluator). Every MaybeXxxMain function
// of the module must be listed (TestEveryHelperMainIsDispatched).
func Helpers() []Helper {
	return []Helper{
		{Name: "sandbox", Main: sandbox.MaybeHelperMain},
		{Name: "git-askpass", Main: git.MaybeAskpassMain},
		{Name: "jsonnet", Main: jsonnet.MaybeJsonnetMain},
	}
}

// DispatchHelpers runs every helper check; it must be the first statement of main (and of
// TestMain in packages whose tests re-execute the test binary). It returns only when the
// process was not started as a helper.
func DispatchHelpers() {
	for _, h := range Helpers() {
		h.Main()
	}
}
