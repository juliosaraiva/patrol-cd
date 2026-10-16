package git

import (
	"os"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

func TestMain(m *testing.M) {
	// The test binary doubles as the GIT_ASKPASS helper.
	MaybeAskpassMain()
	sandbox.MaybeHelperMain()
	os.Exit(m.Run())
}
