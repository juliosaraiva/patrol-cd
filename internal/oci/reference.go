package oci

import (
	"fmt"
	"strings"

	"oras.land/oras-go/v2/registry"
)

// Scheme is the URL prefix that marks an OCI repository in an application source.
const Scheme = "oci://"

// TrimScheme removes one leading "oci://" from repoURL, exactly as the upstream
// clients do (strings.TrimPrefix). Other schemes are left untouched and later
// rejected as invalid references.
func TrimScheme(repoURL string) string {
	return strings.TrimPrefix(repoURL, Scheme)
}

// IsOCIURL reports whether repoURL carries the "oci://" prefix.
func IsOCIURL(repoURL string) bool {
	return strings.HasPrefix(repoURL, Scheme)
}

// Repository identifies a repository inside a registry.
type Repository struct {
	// Registry is the host[:port] of the registry.
	Registry string
	// Path is the repository name inside the registry (for example
	// "charts/nginx").
	Path string
}

// String returns "registry/path".
func (r Repository) String() string { return r.Registry + "/" + r.Path }

// ParseRepository parses a repository URL ("oci://host[:port]/path" or
// "host[:port]/path"). A tag or digest in the URL is rejected: references are
// always passed separately.
func ParseRepository(repoURL string) (Repository, error) {
	trimmed := TrimScheme(repoURL)
	if trimmed == "" {
		return Repository{}, fmt.Errorf("%w: empty repository URL", ErrInvalidReference)
	}
	ref, err := registry.ParseReference(trimmed)
	if err != nil {
		return Repository{}, tag(ErrInvalidReference, fmt.Errorf("invalid repository %q: %w", trimmed, err))
	}
	if ref.Reference != "" {
		return Repository{}, fmt.Errorf("%w: repository URL %q must not contain a tag or digest", ErrInvalidReference, trimmed)
	}
	return Repository{Registry: ref.Registry, Path: ref.Repository}, nil
}

// TagFromVersion converts a semantic version into an OCI tag. OCI tags cannot
// contain '+', so Helm (and upstream) store build metadata with '_' instead.
func TagFromVersion(version string) string {
	return strings.ReplaceAll(version, "+", "_")
}

// VersionFromTag is the inverse of TagFromVersion ("_" back to "+").
func VersionFromTag(t string) string {
	return strings.ReplaceAll(t, "_", "+")
}
