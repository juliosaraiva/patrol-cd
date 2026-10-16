// Package fake is the in-memory reference implementation of the scm contract. It
// implements every capability with deterministic data, paginates internally (so callers
// exercise page caps), and offers fault injection hooks (errors, rate limits, latency).
// It is intended for tests of scm consumers (ApplicationSet generators, webhook
// ingestion, notifications) and is the baseline the conformance suite is validated
// against.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Type is the provider type registered by Register.
const Type = "fake"

// Webhook headers used by the fake provider.
const (
	HeaderEvent     = "X-Fake-Event"
	HeaderDelivery  = "X-Fake-Delivery"
	HeaderSignature = "X-Fake-Signature"
)

// Operation names used for fault injection and call counting (the contract method names).
const (
	OpListRepositories  = "ListRepositories"
	OpListBranches      = "ListBranches"
	OpResolveRevision   = "ResolveRevision"
	OpPathExists        = "PathExists"
	OpListPullRequests  = "ListPullRequests"
	OpSetCommitStatus   = "SetCommitStatus"
	OpUpsertCheckRun    = "UpsertCheckRun"
	OpToken             = "Token"
	OpListInstallations = "ListInstallations"
	OpFindInstallation  = "FindInstallation"
)

// Options configures a fake provider.
type Options struct {
	// Name is reported in Info.
	Name string
	// Capabilities declared; nil declares every capability.
	Capabilities []scm.Capability
	// PageSize is the internal page size used by listings (default 2).
	PageSize int
	// MaxPages caps internal pagination (default scm.DefaultMaxPages).
	MaxPages int
	// Clock overrides time.Now.
	Clock func() time.Time
	// MaxStatusesPerContext, when > 0, makes SetCommitStatus fail with a KindInvalid
	// error wrapping scm.ErrStatusLimit once that many statuses exist for a (commit,
	// context), like GitHub's limit of 1000.
	MaxStatusesPerContext int
}

// Repo is a seeded repository.
type Repo struct {
	Owner         string
	Name          string
	ID            int64
	DefaultBranch string
	// Branches maps branch name → head SHA.
	Branches map[string]string
	// Tags maps tag name → SHA.
	Tags map[string]string
	// Files maps commit SHA → file paths present at that commit. Directories exist
	// implicitly when a file below them exists.
	Files        map[string][]string
	Topics       []string
	Archived     bool
	Private      bool
	Fork         bool
	PullRequests []scm.PullRequest
}

// RecordedCheckRun is a check run stored by UpsertCheckRun.
type RecordedCheckRun struct {
	ID int64
	scm.CheckRun
}

type fault struct {
	err       error
	remaining int // <= 0 means unlimited
}

// Provider is the fake provider. It is safe for concurrent use.
type Provider struct {
	mu            sync.Mutex
	opts          Options
	caps          []scm.Capability
	repos         map[string]*Repo
	installations []scm.Installation
	token         scm.Token
	statuses      map[string][]scm.CommitStatus
	checkRuns     map[string][]*RecordedCheckRun
	nextCheckID   int64
	faults        map[string]*fault // op → fault; "*" applies to every op
	latency       time.Duration
	hook          func(ctx context.Context, op string) error
	calls         map[string]int
}

var (
	_ scm.Provider             = (*Provider)(nil)
	_ scm.RepositoryLister     = (*Provider)(nil)
	_ scm.BranchLister         = (*Provider)(nil)
	_ scm.RevisionResolver     = (*Provider)(nil)
	_ scm.PathChecker          = (*Provider)(nil)
	_ scm.PullRequestLister    = (*Provider)(nil)
	_ scm.WebhookVerifier      = (*Provider)(nil)
	_ scm.CommitStatusReporter = (*Provider)(nil)
	_ scm.CheckRunReporter     = (*Provider)(nil)
	_ scm.CredentialSource     = (*Provider)(nil)
	_ scm.InstallationLister   = (*Provider)(nil)
)

