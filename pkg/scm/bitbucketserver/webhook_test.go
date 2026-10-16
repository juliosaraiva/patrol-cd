package bitbucketserver_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
)

var whSecret = []byte("bbs-webhook-secret")

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

func TestWebhookPush(t *testing.T) {
	t.Parallel()
	body := fixture(t, "repo_refs_changed.json")
	ev, err := bitbucketserver.WebhookVerifier{}.VerifyAndParse(delivery("repo:refs_changed", "req-1", body, whSecret), body, whSecret)
	if err != nil {
		t.Fatal(err)
	}
	// First change only, as upstream; HEAD assumed touched; no changed-file list.
	if !ev.Verified || ev.Provider != "bitbucketserver" || ev.DeliveryID != "req-1" || ev.Type != scm.EventPush ||
		ev.Ref != "refs/heads/main" || ev.Revision != "main" || ev.Before != sha(99) || ev.After != sha(1) ||
		!ev.TouchedDefaultBranch || !ev.ChangedFilesTruncated || len(ev.ChangedFiles) != 0 || ev.Created || ev.Sender != "alice" {
		t.Fatalf("event = %+v", ev)
	}
	r := ev.Repository
	if r.Owner != "PROJ" || r.Name != "platform" || r.ID != "42" || r.CloneURL != "https://bitbucket.example.com/scm/proj/platform.git" ||
		r.SSHURL != "ssh://git@bitbucket.example.com:7999/proj/platform.git" || r.HTMLURL != "https://bitbucket.example.com/projects/PROJ/repos/platform/browse" || !r.Private {
		t.Fatalf("repository = %+v", r)
	}
}

func TestWebhookPullRequests(t *testing.T) {
	t.Parallel()
	v := bitbucketserver.WebhookVerifier{}
	body := fixture(t, "pr_opened.json")
	ev, err := v.VerifyAndParse(delivery("pr:opened", "req-2", body, whSecret), body, whSecret)
	if err != nil {
		t.Fatal(err)
	}
	pr := ev.PullRequest
	if ev.Type != scm.EventPullRequest || ev.Action != scm.PRActionOpened || pr.Number != 1 || pr.Branch != "feature/a" || pr.TargetBranch != "main" ||
		pr.HeadSHA != sha(2) || pr.Author != "alice" || pr.Merged || ev.Repository.Owner != "PROJ" || ev.Repository.CloneURL == "" {
		t.Fatalf("opened = %+v / %+v", ev, pr)
	}
	body = fixture(t, "pr_merged.json")
	ev, err = v.VerifyAndParse(delivery("pr:merged", "req-3", body, whSecret), body, whSecret)
	if err != nil || ev.Action != scm.PRActionClosed || !ev.PullRequest.Merged || ev.Repository.Private {
		t.Fatalf("merged = %+v, %v", ev, err)
	}
}

func TestWebhookPingAndRejections(t *testing.T) {
	t.Parallel()
	v := bitbucketserver.WebhookVerifier{}
	ping := fixture(t, "diagnostics_ping.json")
	ev, err := v.VerifyAndParse(delivery("diagnostics:ping", "p", ping, whSecret), ping, whSecret)
	if err != nil || ev.Type != scm.EventPing {
		t.Fatalf("ping: %+v, %v", ev, err)
	}
	if _, err := v.VerifyAndParse(delivery("diagnostics:ping", "p", ping, nil), ping, whSecret); !errors.Is(err, scm.ErrSignatureMissing) {
		t.Fatalf("unsigned ping with a configured secret: %v", err)
	}
	h := delivery("repo:refs_changed", "x", ping, whSecret)
	h.Set(bitbucketserver.HeaderSignature, scm.SignHMACSHA256(whSecret, ping)) // missing "sha256=" prefix
	if _, err := v.VerifyAndParse(h, ping, whSecret); !errors.Is(err, scm.ErrSignatureMismatch) {
		t.Fatalf("signature without prefix: %v", err)
	}
	if _, err := v.VerifyAndParse(delivery("repo:comment:added", "x", ping, whSecret), ping, whSecret); !errors.Is(err, scm.ErrUnsupportedEvent) {
		t.Fatalf("unsupported: %v", err)
	}
	if _, err := v.VerifyAndParse(delivery("pr:opened", "x", []byte(`{}`), whSecret), []byte(`{}`), whSecret); !errors.Is(err, scm.ErrMalformedPayload) {
		t.Fatalf("pr without pullRequest: %v", err)
	}
	cloud := http.Header{"X-Event-Key": {"repo:push"}, "X-Hook-Uuid": {"{uuid}"}}
	if v.MatchesWebhook(cloud) || !v.MatchesWebhook(delivery("repo:refs_changed", "x", nil, nil)) {
		t.Fatal("MatchesWebhook must claim X-Event-Key without X-Hook-UUID")
	}
}
