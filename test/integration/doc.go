// Package integration holds the in-process integration tests of Patrol CD: every
// control-plane component runs against real kube-apiservers (envtest), real git over smart
// HTTP and the real repo-server tools, driven through the public API (pkg/apiclient) and the
// patrol CLI. The harness is package stack; scenarios are grouped per file (lifecycle_test.go,
// recovery_test.go, ...).
//
// Run with:
//
//	PATROL_INTEGRATION=1 go test -count=1 -timeout 60m ./test/integration/...
//
// (or with KUBEBUILDER_ASSETS set). Without either, every test is skipped.
package integration