// New returns an empty fake provider.
func New(opts Options) *Provider {
	if opts.PageSize <= 0 {
		opts.PageSize = 2
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = scm.DefaultMaxPages
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	caps := opts.Capabilities
	if caps == nil {
		caps = scm.AllCapabilities()
	}
	return &Provider{
		opts:      opts,
		caps:      slices.Clone(caps),
		repos:     map[string]*Repo{},
		statuses:  map[string][]scm.CommitStatus{},
		checkRuns: map[string][]*RecordedCheckRun{},
		faults:    map[string]*fault{},
		calls:     map[string]int{},
		token:     scm.Token{Value: "fake-token", Username: "x-access-token"},
	}
}

// Factory returns an scm.Factory that always yields p (Name is taken from the Config when
// set). Register it with scm.Registry.Register(fake.Type, p.Factory()).
func (p *Provider) Factory() scm.Factory {
	return func(_ context.Context, cfg scm.Config) (scm.Provider, error) {
		if cfg.Name != "" {
			p.mu.Lock()
			p.opts.Name = cfg.Name
			p.mu.Unlock()
		}
		return p, nil
	}
}

// Info implements scm.Provider.
func (p *Provider) Info() scm.Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return scm.Info{Type: Type, Name: p.opts.Name, ContractVersion: scm.ContractVersion, Capabilities: slices.Clone(p.caps)}
}

func key(owner, name string) string { return strings.ToLower(owner + "/" + name) }

// AddRepo seeds or replaces a repository (deep-copied).
func (p *Provider) AddRepo(r Repo) {
	c := r
	c.Branches = maps.Clone(r.Branches)
	c.Tags = maps.Clone(r.Tags)
	c.Files = map[string][]string{}
	for k, v := range r.Files {
		c.Files[k] = slices.Clone(v)
	}
	c.Topics = slices.Clone(r.Topics)
	c.PullRequests = clonePRs(r.PullRequests)
	if c.Branches == nil {
		c.Branches = map[string]string{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repos[key(r.Owner, r.Name)] = &c
}

// SetBranch creates or moves a branch.
func (p *Provider) SetBranch(repo scm.RepoRef, branch, sha string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.repos[key(repo.Owner, repo.Name)]
	if !ok {
		return notFound("SetBranch", "repository "+repo.FullName())
	}
	r.Branches[branch] = sha
	return nil
}

// AddPullRequest appends a pull request to a seeded repository.
func (p *Provider) AddPullRequest(repo scm.RepoRef, pr scm.PullRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.repos[key(repo.Owner, repo.Name)]
	if !ok {
		return notFound("AddPullRequest", "repository "+repo.FullName())
	}
	r.PullRequests = append(r.PullRequests, clonePRs([]scm.PullRequest{pr})...)
	return nil
}

// AddInstallation seeds an installation.
func (p *Provider) AddInstallation(i scm.Installation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.installations = append(p.installations, i)
}

// SetToken sets the token returned by Token.
func (p *Provider) SetToken(t scm.Token) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = t
}

// InjectError makes op ("*" for all ops) fail with err for the next times calls
// (times <= 0: until ClearFaults). Errors that are not *scm.Error are returned as-is.
func (p *Provider) InjectError(op string, err error, times int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults[op] = &fault{err: err, remaining: times}
}

// InjectRateLimit makes every op fail with a RateLimited error carrying retryAfter for
// the next times calls (times <= 0: until ClearFaults).
func (p *Provider) InjectRateLimit(retryAfter time.Duration, times int) {
	p.InjectError("*", &scm.Error{Kind: scm.KindRateLimited, Provider: Type, StatusCode: http.StatusTooManyRequests,
		RetryAfter: retryAfter, Err: fmt.Errorf("fake rate limit exceeded")}, times)
}

// SetStatusLimit changes Options.MaxStatusesPerContext (0 disables the limit).
func (p *Provider) SetStatusLimit(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts.MaxStatusesPerContext = n
}

// SetLatency delays every op by d (honoring context cancellation).
func (p *Provider) SetLatency(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.latency = d
}

// SetFaultHook installs a hook called before every op; a non-nil error fails the op.
func (p *Provider) SetFaultHook(h func(ctx context.Context, op string) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hook = h
}

// ClearFaults removes injected errors, latency and hooks.
func (p *Provider) ClearFaults() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults = map[string]*fault{}
	p.latency = 0
	p.hook = nil
}

