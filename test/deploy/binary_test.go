package deploy_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var binary struct {
	once sync.Once
	dir  string
	path string
	dur  time.Duration
	err  error
}

// patrolcdBinary builds ./cmd/patrolcd once per test binary (with -race when this test binary
// was built with -race) and returns its path. The RBAC runs execute it as the containers'
// /usr/local/bin/patrolcd.
func patrolcdBinary(t testing.TB) string {
	t.Helper()
	binary.once.Do(func() {
		dir, err := os.MkdirTemp("", "patrol-deploy-bin-")
		if err != nil {
			binary.err = err
			return
		}
		binary.dir = dir
		binary.path = filepath.Join(dir, "patrolcd")
		args := []string{"build", "-o", binary.path}
		if raceEnabled {
			args = append(args, "-race")
		}
		args = append(args, "./cmd/patrolcd")
		start := time.Now()
		cmd := exec.Command("go", args...)
		cmd.Dir = repoRoot()
		out, err := cmd.CombinedOutput()
		binary.dur = time.Since(start)
		if err != nil {
			binary.err = fmt.Errorf("go %v: %w\n%s", args, err, out)
		}
	})
	if binary.err != nil {
		t.Fatal(binary.err)
	}
	return binary.path
}

func cleanupBinary() {
	if binary.dir != "" {
		_ = os.RemoveAll(binary.dir)
	}
}
