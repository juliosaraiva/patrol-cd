// Package gogs implements the webhook capability of the scm contract for Gogs.
//
// The Argo CD v3.5.5 baseline supports Gogs only as a webhook source (push events
// verified with `webhook.gogs.secret`); there is no Gogs SCM Provider or Pull Request
// generator. The provider therefore declares only scm.CapabilityWebhooks and needs no
// API URL or credentials.
package gogs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Type is the registered provider type.
const Type = "gogs"

// Webhook headers.
const (
	HeaderEvent    = "X-Gogs-Event"
	HeaderDelivery = "X-Gogs-Delivery"
	// HeaderSignature is the hex HMAC-SHA256 of the raw body.
	HeaderSignature = "X-Gogs-Signature"
	// headerGiteaEvent marks Gitea deliveries, which also carry Gogs headers.
	headerGiteaEvent = "X-Gitea-Event"
)

// Provider is the webhook-only Gogs provider.
type Provider struct {
	info scm.Info
	WebhookVerifier
}

var (
	_ scm.Provider        = (*Provider)(nil)
	_ scm.WebhookVerifier = (*Provider)(nil)
)

// New is the scm.Factory for Gogs. It ignores API and credential settings.
func New(_ context.Context, cfg scm.Config) (scm.Provider, error) {
	return &Provider{info: scm.Info{
		Type:            Type,
		Name:            cfg.Name,
		ContractVersion: scm.ContractVersion,
		Capabilities:    []scm.Capability{scm.CapabilityWebhooks},
	}}, nil
}

// Register adds the Gogs factory to r.
func Register(r *scm.Registry) error { return r.Register(Type, New) }

// Info implements scm.Provider.
func (p *Provider) Info() scm.Info {
	i := p.info
	i.Capabilities = append([]scm.Capability(nil), p.info.Capabilities...)
	return i
}

// WebhookVerifier verifies and normalizes Gogs webhook deliveries. The zero value is
// ready to use.
type WebhookVerifier struct{}

var _ scm.WebhookVerifier = WebhookVerifier{}

// MatchesWebhook reports deliveries carrying X-Gogs-Event, except Gitea deliveries
// (which also send X-Gitea-Event and are claimed by the Gitea verifier). Gogs and
// Gitea deliveries also carry GitHub headers; the GitHub verifier excludes them.
func (WebhookVerifier) MatchesWebhook(h http.Header) bool {
	return h.Get(HeaderEvent) != "" && h.Get(headerGiteaEvent) == ""
}

func whErr(kind scm.ErrorKind, err error) error {
	return &scm.Error{Kind: kind, Provider: Type, Op: "VerifyAndParse", Err: err}
}

// VerifyAndParse implements scm.WebhookVerifier. With a non-empty secret the
// X-Gogs-Signature header must be the hex HMAC-SHA256 of the exact raw body (constant
// time comparison, before the event type is inspected). Only push events are supported,
// as upstream; others fail with scm.ErrUnsupportedEvent after verification.
func (WebhookVerifier) VerifyAndParse(h http.Header, body []byte, secret []byte) (*scm.Event, error) {
	if int64(len(body)) > scm.MaxWebhookPayloadBytes {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %d bytes", scm.ErrPayloadTooLarge, len(body)))
	}
	verified := false
	if len(secret) > 0 {
		if err := scm.VerifyHMACSHA256(h.Get(HeaderSignature), "", body, secret); err != nil {
			return nil, scm.WebhookSignatureError(Type, err)
		}
		verified = true
	}
	event := h.Get(HeaderEvent)
	if event == "" {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: missing %s header", scm.ErrMalformedPayload, HeaderEvent))
	}
	// The delivery ID is not covered by the signature: bound it before anyone stores it.
	deliveryID := h.Get(HeaderDelivery)
	if err := scm.ValidateDeliveryID(deliveryID); err != nil {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%s: %w", HeaderDelivery, err))
	}
	if event != "push" {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %q", scm.ErrUnsupportedEvent, event))
	}
	var p whPush
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %v", scm.ErrMalformedPayload, err))
	}
	if p.Ref == "" {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: push without ref", scm.ErrMalformedPayload))
	}
	ev := &scm.Event{
		Provider:   Type,
		Type:       scm.EventPush,
		DeliveryID: deliveryID,
		Verified:   verified,
		Ref:        p.Ref,
		Revision:   scm.ParseRevision(p.Ref),
		Before:     p.Before,
		After:      p.After,
		Created:    isZeroSHA(p.Before),
		Deleted:    isZeroSHA(p.After),
		Sender:     p.Sender.name(),
	}
	// Upstream: without a repository the event carries no URL and is ignored by the
	// caller; it is still returned here so the caller applies that rule.
	if r := p.Repository; r != nil {
		ref := scm.RepoRef{Owner: r.Owner.name(), Name: r.Name}
		if (ref.Owner == "" || ref.Name == "") && r.FullName != "" {
			if parsed, err := scm.ParseFullName(r.FullName); err == nil {
				ref = parsed
			}
		}
		ev.Repository = &scm.EventRepo{
			RepoRef:       ref,
			HTMLURL:       r.HTMLURL,
			CloneURL:      r.CloneURL,
			SSHURL:        r.SSHURL,
			DefaultBranch: r.DefaultBranch,
			Private:       r.Private,
		}
		if r.ID != 0 {
			ev.Repository.ID = strconv.FormatInt(r.ID, 10)
		}
		ev.TouchedDefaultBranch = r.DefaultBranch != "" && ev.Revision == r.DefaultBranch && strings.HasPrefix(p.Ref, "refs/heads/")
	}
	groups := make([][]string, 0, 3*len(p.Commits))
	for _, c := range p.Commits {
		groups = append(groups, c.Added, c.Modified, c.Removed)
	}
	ev.ChangedFiles, ev.ChangedFilesTruncated = scm.NormalizeChangedFiles(groups...)
	return ev, nil
}

type whUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

func (u whUser) name() string {
	if u.Username != "" {
		return u.Username
	}
	return u.Login
}

type whPush struct {
	Ref     string `json:"ref"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	Repository *struct {
		ID            int64  `json:"id"`
		Owner         whUser `json:"owner"`
		Name          string `json:"name"`
		FullName      string `json:"full_name"`
		Private       bool   `json:"private"`
		HTMLURL       string `json:"html_url"`
		SSHURL        string `json:"ssh_url"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Sender whUser `json:"sender"`
}

func isZeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }
