package commitserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// HTTP routes of the commit-server.
const (
	// PathCommitHydratedManifests accepts POST requests with a CommitHydratedManifestsRequest.
	PathCommitHydratedManifests = "/api/v1/commit-hydrated-manifests"
	// PathHealthz answers GET requests with 200 when the server is serving (no auth).
	PathHealthz = "/healthz"
)

// Files and git namespaces written by the commit-server.
const (
	// ManifestFileName holds the YAML stream of the hydrated manifests of one path.
	ManifestFileName = "manifest.yaml"
	// MetadataFileName holds hydration metadata (JSON), per path and at the repository root.
	MetadataFileName = "hydrator.metadata"
	// ReadmeFileName is rendered from the README template per path.
	ReadmeFileName = "README.md"
	// GitAttributesFileName marks generated files for git hosting services.
	GitAttributesFileName = ".gitattributes"
	// NoteNamespace is the git notes namespace (refs/notes/hydrator.metadata) recording which
	// dry commit a hydrated commit was produced from.
	NoteNamespace = "hydrator.metadata"
)

// Author defaults applied when the request leaves them empty (settings commit.author.name
// and commit.author.email unset).
const (
	DefaultAuthorName  = "Patrol CD"
	DefaultAuthorEmail = "patrol-cd@example.com"
)

// Author is the identity used for the hydrated commit, the orphan initial commit and notes.
type Author struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// PathDetails are the hydrated manifests of one application path.
type PathDetails struct {
	// Path is the directory relative to the repository root (syncSource.path). It must not
	// be the root, escape the repository or enter .git.
	Path string `json:"path"`
	// Manifests are the rendered objects, each a JSON object, in render order.
	Manifests []json.RawMessage `json:"manifests"`
	// Commands are the render commands reported by the repo-server. They are the per-path
	// README and hydrator.metadata data (with the repository URL and the dry SHA).
	Commands []string `json:"commands,omitempty"`
}

// CommitHydratedManifestsRequest asks the commit-server to write hydrated manifests.
type CommitHydratedManifestsRequest struct {
	// Repo is the destination repository with its write credentials.
	Repo apiclient.Repository `json:"repo"`
	// SyncBranch is the branch Applications sync from; created as an orphan when missing.
	SyncBranch string `json:"syncBranch"`
	// TargetBranch receives the commit (SyncBranch, or the hydrateTo branch); created from
	// SyncBranch when missing.
	TargetBranch string `json:"targetBranch"`
	// DrySHA is the resolved dry commit the manifests were rendered from.
	DrySHA string `json:"drySha"`
	// CommitMessage is the rendered commit message (commitMessageTemplate).
	CommitMessage string `json:"commitMessage,omitempty"`
	// Author is the commit identity; empty fields select DefaultAuthorName/DefaultAuthorEmail.
	Author Author `json:"author,omitzero"`
	// ReadmeTemplate is the README template (readmeMessageTemplate). Empty skips README.md.
	ReadmeTemplate string `json:"readmeTemplate,omitempty"`
	// Paths are the application paths to write, at most one entry per path.
	Paths []PathDetails `json:"paths"`
	// DryCommitMetadata is the dry commit's metadata (author, date, message, references),
	// written to the root hydrator.metadata.
	DryCommitMetadata *apiclient.RevisionMetadata `json:"dryCommitMetadata,omitempty"`
	// DryRepoURL is the dry source repository, recorded as repoURL in hydrator.metadata and
	// the README data (where "git checkout <drySha>" must work). Empty selects Repo.URL, as
	// upstream always does.
	DryRepoURL string `json:"dryRepoURL,omitempty"`
}

// LogValue describes the request without credentials or manifest content.
func (r *CommitHydratedManifestsRequest) LogValue() slog.Value {
	if r == nil {
		return slog.StringValue("<nil>")
	}
	return slog.GroupValue(
		slog.String("repo", git.SanitizeURL(r.Repo.URL)),
		slog.Bool("hasCredentials", r.Repo.HasCredentials()),
		slog.String("syncBranch", r.SyncBranch),
		slog.String("targetBranch", r.TargetBranch),
		slog.String("drySha", r.DrySHA),
		slog.Int("paths", len(r.Paths)),
	)
}

// CommitHydratedManifestsResponse is the result of a commit request.
type CommitHydratedManifestsResponse struct {
	// HydratedSHA is the target branch head holding the hydrated manifests: the new commit,
	// or the unchanged head when nothing changed.
	HydratedSHA string `json:"hydratedSha"`
}

// Client is the commit-server API as seen by the hydrator. Implementations: *Service
// (in-process) and *HTTPClient.
type Client interface {
	CommitHydratedManifests(ctx context.Context, req *CommitHydratedManifestsRequest) (*CommitHydratedManifestsResponse, error)
}

// Sentinel errors. The HTTP layer maps them to problem codes.
var (
	// ErrInvalidRequest reports a request that can never succeed as sent.
	ErrInvalidRequest = errors.New("invalid commit request")
	// ErrConcurrentUpdate reports that another writer kept updating the branch or the
	// notes ref and the push retries were exhausted.
	ErrConcurrentUpdate = errors.New("concurrent update of the hydrated branch")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// Error codes of problem responses (same vocabulary as the repo-server).
const (
	CodeInvalidArgument  = "invalid_argument"
	CodeUnauthenticated  = "unauthenticated"
	CodePermissionDenied = "permission_denied"
	CodeAborted          = "aborted"
	CodeUnavailable      = "unavailable"
	CodeDeadlineExceeded = "deadline_exceeded"
	CodeTooLarge         = "resource_exhausted"
	CodeInternal         = "internal"
)

// Error is a commit-server error returned by HTTPClient: the problem detail with its stable
// code and HTTP status.
type Error struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"detail"`
}

func (e *Error) Error() string { return "commit-server: " + e.Message }

// Is lets callers test errors returned by HTTPClient against the package sentinels.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrInvalidRequest:
		return e.Code == CodeInvalidArgument
	case ErrConcurrentUpdate:
		return e.Code == CodeAborted
	}
	return false
}
