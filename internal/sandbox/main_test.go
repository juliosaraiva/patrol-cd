//go:build unix

package sandbox

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// testChildArg switches the test binary into a small child program used by tests
// that need behavior /bin/sh cannot express (for example leaving the process group).
const testChildArg = "__sandbox-test-child"

func TestMain(m *testing.M) {
	MaybeHelperMain()
	if len(os.Args) > 2 && os.Args[1] == testChildArg {
		os.Exit(testChildMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func testChildMain(args []string) int {
	switch args[0] {
	case "setsid-sleep":
		// Leave the sandbox process group while keeping stdout open, then linger.
		if _, err := syscall.Setsid(); err != nil {
			fmt.Fprintln(os.Stderr, "setsid:", err)
			return 2
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "write pid:", err)
			return 2
		}
		time.Sleep(5 * time.Minute)
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown test child mode", args[0])
		return 2
	}
}
