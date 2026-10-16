package gitlab_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
)

var whSecret = []byte("gitlab-webhook-token")

func TestFixturesAreValidJSON(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		var v any
		if err := json.Unmarshal(fixture(t, e.Name()), &v); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
	}
}

func TestVerifyAndParseFixtures(t *testing.T) {
	t.Parallel()
	v := gitlab.WebhookVerifier{}
	tests := []struct {
		event, file string
		check       func(t *testing.T, ev *scm.Event)
	}{
		{gitlab.EventPushHook, "push.json", func(t *testing.T, ev *scm.Event) {
			if ev.Type != scm.EventPush || ev.Ref != "refs/heads/main" || ev.Revision != "main" || !ev.TouchedDefaultBranch {
				t.Fatalf("push = %+v", ev)
			}
			if ev.Before != sha(99) || ev.After != sha(1) || ev.Created || ev.Deleted || ev.Sender != "alice" {
				t.Fatalf("push shas/flags = %+v", ev)
			}
			want := []string{"apps/a/configmap.yaml", "apps/a/values.yaml", "README.md", "apps/old/deployment.yaml"}
			if !slices.Equal(ev.ChangedFiles, want) || ev.ChangedFilesTruncated {
				t.Fatalf("changed files = %v (truncated %v), want %v", ev.ChangedFiles, ev.ChangedFilesTruncated, want)
			}
			r := ev.Repository
			if r == nil || r.Owner != "acme" || r.Name != "platform" || r.ID != "42" || r.HTMLURL != "https://gitlab.example.com/acme/platform" ||
				r.CloneURL != "https://gitlab.example.com/acme/platform.git" || r.SSHURL != "git@gitlab.example.com:acme/platform.git" ||
				r.DefaultBranch != "main" || !r.Private {
				t.Fatalf("repository = %+v", r)
			}
		}},
		{gitlab.EventTagPushHook, "tag_push.json", func(t *testing.T, ev *scm.Event) {
			if ev.Type != scm.EventPush || ev.Ref != "refs/tags/v1.2.0" || ev.Revision != "v1.2.0" || ev.TouchedDefaultBranch || !ev.Created || ev.Repository.Private {
				t.Fatalf("tag push = %+v", ev)
			}
		}},
		{gitlab.EventMergeRequestHook, "merge_request_open.json", func(t *testing.T, ev *scm.Event) {
			pr := ev.PullRequest
			if ev.Type != scm.EventPullRequest || ev.Action != scm.PRActionOpened || pr == nil || ev.Sender != "alice" {
				t.Fatalf("mr = %+v", ev)
			}
			if pr.Number != 1 || pr.Branch != "feature/a" || pr.TargetBranch != "main" || pr.HeadSHA != sha(2) || pr.Title != "Add a" ||
				!slices.Equal(pr.Labels, []string{"preview"}) || pr.Draft || pr.Merged || pr.State != "opened" {
				t.Fatalf("pr = %+v", pr)
			}
			if ev.Repository.ID != "42" || ev.Repository.HTMLURL != "https://gitlab.example.com/acme/platform" {
				t.Fatalf("repository = %+v", ev.Repository)
			}
		}},
		{gitlab.EventMergeRequestHook, "merge_request_merge.json", func(t *testing.T, ev *scm.Event) {
			pr := ev.PullRequest
			if ev.Action != scm.PRActionClosed || !pr.Merged || !pr.Draft || pr.Number != 2 || !slices.Equal(pr.Labels, []string{"preview", "team-a"}) {
				t.Fatalf("merged mr = %+v / %+v", ev, pr)
			}
			// Matching uses the target project ID even when the source is a fork.
			if ev.Repository.ID != "42" {
				t.Fatalf("repository ID = %q", ev.Repository.ID)
			}
		}},
		{gitlab.EventSystemHook, "system_hook_push.json", func(t *testing.T, ev *scm.Event) {
			if ev.Type != scm.EventPush || ev.Revision != "hotfix" || ev.TouchedDefaultBranch {
				t.Fatalf("system hook push = %+v", ev)
			}
			// total_commits_count (25) exceeds the commits carried: list is incomplete.
			if !ev.ChangedFilesTruncated || !slices.Equal(ev.ChangedFiles, []string{"apps/a/values.yaml"}) {
				t.Fatalf("changed files = %v truncated=%v", ev.ChangedFiles, ev.ChangedFilesTruncated)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			body := fixture(t, tc.file)
			ev, err := v.VerifyAndParse(delivery(tc.event, "uuid-1", whSecret), body, whSecret)
			if err != nil {
				t.Fatal(err)
			}
			if !ev.Verified || ev.Provider != gitlab.Type || ev.DeliveryID != "uuid-1" {
				t.Fatalf("event header fields = %+v", ev)
			}
			tc.check(t, ev)
		})
	}
}

func TestVerifyAndParseRejections(t *testing.T) {
	t.Parallel()
	v := gitlab.WebhookVerifier{}
	push := fixture(t, "push.json")
	tests := []struct {
		name     string
		h        http.Header
		body     []byte
		secret   []byte
		kind     scm.ErrorKind
		sentinel error
	}{
		{"wrong token", delivery(gitlab.EventPushHook, "1", []byte("nope")), push, whSecret, scm.KindUnauthorized, scm.ErrSignatureMismatch},
		{"missing token", delivery(gitlab.EventPushHook, "1", nil), push, whSecret, scm.KindUnauthorized, scm.ErrSignatureMissing},
		{"token prefix", delivery(gitlab.EventPushHook, "1", whSecret[:5]), push, whSecret, scm.KindUnauthorized, scm.ErrSignatureMismatch},
		{"unsupported event", delivery("Pipeline Hook", "1", whSecret), []byte(`{"object_kind":"pipeline"}`), whSecret, scm.KindInvalid, scm.ErrUnsupportedEvent},
		{"unsupported system hook", delivery(gitlab.EventSystemHook, "1", whSecret), fixture(t, "system_hook_repository_update.json"), whSecret, scm.KindInvalid, scm.ErrUnsupportedEvent},
		{"malformed", delivery(gitlab.EventPushHook, "1", whSecret), []byte(`{"ref":`), whSecret, scm.KindInvalid, scm.ErrMalformedPayload},
		{"no project", delivery(gitlab.EventPushHook, "1", whSecret), []byte(`{"ref":"refs/heads/main"}`), whSecret, scm.KindInvalid, scm.ErrMalformedPayload},
		{"no event header", http.Header{"X-Gitlab-Token": {string(whSecret)}}, push, whSecret, scm.KindInvalid, scm.ErrMalformedPayload},
		{"too large", delivery(gitlab.EventPushHook, "1", whSecret), make([]byte, scm.MaxWebhookPayloadBytes+1), whSecret, scm.KindInvalid, scm.ErrPayloadTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := v.VerifyAndParse(tc.h, tc.body, tc.secret)
			if scm.KindOf(err) != tc.kind || !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, want %s / %v", err, tc.kind, tc.sentinel)
			}
		})
	}
	// Authentication happens before the event type is inspected.
	_, err := v.VerifyAndParse(delivery("Pipeline Hook", "1", []byte("nope")), []byte(`{}`), whSecret)
	if !errors.Is(err, scm.ErrSignatureMismatch) {
		t.Fatalf("unauthenticated unsupported event: %v", err)
	}
}