// Calls returns how many times op was invoked (including failed calls).
func (p *Provider) Calls(op string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[op]
}

// CommitStatuses returns statuses recorded for repo@sha in call order.
func (p *Provider) CommitStatuses(repo scm.RepoRef, sha string) []scm.CommitStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.statuses[key(repo.Owner, repo.Name)+"@"+sha])
}

// CheckRuns returns check runs recorded for repo@sha.
func (p *Provider) CheckRuns(repo scm.RepoRef, sha string) []RecordedCheckRun {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []RecordedCheckRun
	for _, r := range p.checkRuns[key(repo.Owner, repo.Name)+"@"+sha] {
		out = append(out, *r)
	}
	return out
}

// enter applies call counting, faults, hooks and latency. It must be called without
// holding p.mu.
func (p *Provider) enter(ctx context.Context, op string) error {
	p.mu.Lock()
	p.calls[op]++
	var injected error
	for _, k := range []string{op, "*"} {
		if f, ok := p.faults[k]; ok {
			injected = f.err
			if f.remaining > 0 {
				f.remaining--
				if f.remaining == 0 {
					delete(p.faults, k)
				}
			}
			break
		}
	}
	latency, hook := p.latency, p.hook
	p.mu.Unlock()
	if latency > 0 {
		t := time.NewTimer(latency)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		if err := hook(ctx, op); err != nil {
			return err
		}
	}
	if injected != nil {
		if e, ok := injected.(*scm.Error); ok {
			c := *e
			c.Op = op
			return &c
		}
		return injected
	}
	return nil
}

func notFound(op, what string) error {
	return &scm.Error{Kind: scm.KindNotFound, Provider: Type, Op: op, StatusCode: http.StatusNotFound, Err: fmt.Errorf("%s not found", what)}
}

func invalid(op string, err error) error {
	return &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: err}
}

func (p *Provider) repo(op string, ref scm.RepoRef) (*Repo, error) {
	if err := ref.Validate(); err != nil {
		return nil, invalid(op, err)
	}
	r, ok := p.repos[key(ref.Owner, ref.Name)]
	if !ok {
		return nil, notFound(op, "repository "+ref.FullName())
	}
	return r, nil
}

// paginate exercises scm.CollectPages over an in-memory slice.
func paginate[T any](ctx context.Context, pageSize, maxPages int, all []T) ([]T, error) {
	return scm.CollectPages(ctx, maxPages, func(_ context.Context, page int) ([]T, int, error) {
		if page == 0 {
			page = 1
		}
		start := (page - 1) * pageSize
		if start >= len(all) {
			return nil, 0, nil
		}
		end := min(start+pageSize, len(all))
		next := 0
		if end < len(all) {
			next = page + 1
		}
		return slices.Clone(all[start:end]), next, nil
	})
}

