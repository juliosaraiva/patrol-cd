// Package oci fetches OCI artifacts (plain manifest bundles and Helm charts) from
// OCI distribution registries for the repo-server, re-implementing the behavior of
// the upstream baseline's util/oci on top of oras-go.
//
// The package provides:
//   - [Client.ResolveReference]: tag, digest or semver constraint over tags to a
//     manifest digest;
//   - [Client.ListTags]: all tags of a repository ("_" converted back to "+");
//   - [Client.FetchManifest] and [MetadataFromManifest]: manifest annotations for
//     revision metadata;
//   - [Client.PullArtifact]: download and safely extract the single content layer of
//     an artifact, enforcing the layer media-type allow-list, layer-count, manifest
//     and layer size limits, and archive extraction limits (via internal/sandbox);
//   - [MaxVersion], [IsVersion], [IsConstraint]: semver resolution shared with the
//     Helm package;
//   - [NewTransport]: the TLS/proxy HTTP transport used for repository access.
//
// Credentials are bound to the registry host of the repository URL passed to each
// call and are never logged. The package does not import api/v1alpha1: callers
// translate Repository/RepositoryCredential fields into [Options] (see the
// component document docs/architecture/components/helm-oci.md).
package oci
