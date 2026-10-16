// Package hydrator is the source hydrator controller: for Applications with
// spec.sourceHydrator it renders the dry source through the repo-server and commits the
// result to the sync (or hydrateTo) branch through the commit-server, then requests a normal
// refresh so the Application compares and syncs the hydrated commit.
//
// Two controllers are registered with the manager (Setup, only when hydrator.enabled):
//
//   - app-hydration (ReconcileApp; one reconcile per Application, default one worker)
//     decides whether an Application needs hydration (APP-114): no previous operation, a
//     hard hydrate request, a spec change, a failed operation after its two-minute cooldown
//     (or with an explicit request), or a new dry revision (optionally limited to
//     manifest-generate-paths). It records status.sourceHydrator.lastComparedDryRevision
//     when no hydration follows, consumes the cd.patrol.example/hydrate annotation, polls
//     the dry branch with RequeueAfter, re-enqueues Hydrating operations older than the
//     refresh timeout, and requests the Application's hydration Key.
//   - manifest-hydration (ReconcileKey; keyed by Key, Processors workers; the workqueue
//     never processes one Key concurrently) hydrates every Application sharing the Key in
//     one commit (APP-116): it marks them Hydrating, validates them (APP-117), renders the
//     first one at the dry targetRevision and the others at the resolved dry SHA, calls the
//     commit-server, and records Hydrated (or Failed for the whole group).
//
// Status writes are JSON merge patches of the status subresource carrying the observed
// resourceVersion (optimistic concurrency); conflicts are retried on a fresh read through
// the API reader. Only status.sourceHydrator is written, plus the hydrate annotation
// (removed with a JSON patch test operation, so a newer request is never lost) and the
// refresh annotation after a successful hydration.
//
// Repository access comes from RepositoryResolver (read, for the dry source) and
// WriteCredentialResolver (write, for the destination repository); Repositories adapts
// internal/repository. Credentials are forwarded per request and never persisted or
// logged; failure messages stored in the status have URL passwords redacted.
package hydrator