// ListRepositories implements scm.RepositoryLister.
func (p *Provider) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	const op = OpListRepositories
	if err := p.enter(ctx, op); err != nil {
		return nil, err
	}
	switch q.CloneProtocol {
	case scm.CloneProtocolDefault, scm.CloneProtocolSSH, scm.CloneProtocolHTTPS:
	default:
		return nil, invalid(op, fmt.Errorf("unknown clone protocol %q", q.CloneProtocol))
	}
	if err := scm.ValidateSegment("organization", q.Organization); err != nil {
		return nil, invalid(op, err)
	}
	p.mu.Lock()
	var owned []Repo
	for _, r := range p.repos {
		if strings.EqualFold(r.Owner, q.Organization) {
			owned = append(owned, r.listingSnapshot())
		}
	}
	pageSize, maxPages := p.opts.PageSize, p.opts.MaxPages
	p.mu.Unlock()
	if len(owned) == 0 {
		return nil, notFound(op, "organization "+q.Organization)
	}
	slices.SortFunc(owned, func(a, b Repo) int { return strings.Compare(a.Name, b.Name) })
	page, err := paginate(ctx, pageSize, maxPages, owned)
	if err != nil {
		return nil, err
	}
	var out []scm.Repository
	for _, r := range page {
		if q.Archived == scm.ArchivedExclude && r.Archived {
			continue
		}
		if !hasAll(r.Topics, q.Topics) {
			continue
		}
		cand := toRepository(r, q.CloneProtocol)
		if q.Match != nil && !q.Match(cand) {
			continue
		}
		if q.AllBranches {
			names := slices.Sorted(maps.Keys(r.Branches))
			for _, b := range names {
				e := cand
				e.Branch, e.SHA = b, r.Branches[b]
				out = append(out, e)
			}
			continue
		}
		sha, ok := r.Branches[r.DefaultBranch]
		if !ok {
			continue // empty repository
		}
		cand.SHA = sha
		out = append(out, cand)
	}
	return out, nil
}

// listingSnapshot copies what ListRepositories reads after releasing p.mu. The caller
// must hold p.mu: seeding helpers (SetBranch, AddPullRequest) mutate the maps and
// slices of a stored *Repo in place, so a shallow struct copy would share them. Files
// and PullRequests are not used by listings and are left out.
func (r *Repo) listingSnapshot() Repo {
	c := *r
	c.Branches = maps.Clone(r.Branches)
	c.Topics = slices.Clone(r.Topics)
	c.Tags, c.Files, c.PullRequests = nil, nil, nil
	return c
}

func toRepository(r Repo, proto scm.CloneProtocol) scm.Repository {
	ssh := fmt.Sprintf("git@fake.example:%s/%s.git", r.Owner, r.Name)
	https := fmt.Sprintf("https://fake.example/%s/%s.git", r.Owner, r.Name)
	u := ssh
	if proto == scm.CloneProtocolHTTPS {
		u = https
	}
	return scm.Repository{
		Organization:  r.Owner,
		Name:          r.Name,
		FullName:      r.Owner + "/" + r.Name,
		URL:           u,
		HTTPSURL:      https,
		SSHURL:        ssh,
		WebURL:        fmt.Sprintf("https://fake.example/%s/%s", r.Owner, r.Name),
		Branch:        r.DefaultBranch,
		DefaultBranch: r.DefaultBranch,
		Labels:        slices.Clone(r.Topics),
		RepositoryID:  r.ID,
		Archived:      r.Archived,
		Private:       r.Private,
		Fork:          r.Fork,
	}
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// ListBranches implements scm.BranchLister.
func (p *Provider) ListBranches(ctx context.Context, ref scm.RepoRef, all bool) ([]scm.Branch, error) {
	const op = OpListBranches
	if err := p.enter(ctx, op); err != nil {
		return nil, err
	}
	p.mu.Lock()
	r, err := p.repo(op, ref)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	branches := maps.Clone(r.Branches)
	def := r.DefaultBranch
	pageSize, maxPages := p.opts.PageSize, p.opts.MaxPages
	p.mu.Unlock()
	if !all {
		sha, ok := branches[def]
		if !ok {
			return []scm.Branch{}, nil
		}
		return []scm.Branch{{Name: def, SHA: sha}}, nil
	}
	var list []scm.Branch
	for _, n := range slices.Sorted(maps.Keys(branches)) {
		list = append(list, scm.Branch{Name: n, SHA: branches[n]})
	}
	out, err := paginate(ctx, pageSize, maxPages, list)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []scm.Branch{}
	}
	return out, nil
}

