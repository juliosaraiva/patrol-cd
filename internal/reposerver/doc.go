// Package reposerver is the repo-server (ADR-0006, ADR-0007): it obtains application
// sources (git working copies, Helm charts, OCI artifacts, uploaded directories),
// renders them through internal/render and answers the repository queries of the
// controller, the API server and the ApplicationSet controller. It implements
// apiclient.Client in process (Service) and over HTTP (Handler, Run); the typed HTTP
// client lives in the apiclient package.
//
// The repo-server never reads Kubernetes objects: callers forward resolved credentials
// with each request. Credentials are kept in memory and in per-command 0600 files only
// (internal/git, internal/helm, internal/oci), never logged and never part of a cache
// key in clear text. Non-secret trust material (SSH known hosts, TLS CA bundles, GnuPG
// public keys) is read from mounted ConfigMap directories.
//
// Working copies are persistent per (repository location, credential fingerprint),
// so different credentials never share a checkout; they live under a root directory
// at random names, are mode 0000 while idle, are locked with git.RepoLock (concurrent
// holders allowed for the same revision) and are evicted least recently used.
//
// Caches are in memory and disposable (ADR-0004): resolved git refs (git.RefCache),
// Helm indexes (internal/helm), rendered manifests (LRU bounded by entries and bytes,
// keyed by a SHA-256 over every rendering input and the resolved revisions, with
// upstream's manifest-generation error caching), and secondary results (app details,
// listings, metadata, files, directories, changed files, OCI tags). Errors returned to
// callers have working-copy paths replaced by "<path to cached source>".
package reposerver
