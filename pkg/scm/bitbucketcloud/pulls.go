package bitbucketcloud

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

type bcPull struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"source"`
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"destination"`
	Author struct {
		Nickname    string `json:"nickname"`
		DisplayName string `json:"display_name"`
	} `json:"author"`
	Links struct {
		HTML bcLink `json:"html"`
	} `json:"links"`
}

// ErrLabelsUnsupported is the cause of the KindInvalid error returned when a pull
// request query filters on labels, which Bitbucket Cloud does not have.
var ErrLabelsUnsupported = errors.New("bitbucket cloud pull requests have no labels")

// ListPullRequests implements scm.PullRequestLister with GET
// /repositories/{ws}/{slug}/pullrequests (default state OPEN, as upstream; every page).
// closed lists MERGED, DECLINED and SUPERSEDED pull requests, merged only MERGED, all
// every state; TargetBranch is filtered client-side. Labels are unsupported (no labels
// upstream either): a non-empty PRQuery.Labels fails with KindInvalid wrapping
// ErrLabelsUnsupported. HeadSHA is the source commit hash as returned by Bitbucket
// (abbreviated to 12 characters by the API), Author the author's nickname and State the
// raw Bitbucket state.
func (p *Provider) ListPullRequests(ctx context.Context, ref scm.RepoRef, q scm.PRQuery) ([]scm.PullRequest, error) {
	const op = "ListPullRequests"
	rp, err := repoPath(op, ref)
	if err != nil {
		return nil, err
	}
	if len(q.Labels) > 0 {
		return nil, invalidErr(op, ErrLabelsUnsupported)
	}
	params := url.Values{"pagelen": {strconv.Itoa(prPageLen)}}
	switch q.State {
	case "", scm.PRStateOpen:
		params["state"] = []string{"OPEN"}
	case scm.PRStateMerged:
		params["state"] = []string{"MERGED"}
	case scm.PRStateClosed:
		params["state"] = []string{"MERGED", "DECLINED", "SUPERSEDED"}
	case scm.PRStateAll:
		params["state"] = []string{"OPEN", "MERGED", "DECLINED", "SUPERSEDED"}
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown pull request state %q", q.State))
	}
	pulls, err := collect[bcPull](ctx, p.c, op, p.c.url(rp+"/pullrequests", params))
	if err != nil {
		return nil, err
	}
	out := []scm.PullRequest{}
	for _, pr := range pulls {
		if q.TargetBranch != "" && pr.Destination.Branch.Name != q.TargetBranch {
			continue
		}
		out = append(out, scm.PullRequest{
			Number:       pr.ID,
			Title:        pr.Title,
			Branch:       pr.Source.Branch.Name,
			TargetBranch: pr.Destination.Branch.Name,
			HeadSHA:      pr.Source.Commit.Hash,
			Author:       pr.Author.Nickname,
			URL:          pr.Links.HTML.Href,
			State:        pr.State,
			Draft:        pr.Draft,
			Merged:       pr.State == "MERGED",
		})
	}
	return out, nil
}

// ChangedFiles returns the paths changed between two commits of a repository using the
// diffstat API (GET .../diffstat/{before}..{after}?renames=true, every page), as
// upstream's webhook handler does for verified repo:push events. Both the old and new
// path of every entry are reported (upstream reports only the new path, so deleted and
// renamed-away files are missed). truncated is true when the list was capped or could
// not be computed because a side is a zero SHA (branch creation or deletion); callers
// must then treat every path as changed.
func (p *Provider) ChangedFiles(ctx context.Context, ref scm.RepoRef, before, after string) (files []string, truncated bool, err error) {
	const op = "ChangedFiles"
	rp, err := repoPath(op, ref)
	if err != nil {
		return nil, false, err
	}
	for _, s := range []string{before, after} {
		if isZeroSHA(s) || s == "" {
			return nil, true, nil
		}
		if !hexRE.MatchString(s) {
			return nil, false, invalidErr(op, fmt.Errorf("invalid commit %q", s))
		}
	}
	type side struct {
		Path string `json:"path"`
	}
	stats, err := collect[struct {
		Status string `json:"status"`
		Old    *side  `json:"old"`
		New    *side  `json:"new"`
	}](ctx, p.c, op, p.c.url(rp+"/diffstat/"+before+".."+after, url.Values{"renames": {"true"}, "pagelen": {"500"}}))
	if err != nil {
		return nil, false, err
	}
	var paths []string
	for _, s := range stats {
		if s.New != nil {
			paths = append(paths, s.New.Path)
		}
		if s.Old != nil {
			paths = append(paths, s.Old.Path)
		}
	}
	files, truncated = scm.NormalizeChangedFiles(paths)
	return files, truncated, nil
}

// TouchesMainBranch reports whether revision (a branch name) is the repository's main
// branch, as upstream's webhook handler checks for verified repo:push events.
func (p *Provider) TouchesMainBranch(ctx context.Context, ref scm.RepoRef, revision string) (bool, error) {
	const op = "TouchesMainBranch"
	rp, err := repoPath(op, ref)
	if err != nil {
		return false, err
	}
	main, err := p.mainBranch(ctx, op, rp)
	if err != nil {
		return false, err
	}
	return main != "" && main == revision, nil
}

// APIBaseURLFromRepositoryURL derives the API base URL from a repository API URL
// ("links.self.href" of a webhook payload, Event.Repository.APIURL) by removing
// "/repositories/<fullName>", as upstream does to call back the instance that sent a
// webhook. The input comes from an unauthenticated-body payload: callers must only use
// the result with credentials after checking it against the configured Bitbucket API
// URL (or the ApplicationSet API URL allow-list).
func APIBaseURLFromRepositoryURL(repoAPIURL, fullName string) (string, error) {
	suffix := "/repositories/" + fullName
	if fullName == "" || !strings.HasSuffix(strings.TrimRight(repoAPIURL, "/"), suffix) {
		return "", fmt.Errorf("repository API URL %q does not end with %q", scm.RedactURLString(repoAPIURL), suffix)
	}
	base := strings.TrimSuffix(strings.TrimRight(repoAPIURL, "/"), suffix)
	if _, err := APIBaseURL(base); err != nil {
		return "", err
	}
	return base, nil
}

func isZeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }
