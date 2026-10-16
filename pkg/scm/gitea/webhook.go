package gitea

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Webhook headers sent by Gitea. Gitea also sends the Gogs (X-Gogs-*) and GitHub
// (X-GitHub-*) equivalents for compatibility.
const (
	HeaderEvent     = "X-Gitea-Event"
	HeaderEventType = "X-Gitea-Event-Type"
	HeaderDelivery  = "X-Gitea-Delivery"
	// HeaderSignature is the hex HMAC-SHA256 of the raw body.
	HeaderSignature     = "X-Gitea-Signature"
	headerGogsSignature = "X-Gogs-Signature"
	headerHubSignature  = "X-Hub-Signature-256"
	headerGogsDelivery  = "X-Gogs-Delivery"
)

// WebhookVerifier verifies and normalizes Gitea webhook deliveries. The zero value is
// ready to use and needs no API credentials.
//
// Upstream Argo CD has no Gitea parser: Gitea deliveries carry X-Gogs-Event and are
// handled by its Gogs parser with the `webhook.gogs.secret` (push events only). Webhook
// ingestion keeps that behavior by verifying Gitea deliveries with the Gogs secret; this
// verifier additionally normalizes Gitea pull request events.
type WebhookVerifier struct{}

var _ scm.WebhookVerifier = WebhookVerifier{}

// MatchesWebhook reports deliveries carrying X-Gitea-Event.
func (WebhookVerifier) MatchesWebhook(h http.Header) bool { return h.Get(HeaderEvent) != "" }

func whErr(kind scm.ErrorKind, err error) error {
	return &scm.Error{Kind: kind, Provider: Type, Op: "VerifyAndParse", Err: err}
}

// VerifyAndParse implements scm.WebhookVerifier. With a non-empty secret the HMAC-SHA256
// of the exact raw body must match X-Gitea-Signature (hex), or, when absent,
// X-Gogs-Signature (hex) or X-Hub-Signature-256 ("sha256=<hex>"), compared in constant
// time before the event type is inspected. Supported events: push and pull_request
// (including Gitea's pull request sub-events); others fail with scm.ErrUnsupportedEvent
// after successful verification.
func (WebhookVerifier) VerifyAndParse(h http.Header, body []byte, secret []byte) (*scm.Event, error) {
	if int64(len(body)) > scm.MaxWebhookPayloadBytes {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %d bytes", scm.ErrPayloadTooLarge, len(body)))
	}
	verified := false
	if len(secret) > 0 {
		var err error
		switch {
		case h.Get(HeaderSignature) != "":
			err = scm.VerifyHMACSHA256(h.Get(HeaderSignature), "", body, secret)
		case h.Get(headerGogsSignature) != "":
			err = scm.VerifyHMACSHA256(h.Get(headerGogsSignature), "", body, secret)
		case h.Get(headerHubSignature) != "":
			err = scm.VerifyHMACSHA256(h.Get(headerHubSignature), "sha256=", body, secret)
		default:
			err = scm.ErrSignatureMissing
		}
		if err != nil {
			return nil, scm.WebhookSignatureError(Type, err)
		}
		verified = true
	}
	event := h.Get(HeaderEvent)
	if event == "" {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: missing %s header", scm.ErrMalformedPayload, HeaderEvent))
	}
	delivery := h.Get(HeaderDelivery)
	if delivery == "" {
		delivery = h.Get(headerGogsDelivery)
	}
	// The delivery ID is not covered by the signature: bound it before anyone stores it.
	if err := scm.ValidateDeliveryID(delivery); err != nil {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("delivery ID: %w", err))
	}
	ev := &scm.Event{Provider: Type, DeliveryID: delivery, Verified: verified}
	var err error
	switch event {
	case "push":
		err = parsePush(body, ev)
	case "pull_request", "pull_request_sync", "pull_request_label", "pull_request_assign", "pull_request_milestone":
		err = parsePullRequest(body, ev)
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

type whRepo struct {
	ID            int64  `json:"id"`
	Owner         gtUser `json:"owner"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	HTMLURL       string `json:"html_url"`
	SSHURL        string `json:"ssh_url"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

type whCommit struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
}

type whPush struct {
	Ref          string     `json:"ref"`
	Before       string     `json:"before"`
	After        string     `json:"after"`
	Commits      []whCommit `json:"commits"`
	TotalCommits int        `json:"total_commits"`
	Repository   *whRepo    `json:"repository"`
	Sender       gtUser     `json:"sender"`
}

type whPullRequestEvent struct {
	Action      string  `json:"action"`
	Number      int64   `json:"number"`
	PullRequest *gtPull `json:"pull_request"`
	Repository  *whRepo `json:"repository"`
	Sender      gtUser  `json:"sender"`
}

func toEventRepo(r *whRepo) *scm.EventRepo {
	ref := scm.RepoRef{Owner: r.Owner.name(), Name: r.Name}
	if (ref.Owner == "" || ref.Name == "") && r.FullName != "" {
		if parsed, err := scm.ParseFullName(r.FullName); err == nil {
			ref = parsed
		}
	}
	er := &scm.EventRepo{
		RepoRef:       ref,
		HTMLURL:       r.HTMLURL,
		CloneURL:      r.CloneURL,
		SSHURL:        r.SSHURL,
		DefaultBranch: r.DefaultBranch,
		Private:       r.Private,
	}
	if r.ID != 0 {
		er.ID = strconv.FormatInt(r.ID, 10)
	}
	return er
}

func isZeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }

func parsePush(body []byte, ev *scm.Event) error {
	var p whPush
	if err := decode(body, &p); err != nil {
		return err
	}
	if p.Repository == nil || p.Ref == "" {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: push without repository or ref", scm.ErrMalformedPayload))
	}
	ev.Type = scm.EventPush
	ev.Repository = toEventRepo(p.Repository)
	ev.Ref = p.Ref
	ev.Revision = scm.ParseRevision(p.Ref)
	ev.Before, ev.After = p.Before, p.After
	ev.Created, ev.Deleted = isZeroSHA(p.Before), isZeroSHA(p.After)
	ev.TouchedDefaultBranch = strings.HasPrefix(p.Ref, "refs/heads/") && p.Repository.DefaultBranch != "" && ev.Revision == p.Repository.DefaultBranch
	groups := make([][]string, 0, 3*len(p.Commits))
	for _, c := range p.Commits {
		groups = append(groups, c.Added, c.Modified, c.Removed)
	}
	ev.ChangedFiles, ev.ChangedFilesTruncated = scm.NormalizeChangedFiles(groups...)
	if p.TotalCommits > len(p.Commits) {
		ev.ChangedFilesTruncated = true // Gitea caps the commits carried by a push payload
	}
	ev.Sender = p.Sender.name()
	return nil
}

// prActions maps Gitea pull request actions onto the normalized vocabulary.
var prActions = map[string]string{
	"synchronized":  scm.PRActionSynchronize,
	"label_updated": scm.PRActionLabeled,
	"label_cleared": scm.PRActionUnlabeled,
}

func parsePullRequest(body []byte, ev *scm.Event) error {
	var p whPullRequestEvent
	if err := decode(body, &p); err != nil {
		return err
	}
	if p.PullRequest == nil || p.Repository == nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: pull request event without pull_request or repository", scm.ErrMalformedPayload))
	}
	pr := p.PullRequest
	var labels []string
	for _, l := range pr.Labels {
		if l.Name != "" {
			labels = append(labels, l.Name)
		}
	}
	number := pr.Number
	if number == 0 {
		number = p.Number
	}
	action := p.Action
	if m, ok := prActions[action]; ok {
		action = m
	}
	ev.Type = scm.EventPullRequest
	ev.Action = action
	ev.Repository = toEventRepo(p.Repository)
	ev.PullRequest = &scm.PullRequest{
		Number:       number,
		Title:        pr.Title,
		Branch:       pr.Head.Ref,
		TargetBranch: pr.Base.Ref,
		HeadSHA:      pr.Head.SHA,
		Author:       pr.User.name(),
		Labels:       labels,
		URL:          pr.HTMLURL,
		State:        pr.State,
		Draft:        pr.Draft,
		Merged:       pr.Merged,
	}
	ev.Sender = p.Sender.name()
	return nil
}
