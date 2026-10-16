// Package integration holds the envtest integration tests of the application controller:
// both reconcilers run in a controller-runtime manager against a real kube-apiserver, with
// the real live-state cache, comparison, diff and sync engines and a fake repo-server. The
// API server is also the destination cluster (in-cluster style REST config).
package integration
