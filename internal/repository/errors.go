package repository

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Sentinel errors. Errors returned by this package wrap one of them when the cause is known,
// so callers can use errors.Is and ConditionReason.
var (
	// ErrNotSynced is returned by lookups before the Lister finished its initial listing.
	ErrNotSynced = errors.New("repository lister is not synced")
	// ErrSecretNotFound means a referenced Secret does not exist or is not labeled for
	// Patrol CD (cd.patrol.example/secret-type).
	ErrSecretNotFound = errors.New("secret not found")
	// ErrSecretKeyMissing means a referenced Secret lacks a key the configuration requires.
	ErrSecretKeyMissing = errors.New("secret key missing")
	// ErrSCMProviderNotFound means spec.scmProviderRef names no SCMProvider.
	ErrSCMProviderNotFound = errors.New("scm provider not found")
	// ErrInvalidConfiguration means the repository configuration cannot be used as written.
	ErrInvalidConfiguration = errors.New("invalid repository configuration")
	// ErrInvalidCredentials means stored credential material cannot be parsed (for example a
	// malformed Google service account key).
	ErrInvalidCredentials = errors.New("invalid repository credentials")
	// ErrTokenUnavailable means a short-lived token could not be obtained from its issuer
	// (GitHub, Google, Microsoft Entra ID, Azure Container Registry).
	ErrTokenUnavailable = errors.New("failed to obtain repository access token")
	// ErrRepositoryNotFound is returned by name lookups of the Lister.
	ErrRepositoryNotFound = errors.New("repository not found")
)

// IsConfigurationError reports whether err is a configuration problem (missing Secret or
// key, unknown SCMProvider, invalid configuration or unparsable credentials) rather than a
// connectivity or issuer failure. Upstream reports these as "Configuration error".
func IsConfigurationError(err error) bool {
	return errors.Is(err, ErrSecretNotFound) || errors.Is(err, ErrSecretKeyMissing) ||
		errors.Is(err, ErrSCMProviderNotFound) || errors.Is(err, ErrInvalidConfiguration) ||
		errors.Is(err, ErrInvalidCredentials)
}

// ConditionReason maps an error of this package to the condition reason used on
// Repository and RepositoryCredential status (v1alpha1.Reason*). Nil maps to
// ReasonConnectionSuccessful, unknown errors to ReasonConnectionFailed.
func ConditionReason(err error) string {
	switch {
	case err == nil:
		return v1alpha1.ReasonConnectionSuccessful
	case errors.Is(err, ErrSecretNotFound):
		return v1alpha1.ReasonSecretNotFound
	case errors.Is(err, ErrSecretKeyMissing):
		return v1alpha1.ReasonSecretKeyMissing
	case errors.Is(err, ErrSCMProviderNotFound):
		return v1alpha1.ReasonSCMProviderNotFound
	case errors.Is(err, ErrInvalidCredentials):
		return v1alpha1.ReasonInvalidCredentials
	case errors.Is(err, ErrInvalidConfiguration):
		return v1alpha1.ReasonInvalidConfiguration
	default:
		return v1alpha1.ReasonConnectionFailed
	}
}

// redactedError hides secret values that may have been echoed by a dependency (an HTTP
// error body, a library message). Error() is redacted; Unwrap keeps errors.Is/As working
// for sentinels and typed errors. Callers must log Error(), not the unwrapped chain.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactionMinLen avoids replacing short values (for example "x" or "true") that would
// mangle messages without protecting anything.
const redactionMinLen = 4

// redact returns err with every occurrence of the given secret values in its message
// replaced by "******". It returns err unchanged when nothing matched.
func redact(err error, secrets []string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	out := redactString(msg, secrets)
	if out == msg {
		return err
	}
	return &redactedError{msg: out, err: err}
}

func redactString(s string, secrets []string) string {
	if len(secrets) == 0 || s == "" {
		return s
	}
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		if len(v) >= redactionMinLen {
			vals = append(vals, v)
		}
	}
	// Longest first so a secret containing another one is replaced whole.
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, "******")
	}
	return s
}

// configError builds an error wrapping a sentinel with an explanatory message.
func configError(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), sentinel)
}
