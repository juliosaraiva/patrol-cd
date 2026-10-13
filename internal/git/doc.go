// Package git is the Git transport used by the repo-server, the controller (revision
// resolution) and the commit-server (hydrator writes). It drives the git CLI through
// internal/sandbox.Run and mirrors the observable behavior of upstream Argo CD's
// util/git package (v3.5.5): revision resolution over the remote ref listing
// (branches, tags, annotated tags, semver constraints, symbolic HEAD, truncated SHAs),
// fetch/checkout of a persistent working copy, commit metadata with
// Argocd-reference-commit-* trailers, GnuPG verification, changed files between two
// commits, and the write operations of the source hydrator.
//
// Credentials never reach the command line:
//   - HTTPS username/password are answered by a GIT_ASKPASS helper that is the current
//     executable re-executed in helper mode ([MaybeAskpassMain]); it reads them from a
//     0600 file inside a per-call 0700 temp directory and only answers prompts for the
//     repository's own origin;
//   - bearer tokens and forced basic-auth headers are passed as http.extraHeader
//     through GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n, scoped to the
//     repository's origin;
//   - SSH private keys, known_hosts data, TLS client certificates and CA bundles are
//     written to 0600 files in the same per-call directory, which is removed when the
//     command returns.
//
// Every git invocation runs with an exact environment (HOME=/dev/null,
// GIT_TERMINAL_PROMPT=0, GIT_LFS_SKIP_SMUDGE=1, credential helpers disabled) so the
// server's own git configuration, keys and credential stores are never used.
//
// The package does not import api/v1alpha1. Callers translate Repository /
// RepositoryCredential CRD fields and referenced Secret data into [HTTPSCreds],
// [SSHCreds] or [NopCreds] and client [Option]s (see
// docs/architecture/components/git.md).
package git
