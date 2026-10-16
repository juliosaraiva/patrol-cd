package bitbucketserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Webhook headers.
const (
	HeaderEventKey  = "X-Event-Key"
	HeaderRequestID = "X-Request-Id"
	// HeaderSignature carries "sha256=<hex HMAC-SHA256 of the raw body>".
	HeaderSignature = "X-Hub-Signature"
	// headerHookUUID marks Bitbucket Cloud deliveries, which also send X-Event-Key.
	headerHookUUID = "X-Hook-UUID"
)

// WebhookVerifier verifies and normalizes Bitbucket Server webhook deliveries. The zero
// value is ready to use and needs no API credentials.
type WebhookVerifier struct{}

var _ scm.WebhookVerifier = WebhookVerifier{}

// MatchesWebhook reports deliveries carrying X-Event-Key but not X-Hook-UUID (Bitbucket
// Cloud sends both and is claimed by its own verifier first, as upstream).
func (WebhookVerifier) MatchesWebhook(h http.Header) bool {
	return h.Get(HeaderEventKey) != "" && h.Get(headerHookUUID) == ""
}

func whErr(kind scm.ErrorKind, err error) error {
	return &scm.Error{Kind: kind, Provider: Type, Op: "VerifyAndParse", Err: err}
}

// prActions maps Bitbucket Server pull request event keys onto the normalized
// vocabulary.
var prActions = map[string]string{
	"pr:opened":           scm.PRActionOpened,
	"pr:from_ref_updated": scm.PRActionSynchronize,
	"pr:modified":         scm.PRActionEdited,
	"pr:merged":           scm.PRActionClosed,
	"pr:declined":         scm.PRActionClosed,
	"pr:deleted":          scm.PRActionClosed,
}

// VerifyAndParse implements scm.WebhookVerifier. With a non-empty secret X-Hub-Signature
// must be "sha256=" followed by the hex HMAC-SHA256 of the exact raw body (constant-time
// comparison, before the event type is inspected; upstream lets unsigned
// diagnostics:ping through, Patrol requires the signature for every event).
//
// Supported events: repo:refs_changed (EventPush from the first change, as upstream;
// TouchedDefaultBranch is always true and the payload has no changed-file list, so
// ChangedFilesTruncated is true), the pull request events in prActions, and
// diagnostics:ping (EventPing). Others fail with scm.ErrUnsupportedEvent.
func (WebhookVerifier) VerifyAndParse(h http.Header, body []byte, secret []byte) (*scm.Event, error) {
	if int64(len(body)) > scm.MaxWebhookPayloadBytes {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %d bytes", scm.ErrPayloadTooLarge, len(body)))
	}
	verified := false
	if len(secret) > 0 {
		if err := scm.VerifyHMACSHA256(h.Get(HeaderSignature), "sha256=", body, secret); err != nil {
			return nil, scm.WebhookSignatureError(Type, err)
		}
		verified = true
	}
	key := h.Get(HeaderEventKey)
	if key == "" {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: missing %s header", scm.ErrMalformedPayload, HeaderEventKey))
	}
	// The delivery ID is not covered by the signature: bound it before anyone stores it.
	deliveryID := h.Get(HeaderRequestID)
	if err := scm.ValidateDeliveryID(deliveryID); err != nil {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%s: %w", HeaderRequestID, err))
	}
	ev := &scm.Event{Provider: Type, DeliveryID: deliveryID, Verified: verified}
	var err error
	switch {
	case key == "repo:refs_changed":
		err = parsePush(body, ev)
	case prActions[key] != "":
		err = parsePullRequest(key, body, ev)
	case key == "diagnostics:ping":
		ev.Type = scm.EventPing
	default:
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %q", scm.ErrUnsupportedEvent, key))
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

type whActor struct {
	Name string `json:"name"`
}

type whPush struct {
	EventKey   string  `json:"eventKey"`
	Actor      whActor `json:"actor"`
	Repository *bsRepo `json:"repository"`
	Changes    []struct {
		Ref struct {
			ID        string `json:"id"`
			DisplayID string `json:"displayId"`
			Type      string `json:"type"`
		} `json:"ref"`
		RefID    string `json:"refId"`
		FromHash string `json:"fromHash"`
		ToHash   string `json:"toHash"`
		Type     string `json:"type"` // ADD, UPDATE, DELETE
	} `json:"changes"`
}

type whPullRequest struct {
	EventKey    string  `json:"eventKey"`
	Actor       whActor `json:"actor"`
	PullRequest *struct {
		bsPull
		To struct {
			bsRef
			Repository *bsRepo `json:"repository"`
		} `json:"toRef"`
	} `json:"pullRequest"`
}

// toEventRepo maps a repository; CloneURL and SSHURL are the "http" and "ssh" clone
// links, which upstream matches against Application repository URLs.
func toEventRepo(r *bsRepo) *scm.EventRepo {
	er := &scm.EventRepo{
		RepoRef:  scm.RepoRef{Owner: r.Project.Key, Name: r.Slug},
		HTMLURL:  cloneLink(r.Links.Self, ""),
		CloneURL: cloneLink(r.Links.Clone, "http"),
		SSHURL:   cloneLink(r.Links.Clone, "ssh"),
		Private:  !r.Public,
	}
	if r.ID != 0 {
		er.ID = strconv.FormatInt(r.ID, 10)
	}
	return er
}

func parsePush(body []byte, ev *scm.Event) error {
	var p whPush
	if err := decode(body, &p); err != nil {
		return err
	}
	if p.Repository == nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: refs_changed without repository", scm.ErrMalformedPayload))
	}
	ev.Type = scm.EventPush
	ev.Repository = toEventRepo(p.Repository)
	if len(p.Changes) > 0 {
		c := p.Changes[0]
		ref := c.Ref.ID
		if ref == "" {
			ref = c.RefID
		}
		ev.Ref = ref
		ev.Revision = scm.ParseRevision(ref)
		ev.Before, ev.After = c.FromHash, c.ToHash
		ev.Created, ev.Deleted = c.Type == "ADD", c.Type == "DELETE"
	}
	// Upstream cannot tell from the payload whether the default branch moved and has no
	// changed-file list: every matching application is refreshed.
	ev.TouchedDefaultBranch = true
	ev.ChangedFilesTruncated = true
	ev.Sender = p.Actor.Name
	return nil
}

func parsePullRequest(key string, body []byte, ev *scm.Event) error {
	var p whPullRequest
	if err := decode(body, &p); err != nil {
		return err
	}
	if p.PullRequest == nil || p.PullRequest.To.Repository == nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: %s without pullRequest or target repository", scm.ErrMalformedPayload, key))
	}
	pr := p.PullRequest
	ev.Type = scm.EventPullRequest
	ev.Action = prActions[key]
	ev.Repository = toEventRepo(pr.To.Repository)
	ev.PullRequest = &scm.PullRequest{
		Number:       pr.ID,
		Title:        pr.Title,
		Branch:       pr.From.DisplayID,
		TargetBranch: pr.To.DisplayID,
		HeadSHA:      pr.From.LatestCommit,
		Author:       pr.Author.User.Name,
		URL:          cloneLink(pr.Links.Self, ""),
		State:        pr.State,
		Draft:        pr.Draft,
		Merged:       pr.State == "MERGED" || key == "pr:merged",
	}
	ev.Sender = p.Actor.Name
	return nil
}
