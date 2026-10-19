package deploy_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestMain(m *testing.M) {
	ctrllog.SetLogger(logr.Discard())
	code := m.Run()
	if shared.env != nil {
		if err := shared.env.stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop the shared envtest control plane: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	cleanupBinary()
	os.Exit(code)
}
