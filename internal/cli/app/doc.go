// Package app implements the `patrol app` and `patrol appset` command groups of the Patrol
// CD CLI: Application lifecycle (create, get, list, set, unset, edit, patch, delete,
// add-source, remove-source, confirm-deletion), synchronization (sync, wait, history,
// rollback, terminate-op), inspection (diff, manifests, resources, get-resource, logs),
// resource operations (patch-resource, delete-resource, actions list|run) and
// ApplicationSets (get, list, create, generate, delete).
//
// Commands talk to patrol-server only through the narrow Client interface (client.go),
// implemented over pkg/apiclient. Behavior follows the Argo CD v3.5.5 CLI (flag names,
// defaults, messages and exit codes) where the Patrol API allows it; differences are listed
// in docs/architecture/components/cli-app.md.
//
// Wiring: the CLI core calls Register(root, factory) once; see factory.go for the subset of
// the factory this package uses.
package app
