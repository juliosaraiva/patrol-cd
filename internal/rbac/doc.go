// Package rbac is Patrol CD's evaluator for the Casbin-style RBAC policy grammar used by
// Argo CD (ADR-0009). It is an independent implementation: no Casbin dependency.
//
// # Grammar
//
// A policy is CSV text, one rule per line. Blank lines and lines starting with '#' are
// ignored. Fields are parsed with encoding/csv (leading spaces trimmed, quotes honored):
//
//	p, <subject>, <resource>, <action>, <object>, <allow|deny>
//	g, <member>, <role>
//
// A request (subject, resource, action, object) is allowed for one subject when at least
// one 'p' rule whose subject is the request subject or one of its roles (transitively
// through 'g' lines, at most 10 levels) matches resource, action and object with effect
// "allow", and no such rule matches with effect "deny". Resource, action and object are
// patterns: glob patterns (github.com/gobwas/glob, no separators, so '*' also matches '/')
// in the default "glob" match mode, or unanchored RE2 regular expressions in "regex" mode.
// Subjects and roles are compared literally.
//
// # Request evaluation
//
// [Enforcer.Enforce] reproduces the Argo CD API server's evaluation order:
//
//  1. the default role (policy.default) is evaluated; an allow is final and cannot be
//     overridden by a deny for the user;
//  2. the project of the request is derived from the object for project-scoped resources
//     ("<project>/<name>" or "<project>/<namespace>/<name>") and for "projects" (the
//     object itself); that project's role policies are added for this request only;
//  3. a project-role token (Subjects.ProjectRole) may only act inside its own project;
//  4. the user subject is evaluated, then each group that appears as the member of a 'g'
//     line. Each subject is evaluated independently: a deny for one subject does not
//     cancel an allow obtained through another subject or the default role.
//
// [Enforcer] is safe for concurrent use. Reads are lock-free: every policy update builds a
// new immutable snapshot and swaps an atomic pointer.
package rbac
