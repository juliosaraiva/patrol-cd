// Package auth holds Patrol CD's identity primitives (ADR-0009): local accounts parsed
// from the settings ConfigMap/Secret, bcrypt password hashing and policy, HS256 session
// and API tokens with key rotation, token revocation, login throttling, redirect-URL
// validation and HTTP cookie helpers.
//
// The package is deliberately free of Kubernetes client code and of CRD types. Callers
// adapt their storage through small interfaces:
//
//   - AccountLookup returns the current Account for a name (typically backed by a settings
//     watcher calling AccountsFromMaps on every ConfigMap/Secret change).
//   - TokenRegistry answers whether a project-role token (subject "proj:<project>:<role>")
//     is still recorded in AppProject status.
//   - RevocationStore records revoked token IDs until they expire (see the revocation
//     sub-package for the Secret-backed implementation).
//   - ExternalVerifier verifies bearer tokens issued by an external IdP (see the oidc
//     sub-package).
//
// Secret mutations (password change, token creation/deletion) are pure functions that
// return the new Secret data; callers persist them with resourceVersion-based optimistic
// concurrency and retry on conflict.
package auth
