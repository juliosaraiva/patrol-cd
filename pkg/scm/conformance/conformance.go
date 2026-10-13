// Package conformance is the contract test suite every scm provider must pass. A provider
// test calls Run with a constructor that returns a fresh provider wired to seeded test
// data (an in-memory fake or a local httptest server) and a Fixture describing that data.
// Checks run only for capabilities the provider declares; undeclared capabilities must
// fail with scm.ErrUnsupported.
package conformance

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Data describes the seeded state behind a provider under test.
type Data struct {
	// Organization owns Repositories and ArchivedRepositories; it must hold more
	// repositories than one provider page so that pagination is exercised.
	Organization string
	// Repositories are the names of all non-archived repositories in Organization.
	Repositories []string
	// ArchivedRepositories are archived repositories in Organization (may be empty).
	ArchivedRepositories []string
	// EmptyRepository, when set, is a repository in Organization without a default branch.
	EmptyRepository string
	// Repo is a repository with several branches (more than one page), paths and PRs.
	Repo          scm.RepoRef
	DefaultBranch string
	// Branches maps every branch of Repo to its head SHA.
	Branches map[string]string
	// ExistingPath exists at DefaultBranch; MissingPath does not.
	ExistingPath string
	MissingPath  string
	// PullRequests are all open pull requests of Repo (more than one page).
	PullRequests []scm.PullRequest
	// PRLabel is carried by a non-empty strict subset of PullRequests.
	PRLabel string
	// MissingRepo does not exist.
	MissingRepo scm.RepoRef
	// StatusSHA is a commit in Repo used for commit statuses and check runs.
	StatusSHA string
	// InstallationOwner owns an installation (installations capability).
	InstallationOwner string
}

// RecordedCheckRun is a check run observed by the fixture backend.
type RecordedCheckRun struct {
	ID         int64
	Name       string
	ExternalID string
	Status     scm.CheckStatus
	Conclusion scm.CheckConclusion
}

// Fixture exposes the seeded data and backend hooks of one provider instance.
type Fixture interface {
	Data() Data
	// WebhookDelivery returns headers and raw body of a delivery of the given type for
	// Data().Repo (a push to DefaultBranch changing at least one file, or an "opened"
	// pull request), signed with secret, or unsigned when secret is empty. The same
	// deliveryID must be used verbatim.
	WebhookDelivery(t testing.TB, typ scm.EventType, deliveryID string, secret []byte) (http.Header, []byte)
	// SignatureHeaders lists the headers carrying the signature (removed by the
	// missing-signature check).
	SignatureHeaders() []string
	// InjectRateLimit makes every subsequent backend request fail with the provider's
	// rate-limit response advertising a reset after retryAfter.
	InjectRateLimit(retryAfter time.Duration)
	// CommitStatuses returns statuses recorded for repo@sha.
	CommitStatuses(repo scm.RepoRef, sha string) []scm.CommitStatus
	// CheckRuns returns check runs recorded for repo@sha.
	CheckRuns(repo scm.RepoRef, sha string) []RecordedCheckRun
}

// NewFunc constructs a fresh provider and fixture. It is called once per check.
type NewFunc func(t *testing.T) (scm.Provider, Fixture)

// Options tunes the suite.
type Options struct {
	// Skip lists check names to skip (with the reason recorded by the caller).
	Skip []string
	// Timeout bounds each check (default 30s).
	Timeout time.Duration
}

type check struct {
	name string
	cap  scm.Capability // "" = always
	fn   func(t *testing.T, ctx context.Context, p scm.Provider, f Fixture)
}

