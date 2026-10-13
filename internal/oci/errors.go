package oci

import (
	"errors"
	"net/http"

	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// Sentinel errors. Callers branch on them with errors.Is. Error messages keep the
// upstream wording; the sentinel is attached without changing the text.
var (
	// ErrInvalidReference reports a malformed repository URL, tag or digest.
	ErrInvalidReference = errors.New("oci: invalid reference")
	// ErrNotFound reports a tag, digest or repository the registry does not know.
	ErrNotFound = errors.New("oci: not found")
	// ErrUnauthorized reports a 401/403 answer from the registry.
	ErrUnauthorized = errors.New("oci: unauthorized")
	// ErrNoMatchingVersion reports a semver constraint no tag satisfies.
	ErrNoMatchingVersion = errors.New("oci: no version matching constraint")
	// ErrInvalidConstraint reports a revision that is neither a version, a valid
	// constraint, nor one of the tags.
	ErrInvalidConstraint = errors.New("oci: invalid semver constraint")
	// ErrManifestTooLarge reports a manifest above Limits.MaxManifestBytes.
	ErrManifestTooLarge = errors.New("oci: manifest too large")
	// ErrUnsupportedManifest reports an image index or another non-manifest document.
	ErrUnsupportedManifest = errors.New("oci: unsupported manifest")
	// ErrTooManyLayers reports a manifest with more than MaxLayers layers.
	ErrTooManyLayers = errors.New("oci: too many layers")
	// ErrMediaTypeNotAllowed reports a content layer outside the allow-list.
	ErrMediaTypeNotAllowed = errors.New("oci: layer media type not allowed")
	// ErrContentLayerCount reports zero or several content layers.
	ErrContentLayerCount = errors.New("oci: expected a single content layer")
	// ErrLayerTooLarge reports a content layer above Limits.MaxLayerBytes.
	ErrLayerTooLarge = errors.New("oci: layer too large")
	// ErrInvalidHelmChart reports a Helm chart layer that does not contain exactly
	// one top-level directory.
	ErrInvalidHelmChart = errors.New("oci: invalid helm chart layer")
	// ErrDestinationExists reports a PullArtifact destination that exists and is
	// not an empty directory.
	ErrDestinationExists = errors.New("oci: destination exists")
	// ErrTooManyTags reports a tag listing above the tag cap.
	ErrTooManyTags = errors.New("oci: too many tags")
	// ErrInvalidTLSConfig reports unusable CA or client certificate data.
	ErrInvalidTLSConfig = errors.New("oci: invalid TLS configuration")
)

// taggedError attaches a sentinel to an error without changing its message.
type taggedError struct {
	kind error
	err  error
}

func (e *taggedError) Error() string   { return e.err.Error() }
func (e *taggedError) Unwrap() []error { return []error{e.kind, e.err} }

func tag(kind, err error) error {
	if err == nil {
		return nil
	}
	return &taggedError{kind: kind, err: err}
}

// classify attaches ErrNotFound / ErrUnauthorized to registry errors.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errdef.ErrNotFound) {
		return tag(ErrNotFound, err)
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return tag(ErrUnauthorized, err)
		case http.StatusNotFound:
			return tag(ErrNotFound, err)
		}
	}
	if errors.Is(err, errdef.ErrInvalidReference) || errors.Is(err, errdef.ErrInvalidDigest) ||
		errors.Is(err, errdef.ErrMissingReference) {
		return tag(ErrInvalidReference, err)
	}
	return err
}
