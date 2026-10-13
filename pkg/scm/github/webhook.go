package github

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Webhook headers.
const (
	HeaderEvent        = "X-GitHub-Event"
	HeaderDelivery     = "X-GitHub-Delivery"
	HeaderSignature256 = "X-Hub-Signature-256"
	// HeaderSignatureSHA1 is GitHub's legacy signature. It is never accepted on its own
	// (upstream verifies only X-Hub-Signature-256).
	HeaderSignatureSHA1 = "X-Hub-Signature"
	signaturePrefix     = "sha256="
)

// WebhookVerifier verifies and normalizes GitHub webhook deliveries. The zero value is
// ready to use and needs no API credentials, so webhook ingestion can use it without
// building a full provider.
type WebhookVerifier struct{}

var _ scm.WebhookVerifier = WebhookVerifier{}

// MatchesWebhook reports deliveries carrying X-GitHub-Event, excluding Gogs/Gitea (which
// also send GitHub headers) and "package" events (owned by the GHCR registry parser, as
// upstream).
func (WebhookVerifier) MatchesWebhook(h http.Header) bool {
	ev := h.Get(HeaderEvent)
	return ev != "" && ev != "package" && h.Get("X-Gogs-Event") == "" && h.Get("X-Gitea-Event") == ""
}

func whErr(kind scm.ErrorKind, err error) error {
	return &scm.Error{Kind: kind, Provider: Type, Op: "VerifyAndParse", Err: err}
}

// VerifyAndParse implements scm.WebhookVerifier. With a non-empty secret the
// X-Hub-Signature-256 HMAC-SHA256 over the exact raw body is required and compared in
// constant time; the legacy SHA-1 header alone is rejected as missing. Verification
// happens before the event type is inspected. Supported events: push, pull_request,
// installation, installation_repositories (normalized to installation), repository and
// ping; others fail with scm.ErrUnsupportedEvent after successful verification.
func (WebhookVerifier) VerifyAndParse(h http.Header, body []byte, secret []byte) (*scm.Event, error) {
	if int64(len(body)) > scm.MaxWebhookPayloadBytes {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %d bytes", scm.ErrPayloadTooLarge, len(body)))
	}
	verified := false
	if len(secret) > 0 {
		sig := h.Get(HeaderSignature256)
		if sig == "" {
			err := scm.ErrSignatureMissing
			if h.Get(HeaderSignatureSHA1) != "" {
				err = fmt.Errorf("%w: %s is required (SHA-1 signatures are not accepted)", scm.ErrSignatureMissing, HeaderSignature256)
			}
			return nil, scm.WebhookSignatureError(Type, err)
		}
		if err := scm.VerifyHMACSHA256(sig, signaturePrefix, body, secret); err != nil {
			return nil, scm.WebhookSignatureError(Type, err)
		}
		verified = true
	}
	event := h.Get(HeaderEvent)
	if event == "" {
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: missing %s header", scm.ErrMalformedPayload, HeaderEvent))
	}
	payload, err := extractPayload(h.Get("Content-Type"), body)
	if err != nil {
		return nil, err
	}
	ev := &scm.Event{Provider: Type, DeliveryID: h.Get(HeaderDelivery), Verified: verified}
	switch event {
	case "push":
		err = parsePush(payload, ev)
	case "pull_request":
		err = parsePullRequest(payload, ev)
	case "installation", "installation_repositories":
		err = parseInstallation(event, payload, ev)
	case "repository":
		err = parseRepositoryEvent(payload, ev)
	case "ping":
		err = parsePing(payload, ev)
	default:
		return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %q", scm.ErrUnsupportedEvent, event))
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// extractPayload returns the JSON document of a delivery sent as application/json or as
// application/x-www-form-urlencoded (payload=<json>).
func extractPayload(contentType string, body []byte) ([]byte, error) {
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt == "application/x-www-form-urlencoded" {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: %v", scm.ErrMalformedPayload, err))
		}
		p := form.Get("payload")
		if p == "" {
			return nil, whErr(scm.KindInvalid, fmt.Errorf("%w: form body without payload field", scm.ErrMalformedPayload))
		}
		return []byte(p), nil
	}
	return body, nil
}

func decode(payload []byte, v any) error {
	if err := json.Unmarshal(payload, v); err != nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: %v", scm.ErrMalformedPayload, err))
	}
	return nil
}

type whUser struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

type whRepo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         whUser `json:"owner"`
	Private       bool   `json:"private"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch"`
}

type whInstallation struct {
	ID      int64  `json:"id"`
	Account whUser `json:"account"`
}

type whCommit struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
}

type whPush struct {
	Ref          string          `json:"ref"`
	Before       string          `json:"before"`
	After        string          `json:"after"`
	Created      bool            `json:"created"`
	Deleted      bool            `json:"deleted"`
	Forced       bool            `json:"forced"`
	Commits      []whCommit      `json:"commits"`
	Repository   *whRepo         `json:"repository"`
	Sender       whUser          `json:"sender"`
	Installation *whInstallation `json:"installation"`
}

type whLabel struct {
	Name string `json:"name"`
}

