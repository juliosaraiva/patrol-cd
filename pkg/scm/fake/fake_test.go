package fake_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/conformance"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/fake"
)

type fixture struct {
	p    *fake.Provider
	data conformance.Data
}

func (f *fixture) Data() conformance.Data { return f.data }

func (f *fixture) WebhookDelivery(t testing.TB, typ scm.EventType, id string, secret []byte) (http.Header, []byte) {
	t.Helper()
	d := fake.Delivery{Type: typ, Owner: f.data.Repo.Owner, Name: f.data.Repo.Name, DefaultBranch: f.data.DefaultBranch}
	switch typ {
	case scm.EventPush:
		d.Ref = "refs/heads/" + f.data.DefaultBranch
		d.Before = "0000000000000000000000000000000000000000"
		d.After = f.data.Branches[f.data.DefaultBranch]
		d.ChangedFiles = []string{"apps/a/values.yaml", "apps/a/values.yaml", "README.md"}
	case scm.EventPullRequest:
		d.Action = scm.PRActionOpened
		pr := f.data.PullRequests[0]
		d.PullRequest = &pr
	}
	h, body, err := fake.SignedDelivery(d, id, secret)
	if err != nil {
		t.Fatal(err)
	}
	return h, body
}

func (f *fixture) SignatureHeaders() []string { return []string{fake.HeaderSignature} }

func (f *fixture) LimitStatuses(n int) { f.p.SetStatusLimit(n) }

func (f *fixture) InjectRateLimit(d time.Duration) { f.p.InjectRateLimit(d, 0) }

func (f *fixture) CommitStatuses(r scm.RepoRef, sha string) []scm.CommitStatus {
	return f.p.CommitStatuses(r, sha)
}

func (f *fixture) CheckRuns(r scm.RepoRef, sha string) []conformance.RecordedCheckRun {
	var out []conformance.RecordedCheckRun
	for _, c := range f.p.CheckRuns(r, sha) {
		out = append(out, conformance.RecordedCheckRun{ID: c.ID, Name: c.Name, ExternalID: c.ExternalID, Status: c.Status, Conclusion: c.Conclusion})
	}
	return out
}

func sha(n int) string { return fmt.Sprintf("%040x", n) }

// seeded returns a fake provider with deterministic data spanning several pages.
func seeded(opts fake.Options) (*fake.Provider, conformance.Data) {
	p := fake.New(opts)
	org := "acme"
	var names []string
	for i := range 7 {
		name := fmt.Sprintf("svc-%02d", i)
		names = append(names, name)
		p.AddRepo(fake.Repo{Owner: org, Name: name, ID: int64(1000 + i), DefaultBranch: "main",
			Branches: map[string]string{"main": sha(100 + i)}, Topics: []string{"team-a"}})
	}
	branches := map[string]string{"main": sha(1), "feature-a": sha(2), "feature-b": sha(3), "release-1": sha(4), "hotfix": sha(5)}
	prs := []scm.PullRequest{
		{Number: 1, Title: "Add a", Branch: "feature-a", TargetBranch: "main", HeadSHA: sha(2), Author: "alice", Labels: []string{"preview"}},
		{Number: 2, Title: "Add b", Branch: "feature-b", TargetBranch: "main", HeadSHA: sha(3), Author: "bob", Labels: []string{"preview", "team-a"}},
		{Number: 3, Title: "Hotfix", Branch: "hotfix", TargetBranch: "release-1", HeadSHA: sha(5), Author: "carol"},
		{Number: 4, Title: "Docs", Branch: "docs", TargetBranch: "main", HeadSHA: sha(6), Author: "dave", Labels: []string{"docs"}},
		{Number: 5, Title: "Closed", Branch: "old", TargetBranch: "main", HeadSHA: sha(7), Author: "erin", State: "closed"},
	}
	p.AddRepo(fake.Repo{Owner: org, Name: "platform", ID: 42, DefaultBranch: "main", Branches: branches,
		Files:        map[string][]string{sha(1): {"apps/a/values.yaml", "README.md"}, sha(2): {"apps/b/values.yaml"}},
		PullRequests: prs, Topics: []string{"platform"}})
	p.AddRepo(fake.Repo{Owner: org, Name: "legacy", ID: 7, DefaultBranch: "main", Branches: map[string]string{"main": sha(9)}, Archived: true})
	p.AddRepo(fake.Repo{Owner: org, Name: "empty", ID: 8, DefaultBranch: "main"})
	p.AddInstallation(scm.Installation{ID: 77, Account: "acme", AccountType: "Organization"})
	names = append(names, "platform")
	return p, conformance.Data{
		Organization:         org,
		Repositories:         names,
		ArchivedRepositories: []string{"legacy"},
		EmptyRepository:      "empty",
		Repo:                 scm.RepoRef{Owner: org, Name: "platform"},
		DefaultBranch:        "main",
		Branches:             branches,
		ExistingPath:         "apps/a",
		MissingPath:          "apps/zzz",
		PullRequests:         prs[:4],
		PRLabel:              "preview",
		MissingRepo:          scm.RepoRef{Owner: "nobody", Name: "nothing"},
		StatusSHA:            sha(1),
		InstallationOwner:    "ACME",
	}
}

func TestConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		p, d := seeded(fake.Options{Name: "conf"})
		return p, &fixture{p: p, data: d}
	}, conformance.Options{})
}

func TestConformanceSubsetOfCapabilities(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) (scm.Provider, conformance.Fixture) {
		p, d := seeded(fake.Options{Capabilities: []scm.Capability{scm.CapabilityRepositories, scm.CapabilityWebhooks}})
		return p, &fixture{p: p, data: d}
	}, conformance.Options{})
}

func TestFaultInjection(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, d := seeded(fake.Options{})
	boom := errors.New("boom")
	p.InjectError(fake.OpListBranches, boom, 2)
	for i := range 2 {
		if _, err := p.ListBranches(ctx, d.Repo, true); !errors.Is(err, boom) {
			t.Fatalf("call %d: err = %v, want boom", i, err)
		}
	}
	if _, err := p.ListBranches(ctx, d.Repo, true); err != nil {
		t.Fatalf("fault should be exhausted: %v", err)
	}
	if got := p.Calls(fake.OpListBranches); got != 3 {
		t.Fatalf("Calls = %d, want 3", got)
	}

	p.InjectError(fake.OpResolveRevision, &scm.Error{Kind: scm.KindUnavailable}, 0)
	_, err := p.ResolveRevision(ctx, d.Repo, "main")
	var se *scm.Error
	if !errors.As(err, &se) || se.Op != fake.OpResolveRevision || se.Kind != scm.KindUnavailable {
		t.Fatalf("err = %#v", err)
	}
	p.ClearFaults()

	p.SetLatency(time.Hour)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := p.ListPullRequests(cctx, d.Repo, scm.PRQuery{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("latency must honor ctx: %v", err)
	}
	p.ClearFaults()

	p.SetFaultHook(func(_ context.Context, op string) error {
		if op == fake.OpPathExists {
			return &scm.Error{Kind: scm.KindForbidden}
		}
		return nil
	})
	if _, err := p.PathExists(ctx, d.Repo, "main", "apps"); scm.KindOf(err) != scm.KindForbidden {
		t.Fatalf("hook not applied: %v", err)
	}
}

func TestPaginationCap(t *testing.T) {
	t.Parallel()
	p, d := seeded(fake.Options{PageSize: 1, MaxPages: 3})
	_, err := p.ListBranches(t.Context(), d.Repo, true)
	if !errors.Is(err, scm.ErrPageLimit) || scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("err = %v, want page limit", err)
	}
}