// Run executes the conformance checks as subtests of t.
func Run(t *testing.T, newProvider NewFunc, opts Options) {
	t.Helper()
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	checks := []check{
		{"Info", "", checkInfo},
		{"UnsupportedCapabilities", "", checkUnsupported},
		{"Repositories/Pagination", scm.CapabilityRepositories, checkRepoPagination},
		{"Repositories/Archived", scm.CapabilityRepositories, checkRepoArchived},
		{"Repositories/DefaultBranchOnly", scm.CapabilityRepositories, checkRepoDefaultBranch},
		{"Repositories/AllBranches", scm.CapabilityRepositories, checkRepoAllBranches},
		{"Repositories/MatchPushdown", scm.CapabilityRepositories, checkRepoMatch},
		{"Repositories/CloneProtocol", scm.CapabilityRepositories, checkRepoCloneProtocol},
		{"Repositories/NotFound", scm.CapabilityRepositories, checkRepoNotFound},
		{"Branches/All", scm.CapabilityBranches, checkBranchesAll},
		{"Branches/Default", scm.CapabilityBranches, checkBranchesDefault},
		{"Branches/NotFound", scm.CapabilityBranches, checkBranchesNotFound},
		{"Revisions/Resolve", scm.CapabilityRevisions, checkResolve},
		{"Revisions/NotFound", scm.CapabilityRevisions, checkResolveNotFound},
		{"Paths/Exists", scm.CapabilityPaths, checkPaths},
		{"Paths/RejectsTraversal", scm.CapabilityPaths, checkPathTraversal},
		{"PullRequests/Pagination", scm.CapabilityPullRequests, checkPRs},
		{"PullRequests/Labels", scm.CapabilityPullRequests, checkPRLabels},
		{"PullRequests/NotFound", scm.CapabilityPullRequests, checkPRNotFound},
		{"Webhooks/ValidSignature", scm.CapabilityWebhooks, checkWebhookValid},
		{"Webhooks/TamperedBody", scm.CapabilityWebhooks, checkWebhookTampered},
		{"Webhooks/WrongSecret", scm.CapabilityWebhooks, checkWebhookWrongSecret},
		{"Webhooks/MissingSignature", scm.CapabilityWebhooks, checkWebhookMissingSig},
		{"Webhooks/EmptySecretPolicy", scm.CapabilityWebhooks, checkWebhookEmptySecret},
		{"Webhooks/Dedup", scm.CapabilityWebhooks, checkWebhookDedup},
		{"Webhooks/PullRequest", scm.CapabilityWebhooks, checkWebhookPR},
		{"CommitStatuses/Set", scm.CapabilityCommitStatuses, checkStatus},
		{"CommitStatuses/Invalid", scm.CapabilityCommitStatuses, checkStatusInvalid},
		{"CheckRuns/IdempotentUpsert", scm.CapabilityCheckRuns, checkCheckRuns},
		{"Credentials/Token", scm.CapabilityCredentials, checkToken},
		{"Installations/ListAndFind", scm.CapabilityInstallations, checkInstallations},
		{"RateLimit/ErrorMapping", "", checkRateLimit},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if slices.Contains(opts.Skip, c.name) {
				t.Skip("skipped by provider test options")
			}
			p, f := newProvider(t)
			if c.cap != "" && !p.Info().Has(c.cap) {
				t.Skipf("capability %q not declared", c.cap)
			}
			ctx, cancel := context.WithTimeout(t.Context(), opts.Timeout)
			defer cancel()
			c.fn(t, ctx, p, f)
		})
	}
}

func must[T any](t *testing.T, p scm.Provider) T {
	t.Helper()
	v, err := scm.As[T](p)
	if err != nil {
		t.Fatalf("As: %v", err)
	}
	return v
}

func wantKind(t *testing.T, err error, kind scm.ErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", kind)
	}
	if got := scm.KindOf(err); got != kind {
		t.Fatalf("expected error kind %s, got %q: %v", kind, got, err)
	}
}

func checkInfo(t *testing.T, _ context.Context, p scm.Provider, _ Fixture) {
	info := p.Info()
	if info.Type == "" {
		t.Error("Info.Type is empty")
	}
	if info.ContractVersion != scm.ContractVersion {
		t.Errorf("ContractVersion = %q, want %q", info.ContractVersion, scm.ContractVersion)
	}
	seen := map[scm.Capability]bool{}
	for _, c := range info.Capabilities {
		if !c.Known() {
			t.Errorf("unknown capability %q declared", c)
		}
		if seen[c] {
			t.Errorf("capability %q declared twice", c)
		}
		seen[c] = true
	}
}

