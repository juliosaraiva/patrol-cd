package github_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
)

var whSecret = []byte("It's a Secret to Everybody")

func TestFixturesAreValidJSON(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		var v any
		if err := json.Unmarshal(fixture(t, filepath.Base(e.Name())), &v); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
	}
}

func TestVerifyAndParseFixtures(t *testing.T) {
	t.Parallel()
	v := github.WebhookVerifier{}
	tests := []struct {
		event, file string
		check       func(t *testing.T, ev *scm.Event)
	}{
		{"push", "push.json", func(t *testing.T, ev *scm.Event) {
			if ev.Type != scm.EventPush || ev.Ref != "refs/heads/main" || ev.Revision != "main" || !ev.TouchedDefaultBranch {
				t.Fatalf("push = %+v", ev)
			}
			if ev.Before != sha(99) || ev.After != sha(1) || ev.Created || ev.Deleted || ev.Forced || ev.Sender != "alice" {
				t.Fatalf("push shas/flags = %+v", ev)
			}
			// Upstream order per commit: added, modified, removed; duplicates dropped.
			want := []string{"apps/a/configmap.yaml", "apps/a/values.yaml", "README.md", "apps/old/deployment.yaml"}
			if !slices.Equal(ev.ChangedFiles, want) || ev.ChangedFilesTruncated {
				t.Fatalf("changed files = %v, want %v", ev.ChangedFiles, want)
			}
			r := ev.Repository
			if r == nil || r.Owner != "acme" || r.Name != "platform" || r.ID != "42" || r.HTMLURL != "https://github.com/acme/platform" ||
				r.CloneURL != "https://github.com/acme/platform.git" || r.SSHURL != "git@github.com:acme/platform.git" || r.DefaultBranch != "main" || !r.Private {
				t.Fatalf("repository = %+v", r)
			}
			if r.APIURL != "" {
				t.Fatalf("push payload url is an HTML URL and must not be reported as API URL: %q", r.APIURL)
			}
			if ev.Installation == nil || ev.Installation.ID != 77 {
				t.Fatalf("installation = %+v", ev.Installation)
			}
		}},
		{"push", "push_tag.json", func(t *testing.T, ev *scm.Event) {
			if ev.Revision != "v1.2.0" || ev.TouchedDefaultBranch || !ev.Created || len(ev.ChangedFiles) != 0 || ev.Installation != nil {
				t.Fatalf("tag push = %+v", ev)
			}
		}},
		{"pull_request", "pull_request_opened.json", func(t *testing.T, ev *scm.Event) {
			pr := ev.PullRequest
			if ev.Type != scm.EventPullRequest || ev.Action != scm.PRActionOpened || pr == nil {
				t.Fatalf("pr event = %+v", ev)
			}
			if pr.Number != 1 || pr.Title != "Add a" || pr.Branch != "feature-a" || pr.TargetBranch != "main" || pr.HeadSHA != sha(2) ||
				pr.Author != "alice" || !slices.Equal(pr.Labels, []string{"preview"}) || pr.URL != "https://github.com/acme/platform/pull/1" || pr.Merged || pr.Draft {
				t.Fatalf("pull request = %+v", pr)
			}
			if ev.Repository.APIURL != "https://api.github.com/repos/acme/platform" {
				t.Fatalf("API URL = %q", ev.Repository.APIURL)
			}
		}},
		{"pull_request", "pull_request_labeled.json", func(t *testing.T, ev *scm.Event) {
			if ev.Action != scm.PRActionLabeled || !ev.PullRequest.Draft || !slices.Equal(ev.PullRequest.Labels, []string{"preview", "team-a"}) || ev.Installation != nil {
				t.Fatalf("labeled = %+v %+v", ev, ev.PullRequest)
			}
		}},
		{"pull_request", "pull_request_closed_merged.json", func(t *testing.T, ev *scm.Event) {
			if ev.Action != scm.PRActionClosed || !ev.PullRequest.Merged || ev.PullRequest.State != "closed" || ev.PullRequest.TargetBranch != "release-1" || ev.PullRequest.Labels != nil {
				t.Fatalf("closed = %+v", ev.PullRequest)
			}
		}},
		{"installation", "installation_created.json", func(t *testing.T, ev *scm.Event) {
			if ev.Type != scm.EventInstallation || ev.Action != "created" || ev.Installation == nil || ev.Installation.ID != 77 || ev.Installation.Account != "acme" {
				t.Fatalf("installation = %+v", ev)
			}
			want := []scm.RepoRef{{Owner: "acme", Name: "platform"}, {Owner: "acme", Name: "svc-00"}}
			if !slices.Equal(ev.RepositoriesAdded, want) || ev.RepositoriesRemoved != nil {
				t.Fatalf("repositories = %v / %v", ev.RepositoriesAdded, ev.RepositoriesRemoved)
			}
		}},
		{"installation_repositories", "installation_repositories_added.json", func(t *testing.T, ev *scm.Event) {
			if ev.Type != scm.EventInstallation || ev.Action != "added" || !slices.Equal(ev.RepositoriesAdded, []scm.RepoRef{{Owner: "acme", Name: "svc-01"}}) || len(ev.RepositoriesRemoved) != 0 {
				t.Fatalf("installation_repositories = %+v", ev)
			}
		}},
		{"repository", "repository_archived.json", func(t *testing.T, ev *scm.Event) {
			if ev.Type != scm.EventRepository || ev.Action != "archived" || ev.Repository.Name != "legacy" || ev.Installation.ID != 77 {
				t.Fatalf("repository = %+v", ev)
			}
		}},
		{"ping", "ping.json", func(t *testing.T, ev *scm.Event) {
			if ev.Type != scm.EventPing || ev.Repository == nil || ev.Repository.Name != "platform" || ev.Sender != "admin" {
				t.Fatalf("ping = %+v", ev)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			body := fixture(t, tc.file)
			h := delivery(tc.event, "72d3162e-cc78-11e3-81ab-4c9367dc0958", body, whSecret)
			if !v.MatchesWebhook(h) {
				t.Fatal("MatchesWebhook = false")
			}
			ev, err := v.VerifyAndParse(h, body, whSecret)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Provider != github.Type || !ev.Verified || ev.DeliveryID != "72d3162e-cc78-11e3-81ab-4c9367dc0958" {
				t.Fatalf("event envelope = %+v", ev)
			}
			tc.check(t, ev)
		})
	}
}

func TestWebhookSignatures(t *testing.T) {
	t.Parallel()
	v := github.WebhookVerifier{}
	body := fixture(t, "push.json")
	valid := delivery("push", "d1", body, whSecret)
	with := func(f func(h http.Header)) http.Header {
		h := valid.Clone()
		f(h)
		return h
	}
	tests := []struct {
		name    string
		h       http.Header
		body    []byte
		secret  []byte
		want    error
		kind    scm.ErrorKind
		verifed bool
	}{
		{"valid", valid, body, whSecret, nil, "", true},
		{"tampered body", valid, []byte(strings.Replace(string(body), "apps/a", "apps/x", 1)), whSecret, scm.ErrSignatureMismatch, scm.KindUnauthorized, false},
		{"whitespace appended", valid, append(slices.Clone(body), '\n'), whSecret, scm.ErrSignatureMismatch, scm.KindUnauthorized, false},
		{"wrong secret", valid, body, []byte("wrong"), scm.ErrSignatureMismatch, scm.KindUnauthorized, false},
		{"missing signature", with(func(h http.Header) { h.Del("X-Hub-Signature-256") }), body, whSecret, scm.ErrSignatureMissing, scm.KindUnauthorized, false},
		{"sha1 only is not accepted", with(func(h http.Header) {
			h.Del("X-Hub-Signature-256")
			h.Set("X-Hub-Signature", "sha1=0000000000000000000000000000000000000000")
		}), body, whSecret, scm.ErrSignatureMissing, scm.KindUnauthorized, false},
		{"sha256 value with sha1 prefix", with(func(h http.Header) {
			h.Set("X-Hub-Signature-256", strings.Replace(h.Get("X-Hub-Signature-256"), "sha256=", "sha1=", 1))
		}), body, whSecret, scm.ErrSignatureMismatch, scm.KindUnauthorized, false},
		{"garbage signature", with(func(h http.Header) { h.Set("X-Hub-Signature-256", "sha256=not-hex") }), body, whSecret, scm.ErrSignatureMismatch, scm.KindUnauthorized, false},
		{"no secret configured", with(func(h http.Header) { h.Del("X-Hub-Signature-256") }), body, nil, nil, "", false},
		{"no secret, signed delivery", valid, body, nil, nil, "", false},
		{"missing event header", with(func(h http.Header) { h.Del("X-GitHub-Event") }), body, whSecret, scm.ErrMalformedPayload, scm.KindInvalid, false},
		{"unsupported event", with(func(h http.Header) { h.Set("X-GitHub-Event", "issues") }), body, whSecret, scm.ErrUnsupportedEvent, scm.KindInvalid, false},
		{"unsupported event with bad signature", with(func(h http.Header) {
			h.Set("X-GitHub-Event", "issues")
			h.Set("X-Hub-Signature-256", "sha256=00")
		}), body, whSecret, scm.ErrSignatureMismatch, scm.KindUnauthorized, false},
		{"malformed json", delivery("push", "d2", []byte("{not json"), whSecret), []byte("{not json"), whSecret, scm.ErrMalformedPayload, scm.KindInvalid, false},
		{"push without repository", delivery("push", "d3", []byte(`{"ref":"refs/heads/main"}`), whSecret), []byte(`{"ref":"refs/heads/main"}`), whSecret, scm.ErrMalformedPayload, scm.KindInvalid, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev, err := v.VerifyAndParse(tc.h, tc.body, tc.secret)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if ev.Verified != tc.verifed {
					t.Fatalf("Verified = %v, want %v", ev.Verified, tc.verifed)
				}
				return
			}
			if !errors.Is(err, tc.want) || scm.KindOf(err) != tc.kind || ev != nil {
				t.Fatalf("err = %v (kind %q), want %v / %s", err, scm.KindOf(err), tc.want, tc.kind)
			}
		})
	}
}

