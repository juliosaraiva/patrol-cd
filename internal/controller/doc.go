// Package controller wires the Patrol CD application controller: the Application reconciler
// (internal/controller/app), the Operation reconciler (internal/controller/operation) and the
// retention of terminal Operations (internal/controller/retention), on top of the live-state
// cache (internal/cache), the comparison engine (internal/compare) and the sync engine
// (internal/sync). Run builds every dependency from a REST config and runs the
// controller-runtime manager until its context ends.
//
// Behavior follows docs/architecture/sync-state-machine.md (normative) and ADR-0011; parity
// notes and known gaps are recorded in docs/architecture/components/controller.md.
package controller
