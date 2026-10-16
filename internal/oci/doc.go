// Package oci fetches OCI artifacts (plain manifest bundles and Helm charts) from
// OCI distribution registries for the repo-server, re-implementing the behavior of
// the upstream baseline's util/oci on top of oras-go.
//
// The package provides:
//   - [ParseRepository], [TrimScheme]: repository URLs with or without "oci://"
//     (the prefix is stripped exactly as upstream; tags/digests in the URL are
//     rejected);
//   - [Client.ResolveReference]: tag, digest or semver constraint over tags to a
//     manifest digest;
//   - [Client.ListTags]: all tags of a repository ("_" converted back to "+"),
//     bounded by Options.MaxTags;
//   - [Client.FetchManifest] and [MetadataFromManifest]: digest-verified,
//     size-bounded manifests and their annotations for revision metadata;
//   - [Client.PullArtifact]: download and safely extract the single content layer
//     of an artifact, enforcing the layer media-type allow-list, layer-count,
//     manifest and layer size limits, digest verification before extraction, and
//     archive extraction limits (via internal/sandbox); Helm chart artifacts are
//     flattened to their chart directory;
//   - [Client.FetchHelmChart]: the verified chart .tgz of a Helm OCI chart, used
//     by internal/helm;
//   - [Client.Ping], [Client.CleanCache];
//   - [MaxVersion], [IsVersion], [IsConstraint]: semver resolution shared with the
//     Helm package;
//   - [NewTransport]: the TLS/proxy HTTP transport used for repository access.
//
// Credentials are bound to the registry host of the repository URL passed to each
// call and are never logged. The package does not import api/v1alpha1: callers
// translate Repository/RepositoryCredential fields into [Options] (see the
// component document docs/architecture/components/helm-oci.md).
//
// Test support: package ocitest provides an in-process registry.
package oci