func checkUnsupported(t *testing.T, _ context.Context, p scm.Provider, _ Fixture) {
	info := p.Info()
	probe := func(c scm.Capability, err error) {
		t.Helper()
		if info.Has(c) {
			if err != nil {
				t.Errorf("declared capability %q: As failed: %v", c, err)
			}
			return
		}
		if !errors.Is(err, scm.ErrUnsupported) {
			t.Errorf("undeclared capability %q: As error = %v, want ErrUnsupported", c, err)
			return
		}
		var ue *scm.UnsupportedError
		if !errors.As(err, &ue) || ue.Capability != c || ue.Provider != info.Type {
			t.Errorf("undeclared capability %q: UnsupportedError = %+v", c, ue)
		}
	}
	_, err := scm.As[scm.RepositoryLister](p)
	probe(scm.CapabilityRepositories, err)
	_, err = scm.As[scm.BranchLister](p)
	probe(scm.CapabilityBranches, err)
	_, err = scm.As[scm.RevisionResolver](p)
	probe(scm.CapabilityRevisions, err)
	_, err = scm.As[scm.PathChecker](p)
	probe(scm.CapabilityPaths, err)
	_, err = scm.As[scm.PullRequestLister](p)
	probe(scm.CapabilityPullRequests, err)
	_, err = scm.As[scm.WebhookVerifier](p)
	probe(scm.CapabilityWebhooks, err)
	_, err = scm.As[scm.CommitStatusReporter](p)
	probe(scm.CapabilityCommitStatuses, err)
	_, err = scm.As[scm.CheckRunReporter](p)
	probe(scm.CapabilityCheckRuns, err)
	_, err = scm.As[scm.CredentialSource](p)
	probe(scm.CapabilityCredentials, err)
	_, err = scm.As[scm.InstallationLister](p)
	probe(scm.CapabilityInstallations, err)
}

func names(repos []scm.Repository) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func sorted(s []string) []string {
	c := slices.Clone(s)
	sort.Strings(c)
	return c
}

func checkRepoPagination(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	got, err := must[scm.RepositoryLister](t, p).ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, Archived: scm.ArchivedExclude, AllBranches: true, Match: func(r scm.Repository) bool { return r.Name == "\x00never" }})
	if err != nil {
		t.Fatalf("ListRepositories(match none): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Match returning false must drop every repository, got %d", len(got))
	}
	var seen []scm.Repository
	_, err = must[scm.RepositoryLister](t, p).ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, Archived: scm.ArchivedExclude, Match: func(r scm.Repository) bool {
		seen = append(seen, r)
		return false
	}})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	want := sorted(d.Repositories)
	if d.EmptyRepository != "" {
		want = sorted(append(want, d.EmptyRepository))
	}
	if g := names(seen); !slices.Equal(g, want) {
		t.Fatalf("pagination incomplete: got %v, want %v", g, want)
	}
}

func checkRepoArchived(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	if len(d.ArchivedRepositories) == 0 {
		t.Skip("fixture has no archived repositories")
	}
	rl := must[scm.RepositoryLister](t, p)
	collect := func(a scm.ArchivedFilter) []string {
		var seen []scm.Repository
		if _, err := rl.ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, Archived: a, Match: func(r scm.Repository) bool {
			seen = append(seen, r)
			return false
		}}); err != nil {
			t.Fatalf("ListRepositories(%q): %v", a, err)
		}
		return names(seen)
	}
	excluded := collect(scm.ArchivedExclude)
	included := collect(scm.ArchivedInclude)
	for _, a := range d.ArchivedRepositories {
		if slices.Contains(excluded, a) {
			t.Errorf("archived repository %q returned with ArchivedExclude", a)
		}
		if !slices.Contains(included, a) {
			t.Errorf("archived repository %q missing with ArchivedInclude", a)
		}
	}
}

