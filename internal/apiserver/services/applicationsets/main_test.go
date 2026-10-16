package applicationsets_test

import (
	"os"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/internal/miscapitest"
)

func TestMain(m *testing.M) {
	os.Exit(miscapitest.Main(m, miscapitest.CRDDir(4)))
}
