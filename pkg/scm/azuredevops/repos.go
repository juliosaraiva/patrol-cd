package azuredevops

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// list is Azure DevOps' collection envelope.
type list[T any] struct {
	Count int `json:"count"`
	Value []T `json:"value"`
}

type azRepo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"`
	RemoteURL     string `json:"remoteUrl"`
	SSHURL        string `json:"sshUrl"`
	WebURL        string `json:"webUrl"`
	IsDisabled    bool   `json:"isDisabled"`
	IsFork        bool   `json:"isFork"`
	Project       struct {
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
	} `json:"project"`
}

type azBranchStats struct {
	Name   string `json:"name"`
	Commit struct {
		CommitID string `json:"commitId"`
	} `json:"commit"`
}

func (b azBranchStats) toBranch() scm.Branch { return scm.Branch{Name: b.Name, SHA: b.Commit.CommitID} }

func versioned(q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	q.Set("api-version", apiVersion)
	return q
}

// repoPath returns the escaped "{project}/_apis/git/repositories/{repo}" path.
func repoPath(op string, ref scm.RepoRef) (string, error) {
	if err := validateName("project", ref.Owner); err != nil {
		return "", invalidErr(op, err)
	}
	if err := validateName("repository", ref.Name); err != nil {
		return "", invalidErr(op, err)
	}
	return url.PathEscape(ref.Owner) + "/_apis/git/repositories/" + url.PathEscape(ref.Name), nil
}

// ListRepositories implements scm.RepositoryLister with GET
// {project}/_apis/git/repositories. RepoQuery.Organization is the team project
// (upstream teamProject); the organization is fixed per provider instance.
//
// As upstream, repositories missing a name, default branch, remote URL or ID are
// skipped, Labels are empty, and URL is always the remote (HTTPS) URL: the clone
// protocol is validated but otherwise ignored. Disabled repositories (whose branches
// upstream cannot read) are skipped. Repository.Organization is the team project so that
// Repository.Ref() addresses the repository; the generator's `organization` template
// parameter is Provider.Organization(). Azure DevOps has no topics (a Topics filter
// matches nothing) and no archived flag.
func (p *Provider) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	const op = "ListRepositories"
	switch q.CloneProtocol {
	case scm.CloneProtocolDefault, scm.CloneProtocolSSH, scm.CloneProtocolHTTPS:
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown clone protocol for Azure DevOps %v", q.CloneProtocol))
	}
	project := q.Organization
	if err := validateName("project", project); err != nil {
		return nil, invalidErr(op, err)
	}
	var repos list[azRepo]
	if _, err := p.c.get(ctx, op, url.PathEscape(project)+"/_apis/git/repositories", versioned(nil), &repos); err != nil {
		return nil, err
	}
	var kept []scm.Repository
	for _, r := range repos.Value {
		if r.Name == "" || r.DefaultBranch == "" || r.RemoteURL == "" || r.ID == "" || r.IsDisabled || len(q.Topics) > 0 {
			continue
		}
		if validateName("repository", r.Name) != nil {
			continue
		}
		branch := strings.TrimPrefix(r.DefaultBranch, "refs/heads/")
		repo := scm.Repository{
			Organization:  project,
			Name:          r.Name,
			FullName:      project + "/" + r.Name,
			URL:           r.RemoteURL,
			HTTPSURL:      r.RemoteURL,
			SSHURL:        r.SSHURL,
			WebURL:        r.WebURL,
			Branch:        branch,
			DefaultBranch: branch,
			Labels:        []string{},
			RepositoryID:  r.ID,
			Private:       !strings.EqualFold(r.Project.Visibility, "public"),
			Fork:          r.IsFork,
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
			base := url.PathEscape(project) + "/_apis/git/repositories/" + url.PathEscape(r.RepositoryID.(string)) + "/stats/branches"
			if q.AllBranches {
				var bs list[azBranchStats]
				_, err := p.c.get(gctx, op, base, versioned(nil), &bs)
				if hasStatus(err, http.StatusNotFound) {
					return nil // disabled or deleted repository: no branches (upstream)
				}
				if err != nil {
					return fmt.Errorf("failed getting branches from repository %v, project %v: %w", r.Name, project, err)
				}
				for _, b := range bs.Value {
					e := r
					e.Branch, e.SHA = b.Name, b.Commit.CommitID
					results[i] = append(results[i], e)
				}
				return nil
			}
			var b azBranchStats
			_, err := p.c.get(gctx, op, base, versioned(url.Values{"name": {r.DefaultBranch}}), &b)
			if hasStatus(err, http.StatusNotFound) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("could not get default branch %v from repository %v: %w", r.DefaultBranch, r.Name, err)
			}
			if b.Name == "" || b.Commit.CommitID == "" {
				return &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: op,
					Err: fmt.Errorf("invalid branch result after requesting branch %v from repository %v", r.DefaultBranch, r.Name)}
			}
			e := r
			e.Branch, e.SHA = b.Name, b.Commit.CommitID
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

