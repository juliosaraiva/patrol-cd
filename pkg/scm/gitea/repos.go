package gitea

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

type gtUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

func (u gtUser) name() string {
	if u.Login != "" {
		return u.Login
	}
	return u.Username
}

type gtRepo struct {
	ID            int64    `json:"id"`
	Owner         gtUser   `json:"owner"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Private       bool     `json:"private"`
	Fork          bool     `json:"fork"`
	Empty         bool     `json:"empty"`
	Archived      bool     `json:"archived"`
	HTMLURL       string   `json:"html_url"`
	SSHURL        string   `json:"ssh_url"`
	CloneURL      string   `json:"clone_url"`
	DefaultBranch string   `json:"default_branch"`
	Topics        []string `json:"topics"`
}

type gtBranch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

func (b gtBranch) toBranch() scm.Branch {
	return scm.Branch{Name: b.Name, SHA: b.Commit.ID, Protected: b.Protected}
}

type gtLabel struct {
	Name string `json:"name"`
}

// repoPath returns the escaped "repos/{owner}/{repo}" API path after validating ref.
func repoPath(op string, ref scm.RepoRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", invalidErr(op, err)
	}
	return "repos/" + url.PathEscape(ref.Owner) + "/" + url.PathEscape(ref.Name), nil
}

// escapeSegments escapes every '/'-separated segment of s and keeps the separators
// (Gitea routes branch names and file paths as wildcards).
func escapeSegments(s string) string {
	segs := strings.Split(s, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// collect follows page/limit pagination (Link rel="next"; when a server sends no Link
// header a full page means another page) with the configured page cap. On error no
// partial result is returned.
func collect[T any](ctx context.Context, c *client, op, path string, q url.Values) ([]T, error) {
	out, err := scm.CollectPages(ctx, c.maxPages, func(ctx context.Context, page int) ([]T, int, error) {
		if page <= 0 {
			page = 1
		}
		qq := cloneValues(q)
		qq.Set("limit", strconv.Itoa(pageLimit))
		qq.Set("page", strconv.Itoa(page))
		var items []T
		resp, err := c.get(ctx, op, path, qq, &items)
		if err != nil {
			return nil, 0, err
		}
		next := nextPage(resp.Header)
		if next == 0 && resp.Header.Get("Link") == "" && len(items) >= pageLimit {
			next = page + 1
		}
		return items, next, nil
	})
	if err != nil {
		return nil, fillOp(op, err)
	}
	return out, nil
}

// ListRepositories implements scm.RepositoryLister with GET /orgs/{org}/repos, falling
// back to GET /users/{user}/repos for user owners (Patrol extension; upstream lists
// organizations only). Every page is read (upstream reads the first page only).
//
// As upstream, Labels are the repository's issue labels (GET /repos/{o}/{r}/labels, one
// extra request per repository, so labelMatch works), Organization is the configured
// owner, the https clone protocol selects the repository HTML URL (HTTPSURL is set to
// the same value) and ArchivedExclude drops archived repositories. Topics filter on the
// repository topics. Empty repositories are omitted; a non-empty repository whose
// default branch does not exist fails the listing with upstream's message.
func (p *Provider) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	const op = "ListRepositories"
	switch q.CloneProtocol {
	case scm.CloneProtocolDefault, scm.CloneProtocolSSH, scm.CloneProtocolHTTPS:
	default:
		// Upstream's message names GitHub; kept for parity.
		return nil, invalidErr(op, fmt.Errorf("unknown clone protocol for GitHub %v", q.CloneProtocol))
	}
	owner := strings.TrimSpace(q.Organization)
	if err := scm.ValidateSegment("organization", owner); err != nil {
		return nil, invalidErr(op, err)
	}
	repos, err := collect[gtRepo](ctx, p.c, op, "orgs/"+url.PathEscape(owner)+"/repos", nil)
	if scm.IsNotFound(err) {
		repos, err = collect[gtRepo](ctx, p.c, op, "users/"+url.PathEscape(owner)+"/repos", nil)
	}
	if err != nil {
		return nil, err
	}
	var cands []scm.Repository
	var empty []bool
	for _, r := range repos {
		if q.Archived == scm.ArchivedExclude && r.Archived {
			continue
		}
		if !hasAll(r.Topics, q.Topics) {
			continue
		}
		if err := scm.ValidateSegment("repository", r.Name); err != nil {
			p.logger.Warn("skipping repository with unexpected name", "provider", Type, "repository", r.FullName)
			continue
		}
		cands = append(cands, toRepository(owner, r, q.CloneProtocol))
		empty = append(empty, r.Empty)
	}

	// Repository labels, fetched with bounded parallelism (order preserved).
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i := range cands {
		g.Go(func() error {
			labels, err := collect[gtLabel](gctx, p.c, op, "repos/"+url.PathEscape(owner)+"/"+url.PathEscape(cands[i].Name)+"/labels", nil)
			if err != nil {
				return fmt.Errorf("error listing labels of %s: %w", cands[i].FullName, err)
			}
			cands[i].Labels = make([]string, 0, len(labels))
			for _, l := range labels {
				cands[i].Labels = append(cands[i].Labels, l.Name)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var kept []scm.Repository
	var keptEmpty []bool
	for i, r := range cands {
		if q.Match != nil && !q.Match(r) {
			continue
		}
		kept = append(kept, r)
		keptEmpty = append(keptEmpty, empty[i])
	}

	results := make([][]scm.Repository, len(kept))
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, r := range kept {
		g.Go(func() error {
			base := "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(r.Name)
			if q.AllBranches {
				branches, err := collect[gtBranch](gctx, p.c, op, base+"/branches", nil)
				if err != nil {
					return fmt.Errorf("error listing branches for %s: %w", r.FullName, err)
				}
				for _, b := range branches {
					e := r
					e.Branch, e.SHA = b.Name, b.Commit.ID
					results[i] = append(results[i], e)
				}
				return nil
			}
			if keptEmpty[i] || r.DefaultBranch == "" {
				return nil // empty repository
			}
			var b gtBranch
			_, err := p.c.get(gctx, op, base+"/branches/"+escapeSegments(r.DefaultBranch), nil, &b)
			if hasStatus(err, http.StatusNotFound) {
				return &scm.Error{Kind: scm.KindNotFound, Provider: Type, Op: op, StatusCode: http.StatusNotFound,
					Err: fmt.Errorf("got 404 while getting default branch %q for repo %q - check your repo config: %w", r.DefaultBranch, r.Name, err)}
			}
			if err != nil {
				return fmt.Errorf("error getting default branch for %s: %w", r.FullName, err)
			}
			e := r
			e.SHA = b.Commit.ID
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

func toRepository(owner string, r gtRepo, proto scm.CloneProtocol) scm.Repository {
	u := r.SSHURL
	if proto == scm.CloneProtocolHTTPS {
		u = r.HTMLURL
	}
	return scm.Repository{
		Organization:  owner,
		Name:          r.Name,
		FullName:      owner + "/" + r.Name,
		URL:           u,
		HTTPSURL:      r.HTMLURL,
		SSHURL:        r.SSHURL,
		WebURL:        r.HTMLURL,
		Branch:        r.DefaultBranch,
		DefaultBranch: r.DefaultBranch,
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

// ListBranches implements scm.BranchLister. A missing repository fails with
// KindNotFound.
func (p *Provider) ListBranches(ctx context.Context, ref scm.RepoRef, all bool) ([]scm.Branch, error) {
	const op = "ListBranches"
	base, err := repoPath(op, ref)
	if err != nil {
		return nil, err
	}
	if all {
		bs, err := collect[gtBranch](ctx, p.c, op, base+"/branches", nil)
		if err != nil {
			return nil, err
		}
		out := make([]scm.Branch, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.toBranch())
		}
		return out, nil
	}
	var r gtRepo
	if _, err := p.c.get(ctx, op, base, nil, &r); err != nil {
		return nil, err
	}
	if r.Empty || r.DefaultBranch == "" {
		return []scm.Branch{}, nil
	}
	var b gtBranch
	_, err = p.c.get(ctx, op, base+"/branches/"+escapeSegments(r.DefaultBranch), nil, &b)
	if hasStatus(err, http.StatusNotFound) {
		return []scm.Branch{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []scm.Branch{b.toBranch()}, nil
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ResolveRevision implements scm.RevisionResolver: the ref is looked up as a branch,
// then as a tag, then (when it looks like a hex SHA) as a commit. "refs/heads/" and
// "refs/tags/" prefixes restrict the lookup. Unknown refs map to KindNotFound.
func (p *Provider) ResolveRevision(ctx context.Context, ref scm.RepoRef, rev string) (string, error) {
	const op = "ResolveRevision"
	base, err := repoPath(op, ref)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(rev) == "" {
		return "", invalidErr(op, fmt.Errorf("revision must not be empty"))
	}
	if err := checkNoDotDot(rev); err != nil {
		return "", invalidErr(op, err)
	}
	branchOK, tagOK, commitOK := true, true, hexRE.MatchString(rev)
	name := rev
	switch {
	case strings.HasPrefix(rev, "refs/heads/"):
		name, tagOK, commitOK = strings.TrimPrefix(rev, "refs/heads/"), false, false
	case strings.HasPrefix(rev, "refs/tags/"):
		name, branchOK, commitOK = strings.TrimPrefix(rev, "refs/tags/"), false, false
	}
	if name == "" {
		return "", invalidErr(op, fmt.Errorf("revision %q has an empty name", rev))
	}
	var lastErr error
	if branchOK {
		var b gtBranch
		_, err := p.c.get(ctx, op, base+"/branches/"+escapeSegments(name), nil, &b)
		if err == nil && b.Commit.ID != "" {
			return b.Commit.ID, nil
		}
		if !scm.IsNotFound(err) {
			return "", err
		}
		lastErr = err
	}
	if tagOK {
		var tag struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		_, err := p.c.get(ctx, op, base+"/tags/"+escapeSegments(name), nil, &tag)
		if err == nil && tag.Commit.SHA != "" {
			return tag.Commit.SHA, nil
		}
		if !scm.IsNotFound(err) {
			return "", err
		}
		lastErr = err
	}
	if commitOK {
		var c struct {
			SHA string `json:"sha"`
		}
		_, err := p.c.get(ctx, op, base+"/git/commits/"+url.PathEscape(name), nil, &c)
		if err == nil && c.SHA != "" {
			return c.SHA, nil
		}
		if err != nil && !scm.IsNotFound(err) && !hasStatus(err, http.StatusUnprocessableEntity) {
			return "", err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("revision %q not found", rev)
	}
	return "", &scm.Error{Kind: scm.KindNotFound, Provider: Type, Op: op, StatusCode: http.StatusNotFound, Err: fmt.Errorf("revision %q not found: %w", rev, lastErr)}
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
		segs = append(segs, seg)
	}
	return strings.Join(segs, "/"), nil
}

// PathExists implements scm.PathChecker with GET /repos/{o}/{r}/contents/{path}?ref=:
// files and directories exist (upstream treats the SDK's "expect file, got directory"
// as existing), 404 means missing, including a missing repository or revision. An empty
// revision selects the default branch.
func (p *Provider) PathExists(ctx context.Context, ref scm.RepoRef, revision, path string) (bool, error) {
	const op = "PathExists"
	base, err := repoPath(op, ref)
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
	var q url.Values
	if revision != "" {
		q = url.Values{"ref": {revision}}
	}
	_, err = p.c.get(ctx, op, base+"/contents/"+escapeSegments(clean), q, nil)
	if hasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