func checkRepoDefaultBranch(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	got, err := must[scm.RepositoryLister](t, p).ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, Archived: scm.ArchivedExclude})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	want := sorted(d.Repositories)
	if g := names(got); !slices.Equal(g, want) {
		t.Fatalf("default-branch listing: got %v, want %v (empty repositories must be omitted)", g, want)
	}
	for _, r := range got {
		if r.Branch == "" || r.SHA == "" || r.Branch != r.DefaultBranch {
			t.Errorf("repository %s: branch=%q sha=%q default=%q", r.Name, r.Branch, r.SHA, r.DefaultBranch)
		}
		if r.Organization == "" || r.URL == "" || r.FullName == "" || r.RepositoryID == nil {
			t.Errorf("repository %s incomplete: %+v", r.Name, r)
		}
		if r.Name == d.Repo.Name && r.SHA != d.Branches[d.DefaultBranch] {
			t.Errorf("repository %s default SHA = %q, want %q", r.Name, r.SHA, d.Branches[d.DefaultBranch])
		}
	}
}

func onlyRepo(name string) func(scm.Repository) bool {
	return func(r scm.Repository) bool { return r.Name == name }
}

func checkRepoAllBranches(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	got, err := must[scm.RepositoryLister](t, p).ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, AllBranches: true, Match: onlyRepo(d.Repo.Name)})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	gotBranches := map[string]string{}
	for _, r := range got {
		if r.Name != d.Repo.Name {
			t.Fatalf("Match pushdown ignored: got repository %q", r.Name)
		}
		gotBranches[r.Branch] = r.SHA
	}
	if !maps.Equal(gotBranches, d.Branches) {
		t.Fatalf("all branches: got %v, want %v", gotBranches, d.Branches)
	}
}

func checkRepoMatch(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	got, err := must[scm.RepositoryLister](t, p).ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, Match: onlyRepo(d.Repo.Name)})
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(got) != 1 || got[0].Name != d.Repo.Name {
		t.Fatalf("Match pushdown: got %v", names(got))
	}
}

func checkRepoCloneProtocol(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	rl := must[scm.RepositoryLister](t, p)
	for _, proto := range []scm.CloneProtocol{scm.CloneProtocolDefault, scm.CloneProtocolSSH, scm.CloneProtocolHTTPS} {
		got, err := rl.ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, CloneProtocol: proto, Match: onlyRepo(d.Repo.Name)})
		if err != nil || len(got) != 1 {
			t.Fatalf("ListRepositories(%q): %d repos, err %v", proto, len(got), err)
		}
		want := got[0].SSHURL
		if proto == scm.CloneProtocolHTTPS {
			want = got[0].HTTPSURL
		}
		if want == "" || got[0].URL != want {
			t.Errorf("clone protocol %q: URL=%q want %q", proto, got[0].URL, want)
		}
	}
	_, err := rl.ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization, CloneProtocol: "gopher"})
	wantKind(t, err, scm.KindInvalid)
}

func checkRepoNotFound(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	_, err := must[scm.RepositoryLister](t, p).ListRepositories(ctx, scm.RepoQuery{Organization: f.Data().MissingRepo.Owner})
	wantKind(t, err, scm.KindNotFound)
	if !scm.IsNotFound(err) {
		t.Fatalf("errors.Is(err, ErrNotFound) = false for %v", err)
	}
}

func checkBranchesAll(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	got, err := must[scm.BranchLister](t, p).ListBranches(ctx, d.Repo, true)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	m := map[string]string{}
	for _, b := range got {
		m[b.Name] = b.SHA
	}
	if !maps.Equal(m, d.Branches) {
		t.Fatalf("branches: got %v, want %v", m, d.Branches)
	}
}

func checkBranchesDefault(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	got, err := must[scm.BranchLister](t, p).ListBranches(ctx, d.Repo, false)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(got) != 1 || got[0].Name != d.DefaultBranch || got[0].SHA != d.Branches[d.DefaultBranch] {
		t.Fatalf("default branch: got %+v", got)
	}
}

func checkBranchesNotFound(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	_, err := must[scm.BranchLister](t, p).ListBranches(ctx, f.Data().MissingRepo, true)
	wantKind(t, err, scm.KindNotFound)
}

func checkResolve(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	rr := must[scm.RevisionResolver](t, p)
	for b, sha := range d.Branches {
		got, err := rr.ResolveRevision(ctx, d.Repo, b)
		if err != nil {
			t.Fatalf("ResolveRevision(%s): %v", b, err)
		}
		if got != sha {
			t.Fatalf("ResolveRevision(%s) = %q, want %q", b, got, sha)
		}
	}
}

