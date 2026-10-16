package bitbucketcloud

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

// paged is Bitbucket Cloud's paged response envelope; Next is an absolute URL.
type paged[T any] struct {
	Values  []T    `json:"values"`
	Next    string `json:"next"`
	Page    int    `json:"page"`
	PageLen int    `json:"pagelen"`
}

type bcLink struct {
	Href string `json:"href"`
	Name string `json:"name"`
}

type bcRepo struct {
	UUID       string `json:"uuid"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	FullName   string `json:"full_name"`
	IsPrivate  bool   `json:"is_private"`
	MainBranch *struct {
		Name string `json:"name"`
	} `json:"mainbranch"`
	Parent *struct {
		FullName string `json:"full_name"`
	} `json:"parent"`
	Links struct {
		Clone []bcLink `json:"clone"`
		HTML  bcLink   `json:"html"`
		Self  bcLink   `json:"self"`
	} `json:"links"`
}

func (r bcRepo) mainBranch() string {
	if r.MainBranch == nil {
		return ""
	}
	return r.MainBranch.Name
}

type bcRef struct {
	Name   string `json:"name"`
	Target struct {
		Hash string `json:"hash"`
	} `json:"target"`
}

func (b bcRef) toBranch() scm.Branch { return scm.Branch{Name: b.Name, SHA: b.Target.Hash} }

func cloneLink(links []bcLink, name string) string {
	for _, l := range links {
		if l.Name == name {
			return l.Href
		}
	}
	return ""
}

func repoPath(op string, ref scm.RepoRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", invalidErr(op, err)
	}
	return "repositories/" + url.PathEscape(ref.Owner) + "/" + url.PathEscape(ref.Name), nil
}

// escapeSegments escapes each '/'-separated segment and keeps the separators.
func escapeSegments(s string) string {
	segs := strings.Split(s, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// collect follows the "next" links of paged responses (which must stay on the API
// host) with the configured page cap. On error no partial result is returned.
func collect[T any](ctx context.Context, c *client, op, first string) ([]T, error) {
	out, err := scm.CollectPages(ctx, c.maxPages, func(ctx context.Context, next string) ([]T, string, error) {
		u := next
		if u == "" {
			u = first
		}
		var pg paged[T]
		if _, err := c.do(ctx, op, http.MethodGet, u, &pg); err != nil {
			return nil, "", err
		}
		return pg.Values, pg.Next, nil
	})
	if err != nil {
		return nil, fillOp(op, err)
	}
	return out, nil
}

// ListRepositories implements scm.RepositoryLister with GET
// /repositories/{workspace}?role=member (every page), as upstream. Organization is the
// workspace slug and is reported as the repository organization; Name is the slug;
// RepositoryID the UUID; Labels are empty; the clone URL is the clone link named after
// the protocol (ssh by default). Bitbucket Cloud has no topics (a Topics filter matches
// nothing) and no archived flag. Repositories without a main branch (empty) are omitted.
func (p *Provider) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	const op = "ListRepositories"
	proto := ""
	switch q.CloneProtocol {
	case scm.CloneProtocolDefault, scm.CloneProtocolSSH:
		proto = "ssh"
	case scm.CloneProtocolHTTPS:
		proto = "https"
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown clone protocol for Bitbucket cloud %v", q.CloneProtocol))
	}
	ws := strings.TrimSpace(q.Organization)
	if err := scm.ValidateSegment("workspace", ws); err != nil {
		return nil, invalidErr(op, err)
	}
	first := p.c.url("repositories/"+url.PathEscape(ws), url.Values{"role": {"member"}, "pagelen": {strconv.Itoa(repoPageLen)}})
	repos, err := collect[bcRepo](ctx, p.c, op, first)
	if err != nil {
		return nil, err
	}
	var kept []scm.Repository
	for _, r := range repos {
		if len(q.Topics) > 0 {
			continue
		}
		if scm.ValidateSegment("repository", r.Slug) != nil {
			p.logger.Warn("skipping repository with unexpected slug", "provider", Type, "repository", r.FullName)
			continue
		}
		u := cloneLink(r.Links.Clone, proto)
		if u == "" {
			return nil, &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: op,
				Err: fmt.Errorf("error fetching clone url for repo %s: unknown clone protocol for Bitbucket cloud %v", r.Slug, proto)}
		}
		repo := scm.Repository{
			Organization:  ws,
			Name:          r.Slug,
			FullName:      ws + "/" + r.Slug,
			URL:           u,
			HTTPSURL:      cloneLink(r.Links.Clone, "https"),
			SSHURL:        cloneLink(r.Links.Clone, "ssh"),
			WebURL:        r.Links.HTML.Href,
			Branch:        r.mainBranch(),
			DefaultBranch: r.mainBranch(),
			Labels:        []string{},
			RepositoryID:  r.UUID,
			Private:       r.IsPrivate,
			Fork:          r.Parent != nil,
		}
		if q.Match != nil && !q.Match(repo) {
			continue
		}
		kept = append(kept, repo)
	}

	results := make([][]scm.Repository, len(kept))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, r := range kept {
		g.Go(func() error {
			rp := "repositories/" + url.PathEscape(ws) + "/" + url.PathEscape(r.Name)
			if q.AllBranches {
				bs, err := collect[bcRef](gctx, p.c, op, p.c.url(rp+"/refs/branches", url.Values{"pagelen": {strconv.Itoa(repoPageLen)}}))
				if err != nil {
					return fmt.Errorf("error listing branches for %s: %w", r.FullName, err)
				}
				for _, b := range bs {
					if b.Target.Hash == "" {
						return &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: op, Err: fmt.Errorf("error getting SHA for branch for %s/%s", r.FullName, b.Name)}
					}
					e := r
					e.Branch, e.SHA = b.Name, b.Target.Hash
					results[i] = append(results[i], e)
				}
				return nil
			}
			if r.DefaultBranch == "" {
				return nil // empty repository
			}
			var b bcRef
			_, err := p.c.get(gctx, op, rp+"/refs/branches/"+escapeSegments(r.DefaultBranch), nil, &b)
			if hasStatus(err, http.StatusNotFound) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("error listing branches for %s: %w", r.FullName, err)
			}
			e := r
			e.SHA = b.Target.Hash
			results[i] = []scm.Repository{e}
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
// KindNotFound.
func (p *Provider) ListBranches(ctx context.Context, ref scm.RepoRef, all bool) ([]scm.Branch, error) {
	const op = "ListBranches"
	rp, err := repoPath(op, ref)
	if err != nil {
		return nil, err
	}
	if all {
		bs, err := collect[bcRef](ctx, p.c, op, p.c.url(rp+"/refs/branches", url.Values{"pagelen": {strconv.Itoa(repoPageLen)}}))
		if err != nil {
			return nil, err
		}
		out := make([]scm.Branch, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.toBranch())
		}
		return out, nil
	}
	main, err := p.mainBranch(ctx, op, rp)
	if err != nil {
		return nil, err
	}
	if main == "" {
		return []scm.Branch{}, nil
	}
	var b bcRef
	_, err = p.c.get(ctx, op, rp+"/refs/branches/"+escapeSegments(main), nil, &b)
	if hasStatus(err, http.StatusNotFound) {
		return []scm.Branch{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []scm.Branch{b.toBranch()}, nil
}

func (p *Provider) mainBranch(ctx context.Context, op, rp string) (string, error) {
	var r bcRepo
	if _, err := p.c.get(ctx, op, rp, nil, &r); err != nil {
		return "", err
	}
	return r.mainBranch(), nil
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ResolveRevision implements scm.RevisionResolver. Hex SHAs (7–40 characters) are
// resolved with GET .../commit/{sha}; other refs as a branch, then as a tag
// ("refs/heads/" and "refs/tags/" prefixes restrict the lookup). Unknown refs map to
// KindNotFound.
func (p *Provider) ResolveRevision(ctx context.Context, ref scm.RepoRef, rev string) (string, error) {
	const op = "ResolveRevision"
	rp, err := repoPath(op, ref)
	if err != nil {
		return "", err
	}
	return p.resolve(ctx, op, rp, rev)
}

func (p *Provider) resolve(ctx context.Context, op, rp, rev string) (string, error) {
	if strings.TrimSpace(rev) == "" {
		return "", invalidErr(op, fmt.Errorf("revision must not be empty"))
	}
	if err := checkNoDotDot(rev); err != nil {
		return "", invalidErr(op, err)
	}
	if hexRE.MatchString(rev) {
		var c struct {
			Hash string `json:"hash"`
		}
		_, err := p.c.get(ctx, op, rp+"/commit/"+rev, nil, &c)
		if err == nil && c.Hash != "" {
			return c.Hash, nil
		}
		if err != nil && !scm.IsNotFound(err) {
			return "", err
		}
	}
	kinds := []string{"branches", "tags"}
	name := rev
	switch {
	case strings.HasPrefix(rev, "refs/heads/"):
		name, kinds = strings.TrimPrefix(rev, "refs/heads/"), []string{"branches"}
	case strings.HasPrefix(rev, "refs/tags/"):
		name, kinds = strings.TrimPrefix(rev, "refs/tags/"), []string{"tags"}
	}
	if name == "" {
		return "", invalidErr(op, fmt.Errorf("revision %q has an empty name", rev))
	}
	for _, k := range kinds {
		var r bcRef
		_, err := p.c.get(ctx, op, rp+"/refs/"+k+"/"+escapeSegments(name), nil, &r)
		if err == nil && r.Target.Hash != "" {
			return r.Target.Hash, nil
		}
		if err != nil && !scm.IsNotFound(err) {
			return "", err
		}
	}
	return "", &scm.Error{Kind: scm.KindNotFound, Provider: Type, Op: op, StatusCode: http.StatusNotFound, Err: fmt.Errorf("revision %q not found", rev)}
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

// PathExists implements scm.PathChecker with GET .../src/{commit}/{path}?format=meta
// (200 → true, 404 → false, any other status → error), as upstream. The source API
// addresses commits, so a branch or tag revision is first resolved to its commit (an
// unknown revision or repository yields false); an empty revision selects the main
// branch.
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
	if revision == "" {
		revision, err = p.mainBranch(ctx, op, rp)
		if scm.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if revision == "" {
			return false, nil
		}
	}
	sha := revision
	if len(revision) != 40 || !hexRE.MatchString(revision) {
		sha, err = p.resolve(ctx, op, rp, revision)
		if scm.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	_, err = p.c.get(ctx, op, rp+"/src/"+sha+"/"+clean, url.Values{"format": {"meta"}}, nil)
	if hasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
