package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v88/github"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// ListPullRequests implements scm.PullRequestLister: GET /repos/{o}/{r}/pulls, 100 per
// page, every page. Labels are filtered client-side (all must be present, exact match);
// TargetBranch maps to the API "base" filter. A 404 maps to KindNotFound so callers can
// implement continueOnRepoNotFoundError.
func (p *Provider) ListPullRequests(ctx context.Context, ref scm.RepoRef, q scm.PRQuery) ([]scm.PullRequest, error) {
	const op = "ListPullRequests"
	if err := p.validateRef(op, ref); err != nil {
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
		return nil, p.invalid(op, fmt.Errorf("unknown pull request state %q", q.State))
	}
	pulls, err := collect(ctx, p, op, func(ctx context.Context, lo gh.ListOptions) ([]*gh.PullRequest, *gh.Response, error) {
		return p.client.PullRequests.List(ctx, ref.Owner, ref.Name, &gh.PullRequestListOptions{State: state, Base: q.TargetBranch, ListOptions: lo})
	})
	if err != nil {
		return nil, err
	}
	out := []scm.PullRequest{}
	for _, pr := range pulls {
		if pr == nil {
			continue
		}
		if q.State == scm.PRStateMerged && pr.MergedAt == nil {
			continue
		}
		n := toPullRequest(pr)
		if !hasAll(n.Labels, q.Labels) {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func toPullRequest(pr *gh.PullRequest) scm.PullRequest {
	var labels []string
	for _, l := range pr.Labels {
		if l != nil && l.Name != nil {
			labels = append(labels, l.GetName())
		}
	}
	return scm.PullRequest{
		Number:       int64(pr.GetNumber()),
		Title:        pr.GetTitle(),
		Branch:       pr.GetHead().GetRef(),
		TargetBranch: pr.GetBase().GetRef(),
		HeadSHA:      pr.GetHead().GetSHA(),
		Author:       pr.GetUser().GetLogin(),
		Labels:       labels,
		URL:          pr.GetHTMLURL(),
		State:        pr.GetState(),
		Draft:        pr.GetDraft(),
		Merged:       pr.MergedAt != nil || pr.GetMerged(),
	}
}