func checkResolveNotFound(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	rr := must[scm.RevisionResolver](t, p)
	_, err := rr.ResolveRevision(ctx, d.Repo, "no-such-branch-conformance")
	wantKind(t, err, scm.KindNotFound)
	_, err = rr.ResolveRevision(ctx, d.MissingRepo, d.DefaultBranch)
	wantKind(t, err, scm.KindNotFound)
}

func checkPaths(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	pc := must[scm.PathChecker](t, p)
	ok, err := pc.PathExists(ctx, d.Repo, d.DefaultBranch, d.ExistingPath)
	if err != nil || !ok {
		t.Fatalf("PathExists(%q) = %v, %v; want true", d.ExistingPath, ok, err)
	}
	ok, err = pc.PathExists(ctx, d.Repo, d.DefaultBranch, d.ExistingPath+"/")
	if err != nil || !ok {
		t.Fatalf("PathExists(%q/) = %v, %v; want true (trailing slash tolerated)", d.ExistingPath, ok, err)
	}
	ok, err = pc.PathExists(ctx, d.Repo, d.DefaultBranch, d.MissingPath)
	if err != nil || ok {
		t.Fatalf("PathExists(%q) = %v, %v; want false, nil", d.MissingPath, ok, err)
	}
}

func checkPathTraversal(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	_, err := must[scm.PathChecker](t, p).PathExists(ctx, d.Repo, d.DefaultBranch, "../../../user")
	wantKind(t, err, scm.KindInvalid)
	_, err = must[scm.PathChecker](t, p).PathExists(ctx, scm.RepoRef{Owner: "..", Name: "x"}, d.DefaultBranch, "a")
	wantKind(t, err, scm.KindInvalid)
}

func prNumbers(prs []scm.PullRequest) []int64 {
	out := make([]int64, 0, len(prs))
	for _, p := range prs {
		out = append(out, p.Number)
	}
	slices.Sort(out)
	return out
}

