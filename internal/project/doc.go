// Package project evaluates AppProject policy and validates Application specs against it.
//
// It re-implements the project semantics of Argo CD v3.5.5 (AppProject methods, util/argo
// validation, sync windows, destination service accounts, global projects) as pure
// functions over the api/v1alpha1 types plus small lookup interfaces, so the API server and
// the application controller enforce exactly the same rules (ADR-0013, tenant boundary in
// docs/architecture/overview.md).
//
// # Matching rules
//
//   - Source repositories: both the pattern and the repository URL are normalized with
//     [NormalizeGitURL]; patterns are gobwas globs with '/' as separator ('*' stays inside
//     one path segment, '**' crosses segments); the literal pattern "*" matches everything.
//     A "!" prefix negates an entry: a source matching a negated entry is rejected whatever
//     the order of the entries ("deny wins").
//   - Destinations: server, name and namespace globs (no separators) with "!" negation,
//     evaluated against the registered cluster the Application destination resolves to.
//   - Resource kinds: path.Match patterns on group and kind (and name for cluster-scoped
//     lists).
//   - Sync windows: robfig/cron 5-field schedules (the upstream parser options: no seconds
//     field, no descriptors; a "TZ=" or "CRON_TZ=" prefix is honored) shifted by the UTC
//     offset of the window's time zone at evaluation time.
//
// Upstream behaviors that look surprising are kept on purpose and pinned by tests, except
// where they weaken the tenant boundary: project-scoped repositories and clusters widen a
// project by exactly themselves (never as patterns), and a project token with a jti is
// matched by its jti only. See docs/architecture/components/project.md for the list.
//
// # Concurrency
//
// Every function is safe for concurrent use. Inputs are never modified except by the
// functions documented as mutating (Normalize*, Add*, Remove*, MergeVirtualProject callers
// receive copies).
package project
