package gitea_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
)

var whSecret = []byte("gitea-webhook-secret")

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

func TestVerifyAndParsePush(t *testing.T) {
	t.Parallel()
	body := fixture(t, "push.json")
	ev, err := gitea.WebhookVerifier{}.VerifyAndParse(delivery("push", "d-1", body, whSecret), body, whSecret)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Verified || ev.Provider != "gitea" || ev.DeliveryID != "d-1" || ev.Type != scm.EventPush || ev.Revision != "main" || !ev.TouchedDefaultBranch ||
		ev.Before != sha(99) || ev.After != sha(1) || ev.Sender != "alice" || ev.ChangedFilesTruncated {
		t.Fatalf("event = %+v", ev)
	}
	want := []string{"apps/a/configmap.yaml", "apps/a/values.yaml", "README.md", "apps/old/deployment.yaml"}
	if !slices.Equal(ev.ChangedFiles, want) {
		t.Fatalf("changed files = %v", ev.ChangedFiles)
	}
	r := ev.Repository
	if r.Owner != "acme" || r.Name != "platform" || r.ID != "42" || r.HTMLURL != "https://gitea.example.com/acme/platform" ||
		r.CloneURL != "https://gitea.example.com/acme/platform.git" || r.SSHURL != "git@gitea.example.com:acme/platform.git" || !r.Private {
		t.Fatalf("repository = %+v", r)
	}
}

func TestVerifyAndParsePullRequests(t *testing.T) {
	t.Parallel()
	for file, check := range map[string]func(*scm.Event) bool{
		"pull_request_opened.json": func(ev *scm.Event) bool {
			pr := ev.PullRequest
			return ev.Action == scm.PRActionOpened && pr.Number == 1 && pr.Branch == "feature/a" && pr.TargetBranch == "main" &&
				pr.HeadSHA == sha(2) && pr.Author == "alice" && slices.Equal(pr.Labels, []string{"preview"}) && !pr.Merged
		},
		"pull_request_synchronized.json": func(ev *scm.Event) bool {
			pr := ev.PullRequest
			return ev.Action == scm.PRActionSynchronize && pr.Number == 2 && pr.Draft && pr.Merged && ev.Repository.Owner == "acme"
		},
	} {
		body := fixture(t, file)
		ev, err := gitea.WebhookVerifier{}.VerifyAndParse(delivery("pull_request", "d", body, whSecret), body, whSecret)
		if err != nil || ev.Type != scm.EventPullRequest || !check(ev) {
			t.Errorf("%s: %+v / %+v, %v", file, ev, ev.PullRequest, err)
		}
	}
}

func TestSignatureHeaderFallbacks(t *testing.T) {
	t.Parallel()
	body := fixture(t, "push.json")
	sig := scm.SignHMACSHA256(whSecret, body)
	for _, h := range []http.Header{
		{"X-Gitea-Event": {"push"}, "X-Gitea-Signature": {sig}},
		{"X-Gitea-Event": {"push"}, "X-Gogs-Signature": {sig}},
		{"X-Gitea-Event": {"push"}, "X-Hub-Signature-256": {"sha256=" + sig}},
	} {
		if ev, err := (gitea.WebhookVerifier{}).VerifyAndParse(h, body, whSecret); err != nil || !ev.Verified {
			t.Errorf("headers %v: %v", h, err)
		}
	}
	// X-Gitea-Signature takes precedence: a wrong value there is not rescued by others.
	h := http.Header{"X-Gitea-Event": {"push"}, "X-Gitea-Signature": {"00"}, "X-Gogs-Signature": {sig}}
	if _, err := (gitea.WebhookVerifier{}).VerifyAndParse(h, body, whSecret); !errors.Is(err, scm.ErrSignatureMismatch) {
		t.Fatalf("precedence: %v", err)
	}
}

func TestRejections(t *testing.T) {
	t.Parallel()
	v := gitea.WebhookVerifier{}
	body := fixture(t, "push.json")
	if _, err := v.VerifyAndParse(delivery("issues", "1", []byte(`{}`), whSecret), []byte(`{}`), whSecret); !errors.Is(err, scm.ErrUnsupportedEvent) {
		t.Errorf("issues event: %v", err)
	}
	if _, err := v.VerifyAndParse(delivery("push", "1", []byte(`{"ref":1}`), whSecret), []byte(`{"ref":1}`), whSecret); !errors.Is(err, scm.ErrMalformedPayload) {
		t.Errorf("malformed: %v", err)
	}
	h := delivery("push", "1", body, whSecret)
	h.Del("X-Gitea-Event")
	if _, err := v.VerifyAndParse(h, body, whSecret); !errors.Is(err, scm.ErrMalformedPayload) {
		t.Errorf("missing event header: %v", err)
	}
	if v.MatchesWebhook(http.Header{"X-Gogs-Event": {"push"}}) || !v.MatchesWebhook(delivery("push", "1", body, nil)) {
		t.Error("MatchesWebhook must key on X-Gitea-Event")
	}
}

func TestSignatureProperty(t *testing.T) {
	t.Parallel()
	v := gitea.WebhookVerifier{}
	base := fixture(t, "push.json")
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(t, "secret")
		body := slices.Clone(base)
		h := delivery("push", "p", body, secret)
		if _, err := v.VerifyAndParse(h, body, secret); err != nil {
			t.Fatalf("valid signature rejected: %v", err)
		}
		i := rapid.IntRange(0, len(body)-1).Draw(t, "i")
		bit := byte(1) << rapid.IntRange(0, 7).Draw(t, "bit")
		body[i] ^= bit
		if _, err := v.VerifyAndParse(h, body, secret); !errors.Is(err, scm.ErrSignatureMismatch) {
			t.Fatalf("flipped bit %d of byte %d accepted: %v", bit, i, err)
		}
	})
}
