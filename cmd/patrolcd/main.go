// Command patrolcd is the Patrol CD control-plane multi-call binary: server, controller,
// repo-server, appset-controller, notifications-controller, cmp-server, commit-server,
// k8s-auth, bootstrap (first-run Secrets) and version (docs/reference/cli-patrolcd.md).
// Libraries re-execute it in hidden helper modes (sandbox rlimits, git askpass, Jsonnet
// evaluation), which are dispatched before any argument is parsed.
package main

import (
	"os"

	"github.com/juliosaraiva/patrol-cd/internal/server"
)

func main() {
	// Must stay first: a helper invocation never reaches the command tree.
	server.DispatchHelpers()
	os.Exit(server.Main(os.Args[0], os.Args[1:], os.Stdout, os.Stderr))
}
