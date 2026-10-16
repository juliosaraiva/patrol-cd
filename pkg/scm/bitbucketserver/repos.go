package bitbucketserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// paged is Bitbucket Server's paged response envelope.
type paged[T any] struct {
	Size          int  `json:"size"`
	Limit         int  `json:"limit"`
	IsLastPage    bool `json:"isLastPage"`
	Start         int  `json:"start"`
	NextPageStart *int `json:"nextPageStart"`
	Values        []T  `json:"values"`
}

type bsLink struct {
	Href string `json:"href"`
	Name string `json:"name"`
}

type bsRepo struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
	Public   bool   `json:"public"`
	Project  struct {
		Key string `json:"key"`
	} `json:"project"`
	Origin *struct {
		ID int64 `json:"id"`
	} `json:"origin"`
	Links struct {
		Clone []bsLink `json:"clone"`
		Self  []bsLink `json:"self"`
	} `json:"links"`
}

type bsBranch struct {
	ID           string `json:"id"`
	DisplayID    string `json:"displayId"`
	LatestCommit string `json:"latestCommit"`
	IsDefault    bool   `json:"isDefault"`
}

func (b bsBranch) toBranch() scm.Branch { return scm.Branch{Name: b.DisplayID, SHA: b.LatestCommit} }

func cloneLink(links []bsLink, name string) string {
	for _, l := range links {
		if l.Name == name {
			return l.Href
		}
	}
	return ""
}

// validateProjectKey validates a Bitbucket Server project key taken from untrusted
// input. Regular keys use the scm.ValidateSegment character set. Personal projects are
// addressed as "~" followed by the user slug (REST API: /projects/~{userSlug}/repos),
// so a single leading "~" is accepted before an otherwise safe segment. url.PathEscape
// leaves "~" (an unreserved character) as is.
func validateProjectKey(key string) error {
	rest, _ := strings.CutPrefix(key, "~")
	if scm.ValidateSegment("project", rest) != nil {
		return &scm.Error{Kind: scm.KindInvalid, Op: "Validate", Err: fmt.Errorf("invalid project key %q", key)}
	}
	return nil
}

// repoPath returns the escaped "projects/{key}/repos/{slug}" path after validating ref
// (Owner is the project key, which may name a personal project, Name the slug).
func repoPath(op string, ref scm.RepoRef) (string, error) {
	if err := validateProjectKey(ref.Owner); err != nil {
		return "", invalidErr(op, err)
	}
	if err := scm.ValidateSegment("repository", ref.Name); err != nil {
		return "", invalidErr(op, err)
	}
	return "projects/" + url.PathEscape(ref.Owner) + "/repos/" + url.PathEscape(ref.Name), nil
}

// collect follows start/nextPageStart pagination (limit 100) with the configured page
// cap. On error no partial result is returned.
func collect[T any](ctx context.Context, c *client, op, path string, q url.Values) ([]T, error) {
	out, err := scm.CollectPages(ctx, c.maxPages, func(ctx context.Context, start int) ([]T, int, error) {
		qq := cloneValues(q)
		qq.Set("limit", strconv.Itoa(pageLimit))
		if start > 0 {
			qq.Set("start", strconv.Itoa(start))
		}
		var pg paged[T]
		if _, err := c.get(ctx, op, path, qq, &pg); err != nil {
			return nil, 0, err
		}
		if pg.IsLastPage {
			return pg.Values, 0, nil
		}
		next := start + len(pg.Values)
		if pg.NextPageStart != nil {
			next = *pg.NextPageStart
		}
		if len(pg.Values) == 0 && pg.NextPageStart == nil {
			next = 0 // no progress possible
		}
		return pg.Values, next, nil
	})
	if err != nil {
		return nil, fillOp(op, err)
	}
	return out, nil
}

// defaultBranch returns the default branch of a repository, or nil when it has none:
// GET .../branches/default answers 404 when the default branch is unset or does not
// exist, and an empty body (204) for empty repositories.
func (p *Provider) defaultBranch(ctx context.Context, op, repoPath string) (*bsBranch, error) {
	var b bsBranch
	resp, err := p.c.get(ctx, op, repoPath+"/branches/default", nil, &b)
	if hasStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent || b.DisplayID == "" {
		return nil, nil
	}
	return &b, nil
}

