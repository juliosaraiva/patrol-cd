// Package server assembles the Patrol CD control-plane components behind the patrolcd
// multi-call binary (cmd/patrolcd): patrol-server (API, UI, webhooks), the application
// controller (reconcilers, sharding, tree API, source hydrator), the repo-server, the
// ApplicationSet and notifications controllers, the CMP sidecar server and the commit-server,
// plus the k8s-auth exec credential plugin and the hidden helper modes of the libraries.
//
// Each component has a library entry point (Run, RunController, RunRepoServer, ...) taking a
// validated configuration, so the command wiring and the integration tests share one code
// path. Configuration precedence for every patrol-cmd-params-cm parameter is
//
//	command-line flag > environment variable (PATROL_*) > patrol-cmd-params-cm > compiled default
//
// implemented by ParamFlags on top of settings.ResolveParams. Components without Kubernetes
// access (repo-server, commit-server, cmp-server) read the ConfigMap only through the
// environment the install manifests project it into.
//
// See docs/architecture/components/wiring.md for the dependency graph, flags, ports and
// environment variables, and docs/reference/cli-patrolcd.md for the command reference.
package server
