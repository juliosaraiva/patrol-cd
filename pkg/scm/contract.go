package scm

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ContractVersion is the version of the SCM provider contract implemented by this package.
// Providers report it in [Info.ContractVersion]; a provider built against a different
// contract version is rejected by [Registry.New].
const ContractVersion = "v1"

// Capability names one optional feature of a provider.
type Capability string

// Capabilities defined by contract v1.
const (
	CapabilityRepositories   Capability = "repositories"
	CapabilityBranches       Capability = "branches"
	CapabilityRevisions      Capability = "revisions"
	CapabilityPaths          Capability = "paths"
	CapabilityPullRequests   Capability = "pullRequests"
	CapabilityWebhooks       Capability = "webhooks"
	CapabilityCommitStatuses Capability = "commitStatuses"
	CapabilityCheckRuns      Capability = "checkRuns"
	CapabilityCredentials    Capability = "credentials"
	CapabilityInstallations  Capability = "installations"
)

// AllCapabilities returns every capability defined by the contract, in a stable order.
func AllCapabilities() []Capability {
	return []Capability{
		CapabilityRepositories,
		CapabilityBranches,
		CapabilityRevisions,
		CapabilityPaths,
		CapabilityPullRequests,
		CapabilityWebhooks,
		CapabilityCommitStatuses,
		CapabilityCheckRuns,
		CapabilityCredentials,
		CapabilityInstallations,
	}
}

// Known reports whether c is defined by the contract.
func (c Capability) Known() bool { return slices.Contains(AllCapabilities(), c) }

// Info identifies a provider instance and declares its capabilities.
type Info struct {
	// Type is the provider type as registered (e.g. "github").
	Type string
	// Name is the instance name chosen by the caller (e.g. the SCMProvider object name).
	Name string
	// APIURL is the normalized API base URL the instance talks to (empty for providers that
	// have no HTTP API, such as the in-memory fake).
	APIURL string
	// ContractVersion must equal [ContractVersion].
	ContractVersion string
	// Capabilities lists the declared capabilities. Only declared capabilities are served
	// by [As].
	Capabilities []Capability
}

// Has reports whether the capability is declared.
func (i Info) Has(c Capability) bool { return slices.Contains(i.Capabilities, c) }

// Provider is the minimal interface every SCM provider implements.
type Provider interface {
	Info() Info
}

// RepoRef identifies a repository within a provider: Owner is the organization, user,
// group path or project (provider specific) and Name the repository name.
type RepoRef struct {
	Owner string
	Name  string
}

// FullName returns "owner/name".
func (r RepoRef) FullName() string { return r.Owner + "/" + r.Name }

func (r RepoRef) String() string { return r.FullName() }

// ParseFullName splits "owner/name" into a RepoRef. Owners containing '/' (GitLab
// subgroups) are supported: the last segment is the name.
func ParseFullName(full string) (RepoRef, error) {
	i := strings.LastIndexByte(full, '/')
	if i <= 0 || i == len(full)-1 {
		return RepoRef{}, &Error{Kind: KindInvalid, Op: "ParseFullName", Err: fmt.Errorf("repository full name %q is not of the form owner/name", full)}
	}
	return RepoRef{Owner: full[:i], Name: full[i+1:]}, nil
}

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Validate rejects references that could escape the intended API path when interpolated
// into a URL: empty segments, '/', '.', '..', and characters outside [A-Za-z0-9_.-].
// Providers whose owners legitimately contain '/' (GitLab groups) validate owners
// themselves and only call [ValidateSegment] on each part.
func (r RepoRef) Validate() error {
	if err := ValidateSegment("owner", r.Owner); err != nil {
		return err
	}
	return ValidateSegment("repository", r.Name)
}

// ValidateSegment validates a single URL path segment taken from untrusted input.
func ValidateSegment(what, s string) error {
	if s == "" || s == "." || s == ".." || len(s) > 256 || !safeSegment.MatchString(s) {
		return &Error{Kind: KindInvalid, Op: "Validate", Err: fmt.Errorf("invalid %s name %q", what, s)}
	}
	return nil
}

// CloneProtocol selects which URL is reported in [Repository.URL].
type CloneProtocol string