func checkPRs(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	got, err := must[scm.PullRequestLister](t, p).ListPullRequests(ctx, d.Repo, scm.PRQuery{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if g, w := prNumbers(got), prNumbers(d.PullRequests); !slices.Equal(g, w) {
		t.Fatalf("pull requests: got %v, want %v", g, w)
	}
	byNum := map[int64]scm.PullRequest{}
	for _, pr := range d.PullRequests {
		byNum[pr.Number] = pr
	}
	for _, pr := range got {
		w := byNum[pr.Number]
		if pr.Branch != w.Branch || pr.TargetBranch != w.TargetBranch || pr.HeadSHA != w.HeadSHA || pr.Author != w.Author || pr.Title != w.Title {
			t.Errorf("PR %d = %+v, want %+v", pr.Number, pr, w)
		}
		if !slices.Equal(sorted(pr.Labels), sorted(w.Labels)) {
			t.Errorf("PR %d labels = %v, want %v", pr.Number, pr.Labels, w.Labels)
		}
	}
}

func checkPRLabels(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	got, err := must[scm.PullRequestLister](t, p).ListPullRequests(ctx, d.Repo, scm.PRQuery{Labels: []string{d.PRLabel}})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	var want []scm.PullRequest
	for _, pr := range d.PullRequests {
		if slices.Contains(pr.Labels, d.PRLabel) {
			want = append(want, pr)
		}
	}
	if len(want) == 0 || len(want) == len(d.PullRequests) {
		t.Fatalf("fixture PRLabel %q must label a strict non-empty subset", d.PRLabel)
	}
	if g, w := prNumbers(got), prNumbers(want); !slices.Equal(g, w) {
		t.Fatalf("labeled pull requests: got %v, want %v", g, w)
	}
	got, err = must[scm.PullRequestLister](t, p).ListPullRequests(ctx, d.Repo, scm.PRQuery{Labels: []string{d.PRLabel, "\x00no-such-label"}})
	if err != nil || len(got) != 0 {
		t.Fatalf("labels must all be present: got %v, err %v", prNumbers(got), err)
	}
}

func checkPRNotFound(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	_, err := must[scm.PullRequestLister](t, p).ListPullRequests(ctx, f.Data().MissingRepo, scm.PRQuery{})
	wantKind(t, err, scm.KindNotFound)
}

var conformanceSecret = []byte("conformance-secret")

func checkWebhookValid(t *testing.T, _ context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	wv := must[scm.WebhookVerifier](t, p)
	h, body := f.WebhookDelivery(t, scm.EventPush, "delivery-valid-1", conformanceSecret)
	if !wv.MatchesWebhook(h) {
		t.Fatal("MatchesWebhook = false for the provider's own delivery")
	}
	if wv.MatchesWebhook(http.Header{"Content-Type": {"application/json"}}) {
		t.Fatal("MatchesWebhook = true for a header set without provider headers")
	}
	ev, err := wv.VerifyAndParse(h, body, conformanceSecret)
	if err != nil {
		t.Fatalf("VerifyAndParse: %v", err)
	}
	if !ev.Verified || ev.Type != scm.EventPush || ev.DeliveryID != "delivery-valid-1" || ev.Provider != p.Info().Type {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Repository == nil || !equalFoldRef(ev.Repository.RepoRef, d.Repo) {
		t.Fatalf("event repository = %+v, want %v", ev.Repository, d.Repo)
	}
	if ev.Revision != d.DefaultBranch || !ev.TouchedDefaultBranch || len(ev.ChangedFiles) == 0 || ev.After == "" {
		t.Fatalf("push fields: revision=%q touched=%v files=%v after=%q", ev.Revision, ev.TouchedDefaultBranch, ev.ChangedFiles, ev.After)
	}
}

func equalFoldRef(a, b scm.RepoRef) bool {
	return strings.EqualFold(a.Owner, b.Owner) && strings.EqualFold(a.Name, b.Name)
}

func wantSignatureError(t *testing.T, err error, sentinel error) {
	t.Helper()
	wantKind(t, err, scm.KindUnauthorized)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected %v, got %v", sentinel, err)
	}
}

func checkWebhookTampered(t *testing.T, _ context.Context, p scm.Provider, f Fixture) {
	wv := must[scm.WebhookVerifier](t, p)
	h, body := f.WebhookDelivery(t, scm.EventPush, "delivery-tampered", conformanceSecret)
	for _, i := range []int{0, len(body) / 2, len(body) - 1} {
		tampered := slices.Clone(body)
		tampered[i] ^= 0x01
		_, err := wv.VerifyAndParse(h, tampered, conformanceSecret)
		wantSignatureError(t, err, scm.ErrSignatureMismatch)
	}
	_, err := wv.VerifyAndParse(h, append(slices.Clone(body), ' '), conformanceSecret)
	wantSignatureError(t, err, scm.ErrSignatureMismatch)
}

func checkWebhookWrongSecret(t *testing.T, _ context.Context, p scm.Provider, f Fixture) {
	wv := must[scm.WebhookVerifier](t, p)
	h, body := f.WebhookDelivery(t, scm.EventPush, "delivery-wrong-secret", conformanceSecret)
	_, err := wv.VerifyAndParse(h, body, []byte("another-secret"))
	wantSignatureError(t, err, scm.ErrSignatureMismatch)
}

func checkWebhookMissingSig(t *testing.T, _ context.Context, p scm.Provider, f Fixture) {
	wv := must[scm.WebhookVerifier](t, p)
	h, body := f.WebhookDelivery(t, scm.EventPush, "delivery-missing", conformanceSecret)
	for _, k := range f.SignatureHeaders() {
		h.Del(k)
	}
	_, err := wv.VerifyAndParse(h, body, conformanceSecret)
	wantSignatureError(t, err, scm.ErrSignatureMissing)
	h2, body2 := f.WebhookDelivery(t, scm.EventPush, "delivery-unsigned", nil)
	_, err = wv.VerifyAndParse(h2, body2, conformanceSecret)
	wantSignatureError(t, err, scm.ErrSignatureMissing)
}

func checkWebhookEmptySecret(t *testing.T, _ context.Context, p scm.Provider, f Fixture) {
	wv := must[scm.WebhookVerifier](t, p)
	h, body := f.WebhookDelivery(t, scm.EventPush, "delivery-empty-secret", nil)
	ev, err := wv.VerifyAndParse(h, body, nil)
	if err != nil {
		t.Fatalf("unsigned delivery with empty secret: %v", err)
	}
	if ev.Verified {
		t.Fatal("event without configured secret must report Verified=false")
	}
	h, body = f.WebhookDelivery(t, scm.EventPush, "delivery-empty-secret-signed", conformanceSecret)
	ev, err = wv.VerifyAndParse(h, body, nil)
	if err != nil || ev.Verified {
		t.Fatalf("signed delivery with empty secret: verified=%v err=%v; want unverified event", ev != nil && ev.Verified, err)
	}
}

func checkWebhookDedup(t *testing.T, _ context.Context, p scm.Provider, f Fixture) {
	wv := must[scm.WebhookVerifier](t, p)
	cache := scm.NewDeliveryCache(16, time.Hour)
	h, body := f.WebhookDelivery(t, scm.EventPush, "delivery-dup", conformanceSecret)
	for i, wantDup := range []bool{false, true, true} {
		ev, err := wv.VerifyAndParse(h.Clone(), slices.Clone(body), conformanceSecret)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if got := cache.Seen(scm.DeliveryKey(ev.Provider, ev.DeliveryID)); got != wantDup {
			t.Fatalf("attempt %d: duplicate=%v want %v (delivery %q)", i, got, wantDup, ev.DeliveryID)
		}
	}
	h2, body2 := f.WebhookDelivery(t, scm.EventPush, "delivery-dup-2", conformanceSecret)
	ev, err := wv.VerifyAndParse(h2, body2, conformanceSecret)
	if err != nil {
		t.Fatal(err)
	}
	if cache.Seen(scm.DeliveryKey(ev.Provider, ev.DeliveryID)) {
		t.Fatal("a different delivery ID must not be treated as a duplicate")
	}
}

func checkWebhookPR(t *testing.T, _ context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	wv := must[scm.WebhookVerifier](t, p)
	h, body := f.WebhookDelivery(t, scm.EventPullRequest, "delivery-pr", conformanceSecret)
	ev, err := wv.VerifyAndParse(h, body, conformanceSecret)
	if err != nil {
		t.Fatalf("VerifyAndParse: %v", err)
	}
	if ev.Type != scm.EventPullRequest || ev.Action != scm.PRActionOpened || ev.PullRequest == nil {
		t.Fatalf("pull request event = %+v", ev)
	}
	if ev.PullRequest.Number == 0 || ev.PullRequest.Branch == "" || ev.PullRequest.HeadSHA == "" {
		t.Fatalf("pull request = %+v", ev.PullRequest)
	}
	if ev.Repository == nil || !equalFoldRef(ev.Repository.RepoRef, d.Repo) {
		t.Fatalf("event repository = %+v", ev.Repository)
	}
}

func checkStatus(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	st := scm.CommitStatus{State: scm.CommitStateSuccess, Context: "patrol/conformance", Description: "ok", TargetURL: "https://patrol.example/app"}
	if err := must[scm.CommitStatusReporter](t, p).SetCommitStatus(ctx, d.Repo, d.StatusSHA, st); err != nil {
		t.Fatalf("SetCommitStatus: %v", err)
	}
	got := f.CommitStatuses(d.Repo, d.StatusSHA)
	if len(got) != 1 || got[0] != st {
		t.Fatalf("recorded statuses = %+v, want [%+v]", got, st)
	}
}

func checkStatusInvalid(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	err := must[scm.CommitStatusReporter](t, p).SetCommitStatus(ctx, d.Repo, d.StatusSHA, scm.CommitStatus{State: "bogus", Context: "x"})
	wantKind(t, err, scm.KindInvalid)
	if n := len(f.CommitStatuses(d.Repo, d.StatusSHA)); n != 0 {
		t.Fatalf("invalid status reached the backend (%d recorded)", n)
	}
}

func checkCheckRuns(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	cr := must[scm.CheckRunReporter](t, p)
	run := scm.CheckRun{Name: "patrol/conformance", ExternalID: "app-1", Status: scm.CheckStatusInProgress, Title: "Syncing", Summary: "in progress"}
	id1, err := cr.UpsertCheckRun(ctx, d.Repo, d.StatusSHA, run)
	if err != nil {
		t.Fatalf("UpsertCheckRun create: %v", err)
	}
	run.Status, run.Conclusion, run.Summary = scm.CheckStatusCompleted, scm.CheckConclusionSuccess, "done"
	id2, err := cr.UpsertCheckRun(ctx, d.Repo, d.StatusSHA, run)
	if err != nil {
		t.Fatalf("UpsertCheckRun update: %v", err)
	}
	if id1 == 0 || id1 != id2 {
		t.Fatalf("upsert by external ID must reuse the check run: ids %d, %d", id1, id2)
	}
	other := run
	other.ExternalID = "app-2"
	id3, err := cr.UpsertCheckRun(ctx, d.Repo, d.StatusSHA, other)
	if err != nil || id3 == id1 {
		t.Fatalf("different external ID must create a new run: id=%d err=%v", id3, err)
	}
	runs := f.CheckRuns(d.Repo, d.StatusSHA)
	if len(runs) != 2 {
		t.Fatalf("recorded check runs = %+v, want 2", runs)
	}
	for _, r := range runs {
		if r.ID == id1 && (r.Status != scm.CheckStatusCompleted || r.Conclusion != scm.CheckConclusionSuccess) {
			t.Fatalf("check run %d not updated: %+v", id1, r)
		}
	}
	_, err = cr.UpsertCheckRun(ctx, d.Repo, d.StatusSHA, scm.CheckRun{Name: "x", ExternalID: "y", Status: scm.CheckStatusCompleted})
	wantKind(t, err, scm.KindInvalid)
}

func checkToken(t *testing.T, ctx context.Context, p scm.Provider, _ Fixture) {
	tok, err := must[scm.CredentialSource](t, p).Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.Value == "" || tok.Username == "" {
		t.Fatalf("token incomplete: %v", tok)
	}
	if !tok.ExpiresAt.IsZero() && !tok.ExpiresAt.After(time.Now()) {
		t.Fatalf("token already expired: %v", tok)
	}
	if s := tok.String(); strings.Contains(s, tok.Value) {
		t.Fatal("Token.String leaks the token value")
	}
}

func checkInstallations(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	il := must[scm.InstallationLister](t, p)
	all, err := il.ListInstallations(ctx)
	if err != nil || len(all) == 0 {
		t.Fatalf("ListInstallations = %v, %v", all, err)
	}
	inst, err := il.FindInstallation(ctx, d.InstallationOwner)
	if err != nil || inst.ID == 0 {
		t.Fatalf("FindInstallation(%q) = %+v, %v", d.InstallationOwner, inst, err)
	}
	_, err = il.FindInstallation(ctx, d.MissingRepo.Owner)
	wantKind(t, err, scm.KindNotFound)
}

func checkRateLimit(t *testing.T, ctx context.Context, p scm.Provider, f Fixture) {
	d := f.Data()
	info := p.Info()
	const reset = time.Hour
	f.InjectRateLimit(reset)
	var err error
	switch {
	case info.Has(scm.CapabilityBranches):
		_, err = must[scm.BranchLister](t, p).ListBranches(ctx, d.Repo, true)
	case info.Has(scm.CapabilityRepositories):
		_, err = must[scm.RepositoryLister](t, p).ListRepositories(ctx, scm.RepoQuery{Organization: d.Organization})
	case info.Has(scm.CapabilityPullRequests):
		_, err = must[scm.PullRequestLister](t, p).ListPullRequests(ctx, d.Repo, scm.PRQuery{})
	case info.Has(scm.CapabilityRevisions):
		_, err = must[scm.RevisionResolver](t, p).ResolveRevision(ctx, d.Repo, d.DefaultBranch)
	default:
		t.Skip("no read capability to probe")
	}
	wantKind(t, err, scm.KindRateLimited)
	if !scm.IsRateLimited(err) || !scm.IsRetryable(err) {
		t.Fatalf("rate-limit helpers disagree for %v", err)
	}
	ra, ok := scm.RetryAfter(err)
	if !ok || ra <= 0 || ra > reset+time.Minute {
		t.Fatalf("RetryAfter = %v (ok=%v), want within (0, %v]", ra, ok, reset)
	}
}
