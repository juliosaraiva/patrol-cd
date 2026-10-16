package gitea

import (
	"context"
	"fmt"
	"net/url"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

type gtPull struct {
	Number  int64     `json:"number"`
	Title   string    `json:"title"`
	State   string    `json:"state"`
	HTMLURL string    `json:"html_url"`
	Merged  bool      `json:"merged"`
	Draft   bool      `json:"draft"`
	User    gtUser    `json:"user"`
	Labels  []gtLabel `json:"labels"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// ListPullRequests implements scm.PullRequestLister with GET /repos/{o}/{r}/pulls
// (every page). Upstream lists open pull requests only, which is the default; closed
// includes merged pull requests (Gitea's "closed" state), merged keeps closed ones with
// merged=true, all lists every state. Labels (all required, exact) and TargetBranch
// are filtered client-side. Author is the poster's login; State is Gitea's raw state.
func (p *Provider) ListPullRequests(ctx context.Context, ref scm.RepoRef, q scm.PRQuery) ([]scm.PullRequest, error) {
	const op = "ListPullRequests"
	base, err := repoPath(op, ref)
	if err != nil {
		return nil, err
	}
	state := ""
	switch q.State {
	case "", scm.PRStateOpen:
		state = "open"
	case scm.PRStateClosed, scm.PRStateMerged:
		state = "closed"
	case scm.PRStateAll:
		state = "all"
	default:
		return nil, invalidErr(op, fmt.Errorf("unknown pull request state %q", q.State))
	}
	pulls, err := collect[gtPull](ctx, p.c, op, base+"/pulls", url.Values{"state": {state}})
	if err != nil {
		return nil, err
	}
	out := []scm.PullRequest{}
	for _, pr := range pulls {
		if q.State == scm.PRStateMerged && !pr.Merged {
			continue
		}
		if q.TargetBranch != "" && pr.Base.Ref != q.TargetBranch {
			continue
		}
		var labels []string
		for _, l := range pr.Labels {
			labels = append(labels, l.Name)
		}
		if !hasAll(labels, q.Labels) {
			continue
		}
		out = append(out, scm.PullRequest{
			Number:       pr.Number,
			Title:        pr.Title,
			Branch:       pr.Head.Ref,
			TargetBranch: pr.Base.Ref,
			HeadSHA:      pr.Head.SHA,
			Author:       pr.User.name(),
			Labels:       labels,
			URL:          pr.HTMLURL,
			State:        pr.State,
			Draft:        pr.Draft,
			Merged:       pr.Merged,
		})
	}
	return out, nil
}
