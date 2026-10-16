package bitbucketcloud

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Webhook headers.
const (
	HeaderEventKey = "X-Event-Key"
	// HeaderHookUUID carries the webhook's UUID. Upstream authenticates deliveries by
	// comparing it with `webhook.bitbucket.uuid`; it does not cover the body.
	HeaderHookUUID = "X-Hook-UUID"
	// HeaderRequestUUID identifies a delivery; retries keep it (X-Attempt-Number grows).
	HeaderRequestUUID = "X-Request-UUID"
)

// WebhookVerifier verifies and normalizes Bitbucket Cloud webhook deliveries. The zero
// value is ready to use and needs no API credentials.
type WebhookVerifier struct{}

var _ scm.WebhookVerifier = WebhookVerifier{}

// MatchesWebhook reports deliveries carrying X-Hook-UUID, as upstream (Bitbucket Server
// deliveries carry X-Event-Key only).
func (WebhookVerifier) MatchesWebhook(h http.Header) bool { return h.Get(HeaderHookUUID) != "" }

func whErr(kind scm.ErrorKind, err error) error {
	return &scm.Error{Kind: kind, Provider: Type, Op: "VerifyAndParse", Err: err}
}

func equalSecret(got string, want []byte) bool {
	a := sha256.Sum256([]byte(got))
	b := sha256.Sum256(want)
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// prActions maps Bitbucket Cloud pull request event keys onto the normalized vocabulary.
var prActions = map[string]string{
	"pullrequest:created":   scm.PRActionOpened,
	"pullrequest:updated":   scm.PRActionSynchronize,
	"pullrequest:fulfilled": scm.PRActionClosed,
	"pullrequest:rejected":  scm.PRActionClosed,
}

// VerifyAndParse implements scm.WebhookVerifier. The secret is the webhook UUID
// (upstream `webhook.bitbucket.uuid`, braces included as Bitbucket sends them): with a
// non-empty secret X-Hook-UUID must equal it (constant-time comparison, before the
// event type is inspected). The UUID authenticates the sender, not the body.
//
// Supported events: repo:push (EventPush from the first change, as upstream;
// TouchedDefaultBranch is true and ChangedFilesTruncated is true because the payload
// has neither; callers that verified the UUID may refine both with
// Provider.ChangedFiles and Provider.TouchesMainBranch) and the pull request events in
// prActions. Others fail with scm.ErrUnsupportedEvent.
func (WebhookVerifier) VerifyAndParse(h http.Header, body []byte, secret []byte) (*scm.Event, error) {
	if int64(len(body)) > scm.MaxWebhookPayloadBytes {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %d bytes", scm.ErrPayloadTooLarge, len(body)))
	}
	verified := false
	if len(secret) > 0 {
		got := h.Get(HeaderHookUUID)
		if got == "" {
			return nil, scm.WebhookSignatureError(Type, scm.ErrSignatureMissing)
		}
		if !equalSecret(got, secret) {
			return nil, scm.WebhookSignatureError(Type, scm.ErrSignatureMismatch)
		}
		verified = true
	}
	key := h.Get(HeaderEventKey)
	if key == "" {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: missing %s header", scm.ErrMalformedPayload, HeaderEventKey))
	}
	// The delivery ID is not authenticated: bound it before anyone stores it.
	deliveryID := h.Get(HeaderRequestUUID)
	if err := scm.ValidateDeliveryID(deliveryID); err != nil {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%s: %w", HeaderRequestUUID, err))
	}
	ev := &scm.Event{Provider: Type, DeliveryID: deliveryID, Verified: verified}
	var err error
	switch {
	case key == "repo:push":
		err = parsePush(body, ev)
	case prActions[key] != "":
		err = parsePullRequest(key, body, ev)
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
	Nickname    string `json:"nickname"`
	DisplayName string `json:"display_name"`
}

func (a whActor) name() string {
	if a.Nickname != "" {
		return a.Nickname
	}
	return a.DisplayName
}

type whRef struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Target struct {
		Hash string `json:"hash"`
	} `json:"target"`
}

func (r *whRef) fullRef() string {
	if r == nil || r.Name == "" {
		return ""
	}
	if r.Type == "tag" {
		return "refs/tags/" + r.Name
	}
	return "refs/heads/" + r.Name
}

type whPush struct {
	Actor      whActor `json:"actor"`
	Repository *bcRepo `json:"repository"`
	Push       struct {
		Changes []struct {
			New     *whRef `json:"new"`
			Old     *whRef `json:"old"`
			Created bool   `json:"created"`
			Closed  bool   `json:"closed"`
			Forced  bool   `json:"forced"`
		} `json:"changes"`
	} `json:"push"`
}

type whPullRequest struct {
	Actor       whActor `json:"actor"`
	Repository  *bcRepo `json:"repository"`
	PullRequest *bcPull `json:"pullrequest"`
}

func toEventRepo(r *bcRepo) *scm.EventRepo {
	ref, _ := scm.ParseFullName(r.FullName)
	return &scm.EventRepo{
		RepoRef: ref,
		ID:      r.UUID,
		HTMLURL: r.Links.HTML.Href,
		// links.self is the repository API URL (see APIBaseURLFromRepositoryURL).
		APIURL:        r.Links.Self.Href,
		CloneURL:      cloneLink(r.Links.Clone, "https"),
		SSHURL:        cloneLink(r.Links.Clone, "ssh"),
		DefaultBranch: r.mainBranch(),
		Private:       r.IsPrivate,
	}
}

func parsePush(body []byte, ev *scm.Event) error {
	var p whPush
	if err := decode(body, &p); err != nil {
		return err
	}
	if p.Repository == nil || p.Repository.FullName == "" {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: repo:push without repository", scm.ErrMalformedPayload))
	}
	ev.Type = scm.EventPush
	ev.Repository = toEventRepo(p.Repository)
	if len(p.Push.Changes) > 0 {
		c := p.Push.Changes[0]
		ref := c.New.fullRef()
		if ref == "" {
			ref = c.Old.fullRef() // branch or tag deleted
		}
		ev.Ref = ref
		if c.New != nil {
			ev.Revision = c.New.Name
			ev.After = c.New.Target.Hash
		}
		if c.Old != nil {
			ev.Before = c.Old.Target.Hash
		}
		ev.Created, ev.Deleted, ev.Forced = c.Created, c.Closed, c.Forced
	}
	ev.TouchedDefaultBranch = true
	ev.ChangedFilesTruncated = true
	ev.Sender = p.Actor.name()
	return nil
}

func parsePullRequest(key string, body []byte, ev *scm.Event) error {
	var p whPullRequest
	if err := decode(body, &p); err != nil {
		return err
	}
	if p.PullRequest == nil || p.Repository == nil || p.Repository.FullName == "" {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: %s without pullrequest or repository", scm.ErrMalformedPayload, key))
	}
	pr := p.PullRequest
	ev.Type = scm.EventPullRequest
	ev.Action = prActions[key]
	ev.Repository = toEventRepo(p.Repository)
	ev.PullRequest = &scm.PullRequest{
		Number:       pr.ID,
		Title:        pr.Title,
		Branch:       pr.Source.Branch.Name,
		TargetBranch: pr.Destination.Branch.Name,
		HeadSHA:      pr.Source.Commit.Hash,
		Author:       pr.Author.Nickname,
		URL:          pr.Links.HTML.Href,
		State:        pr.State,
		Draft:        pr.Draft,
		Merged:       pr.State == "MERGED" || key == "pullrequest:fulfilled",
	}
	ev.Sender = p.Actor.name()
	return nil
}
