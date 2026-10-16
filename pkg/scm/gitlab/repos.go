package gitlab

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

type glNamespace struct {
	FullPath string `json:"full_path"`
}

type glProject struct {
	ID                int64       `json:"id"`
	Name              string      `json:"name"`
	Path              string      `json:"path"`
	PathWithNamespace string      `json:"path_with_namespace"`
	DefaultBranch     string      `json:"default_branch"`
	SSHURLToRepo      string      `json:"ssh_url_to_repo"`
	HTTPURLToRepo     string      `json:"http_url_to_repo"`
	WebURL            string      `json:"web_url"`
	Topics            []string    `json:"topics"`
	TagList           []string    `json:"tag_list"`
	Archived          bool        `json:"archived"`
	Visibility        string      `json:"visibility"`
	Namespace         glNamespace `json:"namespace"`
	ForkedFromProject *struct {
		ID int64 `json:"id"`
	} `json:"forked_from_project"`
}

type glBranch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

func (b glBranch) toBranch() scm.Branch {
	return scm.Branch{Name: b.Name, SHA: b.Commit.ID, Protected: b.Protected}
}

var numericRE = regexp.MustCompile(`^[0-9]{1,19}$`)

// validatePath validates a namespace path ("group/subgroup") segment by segment.
func validatePath(what, p string) error {
	if p == "" || len(p) > 1024 {
		return fmt.Errorf("invalid %s path %q", what, p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		if err := scm.ValidateSegment(what, seg); err != nil {
			return err
		}
	}
	return nil
}

// groupID returns the escaped API identifier of a group given as numeric ID or full path.
func groupID(op, group string) (string, error) {
	group = strings.Trim(strings.TrimSpace(group), "/")
	if numericRE.MatchString(group) {
		return group, nil
	}
	if err := validatePath("group", group); err != nil {
		return "", invalidErr(op, err)
	}
	return url.PathEscape(group), nil
}

// projectID returns the escaped API identifier of a project: the numeric ID for
// RepoRef{Name: "<id>"}, otherwise the URL-encoded "owner/name" path.
func projectID(op string, ref scm.RepoRef) (string, error) {
	if ref.Owner == "" {
		if numericRE.MatchString(ref.Name) {
			return ref.Name, nil
		}
		return "", invalidErr(op, fmt.Errorf("project %q needs a namespace or a numeric ID", ref.Name))
	}
	if err := validatePath("namespace", ref.Owner); err != nil {
		return "", invalidErr(op, err)
	}
	if err := scm.ValidateSegment("project", ref.Name); err != nil {
		return "", invalidErr(op, err)
	}
	return url.PathEscape(ref.Owner + "/" + ref.Name), nil
}

// collect follows X-Next-Page pagination (100 per page) with the configured page cap. On
// error no partial result is returned.
func collect[T any](ctx context.Context, c *client, op, path string, q url.Values) ([]T, error) {
	out, err := scm.CollectPages(ctx, c.maxPages, func(ctx context.Context, page int) ([]T, int, error) {
		qq := cloneValues(q)
		qq.Set("per_page", strconv.Itoa(perPage))
		if page > 0 {
			qq.Set("page", strconv.Itoa(page))
		}
		var items []T
		resp, err := c.get(ctx, op, path, qq, &items)
		if err != nil {
			return nil, 0, err
		}
		return items, nextPage(resp.Header), nil
	})
	if err != nil {
		return nil, fillOp(op, err)
	}
	return out, nil
}

// ListRepositories implements scm.RepositoryLister with GET /groups/:id/projects.
//
// Query mapping: Organization is the group (numeric ID or full path); Topics are sent as
// the "topic" filter (comma-separated, matched by GitLab); Archived maps to the
// "archived" parameter exactly as upstream sends it (ArchivedInclude → archived=true,
// ArchivedExclude → archived=false and archived projects are also dropped client-side,
// ArchivedDefault → parameter omitted). Options.IncludeSubgroups and
// Options.IncludeSharedProjects map to include_subgroups and with_shared. Labels are the
// project topics, falling back to the legacy tag list.
func (p *Provider) ListRepositories(ctx context.Context, q scm.RepoQuery) ([]scm.Repository, error) {
	const op = "ListRepositories"
	switch q.CloneProtocol {
	case scm.CloneProtocolDefault, scm.CloneProtocolSSH, scm.CloneProtocolHTTPS:
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown clone protocol for Gitlab %v", q.CloneProtocol))
	}
	gid, err := groupID(op, q.Organization)
	if err != nil {
		return nil, err
	}
	params := url.Values{
		"include_subgroups": {strconv.FormatBool(p.opts.IncludeSubgroups)},
		"with_shared":       {strconv.FormatBool(p.opts.sharedProjects())},
	}
	if len(q.Topics) > 0 {
		params.Set("topic", strings.Join(q.Topics, ","))
	}
	switch q.Archived {
	case scm.ArchivedInclude:
		params.Set("archived", "true")
	case scm.ArchivedExclude:
		params.Set("archived", "false")
	}
	projects, err := collect[glProject](ctx, p.c, op, "groups/"+gid+"/projects", params)
	if err != nil {
		return nil, err
	}

	type cand struct {
		repo scm.Repository
		id   int64
	}
	var kept []cand
	for _, pr := range projects {
		if q.Archived == scm.ArchivedExclude && pr.Archived {
			continue
		}
		r := toRepository(pr, q.CloneProtocol)
		if q.Match != nil && !q.Match(r) {
			continue
		}
		kept = append(kept, cand{repo: r, id: pr.ID})
	}

	results := make([][]scm.Repository, len(kept))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(branchConcurrency)
	for i, k := range kept {
		g.Go(func() error {
			pid := strconv.FormatInt(k.id, 10)
			if q.AllBranches {
				branches, err := collect[glBranch](gctx, p.c, op, "projects/"+pid+"/repository/branches", nil)
				if hasStatus(err, http.StatusNotFound) {
					return nil // upstream: a 404 is not an error here
				}
				if err != nil {
					return fmt.Errorf("error listing branches for %s: %w", k.repo.FullName, err)
				}
				for _, b := range branches {
					e := k.repo
					e.Branch, e.SHA = b.Name, b.Commit.ID
					results[i] = append(results[i], e)
				}
				return nil
			}
			if k.repo.DefaultBranch == "" {
				return nil // empty repository
			}
			var b glBranch
			_, err := p.c.get(gctx, op, "projects/"+pid+"/repository/branches/"+url.PathEscape(k.repo.DefaultBranch), nil, &b)
			if hasStatus(err, http.StatusNotFound) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("error getting default branch for %s: %w", k.repo.FullName, err)
			}
			e := k.repo
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

func toRepository(pr glProject, proto scm.CloneProtocol) scm.Repository {
	u := pr.SSHURLToRepo
	if proto == scm.CloneProtocolHTTPS {
		u = pr.HTTPURLToRepo
	}
	labels := pr.Topics
	if len(labels) == 0 {
		labels = pr.TagList // GitLab before 14.5
	}
	org := pr.Namespace.FullPath
	if org == "" {
		if i := strings.LastIndexByte(pr.PathWithNamespace, '/'); i > 0 {
			org = pr.PathWithNamespace[:i]
		}
	}
	return scm.Repository{
		Organization:  org,
		Name:          pr.Path,
		FullName:      pr.PathWithNamespace,
		URL:           u,
		HTTPSURL:      pr.HTTPURLToRepo,
		SSHURL:        pr.SSHURLToRepo,
		WebURL:        pr.WebURL,
		Branch:        pr.DefaultBranch,
		DefaultBranch: pr.DefaultBranch,
		Labels:        slices.Clone(labels),
		RepositoryID:  pr.ID,
		Archived:      pr.Archived,
		Private:       pr.Visibility != "" && pr.Visibility != "public",
		Fork:          pr.ForkedFromProject != nil,
	}
}

// ListBranches implements scm.BranchLister. A missing project fails with KindNotFound.
func (p *Provider) ListBranches(ctx context.Context, ref scm.RepoRef, all bool) ([]scm.Branch, error) {
	const op = "ListBranches"
	pid, err := projectID(op, ref)
	if err != nil {
		return nil, err
	}
	if all {
		bs, err := collect[glBranch](ctx, p.c, op, "projects/"+pid+"/repository/branches", nil)
		if err != nil {
			return nil, err
		}
		out := make([]scm.Branch, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.toBranch())
		}
		return out, nil
	}
	var pr glProject
	if _, err := p.c.get(ctx, op, "projects/"+pid, nil, &pr); err != nil {
		return nil, err
	}
	if pr.DefaultBranch == "" {
		return []scm.Branch{}, nil
	}
	var b glBranch
	_, err = p.c.get(ctx, op, "projects/"+pid+"/repository/branches/"+url.PathEscape(pr.DefaultBranch), nil, &b)
	if hasStatus(err, http.StatusNotFound) {
		return []scm.Branch{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []scm.Branch{b.toBranch()}, nil
}

// ResolveRevision implements scm.RevisionResolver with GET
// /projects/:id/repository/commits/:ref (branch, tag or SHA; refs/heads/ and refs/tags/
// prefixes are stripped). Unknown refs map to KindNotFound.
func (p *Provider) ResolveRevision(ctx context.Context, ref scm.RepoRef, rev string) (string, error) {
	const op = "ResolveRevision"
	pid, err := projectID(op, ref)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(rev) == "" {
		return "", invalidErr(op, fmt.Errorf("revision must not be empty"))
	}
	if err := checkNoDotDot(rev); err != nil {
		return "", invalidErr(op, err)
	}
	short := strings.TrimPrefix(strings.TrimPrefix(rev, "refs/heads/"), "refs/tags/")
	var c struct {
		ID string `json:"id"`
	}
	if _, err := p.c.get(ctx, op, "projects/"+pid+"/repository/commits/"+url.PathEscape(short), nil, &c); err != nil {
		return "", err
	}
	if !shaRE.MatchString(c.ID) {
		return "", &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: op, Err: fmt.Errorf("unexpected commit ID in response (%d bytes)", len(c.ID))}
	}
	return c.ID, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func checkNoDotDot(p string) error {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q must not contain '..' segments", p)
		}
	}
	return nil
}

// cleanRepoPath rejects '..' segments, drops empty and '.' segments and trims slashes.
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

// PathExists implements scm.PathChecker like upstream: a file is looked up with the
// Repository Files API (HEAD, so no content is downloaded; GET when HEAD is not
// allowed), and when no file exists the Repository Tree API decides whether a directory
// exists. A missing project, revision or path yields false. An empty revision means the
// default branch (HEAD).
func (p *Provider) PathExists(ctx context.Context, ref scm.RepoRef, revision, path string) (bool, error) {
	const op = "PathExists"
	pid, err := projectID(op, ref)
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
		revision = "HEAD"
	}
	refQ := url.Values{"ref": {revision}}
	if clean != "" {
		fileURL := p.c.url("projects/"+pid+"/repository/files/"+url.PathEscape(clean), refQ)
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			_, err := p.c.do(ctx, op, method, fileURL, nil)
			if err == nil {
				return true, nil
			}
			if method == http.MethodHead && hasStatus(err, http.StatusMethodNotAllowed, http.StatusNotImplemented) {
				continue
			}
			if !hasStatus(err, http.StatusNotFound) {
				return false, err
			}
			break
		}
	}
	tq := url.Values{"ref": {revision}, "per_page": {"1"}}
	if clean != "" {
		tq.Set("path", clean)
	}
	var entries []struct {
		Name string `json:"name"`
	}
	_, err = p.c.get(ctx, op, "projects/"+pid+"/repository/tree", tq, &entries)
	if hasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Git does not track empty directories; older GitLab versions answer an unknown path
	// with an empty list instead of 404.
	return len(entries) > 0, nil
}