func TestPRStates(t *testing.T) {
	t.Parallel()
	p, d := seeded(fake.Options{})
	tests := []struct {
		state scm.PRState
		want  int
	}{
		{"", 4}, {scm.PRStateOpen, 4}, {scm.PRStateClosed, 1}, {scm.PRStateAll, 5}, {scm.PRStateMerged, 0},
	}
	for _, tc := range tests {
		got, err := p.ListPullRequests(t.Context(), d.Repo, scm.PRQuery{State: tc.state})
		if err != nil || len(got) != tc.want {
			t.Errorf("state %q: %d PRs, err %v; want %d", tc.state, len(got), err, tc.want)
		}
	}
	got, err := p.ListPullRequests(t.Context(), d.Repo, scm.PRQuery{TargetBranch: "release-1"})
	if err != nil || len(got) != 1 || got[0].Number != 3 {
		t.Fatalf("target branch filter: %+v %v", got, err)
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	r := scm.NewRegistry()
	p, _ := seeded(fake.Options{})
	if err := fake.Register(r, p); err != nil {
		t.Fatal(err)
	}
	if err := fake.Register(r, p); err == nil {
		t.Fatal("duplicate registration must fail")
	}
	got, err := r.New(t.Context(), scm.Config{Type: "FAKE", Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Info().Name != "x" {
		t.Fatalf("name = %q", got.Info().Name)
	}
	if _, err := r.New(t.Context(), scm.Config{Type: "nope"}); !errors.Is(err, scm.ErrUnknownProviderType) {
		t.Fatalf("unknown type: %v", err)
	}
}

func TestWebhookUnsupportedEvent(t *testing.T) {
	t.Parallel()
	p, _ := seeded(fake.Options{})
	h, body, err := fake.SignedDelivery(fake.Delivery{Type: "issue_comment"}, "d1", []byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.VerifyAndParse(h, body, []byte("s"))
	if !errors.Is(err, scm.ErrUnsupportedEvent) {
		t.Fatalf("err = %v", err)
	}
	// Verification happens before event dispatch: a bad signature on an unsupported event
	// is still a signature error.
	_, err = p.VerifyAndParse(h, body, []byte("other"))
	if !errors.Is(err, scm.ErrSignatureMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestSeedingHelpers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, d := seeded(fake.Options{MaxStatusesPerContext: 1})
	missing := scm.RepoRef{Owner: "nobody", Name: "x"}
	if err := p.SetBranch(missing, "b", sha(1)); !scm.IsNotFound(err) {
		t.Fatalf("SetBranch on missing repo: %v", err)
	}
	if err := p.SetBranch(d.Repo, "feature-a", sha(42)); err != nil {
		t.Fatal(err)
	}
	if got, err := p.ResolveRevision(ctx, d.Repo, "refs/heads/feature-a"); err != nil || got != sha(42) {
		t.Fatalf("moved branch resolves to %q, %v", got, err)
	}
	pr := scm.PullRequest{Number: 9, Title: "New", Branch: "feature-a", TargetBranch: "main", HeadSHA: sha(42), Labels: []string{"preview"}}
	if err := p.AddPullRequest(missing, pr); !scm.IsNotFound(err) {
		t.Fatalf("AddPullRequest on missing repo: %v", err)
	}
	if err := p.AddPullRequest(d.Repo, pr); err != nil {
		t.Fatal(err)
	}
	pr.Labels[0] = "mutated"
	got, err := p.ListPullRequests(ctx, d.Repo, scm.PRQuery{Labels: []string{"preview"}})
	if err != nil || len(got) != 3 {
		t.Fatalf("labeled PRs after AddPullRequest: %d, %v", len(got), err)
	}
	p.SetToken(scm.Token{Value: "rotated", Username: "u"})
	if tok, err := p.Token(ctx); err != nil || tok.Value != "rotated" {
		t.Fatalf("token = %v, %v", tok, err)
	}
	st := scm.CommitStatus{State: scm.CommitStateSuccess, Context: "ctx"}
	if err := p.SetCommitStatus(ctx, d.Repo, sha(1), st); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCommitStatus(ctx, d.Repo, sha(1), st); !errors.Is(err, scm.ErrStatusLimit) || scm.KindOf(err) != scm.KindInvalid {
		t.Fatalf("status limit: %v", err)
	}
	p.SetStatusLimit(0)
	if err := p.SetCommitStatus(ctx, d.Repo, sha(1), st); err != nil {
		t.Fatalf("limit disabled: %v", err)
	}
}

// TestConcurrentMutationAndReads runs every seeding mutator concurrently with every
// reader. Under -race it fails if a reader touches repository state (branch maps, topic
// and file slices, pull requests) after releasing the provider lock.
func TestConcurrentMutationAndReads(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, d := seeded(fake.Options{})
	const n = 500
	var wg sync.WaitGroup
	start := make(chan struct{})
	gate := func(f func()) { wg.Go(func() { <-start; f() }) }
	gate(func() {
		for i := range n {
			_ = p.SetBranch(d.Repo, fmt.Sprintf("b-%d", i%7), sha(i))
			_ = p.SetBranch(scm.RepoRef{Owner: d.Organization, Name: "svc-00"}, "main", sha(i))
		}
	})
	gate(func() {
		for i := range n {
			_ = p.AddPullRequest(d.Repo, scm.PullRequest{Number: int64(100 + i), Branch: "x", TargetBranch: "main", HeadSHA: sha(i), Labels: []string{"preview"}})
		}
	})
	gate(func() {
		for i := range n {
			p.AddRepo(fake.Repo{Owner: d.Organization, Name: "svc-01", DefaultBranch: "main",
				Branches: map[string]string{"main": sha(i)}, Topics: []string{"team-a"}, Files: map[string][]string{sha(i): {"a"}}})
			p.AddInstallation(scm.Installation{ID: int64(i), Account: "acme"})
			p.SetToken(scm.Token{Value: fmt.Sprint(i)})
		}
	})
	gate(func() {
		st := scm.CommitStatus{State: scm.CommitStatePending, Context: "c"}
		run := scm.CheckRun{Name: "patrol", ExternalID: "e", Status: scm.CheckStatusInProgress}
		for range n {
			_ = p.SetCommitStatus(ctx, d.Repo, d.StatusSHA, st)
			_, _ = p.UpsertCheckRun(ctx, d.Repo, d.StatusSHA, run)
		}
	})
	readers := []func(){
		func() {
			_, _ = p.ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, AllBranches: true, Topics: []string{"team-a"},
				Match: func(r scm.Repository) bool { return len(r.Labels) >= 0 }})
		},
		func() { _, _ = p.ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization}) },
		func() { _, _ = p.ListBranches(ctx, d.Repo, true) },
		func() { _, _ = p.ResolveRevision(ctx, d.Repo, "b-3") },
		func() { _, _ = p.PathExists(ctx, d.Repo, "main", "apps/a") },
		func() { _, _ = p.ListPullRequests(ctx, d.Repo, scm.PRQuery{Labels: []string{"preview"}}) },
		func() { _, _ = p.ListInstallations(ctx) },
		func() { _, _ = p.FindInstallation(ctx, "acme") },
		func() { _, _ = p.Token(ctx) },
		func() { _ = p.CommitStatuses(d.Repo, d.StatusSHA); _ = p.CheckRuns(d.Repo, d.StatusSHA); _ = p.Info() },
	}
	for _, read := range readers {
		gate(func() {
			for range n {
				read()
			}
		})
	}
	close(start)
	wg.Wait()
	// The final state is still consistent.
	got, err := p.ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, AllBranches: true})
	if err != nil || len(got) == 0 {
		t.Fatalf("ListRepositories after concurrent use: %d, %v", len(got), err)
	}
}

func TestWebhookDeliveryIDBounded(t *testing.T) {
	t.Parallel()
	p, _ := seeded(fake.Options{})
	secret := []byte("s")
	for _, tc := range []struct {
		id      string
		wantErr bool
	}{
		{"0b989ba4-242f-11e5-81e1-c7b1bd4ba7a7", false},
		{"", false},
		{strings.Repeat("d", scm.MaxDeliveryIDBytes), false},
		{strings.Repeat("d", scm.MaxDeliveryIDBytes+1), true},
		{strings.Repeat("d", 1<<20), true},
		{"has space", true},
		{"tab\tid", true},
	} {
		h, body, err := fake.SignedDelivery(fake.Delivery{Type: scm.EventPing}, tc.id, secret)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := p.VerifyAndParse(h, body, secret)
		if tc.wantErr {
			if !errors.Is(err, scm.ErrInvalidDeliveryID) || scm.KindOf(err) != scm.KindInvalid || ev != nil {
				t.Errorf("id of %d bytes: err = %v", len(tc.id), err)
			}
			continue
		}
		if err != nil || ev.DeliveryID != tc.id {
			t.Errorf("id %q: ev=%+v err=%v", tc.id, ev, err)
		}
	}
}