func TestWebhookFormEncoded(t *testing.T) {
	t.Parallel()
	payload := fixture(t, "ping.json")
	body := []byte(url.Values{"payload": {string(payload)}}.Encode())
	h := delivery("ping", "d-form", body, whSecret)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	ev, err := github.WebhookVerifier{}.VerifyAndParse(h, body, whSecret)
	if err != nil || ev.Type != scm.EventPing || !ev.Verified {
		t.Fatalf("form delivery: %+v, %v", ev, err)
	}
	empty := []byte("other=1")
	h = delivery("ping", "d-form2", empty, whSecret)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := (github.WebhookVerifier{}).VerifyAndParse(h, empty, whSecret); !errors.Is(err, scm.ErrMalformedPayload) {
		t.Fatalf("form without payload: %v", err)
	}
}

func TestWebhookPayloadTooLarge(t *testing.T) {
	t.Parallel()
	body := make([]byte, scm.MaxWebhookPayloadBytes+1)
	_, err := github.WebhookVerifier{}.VerifyAndParse(delivery("push", "big", nil, nil), body, nil)
	if !errors.Is(err, scm.ErrPayloadTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestMatchesWebhook(t *testing.T) {
	t.Parallel()
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	tests := []struct {
		name string
		h    http.Header
		want bool
	}{
		{"github push", h("X-GitHub-Event", "push"), true},
		{"github ping", h("X-GitHub-Event", "ping"), true},
		{"ghcr package", h("X-GitHub-Event", "package"), false},
		{"gitea", h("X-GitHub-Event", "push", "X-Gitea-Event", "push"), false},
		{"gogs", h("X-GitHub-Event", "push", "X-Gogs-Event", "push"), false},
		{"gitlab", h("X-Gitlab-Event", "Push Hook"), false},
		{"none", h(), false},
	}
	for _, tc := range tests {
		if got := (github.WebhookVerifier{}).MatchesWebhook(tc.h); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// TestWebhookFuzzProperties checks that arbitrary bodies and headers never panic, that
// signed bodies are never rejected for their signature, and that any mutation of a
// signed body is rejected before parsing.
func TestWebhookFuzzProperties(t *testing.T) {
	t.Parallel()
	v := github.WebhookVerifier{}
	base := fixture(t, "pull_request_opened.json")
	rapid.Check(t, func(t *rapid.T) {
		event := rapid.SampledFrom([]string{"push", "pull_request", "installation", "installation_repositories", "repository", "ping", "issues", "", "package"}).Draw(t, "event")
		var body []byte
		if rapid.Bool().Draw(t, "garbage") {
			body = rapid.SliceOfN(rapid.Byte(), 0, 256).Draw(t, "body")
		} else {
			body = slices.Clone(base)
		}
		h := delivery(event, "fuzz", body, whSecret)
		ev, err := v.VerifyAndParse(h, body, whSecret)
		if errors.Is(err, scm.ErrSignatureMismatch) || errors.Is(err, scm.ErrSignatureMissing) {
			t.Fatalf("correctly signed body rejected: %v", err)
		}
		if err == nil && (ev == nil || !ev.Verified) {
			t.Fatal("accepted event must be verified")
		}
		if len(body) == 0 {
			return
		}
		i := rapid.IntRange(0, len(body)-1).Draw(t, "i")
		mutated := slices.Clone(body)
		mutated[i] ^= byte(rapid.IntRange(1, 255).Draw(t, "x"))
		if _, err := v.VerifyAndParse(h, mutated, whSecret); !errors.Is(err, scm.ErrSignatureMismatch) {
			t.Fatalf("mutated body not rejected: %v", err)
		}
	})
}

func TestProviderDelegatesWebhooks(t *testing.T) {
	t.Parallel()
	s := newSeed(t, "")
	p := newProvider(t, s.config(t, anonymous))
	wv, err := scm.As[scm.WebhookVerifier](p)
	if err != nil {
		t.Fatal(err)
	}
	body := fixture(t, "push.json")
	ev, err := wv.VerifyAndParse(delivery("push", "d", body, whSecret), body, whSecret)
	if err != nil || ev.Type != scm.EventPush {
		t.Fatalf("%v %v", ev, err)
	}
	if len(s.srv.Requests()) != 0 {
		t.Fatal("webhook verification must not call the API")
	}
}