// Clone protocols. The empty value means the provider default (SSH where available).
const (
	CloneProtocolDefault CloneProtocol = ""
	CloneProtocolSSH     CloneProtocol = "ssh"
	CloneProtocolHTTPS   CloneProtocol = "https"
)

// ArchivedFilter selects how archived repositories are treated by ListRepositories.
type ArchivedFilter string

// Archived filters. ArchivedDefault keeps the provider's upstream default (GitHub:
// include, GitLab: exclude).
const (
	ArchivedDefault ArchivedFilter = ""
	ArchivedInclude ArchivedFilter = "include"
	ArchivedExclude ArchivedFilter = "exclude"
)

// RepoQuery selects repositories for [RepositoryLister.ListRepositories].
type RepoQuery struct {
	// Organization is the owner to scan (GitHub organization or user login).
	Organization string
	// Topics, when non-empty, keeps only repositories carrying all of these topics.
	Topics []string
	// Archived controls archived repositories.
	Archived ArchivedFilter
	// CloneProtocol selects Repository.URL. Unknown values fail with KindInvalid.
	CloneProtocol CloneProtocol
	// AllBranches expands every branch of every kept repository into its own entry. When
	// false, one entry per repository is returned for its default branch, and
	// repositories whose default branch does not exist (empty repositories) are omitted.
	AllBranches bool
	// Match, when set, is evaluated for each repository before branch expansion (the
	// candidate has Branch set to the default branch and SHA empty). Returning false
	// drops the repository without spending API calls on its branches. ApplicationSet
	// repository-level filters are pushed down through this hook.
	Match func(Repository) bool
}

// Repository is one repository (or, when branches are expanded, one repository branch).
type Repository struct {
	Organization string
	Name         string
	FullName     string
	// URL is the clone URL selected by RepoQuery.CloneProtocol.
	URL           string
	HTTPSURL      string
	SSHURL        string
	WebURL        string
	Branch        string
	SHA           string
	DefaultBranch string
	// Labels are the repository topics (GitHub) or the provider's equivalent.
	Labels []string
	// RepositoryID is the provider-native repository ID (GitHub: int64).
	RepositoryID any
	Archived     bool
	Private      bool
	Fork         bool
}

// Ref returns the repository reference.
func (r Repository) Ref() RepoRef { return RepoRef{Owner: r.Organization, Name: r.Name} }

// Branch is a branch and its head commit.
type Branch struct {
	Name      string
	SHA       string
	Protected bool
}

// PRState selects pull requests by state.
type PRState string

// Pull request states. The empty value means open.
const (
	PRStateOpen   PRState = "open"
	PRStateClosed PRState = "closed"
	PRStateMerged PRState = "merged"
	PRStateAll    PRState = "all"
)

// PRQuery selects pull requests for [PullRequestLister.ListPullRequests].
type PRQuery struct {
	// Labels keeps only pull requests that carry all of these labels (exact match).
	Labels []string
	// State defaults to open.
	State PRState
	// TargetBranch, when set, keeps only pull requests targeting this branch.
	TargetBranch string
}

// PullRequest is a normalized pull/merge request.
type PullRequest struct {
	Number       int64
	Title        string
	Branch       string
	TargetBranch string
	HeadSHA      string
	Author       string
	Labels       []string
	URL          string
	State        string
	Draft        bool
	Merged       bool
}

// CommitState is the state of a commit status.
type CommitState string

// Commit status states.
const (
	CommitStatePending CommitState = "pending"
	CommitStateSuccess CommitState = "success"
	CommitStateFailure CommitState = "failure"
	CommitStateError   CommitState = "error"
)

// CommitStatus is a commit status to report.
type CommitStatus struct {
	State       CommitState
	Context     string
	Description string
	TargetURL   string
}

// Validate checks the status before any API call is made.
func (s CommitStatus) Validate() error {
	switch s.State {
	case CommitStatePending, CommitStateSuccess, CommitStateFailure, CommitStateError:
	default:
		return &Error{Kind: KindInvalid, Op: "SetCommitStatus", Err: fmt.Errorf("invalid commit status state %q", s.State)}
	}
	if strings.TrimSpace(s.Context) == "" {
		return &Error{Kind: KindInvalid, Op: "SetCommitStatus", Err: fmt.Errorf("commit status context is required")}
	}
	return nil
}

// CheckStatus is the status of a check run.
type CheckStatus string

