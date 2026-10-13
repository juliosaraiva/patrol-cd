package git

import (
	"errors"
	"fmt"
)

// Sentinel errors. Callers branch on them with errors.Is.
var (
	// ErrInvalidRevision reports a revision, branch, namespace or path argument that
	// is rejected before reaching git (for example a value starting with '-', which
	// git could parse as an option), or a non-SHA where a commit SHA is required.
	ErrInvalidRevision = errors.New("git: invalid revision")
	// ErrRevisionNotFound reports a revision that cannot be resolved to a commit.
	// *RevisionNotFoundError matches it.
	ErrRevisionNotFound = errors.New("git: revision not found")
	// ErrAuthentication reports that the remote rejected or required credentials,
	// or that SSH host key verification failed.
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
	// ErrNotInitialized reports a working-copy operation on a root that holds no
	// repository (call Init first).
	ErrNotInitialized = errors.New("git: repository not initialized")
)

// CommandError reports a git command that failed. Message never contains
// credentials: secrets known to the client are redacted from it.
type CommandError struct {
	// Op is the git subcommand (for example "fetch", "ls-remote").
	Op string
	// Message is the failure description including the (redacted) stderr tail.
	Message string
	// Kind is a package sentinel classifying the failure (ErrAuthentication,
	// ErrRevisionNotFound, ...) or nil when the failure is not classified.
	Kind error
	// Err is the underlying error (sandbox error such as *sandbox.ExitError,
	// sandbox.ErrTimeout, or a context error).
	Err error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("git %s: %s", e.Op, e.Message)
}

// Unwrap returns the classification sentinel (if any) and the underlying error, so
// errors.Is matches both (for example ErrAuthentication and sandbox.ErrTimeout).
func (e *CommandError) Unwrap() []error {
	out := make([]error, 0, 2)
	if e.Kind != nil {
		out = append(out, e.Kind)
	}
	if e.Err != nil {
		out = append(out, e.Err)
	}
	return out
}

// RevisionNotFoundError reports a revision that could not be resolved. Its message
// matches upstream ("unable to resolve '<rev>' to a commit SHA"). It matches
// ErrRevisionNotFound.
type RevisionNotFoundError struct {
	Revision string
}

func (e *RevisionNotFoundError) Error() string {
	return fmt.Sprintf("unable to resolve '%s' to a commit SHA", truncateForMessage(e.Revision))
}

// Is reports whether target is ErrRevisionNotFound.
func (e *RevisionNotFoundError) Is(target error) bool { return target == ErrRevisionNotFound }

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
		return fmt.Sprintf("git: signature of %s: %s (key_id=%s)", truncateForMessage(e.Revision), e.Info.Result, e.Info.KeyID)
	}
	return fmt.Sprintf("git: signature of %s: %s", truncateForMessage(e.Revision), e.Info.Result)
}

// Unwrap returns ErrSignatureInvalid.
func (e *SignatureError) Unwrap() error { return ErrSignatureInvalid }

// maxMessageValue bounds untrusted values (revisions, ref names) echoed in errors.
const maxMessageValue = 256

func truncateForMessage(s string) string {
	if len(s) <= maxMessageValue {
		return s
	}
	cut := maxMessageValue
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
