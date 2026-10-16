package app_test

import (
	"os"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

// TestMain starts the shared envtest API server of the end-to-end tests when
// KUBEBUILDER_ASSETS is set (unit tests run either way).
func TestMain(m *testing.M) { os.Exit(clitest.Main(m)) }