func (r *Repo) resolve(rev string) (string, bool) {
	rev = strings.TrimPrefix(rev, "refs/heads/")
	if sha, ok := r.Branches[rev]; ok {
		return sha, true
	}
	if sha, ok := r.Tags[strings.TrimPrefix(rev, "refs/tags/")]; ok {
		return sha, true
	}
	if rev == "HEAD" {
		sha, ok := r.Branches[r.DefaultBranch]
		return sha, ok
	}
	if _, ok := r.Files[rev]; ok {
		return rev, true
	}
	for _, sha := range r.Branches {
		if sha == rev {
			return sha, true
		}
	}
	return "", false
}

// ResolveRevision implements scm.RevisionResolver.
func (p *Provider) ResolveRevision(ctx context.Context, ref scm.RepoRef, rev string) (string, error) {
	const op = OpResolveRevision
	if err := p.enter(ctx, op); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, err := p.repo(op, ref)
	if err != nil {
		return "", err
	}
	sha, ok := r.resolve(rev)
	if !ok {
		return "", notFound(op, "revision "+rev)
	}
	return sha, nil
}

// CleanPath normalizes a repository-relative path, rejecting ".." segments.
func CleanPath(p string) (string, error) {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q must not contain '..'", p)
		}
	}
	c := strings.Trim(path.Clean("/"+p), "/")
	return c, nil
}

// PathExists implements scm.PathChecker.
func (p *Provider) PathExists(ctx context.Context, ref scm.RepoRef, revision, filePath string) (bool, error) {
	const op = OpPathExists
	if err := p.enter(ctx, op); err != nil {
		return false, err
	}
	clean, err := CleanPath(filePath)
	if err != nil {
		return false, invalid(op, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, err := p.repo(op, ref)
	if err != nil {
		if scm.IsNotFound(err) {
			return false, nil // like GitHub's Contents API, a missing repo is a missing path
		}
		return false, err
	}
	sha, ok := r.resolve(revision)
	if !ok {
		return false, nil
	}
	if clean == "" {
		return true, nil
	}
	for _, f := range r.Files[sha] {
		f = strings.Trim(f, "/")
		if f == clean || strings.HasPrefix(f, clean+"/") {
			return true, nil
		}
	}
	return false, nil
}

// ListPullRequests implements scm.PullRequestLister.
func (p *Provider) ListPullRequests(ctx context.Context, ref scm.RepoRef, q scm.PRQuery) ([]scm.PullRequest, error) {
	const op = OpListPullRequests
	if err := p.enter(ctx, op); err != nil {
		return nil, err
	}
	p.mu.Lock()
	r, err := p.repo(op, ref)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	prs := clonePRs(r.PullRequests)
	pageSize, maxPages := p.opts.PageSize, p.opts.MaxPages
	p.mu.Unlock()
	state := q.State
	if state == "" {
		state = scm.PRStateOpen
	}
	all, err := paginate(ctx, pageSize, maxPages, prs)
	if err != nil {
		return nil, err
	}
	out := []scm.PullRequest{}
	for _, pr := range all {
		if !prStateMatches(pr, state) || !hasAll(pr.Labels, q.Labels) {
			continue
		}
		if q.TargetBranch != "" && pr.TargetBranch != q.TargetBranch {
			continue
		}
		out = append(out, pr)
	}
	return out, nil
}

func prStateMatches(pr scm.PullRequest, want scm.PRState) bool {
	st := pr.State
	if st == "" {
		st = string(scm.PRStateOpen)
	}
	switch want {
	case scm.PRStateAll:
		return true
	case scm.PRStateMerged:
		return pr.Merged
	case scm.PRStateClosed:
		return st == string(scm.PRStateClosed)
	default:
		return st == string(scm.PRStateOpen)
	}
}

func clonePRs(in []scm.PullRequest) []scm.PullRequest {
	out := make([]scm.PullRequest, len(in))
	for i, pr := range in {
		pr.Labels = slices.Clone(pr.Labels)
		out[i] = pr
	}
	return out
}

// SetCommitStatus implements scm.CommitStatusReporter.
func (p *Provider) SetCommitStatus(ctx context.Context, ref scm.RepoRef, sha string, st scm.CommitStatus) error {
	const op = OpSetCommitStatus
	if err := st.Validate(); err != nil {
		return err
	}
	if err := p.enter(ctx, op); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, err := p.repo(op, ref)
	if err != nil {
		return err
	}
	if _, ok := r.resolve(sha); !ok {
		return &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, StatusCode: http.StatusUnprocessableEntity, Err: fmt.Errorf("no commit found for SHA %s", sha)}
	}
	st.Description = scm.TruncateRunes(st.Description, 140)
	k := key(ref.Owner, ref.Name) + "@" + sha
	if limit := p.opts.MaxStatusesPerContext; limit > 0 {
		n := 0
		for _, existing := range p.statuses[k] {
			if existing.Context == st.Context {
				n++
			}
		}
		if n >= limit {
			return &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, StatusCode: http.StatusUnprocessableEntity,
				Err: fmt.Errorf("%w: %d statuses for context %q", scm.ErrStatusLimit, n, st.Context)}
		}
	}
	p.statuses[k] = append(p.statuses[k], st)
	return nil
}

