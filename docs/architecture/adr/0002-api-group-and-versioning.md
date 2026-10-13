# ADR-0002: API group, naming and versioning

Status: accepted — 2026-10-12

## Decision
- API group `cd.patrol.example`, version `v1alpha1`. `.example` is reserved (RFC 2606/6761),
  so the group can never collide with a real organization's group during development.
- Annotation/label prefix `cd.patrol.example/`; finalizers `cd.patrol.example/<name>`.
- Kind names follow Argo CD for migration familiarity (`Application`, `AppProject`,
  `ApplicationSet`) but short names are distinct (`papp`, `pproj`, `pappset`) and every CRD
  is in category `patrol`, so Patrol and Argo CD can coexist in one cluster (required for
  side-by-side verification and migration).
- Single served/storage version `v1alpha1`; conversion strategy `None`.

## Finalizing before public release
1. Choose an owned domain (e.g. `patrolcd.io`).
2. Run `hack/rename-api-group.sh cd.patrol.example <new-group>`; it rewrites the Go constant
   `api/v1alpha1.GroupName`, kubebuilder markers, label/annotation prefixes, and docs, then
   `make generate`.
3. Ship `v1beta1` with a conversion webhook (the `internal/conversion` hub/spoke pattern) and
   a storage-version migration runbook (`docs/operations/upgrade.md`).

## Consequences
Objects created with the development group are not automatically migrated to the final
group; the migration tool (`patrol admin migrate-group`) copies them.
