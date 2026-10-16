package gitlab

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Webhook headers.
const (
	HeaderEvent = "X-Gitlab-Event"
	// HeaderToken carries the secret token configured on the GitLab webhook. GitLab does
	// not sign the body: the token authenticates the sender only.
	HeaderToken = "X-Gitlab-Token"
	// HeaderEventUUID identifies the event (stable across manual re-sends); it is used as
	// the delivery ID.
	HeaderEventUUID = "X-Gitlab-Event-UUID"
)

// GitLab event header values.
const (
	EventPushHook         = "Push Hook"
	EventTagPushHook      = "Tag Push Hook"
	EventMergeRequestHook = "Merge Request Hook"
	EventSystemHook       = "System Hook"
)

// WebhookVerifier verifies and normalizes GitLab webhook deliveries. The zero value is
// ready to use and needs no API credentials.
type WebhookVerifier struct{}

var _ scm.WebhookVerifier = WebhookVerifier{}

// MatchesWebhook reports deliveries carrying X-Gitlab-Event.
func (WebhookVerifier) MatchesWebhook(h http.Header) bool { return h.Get(HeaderEvent) != "" }

func whErr(kind scm.ErrorKind, err error) error {
	return &scm.Error{Kind: kind, Provider: Type, Op: "VerifyAndParse", Err: err}
}