// TestWebhookTokenDoesNotCoverBody documents why the conformance TamperedBody check is
// skipped: the GitLab token authenticates the sender, not the payload bytes.
func TestWebhookTokenDoesNotCoverBody(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Replace(string(fixture(t, "push.json")), `"alice"`, `"mallory"`, 1))
	ev, err := gitlab.WebhookVerifier{}.VerifyAndParse(delivery(gitlab.EventPushHook, "1", whSecret), body, whSecret)
	if err != nil || !ev.Verified || ev.Sender != "mallory" {
		t.Fatalf("ev = %+v, err = %v", ev, err)
	}
}

func TestMatchesWebhook(t *testing.T) {
	t.Parallel()
	v := gitlab.WebhookVerifier{}
	if !v.MatchesWebhook(delivery(gitlab.EventPushHook, "1", nil)) {
		t.Fatal("GitLab delivery not matched")
	}
	for _, h := range []http.Header{{"X-Github-Event": {"push"}}, {"X-Gitea-Event": {"push"}}, {"X-Event-Key": {"repo:push"}}, {}} {
		if v.MatchesWebhook(h) {
			t.Errorf("matched foreign headers %v", h)
		}
	}
}

func TestTokenComparisonProperty(t *testing.T) {
	t.Parallel()
	v := gitlab.WebhookVerifier{}
	push := fixture(t, "push.json")
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(t, "secret")
		presented := rapid.OneOf(rapid.Just(string(secret)), rapid.StringN(1, 64, -1)).Draw(t, "presented")
		if strings.ContainsAny(presented, "\r\n\x00") {
			return // not representable as a header value
		}
		h := delivery(gitlab.EventPushHook, "1", nil)
		h.Set(gitlab.HeaderToken, presented)
		ev, err := v.VerifyAndParse(h, push, secret)
		if presented == string(secret) {
			if err != nil || !ev.Verified {
				t.Fatalf("matching token rejected: %v", err)
			}
			return
		}
		if !errors.Is(err, scm.ErrSignatureMismatch) {
			t.Fatalf("token %q accepted for secret %q: %v", presented, secret, err)
		}
	})
}