// ListBranches implements scm.BranchLister. A missing project or repository fails with
// KindNotFound.
func (p *Provider) ListBranches(ctx context.Context, ref scm.RepoRef, all bool) ([]scm.Branch, error) {
	const op = "ListBranches"
	rp, err := repoPath(op, ref)
	if err != nil {
		return nil, err
	}
	if all {
		var bs list[azBranchStats]
		if _, err := p.c.get(ctx, op, rp+"/stats/branches", versioned(nil), &bs); err != nil {
			return nil, err
		}
		out := make([]scm.Branch, 0, len(bs.Value))
		for _, b := range bs.Value {
			out = append(out, b.toBranch())
		}
		return out, nil
	}
	var r azRepo
	if _, err := p.c.get(ctx, op, rp, versioned(nil), &r); err != nil {
		return nil, err
	}
	if r.DefaultBranch == "" {
		return []scm.Branch{}, nil
	}
	var b azBranchStats
	_, err = p.c.get(ctx, op, rp+"/stats/branches", versioned(url.Values{"name": {strings.TrimPrefix(r.DefaultBranch, "refs/heads/")}}), &b)
	if hasStatus(err, http.StatusNotFound) {
		return []scm.Branch{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []scm.Branch{b.toBranch()}, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

type azRef struct {
	Name           string `json:"name"`
	ObjectID       string `json:"objectId"`
	PeeledObjectID string `json:"peeledObjectId"`
}

// ResolveRevision implements scm.RevisionResolver. Full SHAs are checked with GET
// .../commits/{sha}; other refs are looked up with GET .../refs?filter= as a branch, then
// as a tag (annotated tags are peeled), following continuation tokens. "refs/heads/" and
// "refs/tags/" prefixes restrict the lookup. Unknown refs map to KindNotFound.
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
	if shaRE.MatchString(rev) {
		var c struct {
			CommitID string `json:"commitId"`
		}
		_, err := p.c.get(ctx, op, rp+"/commits/"+rev, versioned(nil), &c)
		if err == nil && c.CommitID != "" {
			return c.CommitID, nil
		}
		if err != nil && !scm.IsNotFound(err) {
			return "", err
		}
	}
	candidates := []string{"refs/heads/" + rev, "refs/tags/" + rev}
	if strings.HasPrefix(rev, "refs/heads/") || strings.HasPrefix(rev, "refs/tags/") {
		candidates = []string{rev}
	}
	for _, full := range candidates {
		filter := strings.TrimPrefix(full, "refs/")
		if filter == "heads/" || filter == "tags/" {
			return "", invalidErr(op, fmt.Errorf("revision %q has an empty name", rev))
		}
		refs, err := p.listRefs(ctx, op, rp, filter)
		if err != nil {
			return "", err
		}
		for _, r := range refs {
			if r.Name != full {
				continue
			}
			if r.PeeledObjectID != "" {
				return r.PeeledObjectID, nil
			}
			return r.ObjectID, nil
		}
	}
	return "", &scm.Error{Kind: scm.KindNotFound, Provider: Type, Op: op, StatusCode: http.StatusNotFound, Err: fmt.Errorf("revision %q not found", rev)}
}

// listRefs lists refs starting with filter, following x-ms-continuationtoken pages
// with the configured page cap.
func (p *Provider) listRefs(ctx context.Context, op, rp, filter string) ([]azRef, error) {
	out, err := scm.CollectPages(ctx, p.c.maxPages, func(ctx context.Context, token string) ([]azRef, string, error) {
		q := versioned(url.Values{"filter": {filter}, "peelTags": {"true"}})
		if token != "" {
			q.Set("continuationToken", token)
		}
		var refs list[azRef]
		resp, err := p.c.get(ctx, op, rp+"/refs", q, &refs)
		if err != nil {
			return nil, "", err
		}
		return refs.Value, resp.Header.Get("X-Ms-Continuationtoken"), nil
	})
	if err != nil {
		return nil, fillOp(op, err)
	}
	return out, nil
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
	return "/" + strings.Join(segs, "/"), nil
}

// PathExists implements scm.PathChecker with GET .../items?path=&versionDescriptor
// (metadata only). As upstream the revision is a branch name; a full SHA is looked up as
// a commit and "refs/tags/<t>" as a tag. 404 (GitItemNotFoundException, a missing
// repository or revision) means false; an empty revision selects the default branch.
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
	q := url.Values{"path": {clean}, "$format": {"json"}}
	if revision != "" {
		version, versionType := strings.TrimPrefix(revision, "refs/heads/"), "branch"
		switch {
		case shaRE.MatchString(revision):
			versionType = "commit"
		case strings.HasPrefix(revision, "refs/tags/"):
			version, versionType = strings.TrimPrefix(revision, "refs/tags/"), "tag"
		}
		q.Set("versionDescriptor.version", version)
		q.Set("versionDescriptor.versionType", versionType)
	}
	_, err = p.c.get(ctx, op, rp+"/items", versioned(q), nil)
	if hasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
