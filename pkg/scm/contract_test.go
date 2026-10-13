package scm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

type stubProvider struct {
	caps []scm.Capability
}

func (s stubProvider) Info() scm.Info {
	return scm.Info{Type: "stub", Name: "s1", ContractVersion: scm.ContractVersion, Capabilities: s.caps}
}

func (stubProvider) ListBranches(context.Context, scm.RepoRef, bool) ([]scm.Branch, error) {
	return nil, nil
}

func (stubProvider) Token(context.Context) (scm.Token, error) { return scm.Token{}, nil }

func TestAs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		caps     []scm.Capability
		probe    func(scm.Provider) error
		wantErr  bool
		wantCapa scm.Capability
	}{
		{"declared and implemented", []scm.Capability{scm.CapabilityBranches},
			func(p scm.Provider) error { _, err := scm.As[scm.BranchLister](p); return err }, false, ""},
		{"implemented but not declared", nil,
			func(p scm.Provider) error { _, err := scm.As[scm.CredentialSource](p); return err }, true, scm.CapabilityCredentials},
		{"declared but not implemented", []scm.Capability{scm.CapabilityRepositories},
			func(p scm.Provider) error { _, err := scm.As[scm.RepositoryLister](p); return err }, true, scm.CapabilityRepositories},
		{"not a capability interface", []scm.Capability{scm.CapabilityBranches},
			func(p scm.Provider) error { _, err := scm.As[fmt.Stringer](p); return err }, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.probe(stubProvider{caps: tc.caps})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}
			if !errors.Is(err, scm.ErrUnsupported) || !scm.IsUnsupported(err) {
				t.Fatalf("err %v does not match ErrUnsupported", err)
			}
			var ue *scm.UnsupportedError
			if !errors.As(err, &ue) || ue.Provider != "stub" || ue.Name != "s1" {
				t.Fatalf("UnsupportedError = %+v", ue)
			}
			if tc.wantCapa != "" && ue.Capability != tc.wantCapa {
				t.Fatalf("capability = %q, want %q", ue.Capability, tc.wantCapa)
			}
		})
	}
	if _, err := scm.As[scm.BranchLister](nil); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("nil provider: %v", err)
	}
}

func TestCapabilityFor(t *testing.T) {
	t.Parallel()
	if c, ok := scm.CapabilityFor[scm.PullRequestLister](); !ok || c != scm.CapabilityPullRequests {
		t.Fatalf("got %q %v", c, ok)
	}
	if _, ok := scm.CapabilityFor[error](); ok {
		t.Fatal("error is not a capability")
	}
	for _, c := range scm.AllCapabilities() {
		if !c.Known() {
			t.Fatalf("%q not known", c)
		}
	}
	if scm.Capability("teleport").Known() {
		t.Fatal("unknown capability reported known")
	}
}

func TestRepoRefValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref scm.RepoRef
		ok  bool
	}{
		{scm.RepoRef{Owner: "acme", Name: "platform"}, true},
		{scm.RepoRef{Owner: "Acme-Inc", Name: "my.repo_name-1"}, true},
		{scm.RepoRef{Owner: "", Name: "x"}, false},
		{scm.RepoRef{Owner: "acme", Name: ""}, false},
		{scm.RepoRef{Owner: "..", Name: "x"}, false},
		{scm.RepoRef{Owner: "acme", Name: "."}, false},
		{scm.RepoRef{Owner: "a/b", Name: "x"}, false},
		{scm.RepoRef{Owner: "acme", Name: "x?y"}, false},
		{scm.RepoRef{Owner: "acme", Name: "x%2F.."}, false},
		{scm.RepoRef{Owner: "acme", Name: "x y"}, false},
		{scm.RepoRef{Owner: strings.Repeat("a", 257), Name: "x"}, false},
	}
	for _, tc := range tests {
		err := tc.ref.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%q: err = %v, want ok=%v", tc.ref.FullName(), err, tc.ok)
		}
		if err != nil && scm.KindOf(err) != scm.KindInvalid {
			t.Errorf("%q: kind = %q", tc.ref.FullName(), scm.KindOf(err))
		}
	}
}

func TestParseFullName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want scm.RepoRef
		ok   bool
	}{
		{"acme/platform", scm.RepoRef{Owner: "acme", Name: "platform"}, true},
		{"group/sub/proj", scm.RepoRef{Owner: "group/sub", Name: "proj"}, true},
		{"noslash", scm.RepoRef{}, false},
		{"/x", scm.RepoRef{}, false},
		{"x/", scm.RepoRef{}, false},
	}
	for _, tc := range tests {
		got, err := scm.ParseFullName(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseFullName(%q) = %v, %v", tc.in, got, err)
		}
	}
}

func TestCommitStatusValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		st scm.CommitStatus
		ok bool
	}{
		{scm.CommitStatus{State: scm.CommitStatePending, Context: "c"}, true},
		{scm.CommitStatus{State: scm.CommitStateSuccess, Context: "c"}, true},
		{scm.CommitStatus{State: scm.CommitStateFailure, Context: "c"}, true},
		{scm.CommitStatus{State: scm.CommitStateError, Context: "c"}, true},
		{scm.CommitStatus{State: "done", Context: "c"}, false},
		{scm.CommitStatus{State: scm.CommitStateSuccess, Context: " "}, false},
	}
	for _, tc := range tests {
		if err := tc.st.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.st, err)
		}
	}
}

func TestCheckRunValidate(t *testing.T) {
	t.Parallel()
	base := scm.CheckRun{Name: "n", ExternalID: "e"}
	with := func(f func(*scm.CheckRun)) scm.CheckRun { c := base; f(&c); return c }
	tests := []struct {
		name string
		run  scm.CheckRun
		ok   bool
	}{
		{"queued", with(func(c *scm.CheckRun) { c.Status = scm.CheckStatusQueued }), true},
		{"in progress with output", with(func(c *scm.CheckRun) { c.Status = scm.CheckStatusInProgress; c.Title, c.Summary = "t", "s" }), true},
		{"completed success", with(func(c *scm.CheckRun) {
			c.Status, c.Conclusion = scm.CheckStatusCompleted, scm.CheckConclusionSuccess
		}), true},
		{"completed without conclusion", with(func(c *scm.CheckRun) { c.Status = scm.CheckStatusCompleted }), false},
		{"conclusion without completed", with(func(c *scm.CheckRun) {
			c.Status, c.Conclusion = scm.CheckStatusQueued, scm.CheckConclusionFailure
		}), false},
		{"bad status", with(func(c *scm.CheckRun) { c.Status = "running" }), false},
		{"bad conclusion", with(func(c *scm.CheckRun) { c.Status, c.Conclusion = scm.CheckStatusCompleted, "great" }), false},
		{"missing name", scm.CheckRun{ExternalID: "e", Status: scm.CheckStatusQueued}, false},
		{"missing external id", scm.CheckRun{Name: "n", Status: scm.CheckStatusQueued}, false},
		{"summary without title", with(func(c *scm.CheckRun) { c.Status = scm.CheckStatusQueued; c.Summary = "s" }), false},
	}
	for _, tc := range tests {
		if err := tc.run.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestSecretsNeverRendered(t *testing.T) {
	t.Parallel()
	tok := scm.Token{Value: "ghs_supersecret", Username: "x-access-token", ExpiresAt: time.Unix(1, 0)}
	app := scm.GitHubAppConfig{AppID: 1, PrivateKey: []byte("-----BEGIN RSA PRIVATE KEY-----secretkey")}
	cfg := scm.Config{Type: "github", GitHubApp: &app, Token: func(context.Context) (string, error) { return "pat-secret", nil }}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("x", "token", tok, "app", app, "cfg", cfg)
	rendered := []string{buf.String(), fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v", tok, tok, tok), fmt.Sprintf("%v %+v %#v", app, app, app)}
	for _, s := range rendered {
		for _, secret := range []string{"ghs_supersecret", "secretkey", "pat-secret"} {
			if strings.Contains(s, secret) {
				t.Fatalf("secret %q rendered in %q", secret, s)
			}
		}
	}
	if !strings.Contains(buf.String(), "redacted") {
		t.Fatalf("expected redaction marker in %s", buf.String())
	}
}

func TestShortSHA(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sha  string
		n    int
		want string
	}{
		{"0123456789abcdef", 8, "01234567"},
		{"0123456789abcdef", 7, "0123456"},
		{"abc", 8, "abc"},
		{"", 8, ""},
		{"abc", -1, ""},
	}
	for _, tc := range tests {
		if got := scm.ShortSHA(tc.sha, tc.n); got != tc.want {
			t.Errorf("ShortSHA(%q,%d) = %q, want %q", tc.sha, tc.n, got, tc.want)
		}
	}
}

func TestTruncateRunesProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		n := rapid.IntRange(0, 50).Draw(t, "n")
		got := scm.TruncateRunes(s, n)
		if !strings.HasPrefix(s, got) {
			t.Fatalf("%q is not a prefix of %q", got, s)
		}
		if c := utf8.RuneCountInString(got); c > n {
			t.Fatalf("rune count %d > %d", c, n)
		}
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("truncation produced invalid UTF-8: %q", got)
		}
		if utf8.RuneCountInString(s) <= n && got != s {
			t.Fatalf("short string changed: %q → %q", s, got)
		}
		if scm.TruncateRunes(got, n) != got {
			t.Fatal("truncation is not idempotent")
		}
	})
}
