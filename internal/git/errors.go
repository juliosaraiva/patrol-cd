package git

import (
	"errors"
	"fmt"
)

// Sentinel errors. Callers branch on them with errors.Is.
var (
	// ErrInvalidRevision reports a revision, branch, namespace or path argument that
	// is rejected before reaching git (for example a value starting with '-', which
	// git could parse as an option).
	ErrInvalidRevision = errors.New("git: invalid revision")
	// ErrRevisionNotFound reports a revision that cannot be resolved to a commit.
	ErrRevisionNotFound = errors.New("git: revision not found")
	// ErrAuthentication reports that the remote rejected or required credentials.
	ErrAuthentication = errors.New("git: authentication required")
	// ErrNothingToCommit reports that CommitAll found no changes; the returned SHA is
	// the unchanged HEAD.
	ErrNothingToCommit = errors.New("git: nothing to commit")
	// ErrNoteNotFound reports that a commit has no note in the requested namespace.
	ErrNoteNotFound = errors.New("git: no note found")
	// ErrUnsigned reports a commit or tag without a signature.
	ErrUnsigned = errors.New("git: revision is not signed")
	// ErrSignatureInvalid reports a signature that GnuPG did not accept as good
	// (bad, expired, revoked, or made with a key missing from the keyring).
	// *SignatureError unwraps to it.
	ErrSignatureInvalid = errors.New("git: signature verification failed")
	// ErrInvalidURL reports a repository URL that cannot be used.
	ErrInvalidURL = errors.New("git: invalid repository URL")
	// ErrInvalidCredentials reports credential material that cannot be used (for
	// example an unsafe SSH proxy URL).
	ErrInvalidCredentials = errors.New("git: invalid credentials")
)

// CommandError reports a git command that failed. Message never contains
// credentials: secrets known to the client are redacted from it.
type CommandError struct {
	// Op is the git subcommand (for example "fetch", "ls-remote").
	Op string
	// Message is the failure description including the (redacted) stderr tail.
	Message string
	// Err is the underlying error (sandbox error, *sandbox.ExitError, or a sentinel).
	Err error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("git %s: %s", e.Op, e.Message)
}

// Unwrap returns the underlying error.
func (e *CommandError) Unwrap() error { return e.Err }

// SignatureError reports a signature that was found but not accepted.
// It unwraps to ErrSignatureInvalid.
type SignatureError struct {
	// Revision is the verified commit or tag.
	Revision string
	// Info is the parsed GnuPG status.
	Info SignatureInfo
}

func (e *SignatureError) Error() string {
	if e.Info.KeyID != "" {
		return fmt.Sprintf("git: signature of %s: %s (key_id=%s)", e.Revision, e.Info.Result, e.Info.KeyID)
	}
	return fmt.Sprintf("git: signature of %s: %s", e.Revision, e.Info.Result)
}

// Unwrap returns ErrSignatureInvalid.
func (e *SignatureError) Unwrap() error { return ErrSignatureInvalid }
