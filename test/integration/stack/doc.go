// Package stack runs the Patrol CD control plane in-process for integration tests.
//
// A Stack is one complete installation: patrol-server (API, UI routes and the webhook
// endpoint), the repo-server, the application controller (with its tree API), the
// ApplicationSet controller and the notifications controller, all started through the same
// library entry points as `patrolcd` (internal/server.Run, RunRepoServer, RunController,
// RunAppSetController, RunNotificationsController). They run against real kube-apiservers
// started by envtest:
//
//   - the control cluster (CRDs from deploy/crds and the Operation admission policy), which is
//     also the destination "in-cluster" (PATROL_FAKE_IN_CLUSTER with a kubeconfig of the control
//     cluster, set by Main for this test process only);
//   - one shared destination cluster "dest-1", registered with a client certificate issued by
//     envtest's AddUser (a system:masters user of that API server);
//   - optional dedicated destination clusters per Stack (Options.DedicatedDestinations),
//     registered with a ServiceAccount bearer token obtained through the TokenRequest API. Their
//     kube-apiserver can be stopped and started again (Destination.Stop/Start) while etcd keeps
//     the data; certificates and the token stay valid across the restart.
//
// Sources come from real git repositories served over smart HTTP (git http-backend through
// net/http/cgi) with basic authentication; manifests are rendered by the real repo-server with
// the real git, helm and kustomize binaries. Tests talk to patrol-server through pkg/apiclient
// (local accounts with passwords, RBAC from patrol-rbac-cm) and can run the patrol CLI
// in-process (Stack.CLI).
//
// Envtest runs no kubelet, controller-manager or garbage collector: workloads never start, so
// tests use kinds that are healthy without controllers (ConfigMap, Secret, Service, Namespace,
// ServiceAccount, Role, CRDs) and simulate the health of Jobs and Deployments by writing their
// status (Stack.SetStatus / Cluster.SetStatus). A fake garbage collector removes the
// foregroundDeletion/orphan finalizers of deleted objects in the destination namespaces of a
// Stack so cascading deletions complete. Namespaces are never deleted (no namespace
// controller); every Stack uses its own namespaces.
//
// Every component logs through a capture sink: a test fails at cleanup when a component logged
// an ERROR record that does not match the Stack's allow-list (Options.AllowedErrors and
// Stack.AllowErrors). The complete logs are printed on failure (tail) and written to
// $PATROL_INTEGRATION_LOG_DIR when set.
//
// Tests run only when PATROL_INTEGRATION=1 or KUBEBUILDER_ASSETS is set; otherwise Start skips
// them. With PATROL_INTEGRATION=1 and no KUBEBUILDER_ASSETS the envtest binaries of
// bin/envtest/k8s/<version>-<os>-<arch> are used. Waits are polling loops bounded by
// Timeout, which scales every bound by PATROL_INTEGRATION_TIMEOUT_SCALE (default 1, or 2 when
// CI is set).
package stack
