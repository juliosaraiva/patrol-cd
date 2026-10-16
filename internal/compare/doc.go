// Package compare is the application state comparison engine of the Patrol CD controller.
//
// It re-implements the observable behavior of Argo CD v3.5.5 controller/state.go
// (CompareAppState), controller/health.go, the info enrichment and resource tree assembly of
// controller/cache and controller/appcontroller.go, and the comparison helpers of util/argo,
// on top of the Patrol libraries:
//
//   - desired state: one repo-server GenerateManifest call per source (internal/reposerver/apiclient)
//     with credentials resolved by internal/repository and settings from internal/settings, or
//     caller-supplied local manifests;
//   - live state: the per-cluster cache of internal/cache (LiveStateCache wires it to cluster
//     REST configs and to this package's ResourceInfo population);
//   - managed resource attribution and self-reference checks: internal/tracking;
//   - diffing: internal/diff with the Application's ignoreDifferences and the system overrides;
//   - health: internal/health with internal/lua customizations;
//   - project policy: internal/project.
//
// The engine holds no authoritative state: every Result is recomputed from the repository and
// the live cluster. A Manager is safe for concurrent use.
package compare