// ListRepositories implements scm.RepositoryLister with GET
// /projects/{key}/repos (every page). Organization is the project key; personal
// projects are addressed as "~" + user slug, as in the REST API.
//
// As upstream, the default branch of every repository is read before filtering
// (Bitbucket Server does not return it with the repository) and repositories without
// one are skipped; Labels are empty; the https clone protocol selects the "http" clone
// link. Name is the repository slug (upstream reports the display name, which only
// differs for names with upper-case letters or spaces). ArchivedExclude drops
// repositories flagged archived (Bitbucket 8.0+).
func (p *Provider) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	const op = "ListRepositories"
	linkName := ""
	switch q.CloneProtocol {
	case scm.CloneProtocolDefault, scm.CloneProtocolSSH:
		linkName = "ssh"
	case scm.CloneProtocolHTTPS:
		linkName = "http"
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown clone protocol for Bitbucket Server %v", q.CloneProtocol))
	}
	project := strings.TrimSpace(q.Organization)
	if err := validateProjectKey(project); err != nil {
		return nil, invalidErr(op, err)
	}
	repos, err := collect[bsRepo](ctx, p.c, op, "projects/"+url.PathEscape(project)+"/repos", nil)
	if err != nil {
		return nil, err
	}
	var cands []scm.Repository
	for _, r := range repos {
		if q.Archived == scm.ArchivedExclude && r.Archived {
			continue
		}
		if len(q.Topics) > 0 {
			continue // Bitbucket Server repositories have no topics
		}
		if validateProjectKey(r.Project.Key) != nil || scm.ValidateSegment("repository", r.Slug) != nil {
			p.logger.Warn("skipping repository with unexpected key or slug", "provider", Type, "project", r.Project.Key, "slug", r.Slug)
			continue
		}
		var id any
		if r.ID != 0 {
			id = r.ID
		}
		cands = append(cands, scm.Repository{
			Organization: r.Project.Key,
			Name:         r.Slug,
			FullName:     r.Project.Key + "/" + r.Slug,
			URL:          cloneLink(r.Links.Clone, linkName),
			HTTPSURL:     cloneLink(r.Links.Clone, "http"),
			SSHURL:       cloneLink(r.Links.Clone, "ssh"),
			WebURL:       cloneLink(r.Links.Self, ""),
			Labels:       []string{},
			RepositoryID: id,
			Archived:     r.Archived,
			Private:      !r.Public,
			Fork:         r.Origin != nil,
		})
	}

	// Default branches (one request per repository, bounded parallelism).
	heads := make([]*bsBranch, len(cands))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, r := range cands {
		g.Go(func() error {
			b, err := p.defaultBranch(gctx, op, "projects/"+url.PathEscape(r.Organization)+"/repos/"+url.PathEscape(r.Name))
			if err != nil {
				return fmt.Errorf("error getting default branch for %s: %w", r.FullName, err)
			}
			heads[i] = b
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	type kept struct {
		repo scm.Repository
		head bsBranch
	}
	var keep []kept
	for i, r := range cands {
		if heads[i] == nil {
			p.logger.Debug("repository has no default branch, skipping", "provider", Type, "repository", r.FullName)
			continue
		}
		r.Branch, r.DefaultBranch = heads[i].DisplayID, heads[i].DisplayID
		if q.Match != nil && !q.Match(r) {
			continue
		}
		keep = append(keep, kept{repo: r, head: *heads[i]})
	}
	if !q.AllBranches {
		out := make([]scm.Repository, 0, len(keep))
		for _, k := range keep {
			e := k.repo
			e.SHA = k.head.LatestCommit
			out = append(out, e)
		}
		return out, nil
	}
	results := make([][]scm.Repository, len(keep))
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, k := range keep {
		g.Go(func() error {
			bs, err := collect[bsBranch](gctx, p.c, op, "projects/"+url.PathEscape(k.repo.Organization)+"/repos/"+url.PathEscape(k.repo.Name)+"/branches", nil)
			if err != nil {
				return fmt.Errorf("error listing branches for %s: %w", k.repo.FullName, err)
			}
			for _, b := range bs {
				e := k.repo
				e.Branch, e.SHA = b.DisplayID, b.LatestCommit
				results[i] = append(results[i], e)
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

// ListBranches implements scm.BranchLister. A missing repository fails with
// KindNotFound; a repository without a default branch yields an empty slice.
func (p *Provider) ListBranches(ctx context.Context, ref scm.RepoRef, all bool) ([]scm.Branch, error) {
	const op = "ListBranches"
	rp, err := repoPath(op, ref)
	if err != nil {
		return nil, err
	}
	if all {
		bs, err := collect[bsBranch](ctx, p.c, op, rp+"/branches", nil)
		if err != nil {
			return nil, err
		}
		out := make([]scm.Branch, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.toBranch())
		}
		return out, nil
	}
	// branches/default answers 404 for missing repositories too; check existence first.
	if _, err := p.c.get(ctx, op, rp, nil, nil); err != nil {
		return nil, err
	}
	b, err := p.defaultBranch(ctx, op, rp)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return []scm.Branch{}, nil
	}
	return []scm.Branch{b.toBranch()}, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ResolveRevision implements scm.RevisionResolver with GET
// .../commits?until=<ref>&limit=1 (branch, tag, full ref or SHA). Unknown refs and
// repositories map to KindNotFound.
func (p *Provider) ResolveRevision(ctx context.Context, ref scm.RepoRef, rev string) (string, error) {
	const op = "ResolveRevision"
	rp, err := repoPath(op, ref)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(rev) == "" {
		return "", invalidErr(op, fmt.Errorf("revision must not be empty"))
	}
	if err := checkNoDotDot(rev); err != nil {
		return "", invalidErr(op, err)
	}
	var pg paged[struct {
		ID string `json:"id"`
	}]
	if _, err := p.c.get(ctx, op, rp+"/commits", url.Values{"until": {rev}, "limit": {"1"}}, &pg); err != nil {
		return "", err
	}
	if len(pg.Values) == 0 {
		return "", &scm.Error{Kind: scm.KindNotFound, Provider: Type, Op: op, Err: fmt.Errorf("revision %q has no commits", rev)}
	}
	if !shaRE.MatchString(pg.Values[0].ID) {
		return "", &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: op, Err: fmt.Errorf("unexpected commit ID in response (%d bytes)", len(pg.Values[0].ID))}
	}
	return pg.Values[0].ID, nil
}

func checkNoDotDot(p string) error {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q must not contain '..' segments", p)
		}
	}
	return nil
}

func cleanRepoPath(path string) (string, error) {
	if err := checkNoDotDot(path); err != nil {
		return "", err
	}
	var segs []string
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || seg == "." {
			continue
		}
		segs = append(segs, url.PathEscape(seg))
	}
	return strings.Join(segs, "/"), nil
}

// PathExists implements scm.PathChecker with GET .../browse/{path}?at=<rev>&type=true
// (upstream: no need to page through directory contents). 404 means missing, including
// a missing repository or revision. An empty revision selects the default branch.
func (p *Provider) PathExists(ctx context.Context, ref scm.RepoRef, revision, path string) (bool, error) {
	const op = "PathExists"
	rp, err := repoPath(op, ref)
	if err != nil {
		return false, err
	}
	clean, err := cleanRepoPath(path)
	if err != nil {
		return false, invalidErr(op, err)
	}
	if err := checkNoDotDot(revision); err != nil {
		return false, invalidErr(op, err)
	}
	q := url.Values{"type": {"true"}, "limit": {"1"}}
	if revision != "" {
		q.Set("at", revision)
	}
	target := rp + "/browse"
	if clean != "" {
		target += "/" + clean
	}
	_, err = p.c.get(ctx, op, target, q, nil)
	if hasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