// Check run statuses.
const (
	CheckStatusQueued     CheckStatus = "queued"
	CheckStatusInProgress CheckStatus = "in_progress"
	CheckStatusCompleted  CheckStatus = "completed"
)

// CheckConclusion is the conclusion of a completed check run.
type CheckConclusion string

// Check run conclusions.
const (
	CheckConclusionNone           CheckConclusion = ""
	CheckConclusionSuccess        CheckConclusion = "success"
	CheckConclusionFailure        CheckConclusion = "failure"
	CheckConclusionNeutral        CheckConclusion = "neutral"
	CheckConclusionCancelled      CheckConclusion = "cancelled"
	CheckConclusionSkipped        CheckConclusion = "skipped"
	CheckConclusionTimedOut       CheckConclusion = "timed_out"
	CheckConclusionActionRequired CheckConclusion = "action_required"
)

// CheckRun is a check run to create or update. Upserts are keyed by (Name, ExternalID)
// on the target commit.
type CheckRun struct {
	Name        string
	ExternalID  string
	Status      CheckStatus
	Conclusion  CheckConclusion
	Title       string
	Summary     string
	Text        string
	DetailsURL  string
	StartedAt   time.Time
	CompletedAt time.Time
}

// Validate checks the check run before any API call is made.
func (c CheckRun) Validate() error {
	op := "UpsertCheckRun"
	if strings.TrimSpace(c.Name) == "" {
		return &Error{Kind: KindInvalid, Op: op, Err: fmt.Errorf("check run name is required")}
	}
	if strings.TrimSpace(c.ExternalID) == "" {
		return &Error{Kind: KindInvalid, Op: op, Err: fmt.Errorf("check run external ID is required for idempotent upserts")}
	}
	switch c.Status {
	case CheckStatusQueued, CheckStatusInProgress:
		if c.Conclusion != CheckConclusionNone {
			return &Error{Kind: KindInvalid, Op: op, Err: fmt.Errorf("conclusion %q requires status completed", c.Conclusion)}
		}
	case CheckStatusCompleted:
		switch c.Conclusion {
		case CheckConclusionSuccess, CheckConclusionFailure, CheckConclusionNeutral, CheckConclusionCancelled,
			CheckConclusionSkipped, CheckConclusionTimedOut, CheckConclusionActionRequired:
		default:
			return &Error{Kind: KindInvalid, Op: op, Err: fmt.Errorf("completed check run needs a valid conclusion, got %q", c.Conclusion)}
		}
	default:
		return &Error{Kind: KindInvalid, Op: op, Err: fmt.Errorf("invalid check run status %q", c.Status)}
	}
	if c.Summary != "" && c.Title == "" {
		return &Error{Kind: KindInvalid, Op: op, Err: fmt.Errorf("check run title is required when a summary is set")}
	}
	return nil
}

// Token is a short- or long-lived credential usable for Git over HTTPS and API calls.
// Its String and LogValue methods never reveal the value.
type Token struct {
	Value string
	// ExpiresAt is zero for tokens without a known expiry (personal access tokens).
	ExpiresAt time.Time
	// Username is the HTTP basic-auth user name to pair with Value for Git over HTTPS
	// (GitHub: "x-access-token").
	Username string
}

func (t Token) String() string {
	return fmt.Sprintf("Token{Username:%q, Value:<redacted>, ExpiresAt:%s}", t.Username, t.ExpiresAt.Format(time.RFC3339))
}

// GoString prevents %#v from printing the token value.
func (t Token) GoString() string { return t.String() }

// LogValue implements slog.LogValuer.
func (t Token) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("username", t.Username),
		slog.String("value", "<redacted>"),
		slog.Time("expiresAt", t.ExpiresAt),
	)
}

// Installation is a GitHub App installation (or equivalent provider concept).
type Installation struct {
	ID                  int64
	Account             string
	AccountType         string
	RepositorySelection string
	Suspended           bool
}

// InstallationRef identifies an installation referenced by a webhook event.
type InstallationRef struct {
	ID      int64
	Account string
}

// RepositoryLister lists repositories of an organization or user.
type RepositoryLister interface {
	ListRepositories(ctx context.Context, q RepoQuery) ([]Repository, error)
}