// equalSecret compares a presented secret with the configured one in constant time
// (both are hashed first so that their lengths do not leak either).
func equalSecret(got string, want []byte) bool {
	a := sha256.Sum256([]byte(got))
	b := sha256.Sum256(want)
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// VerifyAndParse implements scm.WebhookVerifier. With a non-empty secret the
// X-Gitlab-Token header must equal it (constant-time comparison); GitLab sends no body
// signature, so Verified=true authenticates the sender, not the body bytes. The token is
// checked before the event type is inspected. Supported events: Push Hook and Tag Push
// Hook (EventPush), Merge Request Hook (EventPullRequest) and System Hook deliveries
// whose object_kind is push, tag_push or merge_request. Other events fail with
// scm.ErrUnsupportedEvent after successful authentication.
func (WebhookVerifier) VerifyAndParse(h http.Header, body []byte, secret []byte) (*scm.Event, error) {
	if int64(len(body)) > scm.MaxWebhookPayloadBytes {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %d bytes", scm.ErrPayloadTooLarge, len(body)))
	}
	verified := false
	if len(secret) > 0 {
		tok := h.Get(HeaderToken)
		if tok == "" {
			return nil, scm.WebhookSignatureError(Type, scm.ErrSignatureMissing)
		}
		if !equalSecret(tok, secret) {
			return nil, scm.WebhookSignatureError(Type, scm.ErrSignatureMismatch)
		}
		verified = true
	}
	event := h.Get(HeaderEvent)
	if event == "" {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: missing %s header", scm.ErrMalformedPayload, HeaderEvent))
	}
	// The delivery ID is not authenticated: bound it before anyone stores it.
	deliveryID := h.Get(HeaderEventUUID)
	if err := scm.ValidateDeliveryID(deliveryID); err != nil {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%s: %w", HeaderEventUUID, err))
	}
	ev := &scm.Event{Provider: Type, DeliveryID: deliveryID, Verified: verified}
	kind := event
	if event == EventSystemHook {
		var k struct {
			ObjectKind string `json:"object_kind"`
		}
		if err := decode(body, &k); err != nil {
			return nil, err
		}
		switch k.ObjectKind {
		case "push":
			kind = EventPushHook
		case "tag_push":
			kind = EventTagPushHook
		case "merge_request":
			kind = EventMergeRequestHook
		default:
			return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: system hook %q", scm.ErrUnsupportedEvent, k.ObjectKind))
		}
	}
	var err error
	switch kind {
	case EventPushHook, EventTagPushHook:
		err = parsePush(body, ev)
	case EventMergeRequestHook:
		err = parseMergeRequest(body, ev)
	default:
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %q", scm.ErrUnsupportedEvent, event))
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
}

func decode(payload []byte, v any) error {
	if err := json.Unmarshal(payload, v); err != nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: %v", scm.ErrMalformedPayload, err))
	}
	return nil
}

type whProject struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	WebURL            string `json:"web_url"`
	GitSSHURL         string `json:"git_ssh_url"`
	GitHTTPURL        string `json:"git_http_url"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	VisibilityLevel   *int   `json:"visibility_level"`
}

type whCommit struct {
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

type whPush struct {
	ObjectKind        string     `json:"object_kind"`
	Ref               string     `json:"ref"`
	Before            string     `json:"before"`
	After             string     `json:"after"`
	UserUsername      string     `json:"user_username"`
	ProjectID         int64      `json:"project_id"`
	Project           *whProject `json:"project"`
	Commits           []whCommit `json:"commits"`
	TotalCommitsCount int        `json:"total_commits_count"`
}

type whMergeRequest struct {
	ObjectKind string `json:"object_kind"`
	User       struct {
		Username string `json:"username"`
	} `json:"user"`
	Project          *whProject `json:"project"`
	ObjectAttributes *struct {
		IID             int64  `json:"iid"`
		Title           string `json:"title"`
		State           string `json:"state"`
		Action          string `json:"action"`
		SourceBranch    string `json:"source_branch"`
		TargetBranch    string `json:"target_branch"`
		TargetProjectID int64  `json:"target_project_id"`
		URL             string `json:"url"`
		Draft           bool   `json:"draft"`
		WorkInProgress  bool   `json:"work_in_progress"`
		LastCommit      struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
	Labels []struct {
		Title string `json:"title"`
	} `json:"labels"`
}

func toEventRepo(p *whProject, id int64) *scm.EventRepo {
	ref, _ := scm.ParseFullName(p.PathWithNamespace)
	if id == 0 {
		id = p.ID
	}
	er := &scm.EventRepo{
		RepoRef:       ref,
		HTMLURL:       p.WebURL,
		CloneURL:      p.GitHTTPURL,
		SSHURL:        p.GitSSHURL,
		DefaultBranch: p.DefaultBranch,
		// visibility_level: 0 private, 10 internal, 20 public.
		Private: p.VisibilityLevel != nil && *p.VisibilityLevel < 20,
	}
	if id != 0 {
		er.ID = strconv.FormatInt(id, 10)
	}
	return er
}

func isZeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }

func parsePush(body []byte, ev *scm.Event) error {
	var p whPush
	if err := decode(body, &p); err != nil {
		return err
	}
	if p.Project == nil || p.Ref == "" {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: push without project or ref", scm.ErrMalformedPayload))
	}
	ev.Type = scm.EventPush
	ev.Repository = toEventRepo(p.Project, p.ProjectID)
	ev.Ref = p.Ref
	ev.Revision = scm.ParseRevision(p.Ref)
	ev.Before, ev.After = p.Before, p.After
	ev.Created, ev.Deleted = isZeroSHA(p.Before), isZeroSHA(p.After)
	ev.TouchedDefaultBranch = strings.HasPrefix(p.Ref, "refs/heads/") && p.Project.DefaultBranch != "" && ev.Revision == p.Project.DefaultBranch
	groups := make([][]string, 0, 3*len(p.Commits))
	for _, c := range p.Commits {
		groups = append(groups, c.Added, c.Modified, c.Removed)
	}
	ev.ChangedFiles, ev.ChangedFilesTruncated = scm.NormalizeChangedFiles(groups...)
	// GitLab includes at most 20 commits; the file list is then incomplete.
	if p.TotalCommitsCount > len(p.Commits) {
		ev.ChangedFilesTruncated = true
	}
	ev.Sender = p.UserUsername
	return nil
}

// mrActions maps GitLab merge request actions to the normalized vocabulary. "update"
// covers new commits, label and title changes; it maps to synchronize so callers that
// refresh on GitHub's synchronize keep upstream's "refresh on update" behavior.
var mrActions = map[string]string{
	"open":   scm.PRActionOpened,
	"reopen": scm.PRActionReopened,
	"close":  scm.PRActionClosed,
	"merge":  scm.PRActionClosed,
	"update": scm.PRActionSynchronize,
}

func parseMergeRequest(body []byte, ev *scm.Event) error {
	var p whMergeRequest
	if err := decode(body, &p); err != nil {
		return err
	}
	if p.Project == nil || p.ObjectAttributes == nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: merge request without project or object_attributes", scm.ErrMalformedPayload))
	}
	a := p.ObjectAttributes
	var labels []string
	for _, l := range p.Labels {
		if l.Title != "" {
			labels = append(labels, l.Title)
		}
	}
	action := a.Action
	if m, ok := mrActions[action]; ok {
		action = m
	}
	ev.Type = scm.EventPullRequest
	ev.Action = action
	// Upstream matches merge request events by the target project ID.
	ev.Repository = toEventRepo(p.Project, a.TargetProjectID)
	ev.PullRequest = &scm.PullRequest{
		Number:       a.IID,
		Title:        a.Title,
		Branch:       a.SourceBranch,
		TargetBranch: a.TargetBranch,
		HeadSHA:      a.LastCommit.ID,
		Labels:       labels,
		URL:          a.URL,
		State:        a.State,
		Draft:        a.Draft || a.WorkInProgress,
		Merged:       a.State == "merged" || a.Action == "merge",
	}
	ev.Sender = p.User.Username
	return nil
}
