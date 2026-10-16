// Package commitserver implements the commit-server, the internal service that writes the
// manifests rendered by the source hydrator to git (ADR-0006: JSON over HTTP with a shared
// bearer token), and its typed client.
//
// One request (CommitHydratedManifestsRequest) carries the write credentials of the
// destination repository, the sync and target branches, the resolved dry commit and, per
// application path, the rendered manifests and render commands. The service:
//
//  1. clones the repository into a fresh per-request directory with the write credentials
//     (kept in memory and in per-command temporary files only, see internal/git);
//  2. checks out the sync branch, creating it as an orphan branch with an empty
//     "Initial commit for <branch>" when it does not exist, then checks out the target
//     branch, creating it from the sync branch when it does not exist;
//  3. returns the target branch head unchanged when its git note (namespace
//     NoteNamespace) records the same dry SHA and manifests digest;
//  4. for every path whose manifest.yaml differs from the committed one, replaces the files
//     the hydrator owns (manifest.yaml, hydrator.metadata, README.md) and, when anything
//     changed, the root hydrator.metadata and .gitattributes; other files are never touched:
//     an owned file name occupied by a directory fails the request instead of being deleted,
//     and a request whose paths overlap another path's owned files is rejected;
//  5. commits (author and message from the request) and pushes without force; a push (or
//     note push) rejected because another writer updated the branch is retried on a fresh
//     clone, so the request is re-applied on top of the new tip;
//  6. records the dry SHA → hydrated SHA mapping as a git note, also when nothing changed
//     (no empty commits are created).
//
// Requests for one repository are serialized inside a process; concurrent writers in other
// processes are handled by the push retry.
package commitserver
