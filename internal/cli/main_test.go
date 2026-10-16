package cli_test

import (
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
)

func TestMain(m *testing.M) { os.Exit(clitest.Main(m)) }

func metaName(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }
