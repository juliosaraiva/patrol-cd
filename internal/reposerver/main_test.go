package reposerver

import (
	"os"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/render/jsonnet"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// TestMain installs the helper modes of the test binary: git askpass, the sandbox
// rlimit helper and the Jsonnet evaluator child. The repo-server talks to no
// Kubernetes API, so no envtest environment is needed.
func TestMain(m *testing.M) {
	git.MaybeAskpassMain()
	sandbox.MaybeHelperMain()
	jsonnet.MaybeJsonnetMain()
	os.Exit(m.Run())
}
