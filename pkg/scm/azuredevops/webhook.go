package azuredevops

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// HeaderActivityID is sent with every Azure DevOps service hook delivery; upstream
// claims deliveries carrying it.
const HeaderActivityID = "X-Vss-Activityid"

// Azure DevOps service hook event types.
const (
	EventGitPush              = "git.push"
	EventPullRequestCreated   = "git.pullrequest.created"
	EventPullRequestUpdated   = "git.pullrequest.updated"
	EventPullRequestMerged    = "git.pullrequest.merged"
	eventPullRequestAbandoned = "abandoned"
)

// WebhookVerifier verifies and normalizes Azure DevOps service hook deliveries. The zero
// value is ready to use and needs no API credentials.
type WebhookVerifier struct{}

var _ scm.WebhookVerifier = WebhookVerifier{}

// MatchesWebhook reports deliveries carrying X-Vss-Activityid.
func (WebhookVerifier) MatchesWebhook(h http.Header) bool { return h.Get(HeaderActivityID) != "" }

func whErr(kind scm.ErrorKind, err error) error {
	return &scm.Error{Kind: kind, Provider: Type, Op: "VerifyAndParse", Err: err}
}

func equalSecret(got string, want []byte) bool {
	a := sha256.Sum256([]byte(got))
	b := sha256.Sum256(want)
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// SplitSecret splits a webhook secret "username:password" (upstream
// `webhook.azuredevops.username` and `webhook.azuredevops.password`) at the first ':'.
// A secret without ':' is a password with an empty user name.
func SplitSecret(secret []byte) (username, password []byte) {
	if i := bytes.IndexByte(secret, ':'); i >= 0 {
		return secret[:i], secret[i+1:]
	}
	return nil, secret
}

// JoinSecret builds the secret VerifyAndParse expects from upstream's two settings.
func JoinSecret(username, password string) []byte {
	if username == "" && password == "" {
		return nil
	}
	return []byte(username + ":" + password)
}

// VerifyAndParse implements scm.WebhookVerifier. The secret is "username:password"
// (see JoinSecret): with a non-empty secret the request must carry HTTP basic
// authentication with exactly those credentials (each compared in constant time) before
// the payload is inspected. Basic authentication covers the sender, not the body.
//
// Supported events (from the payload's eventType): git.push (EventPush from the first
// ref update, as upstream; TouchedDefaultBranch compares the full ref name with the
// repository default branch; the payload has no changed-file list, so
// ChangedFilesTruncated is true) and git.pullrequest.created/updated/merged
// (EventPullRequest). Others fail with scm.ErrUnsupportedEvent. The delivery ID is the
// payload's event "id" (stable across retries), falling back to X-Vss-Activityid.
func (WebhookVerifier) VerifyAndParse(h http.Header, body []byte, secret []byte) (*scm.Event, error) {
	if int64(len(body)) > scm.MaxWebhookPayloadBytes {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %d bytes", scm.ErrPayloadTooLarge, len(body)))
	}
	verified := false
	if len(secret) > 0 {
		wantUser, wantPass := SplitSecret(secret)
		r := http.Request{Header: h}
		user, pass, ok := r.BasicAuth()
		if !ok {
			return nil, scm.WebhookSignatureError(Type, scm.ErrSignatureMissing)
		}
		// Evaluate both comparisons so timing does not reveal which one failed.
		userOK := equalSecret(user, wantUser)
		passOK := equalSecret(pass, wantPass)
		if !userOK || !passOK {
			return nil, scm.WebhookSignatureError(Type, scm.ErrSignatureMismatch)
		}
		verified = true
	}
	var env struct {
		ID        string          `json:"id"`
		EventType string          `json:"eventType"`
		Resource  json.RawMessage `json:"resource"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %v", scm.ErrMalformedPayload, err))
	}
	deliveryID := env.ID
	if deliveryID == "" {
		deliveryID = h.Get(HeaderActivityID)
	}
	if err := scm.ValidateDeliveryID(deliveryID); err != nil {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("delivery ID: %w", err))
	}
	ev := &scm.Event{Provider: Type, DeliveryID: deliveryID, Verified: verified}
	var err error
	switch env.EventType {
	case EventGitPush:
		err = parsePush(env.Resource, ev)
	case EventPullRequestCreated, EventPullRequestUpdated, EventPullRequestMerged:
		err = parsePullRequest(env.EventType, env.Resource, ev)
	case "":
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: missing eventType", scm.ErrMalformedPayload))
	default:
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %q", scm.ErrUnsupportedEvent, env.EventType))
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
}

func decode(payload []byte, v any) error {
	if len(payload) == 0 {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: missing resource", scm.ErrMalformedPayload))
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: %v", scm.ErrMalformedPayload, err))
	}
	return nil
}

type whRepo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	RemoteURL     string `json:"remoteUrl"`
	SSHURL        string `json:"sshUrl"`
	WebURL        string `json:"webUrl"`
	DefaultBranch string `json:"defaultBranch"`
	Project       struct {
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
	} `json:"project"`
}

type whIdentity struct {
	UniqueName  string `json:"uniqueName"`
	DisplayName string `json:"displayName"`
}

func (i whIdentity) name() string {
	name, _, _ := strings.Cut(i.UniqueName, "@")
	return name
}

func toEventRepo(r *whRepo) *scm.EventRepo {
	html := r.WebURL
	if html == "" {
		html = r.RemoteURL // upstream matches applications against the remote URL
	}
	return &scm.EventRepo{
		RepoRef:       scm.RepoRef{Owner: r.Project.Name, Name: r.Name},
		ID:            r.ID,
		HTMLURL:       html,
		CloneURL:      r.RemoteURL,
		SSHURL:        r.SSHURL,
		APIURL:        r.URL,
		DefaultBranch: strings.TrimPrefix(r.DefaultBranch, "refs/heads/"),
		Private:       r.Project.Visibility != "" && !strings.EqualFold(r.Project.Visibility, "public"),
	}
}

func isZeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }

func parsePush(resource []byte, ev *scm.Event) error {
	var p struct {
		RefUpdates []struct {
			Name        string `json:"name"`
			OldObjectID string `json:"oldObjectId"`
			NewObjectID string `json:"newObjectId"`
		} `json:"refUpdates"`
		Repository *whRepo    `json:"repository"`
		PushedBy   whIdentity `json:"pushedBy"`
	}
	if err := decode(resource, &p); err != nil {
		return err
	}
	if p.Repository == nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: git.push without repository", scm.ErrMalformedPayload))
	}
	ev.Type = scm.EventPush
	ev.Repository = toEventRepo(p.Repository)
	if len(p.RefUpdates) > 0 {
		u := p.RefUpdates[0]
		ev.Ref = u.Name
		ev.Revision = scm.ParseRevision(u.Name)
		ev.Before, ev.After = u.OldObjectID, u.NewObjectID
		ev.Created, ev.Deleted = isZeroSHA(u.OldObjectID), isZeroSHA(u.NewObjectID)
		ev.TouchedDefaultBranch = u.Name != "" && u.Name == p.Repository.DefaultBranch
	}
	ev.ChangedFilesTruncated = true // Azure DevOps push payloads list no files
	ev.Sender = p.PushedBy.name()
	return nil
}

func parsePullRequest(eventType string, resource []byte, ev *scm.Event) error {
	var pr struct {
		azPull
		Repository *whRepo `json:"repository"`
	}
	if err := decode(resource, &pr); err != nil {
		return err
	}
	if pr.Repository == nil || pr.PullRequestID == 0 {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: %s without repository or pullRequestId", scm.ErrMalformedPayload, eventType))
	}
	// git.pullrequest.merged means "merge attempted": the pull request is closed only
	// once its status is completed (or abandoned).
	action := scm.PRActionSynchronize
	switch {
	case eventType == EventPullRequestCreated:
		action = scm.PRActionOpened
	case pr.Status == "completed", pr.Status == eventPullRequestAbandoned:
		action = scm.PRActionClosed
	}
	head := ""
	if pr.LastMergeSourceCommit != nil {
		head = pr.LastMergeSourceCommit.CommitID
	}
	ev.Type = scm.EventPullRequest
	ev.Action = action
	ev.Repository = toEventRepo(pr.Repository)
	ev.PullRequest = &scm.PullRequest{
		Number:       pr.PullRequestID,
		Title:        pr.Title,
		Branch:       strings.TrimPrefix(pr.SourceRefName, "refs/heads/"),
		TargetBranch: strings.TrimPrefix(pr.TargetRefName, "refs/heads/"),
		HeadSHA:      head,
		Author:       pr.author(),
		Labels:       pr.labelNames(),
		URL:          pr.URL,
		State:        pr.Status,
		Draft:        pr.IsDraft,
		Merged:       pr.Status == "completed",
	}
	ev.Sender = pr.author()
	return nil
}
