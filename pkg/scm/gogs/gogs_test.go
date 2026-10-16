package gogs_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gogs"
)

var whSecret = []byte("gogs-webhook-secret")

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// delivery returns the headers Gogs sends; Gogs also sends GitHub-style headers.
func delivery(event, id string, body, secret []byte) http.Header {
	h := http.Header{}
	h.Set("X-Gogs-Event", event)
	h.Set("X-Gogs-Delivery", id)
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", id)
	h.Set("Content-Type", "application/json")
	if len(secret) > 0 {
		h.Set("X-Gogs-Signature", scm.SignHMACSHA256(secret, body))
	}
	return h
}

type fixtureT struct{}

func (fixtureT) Data() conformance.Data {
	return conformance.Data{Repo: scm.RepoRef{Owner: "acme", Name: "platform"}, DefaultBranch: "main", MissingRepo: scm.RepoRef{Owner: "nobody", Name: "nothing"}}
}

func (fixtureT) WebhookDelivery(t testing.TB, typ scm.EventType, id string, secret []byte) (http.Header, []byte) {
	if typ != scm.EventPush {
		t.Fatalf("Gogs supports push events only (got %q)", typ)
	}
	body := fixture(t, "push.json")
	return delivery("push", id, body, secret), body
}

func (fixtureT) SignatureHeaders() []string                                   { return []string{gogs.HeaderSignature} }
func (fixtureT) InjectRateLimit(time.Duration)                                {}
func (fixtureT) CommitStatuses(scm.RepoRef, string) []scm.CommitStatus        { return nil }
func (fixtureT) CheckRuns(scm.RepoRef, string) []conformance.RecordedCheckRun { return nil }

func TestConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		r := scm.NewRegistry()
		if err := gogs.Register(r); err != nil {
			t.Fatal(err)
		}
		p, err := r.New(t.Context(), scm.Config{Type: gogs.Type, Name: "gogs"})
		if err != nil {
			t.Fatal(err)
		}
		return p, fixtureT{}
	}, conformance.Options{Skip: []string{
		// Upstream accepts only Gogs push events (no pull request normalization), and Gogs
		// pull request payloads carry no head SHA.
		"Webhooks/PullRequest",
	}})
}

func TestPush(t *testing.T) {
	t.Parallel()
	body := fixture(t, "push.json")
	ev, err := gogs.WebhookVerifier{}.VerifyAndParse(delivery("push", "d-1", body, whSecret), body, whSecret)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Verified || ev.Provider != "gogs" || ev.DeliveryID != "d-1" || ev.Revision != "main" || !ev.TouchedDefaultBranch || ev.Sender != "alice" {
		t.Fatalf("event = %+v", ev)
	}
	r := ev.Repository
	if r.Owner != "acme" || r.Name != "platform" || r.ID != "42" || r.HTMLURL != "https://gogs.example.com/acme/platform" || !r.Private {
		t.Fatalf("repository = %+v", r)
	}
	if !slices.Equal(ev.ChangedFiles, []string{"apps/a/configmap.yaml", "apps/a/values.yaml", "README.md", "apps/old/deployment.yaml"}) {
		t.Fatalf("changed files = %v", ev.ChangedFiles)
	}
	noRepo := []byte(`{"ref":"refs/heads/main","after":"x"}`)
	ev, err = gogs.WebhookVerifier{}.VerifyAndParse(delivery("push", "d-2", noRepo, whSecret), noRepo, whSecret)
	if err != nil || ev.Repository != nil || ev.TouchedDefaultBranch {
		t.Fatalf("push without repository: %+v, %v", ev, err)
	}
}

func TestRejectionsAndMatching(t *testing.T) {
	t.Parallel()
	v := gogs.WebhookVerifier{}
	body := fixture(t, "push.json")
	if _, err := v.VerifyAndParse(delivery("create", "1", body, whSecret), body, whSecret); !errors.Is(err, scm.ErrUnsupportedEvent) {
		t.Errorf("create: %v", err)
	}
	if _, err := v.VerifyAndParse(delivery("create", "1", body, []byte("x")), body, whSecret); !errors.Is(err, scm.ErrSignatureMismatch) {
		t.Errorf("signature is checked before the event type: %v", err)
	}
	h := delivery("push", "1", body, whSecret)
	h.Set("X-Gitea-Event", "push")
	if v.MatchesWebhook(h) {
		t.Error("Gitea deliveries must be left to the Gitea verifier")
	}
	if !v.MatchesWebhook(delivery("push", "1", body, nil)) || v.MatchesWebhook(http.Header{"X-GitHub-Event": {"push"}}) {
		t.Error("MatchesWebhook must key on X-Gogs-Event")
	}
	p, _ := gogs.New(t.Context(), scm.Config{})
	if _, err := scm.As[scm.RepositoryLister](p); !scm.IsUnsupported(err) {
		t.Errorf("Gogs must not declare repositories: %v", err)
	}
}

func TestSignatureProperty(t *testing.T) {
	t.Parallel()
	v := gogs.WebhookVerifier{}
	base := fixture(t, "push.json")
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(t, "secret")
		body := slices.Clone(base)
		h := delivery("push", "p", body, secret)
		if _, err := v.VerifyAndParse(h, body, secret); err != nil {
			t.Fatalf("valid signature rejected: %v", err)
		}
		i := rapid.IntRange(0, len(body)-1).Draw(t, "i")
		body[i] ^= byte(1) << rapid.IntRange(0, 7).Draw(t, "bit")
		if _, err := v.VerifyAndParse(h, body, secret); !errors.Is(err, scm.ErrSignatureMismatch) {
			t.Fatalf("tampered body accepted: %v", err)
		}
	})
}