// UpsertCheckRun implements scm.CheckRunReporter.
func (p *Provider) UpsertCheckRun(ctx context.Context, ref scm.RepoRef, sha string, run scm.CheckRun) (int64, error) {
	const op = OpUpsertCheckRun
	if err := run.Validate(); err != nil {
		return 0, err
	}
	if err := p.enter(ctx, op); err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, err := p.repo(op, ref)
	if err != nil {
		return 0, err
	}
	if _, ok := r.resolve(sha); !ok {
		return 0, &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, StatusCode: http.StatusUnprocessableEntity, Err: fmt.Errorf("no commit found for SHA %s", sha)}
	}
	k := key(ref.Owner, ref.Name) + "@" + sha
	for _, existing := range p.checkRuns[k] {
		if existing.Name == run.Name && existing.ExternalID == run.ExternalID {
			existing.CheckRun = run
			return existing.ID, nil
		}
	}
	p.nextCheckID++
	rec := &RecordedCheckRun{ID: p.nextCheckID, CheckRun: run}
	p.checkRuns[k] = append(p.checkRuns[k], rec)
	return rec.ID, nil
}

// Token implements scm.CredentialSource.
func (p *Provider) Token(ctx context.Context) (scm.Token, error) {
	if err := p.enter(ctx, OpToken); err != nil {
		return scm.Token{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.token, nil
}

// ListInstallations implements scm.InstallationLister.
func (p *Provider) ListInstallations(ctx context.Context) ([]scm.Installation, error) {
	if err := p.enter(ctx, OpListInstallations); err != nil {
		return nil, err
	}
	p.mu.Lock()
	all := slices.Clone(p.installations)
	pageSize, maxPages := p.opts.PageSize, p.opts.MaxPages
	p.mu.Unlock()
	out, err := paginate(ctx, pageSize, maxPages, all)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []scm.Installation{}
	}
	return out, nil
}

// FindInstallation implements scm.InstallationLister (case-insensitive owner match).
func (p *Provider) FindInstallation(ctx context.Context, owner string) (scm.Installation, error) {
	const op = OpFindInstallation
	if err := p.enter(ctx, op); err != nil {
		return scm.Installation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, i := range p.installations {
		if strings.EqualFold(i.Account, owner) {
			return i, nil
		}
	}
	return scm.Installation{}, notFound(op, "installation for "+owner)
}

// Delivery is the body of a fake webhook delivery.
type Delivery struct {
	Type           scm.EventType    `json:"type"`
	Action         string           `json:"action,omitempty"`
	Owner          string           `json:"owner,omitempty"`
	Name           string           `json:"name,omitempty"`
	DefaultBranch  string           `json:"defaultBranch,omitempty"`
	Ref            string           `json:"ref,omitempty"`
	Before         string           `json:"before,omitempty"`
	After          string           `json:"after,omitempty"`
	ChangedFiles   []string         `json:"changedFiles,omitempty"`
	PullRequest    *scm.PullRequest `json:"pullRequest,omitempty"`
	InstallationID int64            `json:"installationID,omitempty"`
	Account        string           `json:"account,omitempty"`
}

// SignedDelivery encodes d as a webhook delivery with the given delivery ID, signed with
// secret (unsigned when secret is empty).
func SignedDelivery(d Delivery, deliveryID string, secret []byte) (http.Header, []byte, error) {
	body, err := json.Marshal(d)
	if err != nil {
		return nil, nil, err
	}
	h := http.Header{}
	h.Set(HeaderEvent, string(d.Type))
	h.Set(HeaderDelivery, deliveryID)
	h.Set("Content-Type", "application/json")
	if len(secret) > 0 {
		h.Set(HeaderSignature, "sha256="+scm.SignHMACSHA256(secret, body))
	}
	return h, body, nil
}

// MatchesWebhook implements scm.WebhookVerifier.
func (p *Provider) MatchesWebhook(h http.Header) bool { return h.Get(HeaderEvent) != "" }

// VerifyAndParse implements scm.WebhookVerifier.
func (p *Provider) VerifyAndParse(h http.Header, body []byte, secret []byte) (*scm.Event, error) {
	const op = "VerifyAndParse"
	if int64(len(body)) > scm.MaxWebhookPayloadBytes {
		return nil, &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: scm.ErrPayloadTooLarge}
	}
	verified := false
	if len(secret) > 0 {
		if err := scm.VerifyHMACSHA256(h.Get(HeaderSignature), "sha256=", body, secret); err != nil {
			return nil, scm.WebhookSignatureError(Type, err)
		}
		verified = true
	}
	deliveryID := h.Get(HeaderDelivery)
	if err := scm.ValidateDeliveryID(deliveryID); err != nil {
		return nil, &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: err}
	}
	var d Delivery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: fmt.Errorf("%w: %v", scm.ErrMalformedPayload, err)}
	}
	if h.Get(HeaderEvent) != string(d.Type) {
		return nil, &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: fmt.Errorf("%w: event header %q does not match body type %q", scm.ErrMalformedPayload, h.Get(HeaderEvent), d.Type)}
	}
	ev := &scm.Event{Provider: Type, Type: d.Type, DeliveryID: deliveryID, Verified: verified, Action: d.Action}
	if d.Owner != "" && d.Name != "" {
		ev.Repository = &scm.EventRepo{
			RepoRef:       scm.RepoRef{Owner: d.Owner, Name: d.Name},
			HTMLURL:       fmt.Sprintf("https://fake.example/%s/%s", d.Owner, d.Name),
			CloneURL:      fmt.Sprintf("https://fake.example/%s/%s.git", d.Owner, d.Name),
			SSHURL:        fmt.Sprintf("git@fake.example:%s/%s.git", d.Owner, d.Name),
			DefaultBranch: d.DefaultBranch,
		}
	}
	if d.InstallationID != 0 {
		ev.Installation = &scm.InstallationRef{ID: d.InstallationID, Account: d.Account}
	}
	switch d.Type {
	case scm.EventPush:
		ev.Ref, ev.Before, ev.After = d.Ref, d.Before, d.After
		ev.Revision = scm.ParseRevision(d.Ref)
		ev.TouchedDefaultBranch = d.DefaultBranch != "" && ev.Revision == d.DefaultBranch
		ev.ChangedFiles, ev.ChangedFilesTruncated = scm.NormalizeChangedFiles(d.ChangedFiles)
	case scm.EventPullRequest:
		if d.PullRequest == nil {
			return nil, &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: fmt.Errorf("%w: pull_request without pullRequest", scm.ErrMalformedPayload)}
		}
		pr := clonePRs([]scm.PullRequest{*d.PullRequest})[0]
		ev.PullRequest = &pr
	case scm.EventPing, scm.EventInstallation, scm.EventRepository:
	default:
		return nil, &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: fmt.Errorf("%w: %q", scm.ErrUnsupportedEvent, d.Type)}
	}
	return ev, nil
}

// Register registers p under Type in r.
func Register(r *scm.Registry, p *Provider) error { return r.Register(Type, p.Factory()) }