type whPR struct {
	Number   int64     `json:"number"`
	Title    string    `json:"title"`
	State    string    `json:"state"`
	HTMLURL  string    `json:"html_url"`
	Draft    bool      `json:"draft"`
	Merged   bool      `json:"merged"`
	MergedAt *string   `json:"merged_at"`
	User     whUser    `json:"user"`
	Labels   []whLabel `json:"labels"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

type whPullRequestEvent struct {
	Action       string          `json:"action"`
	Number       int64           `json:"number"`
	PullRequest  *whPR           `json:"pull_request"`
	Repository   *whRepo         `json:"repository"`
	Sender       whUser          `json:"sender"`
	Installation *whInstallation `json:"installation"`
}

type whRepoMin struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
}

type whInstallationEvent struct {
	Action              string          `json:"action"`
	Installation        *whInstallation `json:"installation"`
	Repositories        []whRepoMin     `json:"repositories"`
	RepositoriesAdded   []whRepoMin     `json:"repositories_added"`
	RepositoriesRemoved []whRepoMin     `json:"repositories_removed"`
	Sender              whUser          `json:"sender"`
}

type whRepositoryEvent struct {
	Action       string          `json:"action"`
	Repository   *whRepo         `json:"repository"`
	Sender       whUser          `json:"sender"`
	Installation *whInstallation `json:"installation"`
}

type whPing struct {
	Zen          string          `json:"zen"`
	HookID       int64           `json:"hook_id"`
	Repository   *whRepo         `json:"repository"`
	Sender       whUser          `json:"sender"`
	Installation *whInstallation `json:"installation"`
}

func toEventRepo(r *whRepo) *scm.EventRepo {
	if r == nil {
		return nil
	}
	ref := scm.RepoRef{Owner: r.Owner.Login, Name: r.Name}
	if ref.Owner == "" {
		ref.Owner = r.Owner.Name // push payloads also carry owner.name
	}
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
	// Push payloads put the HTML URL in "url"; only keep real API URLs.
	if strings.Contains(r.URL, "/repos/") {
		er.APIURL = r.URL
	}
	return er
}

func toInstallationRef(i *whInstallation) *scm.InstallationRef {
	if i == nil || i.ID == 0 {
		return nil
	}
	return &scm.InstallationRef{ID: i.ID, Account: i.Account.Login}
}

func toRepoRefs(in []whRepoMin) []scm.RepoRef {
	var out []scm.RepoRef
	for _, r := range in {
		if ref, err := scm.ParseFullName(r.FullName); err == nil {
			out = append(out, ref)
		}
	}
	return out
}

func parsePush(payload []byte, ev *scm.Event) error {
	var p whPush
	if err := decode(payload, &p); err != nil {
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
	ev.Created, ev.Deleted, ev.Forced = p.Created, p.Deleted, p.Forced
	ev.TouchedDefaultBranch = strings.HasPrefix(p.Ref, "refs/heads/") && p.Repository.DefaultBranch != "" && ev.Revision == p.Repository.DefaultBranch
	groups := make([][]string, 0, 3*len(p.Commits))
	for _, c := range p.Commits {
		groups = append(groups, c.Added, c.Modified, c.Removed)
	}
	ev.ChangedFiles, ev.ChangedFilesTruncated = scm.NormalizeChangedFiles(groups...)
	ev.Sender = p.Sender.Login
	ev.Installation = toInstallationRef(p.Installation)
	return nil
}

func parsePullRequest(payload []byte, ev *scm.Event) error {
	var p whPullRequestEvent
	if err := decode(payload, &p); err != nil {
		return err
	}
	if p.PullRequest == nil || p.Repository == nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: pull_request without pull_request or repository", scm.ErrMalformedPayload))
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
	ev.Type = scm.EventPullRequest
	ev.Action = p.Action
	ev.Repository = toEventRepo(p.Repository)
	ev.PullRequest = &scm.PullRequest{
		Number:       number,
		Title:        pr.Title,
		Branch:       pr.Head.Ref,
		TargetBranch: pr.Base.Ref,
		HeadSHA:      pr.Head.SHA,
		Author:       pr.User.Login,
		Labels:       labels,
		URL:          pr.HTMLURL,
		State:        pr.State,
		Draft:        pr.Draft,
		Merged:       pr.Merged || (pr.MergedAt != nil && *pr.MergedAt != ""),
	}
	ev.Sender = p.Sender.Login
	ev.Installation = toInstallationRef(p.Installation)
	return nil
}

func parseInstallation(event string, payload []byte, ev *scm.Event) error {
	var p whInstallationEvent
	if err := decode(payload, &p); err != nil {
		return err
	}
	if p.Installation == nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: %s without installation", scm.ErrMalformedPayload, event))
	}
	ev.Type = scm.EventInstallation
	ev.Action = p.Action
	ev.Installation = toInstallationRef(p.Installation)
	ev.Sender = p.Sender.Login
	if event == "installation_repositories" {
		ev.RepositoriesAdded = toRepoRefs(p.RepositoriesAdded)
		ev.RepositoriesRemoved = toRepoRefs(p.RepositoriesRemoved)
		return nil
	}
	switch p.Action {
	case "created", "new_permissions_accepted", "unsuspend":
		ev.RepositoriesAdded = toRepoRefs(p.Repositories)
	case "deleted", "suspend":
		ev.RepositoriesRemoved = toRepoRefs(p.Repositories)
	}
	return nil
}

func parseRepositoryEvent(payload []byte, ev *scm.Event) error {
	var p whRepositoryEvent
	if err := decode(payload, &p); err != nil {
		return err
	}
	if p.Repository == nil {
		return whErr(scm.KindInvalid, fmt.Errorf("%w: repository event without repository", scm.ErrMalformedPayload))
	}
	ev.Type = scm.EventRepository
	ev.Action = p.Action
	ev.Repository = toEventRepo(p.Repository)
	ev.Sender = p.Sender.Login
	ev.Installation = toInstallationRef(p.Installation)
	return nil
}

func parsePing(payload []byte, ev *scm.Event) error {
	var p whPing
	if err := decode(payload, &p); err != nil {
		return err
	}
	ev.Type = scm.EventPing
	ev.Repository = toEventRepo(p.Repository)
	ev.Sender = p.Sender.Login
	ev.Installation = toInstallationRef(p.Installation)
	return nil
}