// BranchLister lists branches. With all=false only the default branch is returned (an
// empty slice for repositories without one).
type BranchLister interface {
	ListBranches(ctx context.Context, repo RepoRef, all bool) ([]Branch, error)
}

// RevisionResolver resolves a branch, tag or commit-ish to a full commit SHA. Unknown
// refs fail with KindNotFound.
type RevisionResolver interface {
	ResolveRevision(ctx context.Context, repo RepoRef, ref string) (string, error)
}

// PathChecker reports whether a path exists at a revision. A missing path is (false,
// nil). Paths containing ".." segments fail with KindInvalid.
type PathChecker interface {
	PathExists(ctx context.Context, repo RepoRef, revision, path string) (bool, error)
}

// PullRequestLister lists pull requests of a repository, following every page. A missing
// repository fails with KindNotFound.
type PullRequestLister interface {
	ListPullRequests(ctx context.Context, repo RepoRef, q PRQuery) ([]PullRequest, error)
}

// WebhookVerifier authenticates and normalizes webhook deliveries.
type WebhookVerifier interface {
	// MatchesWebhook reports whether the delivery headers belong to this provider.
	MatchesWebhook(headers http.Header) bool
	// VerifyAndParse verifies the signature over the exact raw body bytes and returns the
	// normalized event. With an empty secret no verification is possible: the event is
	// returned with Verified=false and the caller applies its unsigned-delivery policy.
	VerifyAndParse(headers http.Header, rawBody []byte, secret []byte) (*Event, error)
}

// CommitStatusReporter sets commit statuses.
type CommitStatusReporter interface {
	SetCommitStatus(ctx context.Context, repo RepoRef, sha string, status CommitStatus) error
}

// CheckRunReporter creates or updates check runs idempotently by ExternalID.
type CheckRunReporter interface {
	UpsertCheckRun(ctx context.Context, repo RepoRef, sha string, run CheckRun) (int64, error)
}

// CredentialSource mints or returns a credential for Git over HTTPS.
type CredentialSource interface {
	Token(ctx context.Context) (Token, error)
}

// InstallationLister lists app installations and resolves the installation for an owner.
type InstallationLister interface {
	ListInstallations(ctx context.Context) ([]Installation, error)
	FindInstallation(ctx context.Context, owner string) (Installation, error)
}

// capabilityOf maps a capability interface type to its capability name.
func capabilityOf[T any]() (Capability, bool) {
	switch any((*T)(nil)).(type) {
	case *RepositoryLister:
		return CapabilityRepositories, true
	case *BranchLister:
		return CapabilityBranches, true
	case *RevisionResolver:
		return CapabilityRevisions, true
	case *PathChecker:
		return CapabilityPaths, true
	case *PullRequestLister:
		return CapabilityPullRequests, true
	case *WebhookVerifier:
		return CapabilityWebhooks, true
	case *CommitStatusReporter:
		return CapabilityCommitStatuses, true
	case *CheckRunReporter:
		return CapabilityCheckRuns, true
	case *CredentialSource:
		return CapabilityCredentials, true
	case *InstallationLister:
		return CapabilityInstallations, true
	}
	return "", false
}

// CapabilityFor returns the capability served by the capability interface T.
func CapabilityFor[T any]() (Capability, bool) { return capabilityOf[T]() }

// As returns p as capability interface T if, and only if, p declares the matching
// capability in Info and implements T. Otherwise it returns an *UnsupportedError that
// matches ErrUnsupported.
func As[T any](p Provider) (T, error) {
	var zero T
	if p == nil {
		return zero, &UnsupportedError{Capability: "<nil provider>"}
	}
	info := p.Info()
	c, ok := capabilityOf[T]()
	if !ok {
		return zero, &UnsupportedError{Provider: info.Type, Name: info.Name, Capability: Capability(fmt.Sprintf("%T", (*T)(nil)))}
	}
	impl, implemented := p.(T)
	if !implemented || !info.Has(c) {
		return zero, &UnsupportedError{Provider: info.Type, Name: info.Name, Capability: c}
	}
	return impl, nil
}

// ShortSHA returns the first n characters of sha (or sha itself when shorter).
func ShortSHA(sha string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(sha) <= n {
		return sha
	}
	return sha[:n]
}

// TruncateRunes shortens s to at most n runes without splitting a UTF-8 sequence.
// Invalid UTF-8 bytes count as one rune each and are preserved.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
