// Package examples holds the tests that validate everything under examples/: every
// Patrol CD object is dry-run created against a real kube-apiserver (envtest) with the CRDs
// of deploy/crds, every source (Helm charts, Kustomizations, Jsonnet, directories, the
// Config Management Plugin) is rendered with the real tools, ApplicationSets are expanded
// with the Patrol CD generators, and settings, RBAC, notifications and Lua customizations are
// parsed by the packages that consume them. See docs/guides/examples.md.
package examples
