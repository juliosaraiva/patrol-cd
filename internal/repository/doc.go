// Package repository resolves how Patrol CD components reach a repository: which
// Repository, RepositoryCredential and SCMProvider objects apply to a repository URL, which
// Secret keys hold its credentials, and which short-lived tokens (GitHub App installation
// tokens, Google OAuth tokens, Microsoft Entra tokens, Azure Container Registry refresh
// tokens) must be minted before a request is sent to the repo-server or the commit-server.
//
// The repo-server never reads Kubernetes Secrets and never holds GitHub App private keys
// (ADR-0006); the controller, the API server and the commit-server resolve credentials with
// this package and forward the resulting apiclient.Repository with each request.
//
// Lookup precedence mirrors Argo CD v3.5.5 (util/db):
//
//  1. a Repository of the requested purpose whose spec.url is the same location
//     (upstream SameURL) and whose spec.project is the requested project;
//  2. otherwise such a Repository without spec.project;
//  3. otherwise a stub for the URL;
//  4. a repository without credentials of its own inherits the RepositoryCredential (same
//     purpose) whose normalized spec.urlPrefix is the longest string prefix of the
//     normalized URL (CopyCredentialsFrom field rules);
//  5. the credential method is chosen in upstream order: password or bearer token, SSH
//     key, GitHub App, Google service account key, Azure workload identity, Azure service
//     principal, anonymous (git); Azure workload identity or username/password (Helm);
//     username/password (OCI).
//
// Objects are read from informers over the control-plane namespace (Lister); Secrets are
// visible only when they carry the cd.patrol.example/secret-type label of a repository,
// credential template or SCM provider Secret, and a reference is honored only when that
// label matches the referencing kind and purpose and, for a project-scoped Repository, the
// Secret is bound to its project by an ownerReference. SCMProvider credentials stay on the
// provider's host and transport settings, and token requests follow an endpoint policy
// (allowed issuer hosts, https only, no loopback or link-local destinations, no response
// bodies in errors). Secret values are never logged, never put in error messages and never
// kept beyond the token cache, which is keyed by digests.
package repository
