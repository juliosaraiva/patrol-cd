package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	gh "github.com/google/go-github/v88/github"
	"golang.org/x/sync/errgroup"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

func (p *Provider) invalid(op string, err error) error {
	return &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: op, Err: err}
}

func (p *Provider) validateRef(op string, ref scm.RepoRef) error {
	if err := ref.Validate(); err != nil {
		return p.invalid(op, err)
	}
	return nil
}

// collect follows Link-header pagination with the configured page cap.
func collect[T any](ctx context.Context, p *Provider, op string, fetch func(ctx context.Context, opts gh.ListOptions) ([]T, *gh.Response, error)) ([]T, error) {
	out, err := scm.CollectPages(ctx, p.maxPages, func(ctx context.Context, page int) ([]T, int, error) {
		items, resp, err := fetch(ctx, gh.ListOptions{Page: page, PerPage: perPage})
		if err != nil {
			return nil, 0, p.mapErr(op, resp, err)
		}
		return items, resp.NextPage, nil
	})
	if err != nil {
		var se *scm.Error
		if errors.As(err, &se) && se.Provider == "" {
			c := *se
			c.Provider, c.Op = Type, op
			return nil, &c
		}
		return nil, err
	}
	return out, nil
}

// ListRepositories implements scm.RepositoryLister. It lists GET /orgs/{org}/repos and
// falls back to GET /users/{user}/repos when the owner is not an organization.
func (p *Provider) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	const op = "ListRepositories"
	switch q.CloneProtocol {
	case scm.CloneProtocolDefault, scm.CloneProtocolSSH, scm.CloneProtocolHTTPS:
	default:
		return nil, p.invalid(op, fmt.Errorf("unknown clone protocol for GitHub %v", q.CloneProtocol))
	}
	if err := scm.ValidateSegment("organization", q.Organization); err != nil {
		return nil, p.invalid(op, err)
	}
	repos, err := collect(ctx, p, op, func(ctx context.Context, lo gh.ListOptions) ([]*gh.Repository, *gh.Response, error) {
		return p.client.Repositories.ListByOrg(ctx, q.Organization, &gh.RepositoryListByOrgOptions{ListOptions: lo})
	})
	if scm.IsNotFound(err) {
		repos, err = collect(ctx, p, op, func(ctx context.Context, lo gh.ListOptions) ([]*gh.Repository, *gh.Response, error) {
			return p.client.Repositories.ListByUser(ctx, q.Organization, &gh.RepositoryListByUserOptions{ListOptions: lo})
		})
	}
	if err != nil {
		return nil, err
	}

	// Repository-level filtering (sequential, so Match needs no synchronization).
	var kept []scm.Repository
	for _, r := range repos {
		if r == nil {
			continue
		}
		if q.Archived == scm.ArchivedExclude && r.GetArchived() {
			continue
		}
		if !hasAll(r.Topics, q.Topics) {
			continue
		}
		cand := toRepository(r, q.CloneProtocol)
		if q.Match != nil && !q.Match(cand) {
			continue
		}
		kept = append(kept, cand)
	}

	// Branch expansion with bounded parallelism, preserving order.
	results := make([][]scm.Repository, len(kept))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(branchConcurrency)
	for i, cand := range kept {
		g.Go(func() error {
			ref := cand.Ref()
			if err := p.validateRef(op, ref); err != nil {
				// Names returned by the API are trusted to be well-formed; skip anything
				// that could not be safely interpolated into a URL.
				p.logger.Warn("skipping repository with unexpected name", "provider", Type, "repository", cand.FullName)
				return nil
			}
			if q.AllBranches {
				branches, err := p.listAllBranches(gctx, op, ref)
				if err != nil {
					return fmt.Errorf("error listing branches for %s: %w", cand.FullName, err)
				}
				for _, b := range branches {
					e := cand
					e.Branch, e.SHA = b.Name, b.SHA
					results[i] = append(results[i], e)
				}
				return nil
			}
			if cand.DefaultBranch == "" {
				return nil // empty repository
			}
			b, found, err := p.getBranch(gctx, op, ref, cand.DefaultBranch)
			if err != nil {
				return fmt.Errorf("error getting default branch for %s: %w", cand.FullName, err)
			}
			if found {
				cand.SHA = b.SHA
				results[i] = []scm.Repository{cand}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := []scm.Repository{}
	for _, r := range results {
		out = append(out, r...)
	}
	return out, nil
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func toRepository(r *gh.Repository, proto scm.CloneProtocol) scm.Repository {
	owner := r.GetOwner().GetLogin()
	u := r.GetSSHURL()
	if proto == scm.CloneProtocolHTTPS {
		u = r.GetCloneURL()
	}
	var id any
	if r.ID != nil {
		id = r.GetID()
	}
	return scm.Repository{
		Organization:  owner,
		Name:          r.GetName(),
		FullName:      r.GetFullName(),
		URL:           u,
		HTTPSURL:      r.GetCloneURL(),
		SSHURL:        r.GetSSHURL(),
		WebURL:        r.GetHTMLURL(),
		Branch:        r.GetDefaultBranch(),
		DefaultBranch: r.GetDefaultBranch(),
		Labels:        slices.Clone(r.Topics),
		RepositoryID:  id,
		Archived:      r.GetArchived(),
		Private:       r.GetPrivate(),
		Fork:          r.GetFork(),
	}
}

func (p *Provider) listAllBranches(ctx context.Context, op string, ref scm.RepoRef) ([]scm.Branch, error) {
	bs, err := collect(ctx, p, op, func(ctx context.Context, lo gh.ListOptions) ([]*gh.Branch, *gh.Response, error) {
		return p.client.Repositories.ListBranches(ctx, ref.Owner, ref.Name, &gh.BranchListOptions{ListOptions: lo})
	})
	if err != nil {
		return nil, err
	}
	out := make([]scm.Branch, 0, len(bs))
	for _, b := range bs {
		out = append(out, scm.Branch{Name: b.GetName(), SHA: b.GetCommit().GetSHA(), Protected: b.GetProtected()})
	}
	return out, nil
}

// getBranch returns (branch, false, nil) when the branch does not exist.
func (p *Provider) getBranch(ctx context.Context, op string, ref scm.RepoRef, name string) (scm.Branch, bool, error) {
	b, resp, err := p.client.Repositories.GetBranch(ctx, ref.Owner, ref.Name, name, 0)
	if err != nil {
		if isStatus(resp, err, http.StatusNotFound) {
			return scm.Branch{}, false, nil
		}
		return scm.Branch{}, false, p.mapErr(op, resp, err)
	}
	return scm.Branch{Name: b.GetName(), SHA: b.GetCommit().GetSHA(), Protected: b.GetProtected()}, true, nil
}

// ListBranches implements scm.BranchLister. With all=false it reads the repository's
// default branch and returns it (an empty slice when the repository is empty).
func (p *Provider) ListBranches(ctx context.Context, ref scm.RepoRef, all bool) ([]scm.Branch, error) {
	const op = "ListBranches"
	if err := p.validateRef(op, ref); err != nil {
		return nil, err
	}
	if all {
		return p.listAllBranches(ctx, op, ref)
	}
	repo, resp, err := p.client.Repositories.Get(ctx, ref.Owner, ref.Name)
	if err != nil {
		return nil, p.mapErr(op, resp, err)
	}
	if repo.GetDefaultBranch() == "" {
		return []scm.Branch{}, nil
	}
	b, found, err := p.getBranch(ctx, op, ref, repo.GetDefaultBranch())
	if err != nil {
		return nil, err
	}
	if !found {
		return []scm.Branch{}, nil
	}
	return []scm.Branch{b}, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ResolveRevision implements scm.RevisionResolver via GET /repos/{o}/{r}/commits/{ref}
// with the SHA media type. Unknown refs (404/422) map to KindNotFound.
func (p *Provider) ResolveRevision(ctx context.Context, ref scm.RepoRef, rev string) (string, error) {
	const op = "ResolveRevision"
	if err := p.validateRef(op, ref); err != nil {
		return "", err
	}
	if strings.TrimSpace(rev) == "" {
		return "", p.invalid(op, fmt.Errorf("revision must not be empty"))
	}
	if err := checkNoDotDot(rev); err != nil {
		return "", p.invalid(op, err)
	}
	short := strings.TrimPrefix(strings.TrimPrefix(rev, "refs/heads/"), "refs/tags/")
	sha, resp, err := p.client.Repositories.GetCommitSHA1(ctx, ref.Owner, ref.Name, short, "")
	if err != nil {
		mapped := p.mapErr(op, resp, err)
		var se *scm.Error
		if isStatus(resp, err, http.StatusUnprocessableEntity) && errors.As(mapped, &se) {
			c := *se
			c.Kind = scm.KindNotFound // "No commit found for SHA: <ref>"
			return "", &c
		}
		return "", mapped
	}
	sha = strings.TrimSpace(sha)
	if !shaRE.MatchString(sha) {
		return "", &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: op, Err: fmt.Errorf("unexpected commit SHA response (%d bytes)", len(sha))}
	}
	return sha, nil
}

func checkNoDotDot(p string) error {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q must not contain '..' segments", p)
		}
	}
	return nil
}

// cleanRepoPath normalizes a repository-relative path: '..' segments are rejected, empty
// and '.' segments dropped, and surrounding slashes trimmed.
func cleanRepoPath(path string) (string, error) {
	if err := checkNoDotDot(path); err != nil {
		return "", err
	}
	var segs []string
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || seg == "." {
			continue
		}
		segs = append(segs, seg)
	}
	return strings.Join(segs, "/"), nil
}

// PathExists implements scm.PathChecker with a HEAD request on the Contents API at the
// given revision (falling back to GET when HEAD is not allowed). 404 means the path does
// not exist; as on GitHub, a missing repository or revision also yields false.
func (p *Provider) PathExists(ctx context.Context, ref scm.RepoRef, revision, path string) (bool, error) {
	const op = "PathExists"
	if err := p.validateRef(op, ref); err != nil {
		return false, err
	}
	clean, err := cleanRepoPath(path)
	if err != nil {
		return false, p.invalid(op, err)
	}
	if err := checkNoDotDot(revision); err != nil {
		return false, p.invalid(op, err)
	}
	segs := strings.Split(clean, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := fmt.Sprintf("repos/%s/%s/contents/%s", ref.Owner, ref.Name, strings.Join(segs, "/"))
	if revision != "" {
		u += "?" + url.Values{"ref": {revision}}.Encode()
	}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := p.client.NewRequest(ctx, method, u, nil)
		if err != nil {
			return false, p.mapErr(op, nil, err)
		}
		resp, err := p.client.Do(req, nil)
		if err == nil {
			return true, nil
		}
		if isStatus(resp, err, http.StatusNotFound) {
			return false, nil
		}
		if method == http.MethodHead && isStatus(resp, err, http.StatusMethodNotAllowed, http.StatusNotImplemented) {
			continue
		}
		return false, p.mapErr(op, resp, err)
	}
	return false, &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: op, Err: fmt.Errorf("contents API did not answer HEAD or GET")}
}
