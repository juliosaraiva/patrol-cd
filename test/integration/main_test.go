package integration

import (
	"os"
	"testing"

	"github.com/juliosaraiva/patrol-cd/test/integration/stack"
)

func TestMain(m *testing.M) { os.Exit(stack.Main(m)) }
